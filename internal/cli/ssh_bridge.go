package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xiaot623/sshx/internal/bridge"
	"github.com/xiaot623/sshx/internal/locald"
	"github.com/xiaot623/sshx/internal/protocol"
	"github.com/xiaot623/sshx/internal/remotefs"
)

type sshProxy struct {
	conn   io.ReadWriteCloser
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	waitCh chan error
}

func (r *Runner) startSSHProxy(ctx context.Context, sshArgs []string, remoteCommand string) (*sshProxy, error) {
	cmd := exec.CommandContext(ctx, r.SSHPath, sshCommandArgs(sshArgs, remoteCommand)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	cmd.Stderr = r.Stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, err
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	conn := bridge.NewReadWriteCloser(stdout, stdin, stdin.Close)
	return &sshProxy{conn: conn, cmd: cmd, stdin: stdin, waitCh: waitCh}, nil
}

func (p *sshProxy) stop() {
	if p == nil {
		return
	}
	_ = p.conn.Close()
	select {
	case <-p.waitCh:
	case <-time.After(time.Second):
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		select {
		case <-p.waitCh:
		case <-time.After(time.Second):
		}
	}
}

func (r *Runner) defaultStartBridge(ctx context.Context, target string, sshArgs []string, remoteHome string) (*BridgeSession, error) {
	token, err := r.fetchRemoteToken(ctx, sshArgs, remoteHome)
	if err != nil {
		return nil, err
	}
	localDaemonSocket := defaultLocalDaemonSocketPath()
	sessionID, err := generateUUID()
	if err != nil {
		return nil, err
	}
	var localSession *locald.Session
	if r.autoForward {
		if err := r.ensureLocalDaemon(ctx, localDaemonSocket); err != nil {
			return nil, err
		}
		localSession, err = locald.OpenSession(ctx, localDaemonSocket, locald.Request{
			SSHPath:      r.SSHPath,
			Target:       target,
			SSHArgs:      append([]string(nil), sshArgs...),
			DomainSuffix: domainSuffix(),
			DNSAddr:      domainDNSAddr(),
			SessionID:    sessionID,
			AppVersion:   clientVersion(),
		}, locald.DefaultHeartbeatInterval)
		if err != nil {
			return nil, err
		}
	}
	var lifecycleOnce sync.Once
	closeLifecycle := func() {
		lifecycleOnce.Do(func() {
			if localSession != nil {
				_ = localSession.Close()
			}
		})
	}
	bridgeCtx, cancel := context.WithCancel(ctx)
	if localSession != nil {
		go func() {
			select {
			case <-localSession.Done():
				cancel()
			case <-bridgeCtx.Done():
			}
		}()
	}
	controlProxy, err := r.startSSHProxy(
		bridgeCtx,
		sshArgs,
		remoteShell(remoteServerEnvScript(remoteHome)+"; exec \"$SSHX_SERVER_HOME/sshx\" socket-proxy --socket \"$SSHX_SERVER_HOME/sock\""),
	)
	if err != nil {
		cancel()
		closeLifecycle()
		return nil, err
	}
	readyCh := make(chan error, 1)
	errCh := make(chan error, 1)
	var autoForwardStopped atomic.Bool
	var fsMu sync.RWMutex
	var fsPeer *remotefs.Peer
	go func() {
		opts := bridge.ClientOptions{
			Ready:      readyCh,
			AppVersion: clientVersion(),
			SessionID:  sessionID,
			Allow: func(argv []string) bool {
				return r.commandBridge && r.commandPolicy.Allows(argv)
			},
			Execute: func(commandCtx context.Context, frame protocol.Frame) protocol.Frame {
				if !frame.RemoteFS {
					return bridge.ExecuteLocal(commandCtx, frame)
				}
				fsMu.RLock()
				peer := fsPeer
				fsMu.RUnlock()
				if peer == nil {
					return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: "remote fs data session is unavailable"}
				}
				return r.executeLocalWithRemoteFS(commandCtx, frame, peer)
			},
		}
		if r.autoForward {
			opts.OnPortObserved = func(port int) {
				if autoForwardStopped.Load() {
					return
				}
				_, err := locald.ClientRequest(bridgeCtx, localDaemonSocket, locald.Request{
					Type:         locald.TypeEnsureTargetPort,
					SSHPath:      r.SSHPath,
					Target:       target,
					SSHArgs:      append([]string(nil), sshArgs...),
					RemotePort:   port,
					SessionID:    sessionID,
					DomainSuffix: domainSuffix(),
					DNSAddr:      domainDNSAddr(),
				})
				if err != nil {
					fmt.Fprintf(r.Stderr, "sshx: forward remote port %d: %v\n", port, err)
				}
			}
			opts.OnPortGone = func(port int) {
				if autoForwardStopped.Load() {
					return
				}
				_, err := locald.ClientRequest(bridgeCtx, localDaemonSocket, locald.Request{
					Type:       locald.TypeRemoveTargetPort,
					SSHPath:    r.SSHPath,
					Target:     target,
					SSHArgs:    append([]string(nil), sshArgs...),
					RemotePort: port,
					SessionID:  sessionID,
				})
				if err != nil {
					fmt.Fprintf(r.Stderr, "sshx: remove remote port %d: %v\n", port, err)
				}
			}
		}
		errCh <- bridge.RunClientConnWithOptions(bridgeCtx, controlProxy.conn, opts, token)
		cancel()
		closeLifecycle()
	}()
	select {
	case err := <-readyCh:
		if err != nil {
			cancel()
			closeLifecycle()
			controlProxy.stop()
			return nil, err
		}
	case err := <-errCh:
		cancel()
		closeLifecycle()
		controlProxy.stop()
		return nil, err
	case <-time.After(2 * time.Second):
		cancel()
		closeLifecycle()
		controlProxy.stop()
		return nil, errors.New("timed out waiting for command bridge handshake")
	}

	var dataProxy *sshProxy
	var workspaceBackend *remotefs.RootBackend
	workspaceMountID := "workspace"
	workspace := ""
	if r.remoteFS {
		dataProxy, err = r.startSSHProxy(
			bridgeCtx,
			sshArgs,
			remoteShell(remoteServerEnvScript(remoteHome)+"; exec \"$SSHX_SERVER_HOME/sshx\" socket-proxy --socket \"$SSHX_SERVER_HOME/sock.fs\""),
		)
		if err == nil {
			var peer *remotefs.Peer
			peer, err = remotefs.Connect(bridgeCtx, dataProxy.conn, sessionID, token, remotefs.PeerOptions{})
			if err == nil {
				fsMu.Lock()
				fsPeer = peer
				fsMu.Unlock()
				go func() {
					<-peer.Done()
					cancel()
				}()
			}
		}
		if err == nil {
			var cwd string
			cwd, err = os.Getwd()
			if err == nil {
				workspaceBackend, err = remotefs.OpenRootBackend(cwd)
			}
		}
		if err == nil {
			err = fsPeer.RegisterBackend(workspaceMountID, workspaceBackend)
		}
		if err == nil {
			mountCtx, mountCancel := context.WithTimeout(bridgeCtx, 10*time.Second)
			workspace, err = fsPeer.CreateMount(mountCtx, workspaceMountID)
			mountCancel()
		}
		if err != nil {
			if workspaceBackend != nil {
				_ = workspaceBackend.CloseBackend()
			}
			if fsPeer != nil {
				_ = fsPeer.Close()
			}
			cancel()
			closeLifecycle()
			if dataProxy != nil {
				dataProxy.stop()
			}
			controlProxy.stop()
			return nil, fmt.Errorf("remote fs: %w", err)
		}
	}

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			autoForwardStopped.Store(true)
			fsMu.RLock()
			peer := fsPeer
			fsMu.RUnlock()
			if peer != nil {
				releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
				if workspace != "" {
					_ = peer.ReleaseMount(releaseCtx, workspaceMountID)
				}
				releaseCancel()
				_ = peer.UnregisterBackend(workspaceMountID)
				_ = peer.Close()
			}
			cancel()
			closeLifecycle()
			if dataProxy != nil {
				dataProxy.stop()
			}
			controlProxy.stop()
			select {
			case <-errCh:
			default:
			}
		})
	}
	return &BridgeSession{SessionID: sessionID, Workspace: workspace, Done: bridgeCtx.Done(), stop: stop}, nil
}

