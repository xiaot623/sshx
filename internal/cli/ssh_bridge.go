package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

func (r *Runner) defaultStartBridge(ctx context.Context, target string, sshArgs []string, remoteHome string) (func(), error) {
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
	cmd := exec.CommandContext(
		bridgeCtx,
		r.SSHPath,
		sshCommandArgs(sshArgs, remoteShell(remoteServerEnvScript(remoteHome)+"; exec \"$SSHX_SERVER_HOME/sshx\" socket-proxy --socket \"$SSHX_SERVER_HOME/sock\""))...,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		closeLifecycle()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		closeLifecycle()
		return nil, err
	}
	cmd.Stderr = r.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		closeLifecycle()
		return nil, err
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	conn := bridge.NewReadWriteCloser(stdout, stdin, func() error {
		_ = stdin.Close()
		cancel()
		return nil
	})
	readyCh := make(chan error, 1)
	errCh := make(chan error, 1)
	var autoForwardStopped atomic.Bool
	go func() {
		opts := bridge.ClientOptions{
			Ready:      readyCh,
			AppVersion: clientVersion(),
			SessionID:  sessionID,
			Allow: func(argv []string) bool {
				return r.commandBridge && r.commandPolicy.Allows(argv)
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
		errCh <- bridge.RunClientConnWithOptions(bridgeCtx, conn, opts, token)
		closeLifecycle()
	}()
	select {
	case err := <-readyCh:
		if err != nil {
			cancel()
			closeLifecycle()
			return nil, err
		}
	case err := <-errCh:
		cancel()
		closeLifecycle()
		return nil, err
	case <-time.After(2 * time.Second):
		cancel()
		closeLifecycle()
		return nil, errors.New("timed out waiting for command bridge handshake")
	}
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			autoForwardStopped.Store(true)
			cancel()
			closeLifecycle()
			_ = stdin.Close()
			select {
			case <-errCh:
			case <-time.After(time.Second):
				_ = cmd.Process.Kill()
			}
			select {
			case <-waitCh:
			case <-time.After(time.Second):
			}
		})
	}
	return stop, nil
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
