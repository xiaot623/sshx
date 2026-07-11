package locald

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/xiaot623/sshx/internal/domain"
	"github.com/xiaot623/sshx/internal/forward"
	"github.com/xiaot623/sshx/internal/loopback"
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
	DomainSuffix    string   `json:"domainSuffix,omitempty"`
	DNSAddr         string   `json:"dnsAddr,omitempty"`
	SessionID       string   `json:"sessionId,omitempty"`
	AppVersion      string   `json:"appVersion,omitempty"`
	Sequence        uint64   `json:"sequence,omitempty"`
	ProtocolVersion int      `json:"protocolVersion,omitempty"`
}

type Response struct {
	OK              bool        `json:"ok"`
	Error           string      `json:"error,omitempty"`
	Type            string      `json:"type,omitempty"`
	Version         string      `json:"version,omitempty"`
	Sequence        uint64      `json:"sequence,omitempty"`
	ProtocolVersion int         `json:"protocolVersion,omitempty"`
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

type forwardRecord struct {
	Target   string
	Domain   string
	ListenIP string
}

type targetRecord struct {
	Target   string
	Domain   string
	ListenIP string
	Sessions int
}

type sessionRecord struct {
	ID        string
	TargetKey string
	Version   string
	conn      net.Conn
}

type Server struct {
	SocketPath     string
	Stderr         io.Writer
	Version        string
	LeaseTimeout   time.Duration
	StartupTimeout time.Duration

	mu             sync.Mutex
	forwarders     map[string]*forward.Manager
	forwardRecords map[string]map[int]forwardRecord
	targets        map[string]*targetRecord
	domains        map[string]*domain.Manager
	sessions       map[string]*sessionRecord
	shutdown       chan struct{}
	shutdownOnce   sync.Once
	draining       bool
	connections    map[net.Conn]struct{}
	connWG         sync.WaitGroup
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
	if s.forwardRecords == nil {
		s.forwardRecords = map[string]map[int]forwardRecord{}
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
	if (req.Type == TypeEnsureTargetPort || req.Type == TypeRemoveTargetPort) && !s.hasSession(req.SessionID) {
		_ = enc.Encode(Response{OK: false, Error: "active session lease is required", Version: s.Version, ProtocolVersion: protocol.Version})
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
		return Response{OK: true, Version: s.Version, ProtocolVersion: protocol.Version}
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

func (s *Server) handleSession(ctx context.Context, conn net.Conn, dec *json.Decoder, enc *json.Encoder, req Request) {
	if req.SessionID == "" || req.AppVersion == "" {
		_ = enc.Encode(Response{OK: false, Error: "sessionId and appVersion are required", Version: s.Version, ProtocolVersion: protocol.Version})
		return
	}
	if req.ProtocolVersion != protocol.Version || req.AppVersion != s.Version {
		_ = enc.Encode(Response{OK: false, Error: "local daemon version mismatch", Version: s.Version, ProtocolVersion: protocol.Version})
		s.initiateShutdown()
		return
	}
	s.mu.Lock()
	draining := s.draining
	s.mu.Unlock()
	if draining {
		_ = enc.Encode(Response{OK: false, Error: "local daemon is draining", Version: s.Version, ProtocolVersion: protocol.Version})
		return
	}
	rec, err := s.ensureTarget(ctx, req)
	if err != nil {
		_ = enc.Encode(Response{OK: false, Error: err.Error(), Version: s.Version, ProtocolVersion: protocol.Version})
		return
	}
	key := requestKey(req.SSHPath, defaultSSHArgs(req))
	session := &sessionRecord{ID: req.SessionID, TargetKey: key, Version: req.AppVersion, conn: conn}
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		_ = enc.Encode(Response{OK: false, Error: "local daemon is draining", Version: s.Version, ProtocolVersion: protocol.Version})
		return
	}
	if _, exists := s.sessions[req.SessionID]; exists {
		s.mu.Unlock()
		_ = enc.Encode(Response{OK: false, Error: "session already exists", Version: s.Version, ProtocolVersion: protocol.Version})
		return
	}
	s.sessions[req.SessionID] = session
	rec.Sessions++
	s.mu.Unlock()
	defer s.releaseSession(req.SessionID)

	if err := enc.Encode(Response{OK: true, Type: TypeOpenSession, Version: s.Version, ProtocolVersion: protocol.Version, Domain: rec.Domain, ListenIP: rec.ListenIP}); err != nil {
		return
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.LeaseTimeout))
		var heartbeat Request
		if err := dec.Decode(&heartbeat); err != nil {
			return
		}
		if heartbeat.Type != TypeHeartbeat || heartbeat.SessionID != req.SessionID {
			_ = enc.Encode(Response{OK: false, Error: "invalid session heartbeat", Version: s.Version, ProtocolVersion: protocol.Version})
			return
		}
		if heartbeat.ProtocolVersion != protocol.Version || heartbeat.AppVersion != s.Version {
			_ = enc.Encode(Response{OK: false, Error: "local daemon version mismatch", Version: s.Version, ProtocolVersion: protocol.Version, Sequence: heartbeat.Sequence})
			s.initiateShutdown()
			return
		}
		if err := enc.Encode(Response{OK: true, Type: TypeHeartbeatAck, Version: s.Version, ProtocolVersion: protocol.Version, Sequence: heartbeat.Sequence}); err != nil {
			return
		}
	}
}

func (s *Server) releaseSession(sessionID string) {
	var fwd *forward.Manager
	var domainName string
	var shutdown bool
	s.mu.Lock()
	session := s.sessions[sessionID]
	if session != nil {
		delete(s.sessions, sessionID)
		if rec := s.targets[session.TargetKey]; rec != nil {
			if rec.Sessions > 0 {
				rec.Sessions--
			}
			if rec.Sessions == 0 {
				fwd = s.forwarders[session.TargetKey]
				delete(s.forwarders, session.TargetKey)
				delete(s.forwardRecords, session.TargetKey)
				domainName = rec.Domain
				delete(s.targets, session.TargetKey)
			}
		}
	}
	shutdown = len(s.sessions) == 0
	if shutdown {
		s.draining = true
	}
	s.mu.Unlock()
	if fwd != nil {
		fwd.Stop()
	}
	if domainName != "" {
		s.unregisterDomain(domainName)
	}
	if shutdown {
		s.initiateShutdown()
	}
}

func (s *Server) unregisterDomain(name string) {
	s.mu.Lock()
	domains := make([]*domain.Manager, 0, len(s.domains))
	for _, dom := range s.domains {
		domains = append(domains, dom)
	}
	s.mu.Unlock()
	for _, dom := range domains {
		dom.Unregister(name)
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

func (s *Server) ensureTargetPort(ctx context.Context, req Request) Response {
	if req.RemotePort <= 0 {
		return Response{OK: false, Error: "remotePort is required"}
	}
	rec, err := s.ensureTarget(ctx, req)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	sshArgs := defaultSSHArgs(req)
	fwd := s.forwarder(ctx, req.SSHPath, sshArgs)
	f, err := fwd.Ensure(req.RemotePort, rec.ListenIP)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	s.rememberForward(req.SSHPath, sshArgs, req.RemotePort, rec.Target, rec.Domain, rec.ListenIP)
	return Response{OK: true, LocalPort: f.LocalPort, Domain: rec.Domain, ListenIP: rec.ListenIP}
}

func (s *Server) removeTargetPort(req Request) Response {
	if req.Target == "" || req.SSHPath == "" || req.RemotePort <= 0 {
		return Response{OK: false, Error: "sshPath, target and remotePort are required"}
	}
	key := requestKey(req.SSHPath, defaultSSHArgs(req))
	var fwd *forward.Manager
	s.mu.Lock()
	fwd = s.forwarders[key]
	if records := s.forwardRecords[key]; records != nil {
		delete(records, req.RemotePort)
		if len(records) == 0 {
			delete(s.forwardRecords, key)
		}
	}
	s.mu.Unlock()
	if fwd != nil {
		fwd.Remove(req.RemotePort)
	}
	return Response{OK: true}
}

func (s *Server) rememberForward(sshPath string, sshArgs []string, remotePort int, target string, domain string, listenIP string) {
	key := requestKey(sshPath, sshArgs)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forwardRecords == nil {
		s.forwardRecords = map[string]map[int]forwardRecord{}
	}
	if s.forwardRecords[key] == nil {
		s.forwardRecords[key] = map[int]forwardRecord{}
	}
	s.forwardRecords[key][remotePort] = forwardRecord{Target: target, Domain: domain, ListenIP: listenIP}
}

func (s *Server) ensureTarget(ctx context.Context, req Request) (*targetRecord, error) {
	if req.Target == "" || req.SSHPath == "" || req.DomainSuffix == "" || req.DNSAddr == "" {
		return nil, errors.New("sshPath, target, domainSuffix and dnsAddr are required")
	}
	sshArgs := defaultSSHArgs(req)
	key := requestKey(req.SSHPath, sshArgs)
	s.mu.Lock()
	if rec := s.targets[key]; rec != nil {
		s.mu.Unlock()
		return rec, nil
	}
	s.mu.Unlock()

	dom, err := s.domain(ctx, req)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.targets == nil {
		s.targets = map[string]*targetRecord{}
	}
	if rec := s.targets[key]; rec != nil {
		s.mu.Unlock()
		return rec, nil
	}
	listenIP, err := s.allocateLoopbackIPLocked()
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	rec := &targetRecord{
		Target:   req.Target,
		Domain:   dom.NameForTarget(req.Target),
		ListenIP: listenIP,
	}
	if err := dom.Register(rec.Domain, net.ParseIP(rec.ListenIP)); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.targets[key] = rec
	s.mu.Unlock()
	return rec, nil
}

func (s *Server) allocateLoopbackIPLocked() (string, error) {
	used := make(map[string]struct{}, len(s.targets))
	for _, rec := range s.targets {
		used[rec.ListenIP] = struct{}{}
	}
	for i := 0; i < loopback.Size; i++ {
		ip := loopback.Address(i)
		if _, exists := used[ip]; !exists {
			return ip, nil
		}
	}
	return "", fmt.Errorf("target loopback address pool exhausted (%d addresses in use)", loopback.Size)
}

func (s *Server) forwarder(ctx context.Context, sshPath string, sshArgs []string) *forward.Manager {
	key := requestKey(sshPath, sshArgs)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forwarders == nil {
		s.forwarders = map[string]*forward.Manager{}
	}
	if f := s.forwarders[key]; f != nil {
		return f
	}
	f := forward.NewManager(ctx, sshPath, sshArgs, s.Stderr)
	s.forwarders[key] = f
	return f
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

func (s *Server) listPorts() Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Forwarded
	for key, fwd := range s.forwarders {
		for _, entry := range fwd.List() {
			record := s.forwardRecords[key][entry.RemotePort]
			out = append(out, Forwarded{
				Target:     record.Target,
				Domain:     record.Domain,
				ListenIP:   record.ListenIP,
				LocalPort:  entry.LocalPort,
				RemotePort: entry.RemotePort,
			})
		}
	}
	return Response{OK: true, Forwards: out}
}

func (s *Server) domain(ctx context.Context, req Request) (*domain.Manager, error) {
	key := fmt.Sprintf("%s\x00%s", req.DomainSuffix, req.DNSAddr)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.domains == nil {
		s.domains = map[string]*domain.Manager{}
	}
	if d := s.domains[key]; d != nil {
		return d, nil
	}
	d := domain.NewManager(req.DomainSuffix, req.DNSAddr, s.Stderr)
	if err := d.Start(ctx); err != nil {
		return nil, err
	}
	s.domains[key] = d
	return d, nil
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