func (r *Runner) executeLocalWithRemoteFS(ctx context.Context, frame protocol.Frame, peer *remotefs.Peer) protocol.Frame {
	if !safeMountComponent(frame.MountID) || !safeMountComponent(frame.SessionID) || !safeMountComponent(frame.ID) {
		return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: "remote fs command is missing mount/session identity"}
	}
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	mountPath := filepath.Join(base, fmt.Sprintf("sshx-%d", os.Getuid()), "mounts", frame.SessionID, frame.ID, "workspace")
	requestMountRoot := filepath.Dir(mountPath)
	driver := remotefs.GoFuseDriver{}
	mount, err := driver.Mount(ctx, mountPath, peer.RemoteBackend(frame.MountID), remotefs.MountOptions{})
	if err != nil {
		return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: fmt.Sprintf("mount remote workspace: %v", err)}
	}
	frame.Cwd = mount.Path()
	if frame.Env == nil {
		frame.Env = map[string]string{}
	}
	frame.Env["SSHX_WORKSPACE"] = mount.Path()
	response := bridge.ExecuteLocal(ctx, frame)
	unmountCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	unmountErr := mount.Unmount(unmountCtx)
	cancel()
	if unmountErr == nil {
		_ = os.RemoveAll(requestMountRoot)
	}
	if unmountErr != nil && response.Type == protocol.TypeCommandResult && response.ExitCode == 0 {
		return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: fmt.Sprintf("unmount remote workspace: %v", unmountErr)}
	}
	if unmountErr != nil {
		warning := fmt.Sprintf("sshx: unmount remote workspace: %v", unmountErr)
		fmt.Fprintln(r.Stderr, warning)
		if response.Type == protocol.TypeCommandResult {
			stderr, _ := base64.StdEncoding.DecodeString(response.Stderr)
			if len(stderr) > 0 && stderr[len(stderr)-1] != '\n' {
				stderr = append(stderr, '\n')
			}
			stderr = append(stderr, warning...)
			stderr = append(stderr, '\n')
			response.Stderr = base64.StdEncoding.EncodeToString(stderr)
		} else if response.Error != "" {
			response.Error += "; " + warning
		}
	}
	return response
}

