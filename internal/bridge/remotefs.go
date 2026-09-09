package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/xiaot623/sshx/internal/remotefs"
)

func (s *Server) listenFS(ctx context.Context) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(s.FSSocketPath), 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(s.FSSocketPath)
	listener, err := net.Listen("unix", s.FSSocketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(s.FSSocketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	ownedSocket, _ := os.Stat(s.FSSocketPath)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	go func() {
		defer removeSocketIfOwned(s.FSSocketPath, ownedSocket)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			if s.draining {
				s.mu.Unlock()
				_ = conn.Close()
				continue
			}
			s.connections[conn] = struct{}{}
			s.connWG.Add(1)
			s.mu.Unlock()
			go func() {
				defer s.connWG.Done()
				defer s.removeConnection(conn)
				s.handleFSConn(ctx, conn)
			}()
		}
	}()
	return listener, nil
}

func (s *Server) handleFSConn(ctx context.Context, conn net.Conn) {
	var sessionID string
	reserved := false
	peer, err := remotefs.Accept(ctx, conn, func(candidate, token string) error {
		if s.Token != "" && token != s.Token {
			return errors.New("invalid sshx server token")
		}
		if !safeSessionID(candidate) {
			return errors.New("invalid remote fs sessionId")
		}
		if s.pickClient("", candidate, "remotefs.fs.v1") == nil {
			return ErrNoClient
		}
		s.mu.Lock()
		if s.fsPeers[candidate] != nil || s.fsConnecting[candidate] {
			s.mu.Unlock()
			return errors.New("remote fs data session already exists")
		}
		s.fsConnecting[candidate] = true
		reserved = true
		s.mu.Unlock()
		sessionID = candidate
		return nil
	}, remotefs.PeerOptions{
		OnMount: func(mountCtx context.Context, peer *remotefs.Peer, mountID, mountPath string, options remotefs.MountOptions) (string, error) {
			return s.mountRemoteFS(mountCtx, ctx, sessionID, peer, mountID, mountPath, options)
		},
		OnUnmount: func(unmountCtx context.Context, mountID string) error {
			return s.unmountRemoteFS(unmountCtx, sessionID, mountID)
		},
	})
	if reserved {
		s.mu.Lock()
		delete(s.fsConnecting, sessionID)
		s.mu.Unlock()
	}
	if err != nil {
		return
	}
	s.mu.Lock()
	hasClient := false
	for _, client := range s.clients {
		if client.sessionID == sessionID {
			hasClient = true
			break
		}
	}
	if existing := s.fsPeers[sessionID]; existing != nil || s.draining || !hasClient {
		s.mu.Unlock()
		_ = peer.Close()
		return
	}
	s.fsPeers[sessionID] = peer
	s.lastActive = time.Now()
	s.mu.Unlock()
	<-peer.Done()
	s.mu.Lock()
	if s.fsPeers[sessionID] == peer {
		delete(s.fsPeers, sessionID)
	}
	s.lastActive = time.Now()
	s.mu.Unlock()
}

