package cli

import (
	"context"
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
	"syscall"
	"time"

	"github.com/xiaot623/sshx/internal/bridge"
	"github.com/xiaot623/sshx/internal/identity"
	"github.com/xiaot623/sshx/internal/locald"
	sshmux "github.com/xiaot623/sshx/internal/mux"
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
	connection := r.connection
	if connection.TargetID == "" {
		connection.TargetID = identity.TargetID(identity.Target{User: "unknown", Hostname: target, Port: 22})
	}
	if connection.ContextID == "" {
		connection.ContextID = identity.ContextID("direct", connection.TargetID, "cli")
	}
	if connection.SessionID == "" {
		connection.SessionID, err = generateUUID()
		if err != nil {
			return nil, err
		}
	}
	sessionID := connection.SessionID
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
			LeaseID:      sessionID,
			TargetID:     connection.TargetID,
			ControlPath:  sshControlPath(sshArgs),
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
		remoteShell(remoteServerEnvScript(remoteHome)+"; exec \"$SSHX_SERVER_HOME/sshx\" mux-proxy --control \"$SSHX_SERVER_HOME/sock\" --fs \"$SSHX_SERVER_HOME/sock.fs\""),
	)
	if err != nil {
		cancel()
		closeLifecycle()
		return nil, err
	}
	muxSession := sshmux.New(controlProxy.conn)
	readyCh := make(chan error, 1)
	errCh := make(chan error, 1)
	var autoForwardStopped atomic.Bool
	var fsMu sync.RWMutex
	var fsPeer *remotefs.Peer
	var mountManager *remoteMountManager
	var workspaceBackend *remotefs.RootBackend
	workspaceMountID := "workspace"
	mountRoot := ""
	workspace := ""
	readOnly := os.Getenv("FS_READ_ONLY") == "1"
	if r.remoteFS {
		mountManager = newRemoteMountManager(sessionID, readOnly)
	}
	go func() {
		opts := bridge.ClientOptions{
			Ready:      readyCh,
			AppVersion: clientVersion(),
			RuntimeID:  identity.RuntimeID,
			TargetID:   connection.TargetID,
			ContextID:  connection.ContextID,
			SessionID:  sessionID,
			Allow: func(argv []string) bool {
				return (r.commandBridge || r.remoteFS) && r.commandPolicy.Allows(argv)
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
				if mountManager == nil {
					return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: "remote fs mount manager is unavailable"}
				}
				return mountManager.Execute(commandCtx, frame, peer)
			},
		}
		opts.Capabilities = []string{"command.exec.batch-stdin", "heartbeat.v1"}
		if r.remoteFS {
			opts.Capabilities = append(opts.Capabilities, "remotefs.fs.v1")
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
					LeaseID:      sessionID,
					TargetID:     connection.TargetID,
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
					LeaseID:    sessionID,
					TargetID:   connection.TargetID,
				})
				if err != nil {
					fmt.Fprintf(r.Stderr, "sshx: remove remote port %d: %v\n", port, err)
				}
			}
		}
		errCh <- bridge.RunClientConnWithOptions(bridgeCtx, muxSession.Channel(sshmux.ChannelControl), opts, token)
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

	if r.remoteFS {
		var peer *remotefs.Peer
		peer, err = remotefs.Connect(bridgeCtx, muxSession.Channel(sshmux.ChannelFS), sessionID, token, remotefs.PeerOptions{})
		if err != nil {
			cancel()
			closeLifecycle()
			_ = muxSession.Close()
			controlProxy.stop()
			return nil, fmt.Errorf("remote fs: %w", err)
		}
		fsMu.Lock()
		fsPeer = peer
		fsMu.Unlock()
		go func() {
			<-peer.Done()
			cancel()
		}()
		if !r.integrationSidecar {
			cwd, cwdErr := os.Getwd()
			if cwdErr != nil {
				err = cwdErr
			} else if within, withinErr := remotefs.PathWithin(localReverseMountsRoot(), cwd); withinErr != nil {
				err = withinErr
			} else if within {
				err = bridge.ErrMountedCwd
			}
			if err == nil {
				var layout remotefs.ExportLayout
				layout, err = remotefs.CurrentExportLayout(cwd)
				if err == nil {
					workspaceBackend, err = remotefs.OpenRootBackendWithOptions(layout.RootPath, remotefs.RootBackendOptions{DisableDelete: true})
				}
				if err == nil {
					err = peer.RegisterBackend(workspaceMountID, workspaceBackend)
				}
				if err == nil {
					mountCtx, mountCancel := context.WithTimeout(bridgeCtx, 10*time.Second)
					mountRoot, err = peer.CreateMountAtWithOptions(mountCtx, workspaceMountID, layout.MountPath, remotefs.MountOptions{ReadOnly: readOnly})
					mountCancel()
				}
				if err == nil {
					workspace, err = remotefs.WorkspacePathBelow(mountRoot, filepath.ToSlash(layout.RelativeCwd))
				}
			}
			if err != nil {
				if mountRoot != "" {
					releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
					_ = peer.ReleaseMount(releaseCtx, workspaceMountID)
					releaseCancel()
				}
				_ = peer.UnregisterBackend(workspaceMountID)
				if workspaceBackend != nil {
					_ = workspaceBackend.CloseBackend()
				}
				if mountManager != nil {
					mountManager.Close()
				}
				_ = peer.Close()
				cancel()
				closeLifecycle()
				_ = muxSession.Close()
				controlProxy.stop()
				return nil, fmt.Errorf("remote fs: %w", err)
			}
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
				if mountRoot != "" {
					releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 2*time.Second)
					_ = peer.ReleaseMount(releaseCtx, workspaceMountID)
					releaseCancel()
				}
				if workspaceBackend != nil {
					_ = peer.UnregisterBackend(workspaceMountID)
				}
				_ = peer.Close()
			}
			if workspaceBackend != nil {
				_ = workspaceBackend.CloseBackend()
			}
			if mountManager != nil {
				mountManager.Close()
			}
			cancel()
			_ = muxSession.Close()
			closeLifecycle()
			controlProxy.stop()
			select {
			case <-errCh:
			default:
			}
		})
	}
	return &BridgeSession{SessionID: sessionID, ContextID: connection.ContextID, RemoteFS: r.remoteFS, MountRoot: mountRoot, Workspace: workspace, ReadOnly: readOnly, Done: bridgeCtx.Done(), stop: stop}, nil
}

