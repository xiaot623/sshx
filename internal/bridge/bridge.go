package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xiaot623/sshx/internal/identity"
	"github.com/xiaot623/sshx/internal/ports"
	"github.com/xiaot623/sshx/internal/processlock"
	"github.com/xiaot623/sshx/internal/protocol"
	"github.com/xiaot623/sshx/internal/remotefs"
	"github.com/xiaot623/sshx/internal/version"
)

var ErrNoClient = errors.New("no active sshx client bridge session")

type CommandResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

type CommandAllowed func([]string) bool

type ClientOptions struct {
	Ready             chan<- error
	Allow             CommandAllowed
	Execute           func(context.Context, protocol.Frame) protocol.Frame
	OnPortObserved    func(port int)
	OnPortGone        func(port int)
	AppVersion        string
	RuntimeID         string
	TargetID          string
	ContextID         string
	SessionID         string
	Capabilities      []string
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
}

const (
	portGoneMissingScans      = 2
	DefaultHeartbeatInterval  = 5 * time.Second
	DefaultHeartbeatTimeout   = 15 * time.Second
	DefaultServerDrainTimeout = 10 * time.Second
	DefaultServerStartTimeout = 10 * time.Second
)

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

	mu            sync.Mutex
	clients       []*clientConn
	fsPeers       map[string]*remotefs.Peer
	fsBackends    map[string]map[string]*remotefs.RootBackend
	fsExporting   map[string]map[string]chan struct{}
	fsConnecting  map[string]bool
	observedPorts map[int]bool
	portMisses    map[int]int
	lastActive    time.Time
	everHadClient bool
	shutdown      chan struct{}
	shutdownOnce  sync.Once
	listener      net.Listener
	cancel        context.CancelFunc
	draining      bool
	connections   map[net.Conn]struct{}
	connWG        sync.WaitGroup
}

type clientConn struct {
	enc          *protocol.Encoder
	dec          *protocol.Decoder
	c            net.Conn
	sessionID    string
	targetID     string
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
	if s.shutdown == nil {
		s.shutdown = make(chan struct{})
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
	if s.fsConnecting == nil {
		s.fsConnecting = map[string]bool{}
	}
	if s.FSSocketPath == "" {
		s.FSSocketPath = s.SocketPath + ".fs"
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
	}, remotefs.PeerOptions{})
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
			everHad := s.everHadClient
			lastActive := s.lastActive
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
			lastActive = s.lastActive
			everHad = s.everHadClient
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

func (s *Server) observePorts(ctx context.Context) {
	ticker := time.NewTicker(s.PortScanInterval)
	defer ticker.Stop()
	s.scanAndBroadcastPorts()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scanAndBroadcastPorts()
		}
	}
}

func (s *Server) scanAndBroadcastPorts() {
	portList, err := ports.ScanLoopbackListening()
	if err != nil {
		return
	}
	observed, gone := s.applyPortScan(portList)
	for _, port := range observed {
		s.broadcast(protocol.Frame{Type: protocol.TypePortObserved, Host: "localhost", Port: port})
	}
	for _, port := range gone {
		s.broadcast(protocol.Frame{Type: protocol.TypePortGone, Host: "localhost", Port: port})
	}
}

