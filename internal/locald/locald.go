package locald

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xiaot623/sshx/internal/domain"
	"github.com/xiaot623/sshx/internal/forward"
	"github.com/xiaot623/sshx/internal/identity"
	"github.com/xiaot623/sshx/internal/processlock"
	"github.com/xiaot623/sshx/internal/protocol"
)

const (
	TypePing             = "ping"
	TypeEnsureTargetPort = "ensureTargetPort"
	TypeRemoveTargetPort = "removeTargetPort"
	TypeListPorts        = "listPorts"
	TypeOpenSession      = "session.open"
	TypeHeartbeat        = "heartbeat"
	TypeHeartbeatAck     = "heartbeat.ack"
	TypeShutdown         = "shutdown"
)

const (
	DefaultHeartbeatInterval = 5 * time.Second
	DefaultLeaseTimeout      = 15 * time.Second
	DefaultStartupTimeout    = 10 * time.Second
)

type Request struct {
	Type            string   `json:"type"`
	SSHPath         string   `json:"sshPath,omitempty"`
	Target          string   `json:"target,omitempty"`
	SSHArgs         []string `json:"sshArgs,omitempty"`
	RemotePort      int      `json:"remotePort,omitempty"`
	RemoteHost      string   `json:"remoteHost,omitempty"`
	DomainSuffix    string   `json:"domainSuffix,omitempty"`
	DNSAddr         string   `json:"dnsAddr,omitempty"`
	SessionID       string   `json:"sessionId,omitempty"`
	LeaseID         string   `json:"leaseId,omitempty"`
	TargetID        string   `json:"targetId,omitempty"`
	ControlPath     string   `json:"controlPath,omitempty"`
	RuntimeID       string   `json:"runtimeId,omitempty"`
	AppVersion      string   `json:"appVersion,omitempty"`
	Sequence        uint64   `json:"sequence,omitempty"`
	ProtocolVersion int      `json:"protocolVersion,omitempty"`
	ProtocolMin     int      `json:"protocolMin,omitempty"`
	ProtocolMax     int      `json:"protocolMax,omitempty"`
}

type Response struct {
	OK              bool        `json:"ok"`
	Error           string      `json:"error,omitempty"`
	Type            string      `json:"type,omitempty"`
	Version         string      `json:"version,omitempty"`
	Sequence        uint64      `json:"sequence,omitempty"`
	ProtocolVersion int         `json:"protocolVersion,omitempty"`
	ProtocolMin     int         `json:"protocolMin,omitempty"`
	ProtocolMax     int         `json:"protocolMax,omitempty"`
	RuntimeID       string      `json:"runtimeId,omitempty"`
	LocalPort       int         `json:"localPort,omitempty"`
	Domain          string      `json:"domain,omitempty"`
	ListenIP        string      `json:"listenIp,omitempty"`
	Forwards        []Forwarded `json:"forwards,omitempty"`
}

type Forwarded struct {
	Target     string `json:"target"`
	Domain     string `json:"domain,omitempty"`
	ListenIP   string `json:"listenIp,omitempty"`
	LocalPort  int    `json:"localPort"`
	RemotePort int    `json:"remotePort"`
}

type targetRecord struct {
	Target        string
	Domain        string
	ListenIP      string
	Sessions      int
	domainManager *domain.Manager
}

type sessionRecord struct {
	TargetKey   string
	SSHPath     string
	SSHArgs     []string
	ControlPath string
	conn        net.Conn
}

type Server struct {
	SocketPath     string
	Stderr         io.Writer
	Version        string
	LeaseTimeout   time.Duration
	StartupTimeout time.Duration
	HandoffGrace   time.Duration

	mu           sync.Mutex
	forwarders   map[string]*forward.Manager
	targets      map[string]*targetRecord
	domains      map[string]*domain.Manager
	sessions     map[string]*sessionRecord
	shutdown     chan struct{}
	shutdownOnce sync.Once
	draining     bool
	connections  map[net.Conn]struct{}
	connWG       sync.WaitGroup
}

func DefaultSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "sshx-local.sock")
	}
	return filepath.Join(home, ".sshx", "local.sock")
}

func (s *Server) Serve(ctx context.Context) error {
	if s.SocketPath == "" {
		return errors.New("local daemon socket path is required")
	}
	if s.Stderr == nil {
		s.Stderr = io.Discard
	}
	if s.LeaseTimeout <= 0 {
		s.LeaseTimeout = DefaultLeaseTimeout
	}
	if s.StartupTimeout <= 0 {
		s.StartupTimeout = DefaultStartupTimeout
	}
	if s.sessions == nil {
		s.sessions = map[string]*sessionRecord{}
	}
	if s.shutdown == nil {
		s.shutdown = make(chan struct{})
	}
	if s.connections == nil {
		s.connections = map[net.Conn]struct{}{}
	}
	if s.forwarders == nil {
		s.forwarders = map[string]*forward.Manager{}
	}
	if s.targets == nil {
		s.targets = map[string]*targetRecord{}
	}
	if s.domains == nil {
		s.domains = map[string]*domain.Manager{}
	}
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
	ownedSocket, _ := os.Stat(s.SocketPath)
	defer removeOwnedSocket(s.SocketPath, ownedSocket)
	defer func() {
		s.stopResources()
		s.closeConnections()
		s.connWG.Wait()
	}()
	if err := os.Chmod(s.SocketPath, 0o600); err != nil {
		return err
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-s.shutdown:
		}
		_ = ln.Close()
	}()
	go s.monitorStartup(ctx)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
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
			s.handleConn(ctx, conn)
		}()
	}
}

