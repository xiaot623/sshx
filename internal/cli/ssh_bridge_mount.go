package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xiaot623/sshx/internal/bridge"
	"github.com/xiaot623/sshx/internal/protocol"
	"github.com/xiaot623/sshx/internal/remotefs"
)

type remoteMountEntry struct {
	mount     remotefs.Mount
	mountPath string
	cancel    context.CancelFunc
}

type remoteMountManager struct {
	sessionID      string
	readOnly       bool
	rootPath       string
	lease          *os.File
	initErr        error
	driver         remotefs.MountDriver
	lifetimeCtx    context.Context
	lifetimeCancel context.CancelFunc
	mu             sync.Mutex
	mounts         map[string]remoteMountEntry
	closing        bool
	active         sync.WaitGroup
}

func newRemoteMountManager(sessionID string, readOnly bool) *remoteMountManager {
	lifetimeCtx, lifetimeCancel := context.WithCancel(context.Background())
	m := &remoteMountManager{
		sessionID:      sessionID,
		readOnly:       readOnly,
		mounts:         map[string]remoteMountEntry{},
		driver:         remotefs.GoFuseDriver{},
		lifetimeCtx:    lifetimeCtx,
		lifetimeCancel: lifetimeCancel,
	}
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

func (m *remoteMountManager) OnMount(requestCtx context.Context, peer *remotefs.Peer, mountID, mountHierarchy string, options remotefs.MountOptions) (string, error) {
	if !safeMountComponent(mountID) {
		return "", errors.New("remote fs mountId is invalid")
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return "", errors.New("remote fs session is closing")
	}
	if m.initErr != nil {
		err := m.initErr
		m.mu.Unlock()
		return "", fmt.Errorf("initialize local remote fs mounts: %w", err)
	}
	if entry, exists := m.mounts[mountID]; exists {
		path := entry.mount.Path()
		m.mu.Unlock()
		return path, nil
	}
	m.active.Add(1)
	m.mu.Unlock()
	defer m.active.Done()

	base := filepath.Join(m.rootPath, mountID)
	if m.readOnly {
		options.ReadOnly = true
	}
	// The request context only owns mount setup. A successful mount must outlive
	// the mount.create handler; Close() cancels the session lifetime context.
	mountCtx, mountCancel := context.WithCancel(m.lifetimeCtx)
	stopRequestCancel := context.AfterFunc(requestCtx, mountCancel)
	mount, err := remotefs.MountLocal(mountCtx, m.driver, base, mountHierarchy, peer.RemoteBackend(mountID), options)
	stopRequestCancel()
	if err != nil {
		mountCancel()
		return "", err
	}
	m.mu.Lock()
	if existing, exists := m.mounts[mountID]; exists {
		m.mu.Unlock()
		_ = mount.Unmount(context.Background())
		mountCancel()
		return existing.mount.Path(), nil
	}
	if m.closing || requestCtx.Err() != nil {
		m.mu.Unlock()
		_ = mount.Unmount(context.Background())
		mountCancel()
		if err := requestCtx.Err(); err != nil {
			return "", err
		}
		return "", errors.New("remote fs session is closing")
	}
	m.mounts[mountID] = remoteMountEntry{mount: mount, mountPath: base, cancel: mountCancel}
	path := mount.Path()
	m.mu.Unlock()
	return path, nil
}

func (m *remoteMountManager) OnUnmount(_ context.Context, mountID string) error {
	m.mu.Lock()
	entry, exists := m.mounts[mountID]
	if !exists {
		m.mu.Unlock()
		return nil
	}
	delete(m.mounts, mountID)
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := entry.mount.Unmount(ctx)
	cancel()
	if entry.cancel != nil {
		entry.cancel()
	}
	_ = os.RemoveAll(entry.mountPath)
	return err
}

func (m *remoteMountManager) Execute(ctx context.Context, frame protocol.Frame) protocol.Frame {
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
	entry, exists := m.mounts[frame.MountID]
	if !exists {
		m.mu.Unlock()
		// Mounts are created by OnMount (mount.create), not by command.exec.
		return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: "remote fs mount is not available"}
	}
	m.active.Add(1)
	m.mu.Unlock()
	defer m.active.Done()
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
		if entry.cancel != nil {
			entry.cancel()
		}
		_ = os.RemoveAll(entry.mountPath)
	}
	if m.lifetimeCancel != nil {
		m.lifetimeCancel()
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
