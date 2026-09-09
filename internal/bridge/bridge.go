package bridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/xiaot623/sshx/internal/identity"
	"github.com/xiaot623/sshx/internal/processlock"
	"github.com/xiaot623/sshx/internal/protocol"
	"github.com/xiaot623/sshx/internal/remotefs"
	"github.com/xiaot623/sshx/internal/version"
)

var ErrNoClient = errors.New("no active sshx client bridge session")
var ErrMountedCwd = errors.New("cwd is inside an sshx mount; leave the mount before running this command")

const (
	portGoneMissingScans      = 2
	DefaultHeartbeatInterval  = 5 * time.Second
	DefaultHeartbeatTimeout   = 15 * time.Second
	DefaultServerDrainTimeout = 10 * time.Second
	DefaultServerStartTimeout = 10 * time.Second
)

var defaultCapabilities = []string{"command.exec.batch-stdin", "heartbeat.v1", "remotefs.fs.v1"}

type Server struct {
	SocketPath       string
	InfoPath         string
	Token            string
	PortScanInterval time.Duration
	StartupTimeout   time.Duration
	HeartbeatTimeout time.Duration
	DrainTimeout     time.Duration
	Version          string
	FSSocketPath     string
	MountRoot        string
	MountDriver      remotefs.MountDriver

	mu             sync.Mutex
	clients        []*clientConn
	fsPeers        map[string]*remotefs.Peer
	fsBackends     map[string]map[string]*remotefs.RootBackend
	fsExporting    map[string]map[string]chan struct{}
	fsClientMounts map[string]map[string]struct{}
	fsOffering     map[string]map[string]chan struct{}
	fsConnecting   map[string]bool
	fsMounts       map[string]remotefs.Mount
	fsMounting     map[string]bool
	fsUnmounting   map[string]chan struct{}
	observedPorts  map[int]bool
	portMisses     map[int]int
	lastActive     time.Time
	everHadClient  bool
	shutdownOnce   sync.Once
	listener       net.Listener
	cancel         context.CancelFunc
	draining       bool
	connections    map[net.Conn]struct{}
	connWG         sync.WaitGroup
}

type clientConn struct {
	enc          *protocol.Encoder
	dec          *protocol.Decoder
	c            net.Conn
	sessionID    string
	contextID    string
	capabilities map[string]bool
	writeMu      sync.Mutex
	pendingMu    sync.Mutex
	pending      map[string]chan protocol.Frame
	lastSeen     time.Time
	closeOnce    sync.Once
	done         chan struct{}
}

func (s *Server) Serve(ctx context.Context) error {
	if s.SocketPath == "" {
		return errors.New("socket path is required")
	}
	if s.Version == "" {
		s.Version = version.Version
	}
	if s.HeartbeatTimeout <= 0 {
		s.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	if s.DrainTimeout <= 0 {
		s.DrainTimeout = DefaultServerDrainTimeout
	}
	if s.StartupTimeout <= 0 {
		s.StartupTimeout = DefaultServerStartTimeout
	}
	if s.connections == nil {
		s.connections = map[net.Conn]struct{}{}
	}
	if s.fsPeers == nil {
		s.fsPeers = map[string]*remotefs.Peer{}
	}
	if s.fsBackends == nil {
		s.fsBackends = map[string]map[string]*remotefs.RootBackend{}
	}
	if s.fsExporting == nil {
		s.fsExporting = map[string]map[string]chan struct{}{}
	}
	if s.fsClientMounts == nil {
		s.fsClientMounts = map[string]map[string]struct{}{}
	}
	if s.fsOffering == nil {
		s.fsOffering = map[string]map[string]chan struct{}{}
	}
	if s.fsConnecting == nil {
		s.fsConnecting = map[string]bool{}
	}
	if s.fsMounts == nil {
		s.fsMounts = map[string]remotefs.Mount{}
	}
	if s.fsMounting == nil {
		s.fsMounting = map[string]bool{}
	}
	if s.fsUnmounting == nil {
		s.fsUnmounting = map[string]chan struct{}{}
	}
	if s.FSSocketPath == "" {
		s.FSSocketPath = s.SocketPath + ".fs"
	}
	if s.MountRoot == "" {
		s.MountRoot = filepath.Join(filepath.Dir(s.SocketPath), "mounts")
	}
	if s.MountDriver == nil {
		s.MountDriver = remotefs.GoFuseDriver{}
	}
	serveCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(s.SocketPath), 0o700); err != nil {
		return err
	}
	lock, err := processlock.Acquire(s.SocketPath + ".lock")
	if err != nil {
		return err
	}
	defer lock.Release()
	s.cleanupStaleMounts()
	_ = os.Remove(s.SocketPath)
	ln, err := net.Listen("unix", s.SocketPath)
	if err != nil {
		return err
	}
	defer ln.Close()
	s.listener = ln
	ownedSocket, _ := os.Stat(s.SocketPath)
	defer removeSocketIfOwned(s.SocketPath, ownedSocket)
	defer func() {
		s.closeConnections()
		s.connWG.Wait()
	}()
	s.markActive()
	if err := os.Chmod(s.SocketPath, 0o600); err != nil {
		return err
	}
	fsListener, err := s.listenFS(serveCtx)
	if err != nil {
		return err
	}
	defer fsListener.Close()
	if s.InfoPath != "" {
		if err := WriteServerInfo(s.InfoPath, s.SocketPath, s.Token, s.Version); err != nil {
			return err
		}
		defer os.Remove(s.InfoPath)
	}
	if s.PortScanInterval > 0 {
		go s.observePorts(serveCtx)
	}
	go s.monitorLeases(serveCtx)

	go func() {
		<-serveCtx.Done()
		_ = ln.Close()
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			if serveCtx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.mu.Lock()
		if s.draining {
			s.mu.Unlock()
			_ = c.Close()
			continue
		}
		s.connections[c] = struct{}{}
		s.connWG.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.connWG.Done()
			defer s.removeConnection(c)
			s.handleConn(c)
		}()
	}
}