func removeOwnedSocket(path string, owned os.FileInfo) {
	if owned == nil {
		return
	}
	current, err := os.Stat(path)
	if err == nil && os.SameFile(owned, current) {
		_ = os.Remove(path)
	}
}

func (s *Server) monitorStartup(ctx context.Context) {
	timer := time.NewTimer(s.StartupTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-s.shutdown:
	case <-timer.C:
		s.mu.Lock()
		empty := len(s.sessions) == 0
		if empty {
			s.draining = true
		}
		s.mu.Unlock()
		if empty {
			s.initiateShutdown()
		}
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(bufio.NewReader(conn))
	enc := json.NewEncoder(conn)
	var req Request
	if err := dec.Decode(&req); err != nil {
		_ = enc.Encode(Response{OK: false, Error: err.Error()})
		return
	}
	if req.Type == TypeOpenSession {
		s.handleSession(ctx, conn, dec, enc, req)
		return
	}
	if (req.Type == TypeEnsureTargetPort || req.Type == TypeRemoveTargetPort) && !s.hasSession(requestLeaseID(req)) {
		_ = enc.Encode(Response{
			OK:              false,
			Error:           "active session lease is required",
			Version:         s.Version,
			ProtocolVersion: protocol.Version})
		return
	}
	resp := s.handle(ctx, req)
	_ = enc.Encode(resp)
	if req.Type == TypeShutdown && resp.OK {
		s.initiateShutdown()
	}
}

func (s *Server) hasSession(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.sessions[sessionID]
	return ok
}

func (s *Server) handle(ctx context.Context, req Request) Response {
	switch req.Type {
	case TypePing:
		return protocolResponse(s.Version, true)
	case TypeEnsureTargetPort:
		return s.ensureTargetPort(ctx, req)
	case TypeRemoveTargetPort:
		return s.removeTargetPort(req)
	case TypeListPorts:
		return s.listPorts()
	case TypeShutdown:
		return Response{OK: true, Version: s.Version}
	default:
		return Response{OK: false, Error: "unknown local daemon request type"}
	}
}

func (s *Server) initiateShutdown() {
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		s.draining = true
		s.mu.Unlock()
		if s.shutdown != nil {
			close(s.shutdown)
		}
	})
}

func (s *Server) stopResources() {
	s.mu.Lock()
	forwarders := make([]*forward.Manager, 0, len(s.forwarders))
	for _, fwd := range s.forwarders {
		forwarders = append(forwarders, fwd)
	}
	domains := make([]*domain.Manager, 0, len(s.domains))
	for _, dom := range s.domains {
		domains = append(domains, dom)
	}
	connections := make([]net.Conn, 0, len(s.sessions))
	for _, session := range s.sessions {
		connections = append(connections, session.conn)
	}
	s.mu.Unlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
	for _, fwd := range forwarders {
		fwd.Stop()
	}
	for _, dom := range domains {
		dom.Stop()
	}
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

func defaultSSHArgs(req Request) []string {
	if len(req.SSHArgs) > 0 {
		return req.SSHArgs
	}
	return []string{req.Target}
}

func requestKey(sshPath string, sshArgs []string) string {
	return sshPath + "\x00" + strings.Join(sshArgs, "\x00")
}

func targetKey(req Request) string {
	if req.TargetID != "" {
		return req.TargetID
	}
	return requestKey(req.SSHPath, defaultSSHArgs(req))
}

func requestLeaseID(req Request) string {
	if req.LeaseID != "" {
		return req.LeaseID
	}
	return req.SessionID
}

func requestCompatible(req Request) bool {
	frame := protocol.Frame{
		ProtocolVersion: req.ProtocolVersion,
		ProtocolMin:     req.ProtocolMin,
		ProtocolMax:     req.ProtocolMax}
	return protocol.FrameCompatible(frame) && req.RuntimeID == identity.LocalRuntimeID
}

func protocolResponse(version string, ok bool) Response {
	return Response{
		OK:              ok,
		Version:         version,
		ProtocolVersion: protocol.Version,
		ProtocolMin:     protocol.MinVersion,
		ProtocolMax:     protocol.MaxVersion,
		RuntimeID:       identity.LocalRuntimeID}
}

func ClientRequest(ctx context.Context, socketPath string, req Request) (Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return Response{}, err
	}
	if !resp.OK {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}
