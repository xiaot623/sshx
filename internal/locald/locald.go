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

	"github.com/xiaot623/sshx/internal/domain"
	"github.com/xiaot623/sshx/internal/forward"
)

const (
	TypePing             = "ping"
	TypeRegisterTarget   = "registerTarget"
	TypeUnregisterTarget = "unregisterTarget"
	TypeEnsureTargetPort = "ensureTargetPort"
	TypeRemoveTargetPort = "removeTargetPort"
	TypeListPorts        = "listPorts"
)

type Request struct {
	Type         string   `json:"type"`
	SSHPath      string   `json:"sshPath,omitempty"`
	Target       string   `json:"target,omitempty"`
	SSHArgs      []string `json:"sshArgs,omitempty"`
	RemotePort   int      `json:"remotePort,omitempty"`
	DomainSuffix string   `json:"domainSuffix,omitempty"`
	DNSAddr      string   `json:"dnsAddr,omitempty"`
}

type Response struct {
	OK        bool        `json:"ok"`
	Error     string      `json:"error,omitempty"`
	LocalPort int         `json:"localPort,omitempty"`
	Domain    string      `json:"domain,omitempty"`
	ListenIP  string      `json:"listenIp,omitempty"`
	Forwards  []Forwarded `json:"forwards,omitempty"`
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
	Refs     int
}

type Server struct {
	SocketPath string
	Stderr     io.Writer

	mu             sync.Mutex
	forwarders     map[string]*forward.Manager
	forwardRecords map[string]map[int]forwardRecord
	targets        map[string]*targetRecord
	domains        map[string]*domain.Manager
	nextLoopback   int
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
	_ = os.Remove(s.SocketPath)
	ln, err := net.Listen("unix", s.SocketPath)
	if err != nil {
		return err
	}
	defer ln.Close()
	defer os.Remove(s.SocketPath)
	if err := os.Chmod(s.SocketPath, 0o600); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handleConn(ctx, conn)
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
	resp := s.handle(ctx, req)
	_ = enc.Encode(resp)
}

func (s *Server) handle(ctx context.Context, req Request) Response {
	switch req.Type {
	case TypePing:
		return Response{OK: true}
	case TypeRegisterTarget:
		return s.registerTarget(ctx, req)
	case TypeUnregisterTarget:
		return s.unregisterTarget(req)
	case TypeEnsureTargetPort:
		return s.ensureTargetPort(ctx, req)
	case TypeRemoveTargetPort:
		return s.removeTargetPort(req)
	case TypeListPorts:
		return s.listPorts()
	default:
		return Response{OK: false, Error: "unknown local daemon request type"}
	}
}

func (s *Server) registerTarget(ctx context.Context, req Request) Response {
	rec, err := s.ensureTarget(ctx, req)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	s.mu.Lock()
	rec.Refs++
	s.mu.Unlock()
	return Response{OK: true, Domain: rec.Domain, ListenIP: rec.ListenIP}
}

func (s *Server) unregisterTarget(req Request) Response {
	if req.Target == "" || req.SSHPath == "" {
		return Response{OK: false, Error: "sshPath and target are required"}
	}
	key := requestKey(req.SSHPath, defaultSSHArgs(req))
	var fwd *forward.Manager
	s.mu.Lock()
	if rec := s.targets[key]; rec != nil {
		if rec.Refs > 0 {
			rec.Refs--
		}
		if rec.Refs == 0 {
			fwd = s.forwarders[key]
			delete(s.forwardRecords, key)
		}
	}
	s.mu.Unlock()
	if fwd != nil {
		fwd.Stop()
	}
	return Response{OK: true}
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
	rec := &targetRecord{
		Target:   req.Target,
		Domain:   dom.NameForTarget(req.Target),
		ListenIP: s.allocateLoopbackIPLocked(),
	}
	s.targets[key] = rec
	s.mu.Unlock()

	if err := dom.Register(rec.Domain, net.ParseIP(rec.ListenIP)); err != nil {
		return nil, err
	}
	return rec, nil
}

func (s *Server) allocateLoopbackIPLocked() string {
	if s.nextLoopback == 0 {
		s.nextLoopback = 1
	}
	offset := s.nextLoopback
	s.nextLoopback++
	second := 64 + offset/(254*254)
	third := (offset / 254) % 254
	fourth := offset%254 + 1
	if second > 126 {
		second = 126
	}
	return fmt.Sprintf("127.%d.%d.%d", second, third, fourth)
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