func safeMountComponent(value string) bool {
	return value != "" && value != "." && value != ".." &&
		filepath.Base(value) == value && !strings.ContainsAny(value, `/\`)
}

func (r *Runner) fetchRemoteToken(ctx context.Context, sshArgs []string, remoteHome string) (string, error) {
	cmd := exec.CommandContext(ctx, r.SSHPath, internalSSHArgs(sshArgs, remoteShell(remoteServerEnvScript(remoteHome)+"; cat \"$SSHX_SERVER_HOME/server-info\""))...)
	b, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var info bridge.ServerInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return "", err
	}
	return info.Token, nil
}

func (r *Runner) ensureLocalDaemon(ctx context.Context, socketPath string) error {
	if resp, err := locald.ClientRequest(ctx, socketPath, locald.Request{Type: locald.TypePing}); err == nil {
		if resp.Version == clientVersion() && resp.ProtocolVersion == protocol.Version {
			return nil
		}
		_, _ = locald.ClientRequest(ctx, socketPath, locald.Request{Type: locald.TypeShutdown, AppVersion: clientVersion(), ProtocolVersion: protocol.Version})
		if !waitForSocketRemoval(ctx, socketPath, time.Second) {
			terminateLegacyLocalDaemons(socketPath)
			_ = waitForSocketRemoval(ctx, socketPath, time.Second)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(filepath.Dir(socketPath), "local-daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(context.Background(), exe, "local-daemon", "--socket", socketPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if resp, err := locald.ClientRequest(ctx, socketPath, locald.Request{Type: locald.TypePing}); err == nil {
			if resp.Version == clientVersion() && resp.ProtocolVersion == protocol.Version {
				_ = logFile.Close()
				return nil
			}
			lastErr = fmt.Errorf("local daemon version is %q/protocol %d", resp.Version, resp.ProtocolVersion)
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = logFile.Close()
	if lastErr != nil {
		return lastErr
	}
	return errors.New("local daemon did not start")
}

func waitForSocketRemoval(ctx context.Context, path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, socketErr := os.Stat(path)
		_, lockErr := os.Stat(path + ".lock")
		if errors.Is(socketErr, os.ErrNotExist) && errors.Is(lockErr, os.ErrNotExist) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(25 * time.Millisecond):
		}
	}
	return false
}

func terminateLegacyLocalDaemons(socketPath string) {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return
	}
	needle := "local-daemon --socket " + socketPath
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		pidText, command, ok := strings.Cut(line, " ")
		if !ok || !strings.Contains(strings.TrimSpace(command), needle) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidText))
		if err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Signal(os.Interrupt)
		}
	}
}