func sshControlPath(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-S" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, "-S") && len(arg) > 2 {
			return arg[2:]
		}
		if arg == "-o" && i+1 < len(args) {
			if key, value, ok := strings.Cut(args[i+1], "="); ok && strings.EqualFold(key, "ControlPath") {
				return value
			}
			i++
		}
	}
	return ""
}

type remoteMountEntry struct {
	mount     remotefs.Mount
	mountPath string
}

type remoteMountManager struct {
	sessionID string
	readOnly  bool
	rootPath  string
	lease     *os.File
	initErr   error
	mu        sync.Mutex
	mounts    map[string]remoteMountEntry
	closing   bool
	active    sync.WaitGroup
}

func newRemoteMountManager(sessionID string, readOnly bool) *remoteMountManager {
	m := &remoteMountManager{sessionID: sessionID, readOnly: readOnly, mounts: map[string]remoteMountEntry{}}
	root := localReverseMountsRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		m.initErr = err
		return m
	}
	cleanupLock, err := os.OpenFile(filepath.Join(root, ".cleanup.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		m.initErr = err
		return m
	}
	defer cleanupLock.Close()
	if err := syscall.Flock(int(cleanupLock.Fd()), syscall.LOCK_EX); err != nil {
		m.initErr = err
		return m
	}
	defer syscall.Flock(int(cleanupLock.Fd()), syscall.LOCK_UN)
	m.rootPath = filepath.Join(root, sessionID)
	if err := os.MkdirAll(m.rootPath, 0o700); err != nil {
		m.initErr = err
		return m
	}
	m.lease, err = os.OpenFile(filepath.Join(m.rootPath, ".lease"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		m.initErr = err
		return m
	}
	if err := syscall.Flock(int(m.lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = m.lease.Close()
		m.lease = nil
		m.initErr = err
		return m
	}
	cleanupStaleReverseMounts(root, sessionID)
	return m
}

func (m *remoteMountManager) Execute(ctx context.Context, frame protocol.Frame, peer *remotefs.Peer) protocol.Frame {
	if !safeMountComponent(frame.MountID) || !safeMountComponent(frame.SessionID) {
		return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: "remote fs command is missing mount/session identity"}
	}
	if frame.SessionID != m.sessionID {
		return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: "remote fs session identity changed"}
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: "remote fs session is closing"}
	}
	if m.initErr != nil {
		m.mu.Unlock()
		return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: fmt.Sprintf("initialize local remote fs mounts: %v", m.initErr)}
	}
	m.active.Add(1)
	m.mu.Unlock()
	defer m.active.Done()
	m.mu.Lock()
	entry, exists := m.mounts[frame.MountID]
	if !exists {
		mountRoot := filepath.Join(m.rootPath, frame.MountID)
		mountPath, err := remotefs.MountPathBelow(mountRoot, frame.MountPath)
		if err != nil {
			m.mu.Unlock()
			return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: err.Error()}
		}
		if err := os.MkdirAll(mountRoot, 0o700); err != nil {
			m.mu.Unlock()
			return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: err.Error()}
		}
		if err := os.WriteFile(filepath.Join(mountRoot, ".mount-path"), []byte(frame.MountPath+"\n"), 0o600); err != nil {
			m.mu.Unlock()
			return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: err.Error()}
		}
		driver := remotefs.GoFuseDriver{}
		mount, err := driver.Mount(ctx, mountPath, peer.RemoteBackend(frame.MountID), remotefs.MountOptions{ReadOnly: frame.MountReadOnly || m.readOnly})
		if err != nil {
			_ = os.RemoveAll(mountRoot)
			m.mu.Unlock()
			return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: fmt.Sprintf("mount remote workspace: %v", err)}
		}
		entry = remoteMountEntry{mount: mount, mountPath: mountRoot}
		m.mounts[frame.MountID] = entry
	}
	m.mu.Unlock()
	workspace, err := remotefs.WorkspacePathBelow(entry.mount.Path(), frame.Cwd)
	if err != nil {
		return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: err.Error()}
	}
	frame.Cwd = workspace
	if frame.Env == nil {
		frame.Env = map[string]string{}
	}
	frame.Env["SSHX_REMOTE_FS"] = "1"
	if frame.MountReadOnly {
		frame.Env["FS_READ_ONLY"] = "1"
	} else {
		frame.Env["FS_READ_ONLY"] = "0"
	}
	return bridge.ExecuteLocal(ctx, frame)
}