func removeSocketIfOwned(path string, owned os.FileInfo) {
	if owned == nil {
		return
	}
	current, err := os.Stat(path)
	if err == nil && os.SameFile(owned, current) {
		_ = os.Remove(path)
	}
}

func (s *Server) monitorLeases(ctx context.Context) {
	interval := s.HeartbeatTimeout / 3
	if interval > 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			s.mu.Lock()
			clients := append([]*clientConn(nil), s.clients...)
			s.mu.Unlock()
			for _, client := range clients {
				client.pendingMu.Lock()
				lastSeen := client.lastSeen
				client.pendingMu.Unlock()
				if now.Sub(lastSeen) >= s.HeartbeatTimeout {
					s.removeClient(client)
				}
			}
			s.mu.Lock()
			empty := len(s.clients) == 0
			lastActive := s.lastActive
			everHad := s.everHadClient
			s.mu.Unlock()
			shouldDrain := empty && ((everHad && now.Sub(lastActive) >= s.DrainTimeout) || (!everHad && now.Sub(lastActive) >= s.StartupTimeout))
			if shouldDrain {
				s.mu.Lock()
				shouldDrain = len(s.clients) == 0
				if shouldDrain {
					s.draining = true
				}
				s.mu.Unlock()
			}
			if shouldDrain {
				s.initiateShutdown()
				return
			}
		}
	}
}

func (s *Server) handleConn(c net.Conn) {
	dec := protocol.NewDecoder(c)
	enc := protocol.NewEncoder(c)
	hello, err := dec.Decode()
	if err != nil {
		_ = c.Close()
		return
	}
	if hello.Type != protocol.TypeHello {
		_ = enc.Encode(protocol.Frame{Type: protocol.TypeError, Error: "expected sshx hello"})
		_ = c.Close()
		return
	}
	if s.Token != "" && hello.Token != s.Token {
		_ = enc.Encode(protocol.Frame{Type: protocol.TypeError, Error: "invalid sshx server token"})
		_ = c.Close()
		return
	}
	if !protocol.FrameCompatible(hello) || hello.RuntimeID != identity.RuntimeID {
		_ = enc.Encode(protocol.Frame{Type: protocol.TypeServerDrain, AppVersion: s.Version, RuntimeID: identity.RuntimeID, ProtocolMin: protocol.MinVersion, ProtocolMax: protocol.MaxVersion, Error: "sshx runtime protocol is incompatible"})
		_ = c.Close()
		return
	}
	switch hello.Role {
	case protocol.RoleClient:
		if hello.SessionID == "" {
			_ = enc.Encode(protocol.Frame{Type: protocol.TypeError, Error: "client sessionId is required"})
			_ = c.Close()
			return
		}
		if hello.TargetID == "" || hello.ContextID == "" {
			_ = enc.Encode(protocol.Frame{Type: protocol.TypeError, Error: "client targetId and contextId are required"})
			_ = c.Close()
			return
		}
		if len(hello.Capabilities) == 0 {
			hello.Capabilities = defaultCapabilities
		}
		capabilities := make(map[string]bool, len(hello.Capabilities))
		for _, capability := range hello.Capabilities {
			capabilities[capability] = true
		}
		cc := &clientConn{enc: enc, dec: dec, c: c, sessionID: hello.SessionID, contextID: hello.ContextID, capabilities: capabilities, pending: map[string]chan protocol.Frame{}, lastSeen: time.Now(), done: make(chan struct{})}
		if !s.addClient(cc) {
			_ = cc.send(protocol.Frame{Type: protocol.TypeServerDrain, ProtocolVersion: protocol.Version, AppVersion: s.Version, Error: "sshx server is draining"})
			cc.close()
			return
		}
		if err := cc.send(protocol.Frame{Type: protocol.TypeCapabilities, ProtocolVersion: protocol.Version, ProtocolMin: protocol.MinVersion, ProtocolMax: protocol.MaxVersion, RuntimeID: identity.RuntimeID, AppVersion: s.Version, Capabilities: defaultCapabilities}); err != nil {
			s.removeClient(cc)
			return
		}
		s.sendCurrentPorts(cc)
		cc.readLoop(s)
	case protocol.RoleRequester:
		s.handleRequester(c, dec, enc, hello.ContextID, hello.SessionID)
	default:
		_ = enc.Encode(protocol.Frame{Type: protocol.TypeError, Error: "unknown bridge role"})
		_ = c.Close()
	}
}

