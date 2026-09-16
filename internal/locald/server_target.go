package locald

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/xiaot623/sshx/internal/domain"
	"github.com/xiaot623/sshx/internal/forward"
	"github.com/xiaot623/sshx/internal/loopback"
)

func (s *Server) ensureTargetPort(ctx context.Context, req Request) Response {
	if req.RemotePort <= 0 {
		return Response{OK: false, Error: "remotePort is required"}
	}
	rec, err := s.ensureTarget(ctx, req)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	fwd := s.forwarder(ctx, targetKey(req))
	f, err := fwd.Ensure(req.RemotePort, rec.ListenIP, req.RemoteHost)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	return Response{OK: true, LocalPort: f.LocalPort, Domain: rec.Domain, ListenIP: rec.ListenIP}
}

func (s *Server) removeTargetPort(req Request) Response {
	if req.Target == "" || req.SSHPath == "" || req.RemotePort <= 0 {
		return Response{OK: false, Error: "sshPath, target and remotePort are required"}
	}
	key := targetKey(req)
	var fwd *forward.Manager
	s.mu.Lock()
	fwd = s.forwarders[key]
	s.mu.Unlock()
	if fwd != nil {
		fwd.Remove(req.RemotePort)
	}
	return Response{OK: true}
}

func (s *Server) ensureTarget(ctx context.Context, req Request) (*targetRecord, error) {
	if req.Target == "" || req.SSHPath == "" || req.DomainSuffix == "" || req.DNSAddr == "" {
		return nil, errors.New("sshPath, target, domainSuffix and dnsAddr are required")
	}
	key := targetKey(req)
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
	domainName, err := dom.RegisterTarget(req.Target, net.ParseIP(listenIP))
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	rec := &targetRecord{
		Target:        req.Target,
		Domain:        domainName,
		ListenIP:      listenIP,
		domainManager: dom,
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

func (s *Server) forwarder(ctx context.Context, key string) *forward.Manager {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forwarders == nil {
		s.forwarders = map[string]*forward.Manager{}
	}
	if f := s.forwarders[key]; f != nil {
		return f
	}
	f := forward.NewDynamicManager(ctx, func() (string, []string, string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, session := range s.sessions {
			if session.TargetKey == key {
				return session.SSHPath, append([]string(nil), session.SSHArgs...), session.ControlPath
			}
		}
		return "", nil, ""
	})
	s.forwarders[key] = f
	return f
}

func (s *Server) listPorts() Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Forwarded
	for key, fwd := range s.forwarders {
		rec := s.targets[key]
		for _, entry := range fwd.List() {
			item := Forwarded{
				ListenIP:   entry.ListenIP,
				LocalPort:  entry.LocalPort,
				RemotePort: entry.RemotePort,
			}
			if rec != nil {
				item.Target = rec.Target
				item.Domain = rec.Domain
			}
			out = append(out, item)
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