func (m *remoteMountManager) Close() {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return
	}
	m.closing = true
	m.mu.Unlock()
	m.active.Wait()
	m.mu.Lock()
	entries := make([]remoteMountEntry, 0, len(m.mounts))
	for _, entry := range m.mounts {
		entries = append(entries, entry)
	}
	m.mounts = map[string]remoteMountEntry{}
	m.mu.Unlock()
	for _, entry := range entries {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = entry.mount.Unmount(ctx)
		cancel()
		_ = os.RemoveAll(entry.mountPath)
	}
	if m.lease != nil {
		_ = syscall.Flock(int(m.lease.Fd()), syscall.LOCK_UN)
		_ = m.lease.Close()
	}
	if m.rootPath != "" {
		_ = os.RemoveAll(m.rootPath)
	}
}

func localReverseMountsRoot() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, fmt.Sprintf("sshx-%d", os.Getuid()), "mounts")
}

func cleanupStaleReverseMounts(root, currentSession string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == currentSession || !safeMountComponent(entry.Name()) {
			continue
		}
		sessionRoot := filepath.Join(root, entry.Name())
		lease, err := os.OpenFile(filepath.Join(sessionRoot, ".lease"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			continue
		}
		if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = lease.Close()
			continue
		}
		clean := true
		exports, readErr := os.ReadDir(sessionRoot)
		if readErr != nil {
			clean = false
		}
		for _, export := range exports {
			if !export.IsDir() || !safeMountComponent(export.Name()) {
				continue
			}
			exportRoot := filepath.Join(sessionRoot, export.Name())
			marker, readErr := os.ReadFile(filepath.Join(exportRoot, ".mount-path"))
			if readErr != nil {
				clean = false
				continue
			}
			mountPath, resolveErr := remotefs.MountPathBelow(exportRoot, strings.TrimSpace(string(marker)))
			if resolveErr != nil || !detachStaleMount(mountPath) {
				clean = false
			}
		}
		if clean {
			_ = os.RemoveAll(sessionRoot)
		}
		_ = syscall.Flock(int(lease.Fd()), syscall.LOCK_UN)
		_ = lease.Close()
	}
}

func detachStaleMount(path string) bool {
	if mounted, err := pathIsMountPoint(path); err == nil && !mounted {
		return true
	}
	err := syscall.Unmount(path, 0)
	if err == nil || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOENT) {
		return true
	}
	for _, candidate := range []string{"fusermount3", "fusermount"} {
		if binary, lookErr := exec.LookPath(candidate); lookErr == nil && exec.Command(binary, "-uz", path).Run() == nil {
			return true
		}
	}
	return false
}

func pathIsMountPoint(path string) (bool, error) {
	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	pathInfo, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	parentStat, parentOK := parentInfo.Sys().(*syscall.Stat_t)
	pathStat, pathOK := pathInfo.Sys().(*syscall.Stat_t)
	if !parentOK || !pathOK {
		return true, nil
	}
	return parentStat.Dev != pathStat.Dev, nil
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
		if localDaemonResponseCompatible(resp) {
			return nil
		}
		_, _ = locald.ClientRequest(ctx, socketPath, locald.Request{Type: locald.TypeShutdown, AppVersion: clientVersion(), RuntimeID: identity.LocalRuntimeID, ProtocolVersion: protocol.Version, ProtocolMin: protocol.MinVersion, ProtocolMax: protocol.MaxVersion})
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
			if localDaemonResponseCompatible(resp) {
				_ = logFile.Close()
				return nil
			}
			lastErr = fmt.Errorf("local daemon runtime is %q/protocol %d-%d", resp.RuntimeID, resp.ProtocolMin, resp.ProtocolMax)
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

func localDaemonResponseCompatible(resp locald.Response) bool {
	frame := protocol.Frame{ProtocolVersion: resp.ProtocolVersion, ProtocolMin: resp.ProtocolMin, ProtocolMax: resp.ProtocolMax}
	return protocol.FrameCompatible(frame) && resp.RuntimeID == identity.LocalRuntimeID
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