func (s *Server) handleRequester(c net.Conn, dec *protocol.Decoder, enc *protocol.Encoder, requesterContextID, requesterSessionID string) {
	defer c.Close()
	s.markActive()
	req, err := dec.Decode()
	if err != nil {
		return
	}
	if req.Type != protocol.TypeCommandExec {
		_ = enc.Encode(protocol.Frame{Type: protocol.TypeError, ID: req.ID, Error: "expected command.exec"})
		return
	}
	if requesterSessionID != "" && req.SessionID != requesterSessionID {
		_ = enc.Encode(protocol.Frame{Type: protocol.TypeCommandError, ID: req.ID, Error: "requester sessionId does not match command sessionId"})
		return
	}
	if requesterContextID != "" && req.ContextID != requesterContextID {
		_ = enc.Encode(protocol.Frame{Type: protocol.TypeCommandError, ID: req.ID, Error: "requester contextId does not match command contextId"})
		return
	}
	if req.RemoteFS {
		if req.Cwd == "" {
			_ = enc.Encode(protocol.Frame{Type: protocol.TypeCommandError, ID: req.ID, Error: "remote fs requires sessionId and cwd"})
			return
		}
		if req.RequestID == "" || req.RequestID != req.ID {
			_ = enc.Encode(protocol.Frame{Type: protocol.TypeCommandError, ID: req.ID, Error: "requestId is required and must match command id"})
			return
		}
	}
	requestedSessionID := req.SessionID
	var lastErr error
	for {
		client := s.pickClient(req.ContextID, requestedSessionID, "command.exec.batch-stdin")
		if client == nil {
			err := ErrNoClient
			if lastErr != nil {
				err = lastErr
			}
			_ = enc.Encode(protocol.Frame{Type: protocol.TypeCommandError, ID: req.ID, Error: err.Error()})
			return
		}

		attempt := req
		if attempt.SessionID == "" {
			attempt.SessionID = client.sessionID
		}
		if attempt.RemoteFS {
			s.mu.Lock()
			fsPeer := s.fsPeers[attempt.SessionID]
			s.mu.Unlock()
			if fsPeer == nil {
				_ = enc.Encode(protocol.Frame{Type: protocol.TypeCommandError, ID: attempt.ID, Error: "remote fs data session is unavailable"})
				return
			}
			layout, err := remotefs.CurrentExportLayout(attempt.Cwd)
			if err != nil {
				_ = enc.Encode(protocol.Frame{Type: protocol.TypeCommandError, ID: attempt.ID, Error: fmt.Sprintf("resolve remote home mount: %v", err)})
				return
			}
			mountID := exportMountID(layout.RootPath, layout.MountPath)
			if err := s.ensureExportBackend(attempt.SessionID, mountID, layout.RootPath, fsPeer); err != nil {
				_ = enc.Encode(protocol.Frame{Type: protocol.TypeCommandError, ID: attempt.ID, Error: err.Error()})
				return
			}
			if err := s.ensureClientMount(attempt.SessionID, mountID, layout.MountPath, fsPeer, attempt.MountReadOnly); err != nil {
				_ = enc.Encode(protocol.Frame{Type: protocol.TypeCommandError, ID: attempt.ID, Error: err.Error()})
				return
			}
			if attempt.Env == nil {
				attempt.Env = map[string]string{}
			}
			attempt.Env["SSHX_REMOTE_CWD"] = attempt.Cwd
			attempt.Env["SSHX_REMOTE_FS"] = "1"
			attempt.MountID = mountID
			attempt.MountPath = layout.MountPath
			attempt.Cwd = filepath.ToSlash(layout.RelativeCwd)
		}
		resp, err := client.request(attempt)
		if err != nil {
			lastErr = err
			s.removeClient(client)
			continue
		}
		_ = enc.Encode(resp)
		return
	}
}