func safeSessionID(sessionID string) bool {
	return sessionID != "" && sessionID != "." && sessionID != ".." &&
		filepath.Base(sessionID) == sessionID && !strings.ContainsAny(sessionID, `/\`)
}

func (s *Server) mountRemoteFS(requestCtx, lifetimeCtx context.Context, sessionID string, peer *remotefs.Peer, mountID, mountHierarchy string, options remotefs.MountOptions) (string, error) {
	if mountID == "" {
		return "", errors.New("remote fs mountId is required")
	}
	s.mu.Lock()
	if s.fsMounting[sessionID] || s.fsMounts[sessionID] != nil {
		s.mu.Unlock()
		return "", errors.New("remote fs mount already exists")
	}
	s.fsMounting[sessionID] = true
	s.mu.Unlock()
	sessionPath := filepath.Join(s.MountRoot, sessionID)
	// The request context only owns mount setup. A successful mount must outlive
	// the mount.create handler and is released explicitly by the peer lifecycle.
	mountCtx, mountCancel := context.WithCancel(lifetimeCtx)
	stopRequestCancel := context.AfterFunc(requestCtx, mountCancel)
	mount, err := remotefs.MountLocal(mountCtx, s.MountDriver, sessionPath, mountHierarchy, peer.RemoteBackend(mountID), options)
	stopRequestCancel()
	if err != nil {
		mountCancel()
		s.mu.Lock()
		delete(s.fsMounting, sessionID)
		s.mu.Unlock()
		return "", err
	}
	path := mount.Path()
	s.mu.Lock()
	delete(s.fsMounting, sessionID)
	peerClosed := false
	select {
	case <-peer.Done():
		peerClosed = true
	default:
	}
	if s.draining || peerClosed || requestCtx.Err() != nil {
		s.mu.Unlock()
		_ = mount.Unmount(context.Background())
		mountCancel()
		if err := requestCtx.Err(); err != nil {
			return "", err
		}
		return "", errors.New("remote fs session closed while mounting")
	}
	s.fsMounts[sessionID] = &lifetimeMount{Mount: mount, cancel: mountCancel, mountID: mountID}
	s.lastActive = time.Now()
	s.mu.Unlock()
	return path, nil
}

type lifetimeMount struct {
	remotefs.Mount
	cancel  context.CancelFunc
	mountID string
}

func (m *lifetimeMount) Unmount(ctx context.Context) error {
	err := m.Mount.Unmount(ctx)
	if err == nil {
		m.cancel()
	}
	return err
}

func (s *Server) unmountRemoteFS(ctx context.Context, sessionID, mountID string) error {
	for {
		s.mu.Lock()
		if s.fsMounting[sessionID] {
			s.mu.Unlock()
			return syscall.EBUSY
		}
		mount, _ := s.fsMounts[sessionID].(*lifetimeMount)
		if mount == nil || mount.mountID != mountID {
			s.mu.Unlock()
			return nil
		}
		if done := s.fsUnmounting[sessionID]; done != nil {
			s.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		done := make(chan struct{})
		s.fsUnmounting[sessionID] = done
		s.mu.Unlock()

		err := mount.Unmount(ctx)
		s.mu.Lock()
		if err == nil {
			delete(s.fsMounts, sessionID)
		}
		delete(s.fsUnmounting, sessionID)
		s.lastActive = time.Now()
		close(done)
		s.mu.Unlock()
		if err == nil {
			_ = os.RemoveAll(filepath.Join(s.MountRoot, sessionID))
		}
		return err
	}
}

func (s *Server) cleanupStaleMounts() {
	entries, err := os.ReadDir(s.MountRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !safeSessionID(entry.Name()) {
			continue
		}
		sessionPath := filepath.Join(s.MountRoot, entry.Name())
		mountPath := filepath.Join(sessionPath, "workspace")
		if marker, readErr := os.ReadFile(filepath.Join(sessionPath, ".mount-path")); readErr == nil {
			if resolved, resolveErr := remotefs.MountPathBelow(sessionPath, strings.TrimSpace(string(marker))); resolveErr == nil {
				mountPath = resolved
			} else {
				continue
			}
		}
		if mounted, statErr := managedPathMounted(mountPath); statErr == nil && !mounted {
			_ = os.RemoveAll(sessionPath)
			continue
		}
		unmountErr := syscall.Unmount(mountPath, 0)
		detached := unmountErr == nil || errors.Is(unmountErr, syscall.EINVAL) || errors.Is(unmountErr, syscall.ENOENT)
		if !detached {
			if binary, lookErr := exec.LookPath("fusermount3"); lookErr == nil {
				detached = exec.Command(binary, "-uz", mountPath).Run() == nil
			} else if binary, lookErr := exec.LookPath("fusermount"); lookErr == nil {
				detached = exec.Command(binary, "-uz", mountPath).Run() == nil
			}
		}
		if detached {
			_ = os.RemoveAll(sessionPath)
		}
	}
}

func managedPathMounted(path string) (bool, error) {
	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return false, err
	}
	pathInfo, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	parentStat, parentOK := parentInfo.Sys().(*syscall.Stat_t)
	pathStat, pathOK := pathInfo.Sys().(*syscall.Stat_t)
	if !parentOK || !pathOK {
		return true, nil
	}
	return parentStat.Dev != pathStat.Dev, nil
}

func exportMountID(rootPath, mountPath string) string {
	sum := sha256.Sum256([]byte(rootPath + "\x00" + mountPath))
	return "export-" + hex.EncodeToString(sum[:8])
}

func (s *Server) ensureExportBackend(sessionID, mountID, rootPath string, peer *remotefs.Peer) error {
	for {
		s.mu.Lock()
		if backends := s.fsBackends[sessionID]; backends != nil && backends[mountID] != nil {
			s.mu.Unlock()
			return nil
		}
		if exporting := s.fsExporting[sessionID]; exporting != nil {
			if wait := exporting[mountID]; wait != nil {
				s.mu.Unlock()
				<-wait
				continue
			}
		} else {
			s.fsExporting[sessionID] = map[string]chan struct{}{}
		}
		wait := make(chan struct{})
		s.fsExporting[sessionID][mountID] = wait
		s.mu.Unlock()
		break
	}
	finish := func() {
		s.mu.Lock()
		if exporting := s.fsExporting[sessionID]; exporting != nil {
			if wait := exporting[mountID]; wait != nil {
				delete(exporting, mountID)
				close(wait)
			}
			if len(exporting) == 0 {
				delete(s.fsExporting, sessionID)
			}
		}
		s.mu.Unlock()
	}
	// Exclude the session mount tree so reverse mounts cannot recurse into a home export.
	backend, err := remotefs.OpenRootBackendWithOptions(rootPath, remotefs.RootBackendOptions{DisableDelete: true}, s.MountRoot)
	if err != nil {
		finish()
		return fmt.Errorf("open remote workspace: %w", err)
	}
	if err := peer.RegisterBackend(mountID, backend); err != nil {
		_ = backend.CloseBackend()
		finish()
		return err
	}
	s.mu.Lock()
	if s.fsPeers[sessionID] != peer {
		s.mu.Unlock()
		_ = peer.UnregisterBackend(mountID)
		_ = backend.CloseBackend()
		finish()
		return errors.New("remote fs session closed while exporting the working tree")
	}
	if s.fsBackends[sessionID] == nil {
		s.fsBackends[sessionID] = map[string]*remotefs.RootBackend{}
	}
	if existing := s.fsBackends[sessionID][mountID]; existing != nil {
		s.mu.Unlock()
		_ = peer.UnregisterBackend(mountID)
		_ = backend.CloseBackend()
		finish()
		return nil
	}
	s.fsBackends[sessionID][mountID] = backend
	s.mu.Unlock()
	finish()
	return nil
}

func (s *Server) ensureClientMount(sessionID, mountID, mountPath string, peer *remotefs.Peer, readOnly bool) error {
	for {
		s.mu.Lock()
		if mounts := s.fsClientMounts[sessionID]; mounts != nil {
			if _, offered := mounts[mountID]; offered {
				s.mu.Unlock()
				return nil
			}
		}
		if offering := s.fsOffering[sessionID]; offering != nil {
			if wait := offering[mountID]; wait != nil {
				s.mu.Unlock()
				<-wait
				continue
			}
		} else {
			s.fsOffering[sessionID] = map[string]chan struct{}{}
		}
		wait := make(chan struct{})
		s.fsOffering[sessionID][mountID] = wait
		s.mu.Unlock()
		break
	}
	finish := func() {
		s.mu.Lock()
		if offering := s.fsOffering[sessionID]; offering != nil {
			if wait := offering[mountID]; wait != nil {
				delete(offering, mountID)
				close(wait)
			}
			if len(offering) == 0 {
				delete(s.fsOffering, sessionID)
			}
		}
		s.mu.Unlock()
	}
	mountCtx, mountCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, err := peer.CreateMountAtWithOptions(mountCtx, mountID, mountPath, remotefs.MountOptions{ReadOnly: readOnly})
	mountCancel()
	if err != nil {
		finish()
		return fmt.Errorf("mount remote workspace: %w", err)
	}
	s.mu.Lock()
	if s.fsPeers[sessionID] != peer {
		s.mu.Unlock()
		finish()
		return errors.New("remote fs session closed while offering the mount")
	}
	if s.fsClientMounts[sessionID] == nil {
		s.fsClientMounts[sessionID] = map[string]struct{}{}
	}
	s.fsClientMounts[sessionID][mountID] = struct{}{}
	s.mu.Unlock()
	finish()
	return nil
}