func (s *Server) applyPortScan(portList []int) ([]int, []int) {
	current := map[int]bool{}
	for _, port := range portList {
		if port > 0 {
			current[port] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.observedPorts == nil {
		s.observedPorts = map[int]bool{}
	}
	if s.portMisses == nil {
		s.portMisses = map[int]int{}
	}
	var observed []int
	for port := range current {
		if !s.observedPorts[port] {
			observed = append(observed, port)
		}
		s.observedPorts[port] = true
		delete(s.portMisses, port)
	}
	var gone []int
	for port := range s.observedPorts {
		if current[port] {
			continue
		}
		s.portMisses[port]++
		if s.portMisses[port] >= portGoneMissingScans {
			delete(s.observedPorts, port)
			delete(s.portMisses, port)
			gone = append(gone, port)
		}
	}
	sort.Ints(observed)
	sort.Ints(gone)
	return observed, gone
}

func (s *Server) currentPorts() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	ports := make([]int, 0, len(s.observedPorts))
	for port := range s.observedPorts {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

func (s *Server) sendCurrentPorts(client *clientConn) {
	for _, port := range s.currentPorts() {
		_ = client.send(protocol.Frame{Type: protocol.TypePortObserved, Host: "localhost", Port: port})
	}
}

func (s *Server) broadcast(frame protocol.Frame) {
	s.mu.Lock()
	clients := append([]*clientConn(nil), s.clients...)
	s.mu.Unlock()
	for _, client := range clients {
		_ = client.send(frame)
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
			hello.Capabilities = []string{"command.exec.batch-stdin", "heartbeat.v1", "remotefs.fs.v1"}
		}
		capabilities := make(map[string]bool, len(hello.Capabilities))
		for _, capability := range hello.Capabilities {
			capabilities[capability] = true
		}
		cc := &clientConn{enc: enc, dec: dec, c: c, sessionID: hello.SessionID, targetID: hello.TargetID, contextID: hello.ContextID, capabilities: capabilities, pending: map[string]chan protocol.Frame{}, lastSeen: time.Now(), done: make(chan struct{})}
		if !s.addClient(cc) {
			_ = cc.send(protocol.Frame{Type: protocol.TypeServerDrain, ProtocolVersion: protocol.Version, AppVersion: s.Version, Error: "sshx server is draining"})
			cc.close()
			return
		}
		if err := cc.send(protocol.Frame{Type: protocol.TypeCapabilities, ProtocolVersion: protocol.Version, ProtocolMin: protocol.MinVersion, ProtocolMax: protocol.MaxVersion, RuntimeID: identity.RuntimeID, AppVersion: s.Version, Capabilities: []string{"command.exec.batch-stdin", "heartbeat.v1", "remotefs.fs.v1"}}); err != nil {
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
	backend, err := remotefs.OpenRootBackendWithOptions(rootPath, remotefs.RootBackendOptions{DisableDelete: true})
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
	if sessionID != "" {
		for _, client := range s.clients {
			if client.sessionID == sessionID && (contextID == "" || client.contextID == contextID) && (capability == "" || client.capabilities[capability]) {
				return client
			}
		}
		return nil
	}
	for _, client := range s.clients {
		if (contextID == "" || client.contextID == contextID) && (capability == "" || client.capabilities[capability]) {
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
		c.pendingMu.Lock()
		for id := range c.pending {
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()
	})
}

func (s *Server) initiateShutdown() {
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		s.draining = true
		s.mu.Unlock()
		if s.shutdown != nil {
			close(s.shutdown)
		}
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

func RequestCommand(ctx context.Context, socketPath string, argv []string, stdin []byte, env map[string]string, cwd string, token ...string) (CommandResult, error) {
	return RequestCommandForSessionWithTimeout(ctx, socketPath, argv, stdin, env, cwd, "", false, 0, token...)
}

func RequestCommandWithTimeout(ctx context.Context, socketPath string, argv []string, stdin []byte, env map[string]string, cwd string, timeout time.Duration, token ...string) (CommandResult, error) {
	return RequestCommandForSessionWithTimeout(ctx, socketPath, argv, stdin, env, cwd, "", false, timeout, token...)
}

func RequestCommandForSessionWithTimeout(ctx context.Context, socketPath string, argv []string, stdin []byte, env map[string]string, cwd, sessionID string, remoteFS bool, timeout time.Duration, token ...string) (CommandResult, error) {
	return RequestCommandForSessionWithMountOptions(ctx, socketPath, argv, stdin, env, cwd, sessionID, remoteFS, false, timeout, token...)
}

func RequestCommandForSessionWithMountOptions(ctx context.Context, socketPath string, argv []string, stdin []byte, env map[string]string, cwd, sessionID string, remoteFS, readOnly bool, timeout time.Duration, token ...string) (CommandResult, error) {
	return RequestCommandForContextWithMountOptions(ctx, socketPath, argv, stdin, env, cwd, "", sessionID, remoteFS, readOnly, timeout, token...)
}

func RequestCommandForContextWithMountOptions(ctx context.Context, socketPath string, argv []string, stdin []byte, env map[string]string, cwd, contextID, sessionID string, remoteFS, readOnly bool, timeout time.Duration, token ...string) (CommandResult, error) {
	if len(argv) == 0 {
		return CommandResult{ExitCode: 2}, errors.New("local command is required")
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return CommandResult{ExitCode: 1}, err
	}
	defer c.Close()
	enc := protocol.NewEncoder(c)
	dec := protocol.NewDecoder(c)
	if err := enc.Encode(protocol.Frame{Type: protocol.TypeHello, ProtocolVersion: protocol.Version, ProtocolMin: protocol.MinVersion, ProtocolMax: protocol.MaxVersion, RuntimeID: identity.RuntimeID, Role: protocol.RoleRequester, ContextID: contextID, SessionID: sessionID, Token: firstToken(token)}); err != nil {
		return CommandResult{ExitCode: 1}, err
	}
	id, idErr := identity.UUID()
	if idErr != nil {
		return CommandResult{ExitCode: 1}, idErr
	}
	if err := enc.Encode(protocol.Frame{
		Type:          protocol.TypeCommandExec,
		ID:            id,
		RequestID:     id,
		Argv:          argv,
		Env:           env,
		Cwd:           cwd,
		ContextID:     contextID,
		SessionID:     sessionID,
		RemoteFS:      remoteFS,
		MountReadOnly: readOnly,
		Stdin:         base64.StdEncoding.EncodeToString(stdin),
		TimeoutMillis: durationMillis(timeout),
	}); err != nil {
		return CommandResult{ExitCode: 1}, err
	}
	resp, err := dec.Decode()
	if err != nil {
		return CommandResult{ExitCode: 1}, err
	}
	if resp.Type == protocol.TypeCommandError || resp.Type == protocol.TypeError {
		exitCode := resp.ExitCode
		if exitCode == 0 {
			exitCode = 1
		}
		return CommandResult{ExitCode: exitCode}, errors.New(resp.Error)
	}
	if resp.Type != protocol.TypeCommandResult {
		return CommandResult{ExitCode: 1}, fmt.Errorf("unexpected response type %q", resp.Type)
	}
	stdout, err := base64.StdEncoding.DecodeString(resp.Stdout)
	if err != nil {
		return CommandResult{ExitCode: 1}, err
	}
	stderr, err := base64.StdEncoding.DecodeString(resp.Stderr)
	if err != nil {
		return CommandResult{ExitCode: 1}, err
	}
	return CommandResult{ExitCode: resp.ExitCode, Stdout: stdout, Stderr: stderr}, nil
}

func RunClient(ctx context.Context, socketPath string) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return err
	}
	return RunClientConn(ctx, c)
}

type readWriteCloser struct {
	io.Reader
	io.Writer
	close func() error
}

func (r readWriteCloser) Close() error {
	if r.close == nil {
		return nil
	}
	return r.close()
}

func NewReadWriteCloser(reader io.Reader, writer io.Writer, close func() error) io.ReadWriteCloser {
	return readWriteCloser{Reader: reader, Writer: writer, close: close}
}

func RunClientConn(ctx context.Context, c io.ReadWriteCloser) error {
	return RunClientConnReady(ctx, c, nil)
}

func RunClientConnReady(ctx context.Context, c io.ReadWriteCloser, ready chan<- error) error {
	return RunClientConnReadyPolicy(ctx, c, ready, nil)
}

func RunClientConnReadyPolicy(ctx context.Context, c io.ReadWriteCloser, ready chan<- error, allow CommandAllowed, token ...string) error {
	return RunClientConnWithOptions(ctx, c, ClientOptions{Ready: ready, Allow: allow}, token...)
}

func RunClientConnWithOptions(ctx context.Context, c io.ReadWriteCloser, opts ClientOptions, token ...string) error {
	defer c.Close()
	if opts.AppVersion == "" {
		opts.AppVersion = version.Version
	}
	if opts.SessionID == "" {
		sessionID, err := identity.UUID()
		if err != nil {
			signalReady(opts.Ready, err)
			return err
		}
		opts.SessionID = sessionID
	}
	if opts.RuntimeID == "" {
		opts.RuntimeID = identity.RuntimeID
	}
	if opts.TargetID == "" {
		opts.TargetID = "default"
	}
	if opts.ContextID == "" {
		opts.ContextID = "default"
	}
	if len(opts.Capabilities) == 0 {
		opts.Capabilities = []string{"command.exec.batch-stdin", "heartbeat.v1", "remotefs.fs.v1"}
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if opts.HeartbeatTimeout <= 0 {
		opts.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	go func() {
		<-ctx.Done()
		_ = c.Close()
	}()
	enc := protocol.NewEncoder(c)
	dec := protocol.NewDecoder(c)
	var writeMu sync.Mutex
	send := func(frame protocol.Frame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return enc.Encode(frame)
	}
	if err := send(protocol.Frame{Type: protocol.TypeHello, ProtocolVersion: protocol.Version, ProtocolMin: protocol.MinVersion, ProtocolMax: protocol.MaxVersion, RuntimeID: opts.RuntimeID, AppVersion: opts.AppVersion, TargetID: opts.TargetID, ContextID: opts.ContextID, SessionID: opts.SessionID, Capabilities: opts.Capabilities, Role: protocol.RoleClient, Token: firstToken(token)}); err != nil {
		signalReady(opts.Ready, err)
		return err
	}
	clientCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var lastAck atomic.Int64
	lastAck.Store(time.Now().UnixNano())
	go func() {
		ticker := time.NewTicker(opts.HeartbeatInterval)
		defer ticker.Stop()
		var sequence uint64
		for {
			select {
			case <-clientCtx.Done():
				return
			case <-ticker.C:
				if time.Since(time.Unix(0, lastAck.Load())) >= opts.HeartbeatTimeout {
					_ = c.Close()
					return
				}
				sequence++
				if err := send(protocol.Frame{Type: protocol.TypeHeartbeat, ProtocolVersion: protocol.Version, ProtocolMin: protocol.MinVersion, ProtocolMax: protocol.MaxVersion, RuntimeID: opts.RuntimeID, AppVersion: opts.AppVersion, ContextID: opts.ContextID, SessionID: opts.SessionID, Sequence: sequence}); err != nil {
					_ = c.Close()
					return
				}
			}
		}
	}()
	readySignaled := false
	for {
		frame, err := dec.Decode()
		if err != nil {
			if ctx.Err() != nil {
				if !readySignaled {
					signalReady(opts.Ready, nil)
				}
				return nil
			}
			if !readySignaled {
				signalReady(opts.Ready, err)
			}
			return err
		}
		if !readySignaled && frame.Type == protocol.TypeCapabilities {
			if !protocol.FrameCompatible(frame) || frame.RuntimeID != opts.RuntimeID {
				err := errors.New("sshx bridge runtime is incompatible")
				signalReady(opts.Ready, err)
				return err
			}
			readySignaled = true
			signalReady(opts.Ready, nil)
			continue
		}
		if frame.Type == protocol.TypeHeartbeatAck {
			if !protocol.FrameCompatible(frame) || frame.RuntimeID != opts.RuntimeID {
				return errors.New("sshx bridge heartbeat runtime is incompatible")
			}
			lastAck.Store(time.Now().UnixNano())
			continue
		}
		if frame.Type == protocol.TypeServerDrain {
			if !readySignaled {
				signalReady(opts.Ready, errors.New(frame.Error))
			}
			return fmt.Errorf("sshx server draining: %s", frame.Error)
		}
		if frame.Type == protocol.TypePortObserved {
			if opts.OnPortObserved != nil && frame.Port > 0 {
				opts.OnPortObserved(frame.Port)
			}
			continue
		}
		if frame.Type == protocol.TypePortGone {
			if opts.OnPortGone != nil && frame.Port > 0 {
				opts.OnPortGone(frame.Port)
			}
			continue
		}
		if frame.Type != protocol.TypeCommandExec {
			continue
		}
		if opts.Allow != nil && !opts.Allow(frame.Argv) {
			if err := enc.Encode(protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: "command denied by sshx policy"}); err != nil {
				return err
			}
			continue
		}
		go func(frame protocol.Frame) {
			execute := opts.Execute
			if execute == nil {
				execute = ExecuteLocal
			}
			resp := execute(clientCtx, frame)
			_ = send(resp)
		}(frame)
	}
}

func firstToken(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	return tokens[0]
}

func signalReady(ready chan<- error, err error) {
	if ready == nil {
		return
	}
	select {
	case ready <- err:
	default:
	}
}

func SocketProxy(ctx context.Context, socketPath string, stdin io.Reader, stdout io.Writer) error {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return err
	}
	defer c.Close()
	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(c, stdin)
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		errCh <- err
	}()
	go func() {
		_, err := io.Copy(stdout, c)
		errCh <- err
	}()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		if err == io.EOF {
			return nil
		}
		return err
	}
}

func ExecuteLocal(ctx context.Context, frame protocol.Frame) protocol.Frame {
	if len(frame.Argv) == 0 {
		return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: "command argv is empty"}
	}
	stdin, err := base64.StdEncoding.DecodeString(frame.Stdin)
	if err != nil {
		return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: err.Error()}
	}
	commandCtx := ctx
	cancel := func() {}
	if frame.TimeoutMillis > 0 {
		commandCtx, cancel = context.WithTimeout(ctx, time.Duration(frame.TimeoutMillis)*time.Millisecond)
	}
	defer cancel()
	cmd := exec.CommandContext(commandCtx, frame.Argv[0], frame.Argv[1:]...)
	// CommandContext only kills the direct child. Shell commands can leave
	// descendants running with stdout/stderr still open, which makes Wait block
	// until those descendants exit. Give the command its own process group and
	// cancel the whole group so explicit timeouts cover the complete command
	// tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.Stdin = bytes.NewReader(stdin)
	if frame.Cwd != "" {
		cmd.Dir = frame.Cwd
	} else if frame.Env["SSHX_REMOTE_FS"] == "0" {
		if home, homeErr := os.UserHomeDir(); homeErr == nil {
			cmd.Dir = home
		}
	}
	if len(frame.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range frame.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	if frame.TimeoutMillis > 0 && errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
		return protocol.Frame{
			Type:     protocol.TypeCommandError,
			ID:       frame.ID,
			ExitCode: 124,
			Error:    fmt.Sprintf("command timed out after %s", time.Duration(frame.TimeoutMillis)*time.Millisecond),
		}
	}
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: err.Error()}
		}
	}
	return protocol.Frame{
		Type:     protocol.TypeCommandResult,
		ID:       frame.ID,
		ExitCode: exitCode,
		Stdout:   base64.StdEncoding.EncodeToString(stdout.Bytes()),
		Stderr:   base64.StdEncoding.EncodeToString(stderr.Bytes()),
	}
}

func durationMillis(timeout time.Duration) int64 {
	if timeout <= 0 {
		return 0
	}
	millis := timeout / time.Millisecond
	if timeout%time.Millisecond != 0 {
		millis++
	}
	return int64(millis)
}

type syncBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.b = append(b.b, p...)
	return len(p), nil
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.b...)
}
