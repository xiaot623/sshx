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
	TypePing       = "ping"
	TypeEnsurePort = "ensurePort"
	TypeListPorts  = "listPorts"
)

type Request struct {
	Type           string   `json:"type"`
	SSHPath        string   `json:"sshPath,omitempty"`
	Target         string   `json:"target,omitempty"`
	SSHArgs        []string `json:"sshArgs,omitempty"`
	RemotePort     int      `json:"remotePort,omitempty"`
	DomainsEnabled bool     `json:"domainsEnabled,omitempty"`
	DomainSuffix   string   `json:"domainSuffix,omitempty"`
	DNSAddr        string   `json:"dnsAddr,omitempty"`
}

type Response struct {
	OK        bool        `json:"ok"`
	Error     string      `json:"error,omitempty"`
	LocalPort int         `json:"localPort,omitempty"`
	Domain    string      `json:"domain,omitempty"`
	Forwards  []Forwarded `json:"forwards,omitempty"`
}

type Forwarded struct {
	Target     string `json:"target"`
	LocalPort  int    `json:"localPort"`
	RemotePort int    `json:"remotePort"`
}

type Server struct {
	SocketPath string
	Stderr     io.Writer

	mu         sync.Mutex
	forwarders map[string]*forward.Manager
	targets    map[string]string
	domains    map[string]*domain.Manager
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
	if s.targets == nil {
		s.targets = map[string]string{}
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
	case TypeEnsurePort:
		return s.ensurePort(ctx, req)
	case TypeListPorts:
		return s.listPorts()
	default:
		return Response{OK: false, Error: "unknown local daemon request type"}
	}
}

func (s *Server) ensurePort(ctx context.Context, req Request) Response {
	if req.Target == "" || req.SSHPath == "" || req.RemotePort <= 0 {
		return Response{OK: false, Error: "sshPath, target and remotePort are required"}
	}
	sshArgs := req.SSHArgs
	if len(sshArgs) == 0 {
		sshArgs = []string{req.Target}
	}
	fwd := s.forwarder(ctx, req.SSHPath, sshArgs)
	s.rememberTarget(req.SSHPath, sshArgs, req.Target)
	f, err := fwd.Ensure(req.RemotePort, req.DomainsEnabled)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	resp := Response{OK: true, LocalPort: f.LocalPort}
	if req.DomainsEnabled {
		dom, err := s.domain(ctx, req)
		if err != nil {
			return Response{OK: false, Error: err.Error(), LocalPort: f.LocalPort}
		}
		resp.Domain = dom.NameForTarget(req.Target)
	}
	return resp
}

func (s *Server) rememberTarget(sshPath string, sshArgs []string, target string) {
	key := sshPath + "\x00" + strings.Join(sshArgs, "\x00")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.targets == nil {
		s.targets = map[string]string{}
	}
	s.targets[key] = target
}

func (s *Server) forwarder(ctx context.Context, sshPath string, sshArgs []string) *forward.Manager {
	key := sshPath + "\x00" + strings.Join(sshArgs, "\x00")
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

func (s *Server) listPorts() Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Forwarded
	for key, fwd := range s.forwarders {
		target := s.targets[key]
		for _, entry := range fwd.List() {
			out = append(out, Forwarded{
				Target:     target,
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