func (s *Server) addClient(c *clientConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return false
	}
	for _, existing := range s.clients {
		if existing.sessionID == c.sessionID {
			return false
		}
	}
	s.clients = append(s.clients, c)
	s.everHadClient = true
	s.lastActive = time.Now()
	return true
}

func (s *Server) removeClient(c *clientConn) {
	s.mu.Lock()
	found := false
	var fsPeer *remotefs.Peer
	var backends map[string]*remotefs.RootBackend
	for i, existing := range s.clients {
		if existing == c {
			s.clients = append(s.clients[:i], s.clients[i+1:]...)
			found = true
			break
		}
	}
	if found {
		s.lastActive = time.Now()
		fsPeer = s.fsPeers[c.sessionID]
		delete(s.fsPeers, c.sessionID)
		backends = s.fsBackends[c.sessionID]
		delete(s.fsBackends, c.sessionID)
		delete(s.fsClientMounts, c.sessionID)
	}
	s.mu.Unlock()
	if found {
		c.close()
		if fsPeer != nil {
			for mountID, backend := range backends {
				_ = fsPeer.UnregisterBackend(mountID)
				_ = backend.CloseBackend()
			}
			_ = fsPeer.Close()
		} else {
			for _, backend := range backends {
				_ = backend.CloseBackend()
			}
		}
	}
}

func (s *Server) markActive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActive = time.Now()
}

func (s *Server) pickClient(contextID, sessionID, capability string) *clientConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, client := range s.clients {
		if (sessionID == "" || client.sessionID == sessionID) && (contextID == "" || client.contextID == contextID) && (capability == "" || client.capabilities[capability]) {
			return client
		}
	}
	return nil
}

func (c *clientConn) request(req protocol.Frame) (protocol.Frame, error) {
	responses := make(chan protocol.Frame, 1)
	c.pendingMu.Lock()
	c.pending[req.ID] = responses
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, req.ID)
		c.pendingMu.Unlock()
	}()
	if err := c.send(req); err != nil {
		return protocol.Frame{}, err
	}
	select {
	case resp := <-responses:
		return resp, nil
	case <-c.done:
		return protocol.Frame{}, io.EOF
	}
}

func (c *clientConn) send(frame protocol.Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.enc.Encode(frame)
}

func (c *clientConn) readLoop(s *Server) {
	defer s.removeClient(c)
	for {
		frame, err := c.dec.Decode()
		if err != nil {
			return
		}
		switch frame.Type {
		case protocol.TypeHeartbeat:
			if !protocol.FrameCompatible(frame) || frame.RuntimeID != identity.RuntimeID {
				_ = c.send(protocol.Frame{Type: protocol.TypeServerDrain, ProtocolMin: protocol.MinVersion, ProtocolMax: protocol.MaxVersion, RuntimeID: identity.RuntimeID, AppVersion: s.Version, Error: "sshx runtime protocol is incompatible"})
				return
			}
			c.pendingMu.Lock()
			c.lastSeen = time.Now()
			c.pendingMu.Unlock()
			if err := c.send(protocol.Frame{Type: protocol.TypeHeartbeatAck, ProtocolVersion: protocol.Version, ProtocolMin: protocol.MinVersion, ProtocolMax: protocol.MaxVersion, RuntimeID: identity.RuntimeID, AppVersion: s.Version, SessionID: frame.SessionID, Sequence: frame.Sequence}); err != nil {
				return
			}
		case protocol.TypeCommandResult, protocol.TypeCommandError:
			c.pendingMu.Lock()
			responses := c.pending[frame.ID]
			c.pendingMu.Unlock()
			if responses != nil {
				select {
				case responses <- frame:
				default:
				}
			}
		}
	}
}

func (c *clientConn) close() {
	c.closeOnce.Do(func() {
		_ = c.c.Close()
		close(c.done)
	})
}

func (s *Server) initiateShutdown() {
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		s.draining = true
		s.mu.Unlock()
		if s.cancel != nil {
			s.cancel()
		}
		if s.listener != nil {
			_ = s.listener.Close()
		}
	})
}

func (s *Server) closeConnections() {
	s.mu.Lock()
	connections := make([]net.Conn, 0, len(s.connections))
	for conn := range s.connections {
		connections = append(connections, conn)
	}
	s.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
}

func (s *Server) removeConnection(conn net.Conn) {
	s.mu.Lock()
	delete(s.connections, conn)
	s.mu.Unlock()
}
