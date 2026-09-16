package locald

import (
	"context"
	"encoding/json"
	"net"
	"time"

	"github.com/xiaot623/sshx/internal/domain"
	"github.com/xiaot623/sshx/internal/forward"
	"github.com/xiaot623/sshx/internal/protocol"
)

func (s *Server) handleSession(ctx context.Context, conn net.Conn, dec *json.Decoder, enc *json.Encoder, req Request) {
	leaseID := requestLeaseID(req)
	if leaseID == "" {
		_ = enc.Encode(Response{
			OK:              false,
			Error:           "leaseId is required",
			Version:         s.Version,
			ProtocolVersion: protocol.Version})
		return
	}
	if !requestCompatible(req) {
		resp := protocolResponse(s.Version, false)
		resp.Error = "local daemon runtime protocol is incompatible"
		_ = enc.Encode(resp)
		return
	}
	s.mu.Lock()
	draining := s.draining
	s.mu.Unlock()
	if draining {
		_ = enc.Encode(Response{
			OK:              false,
			Error:           "local daemon is draining",
			Version:         s.Version,
			ProtocolVersion: protocol.Version})
		return
	}
	rec, err := s.ensureTarget(ctx, req)
	if err != nil {
		_ = enc.Encode(Response{OK: false, Error: err.Error(), Version: s.Version, ProtocolVersion: protocol.Version})
		return
	}
	key := targetKey(req)
	session := &sessionRecord{
		TargetKey:   key,
		SSHPath:     req.SSHPath,
		SSHArgs:     append([]string(nil), defaultSSHArgs(req)...),
		ControlPath: req.ControlPath,
		conn:        conn}
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		_ = enc.Encode(Response{
			OK:              false,
			Error:           "local daemon is draining",
			Version:         s.Version,
			ProtocolVersion: protocol.Version})
		return
	}
	if _, exists := s.sessions[leaseID]; exists {
		s.mu.Unlock()
		_ = enc.Encode(Response{
			OK:              false,
			Error:           "session already exists",
			Version:         s.Version,
			ProtocolVersion: protocol.Version})
		return
	}
	s.sessions[leaseID] = session
	rec.Sessions++
	s.mu.Unlock()
	defer s.releaseSession(leaseID)

	opened := protocolResponse(s.Version, true)
	opened.Type, opened.Domain, opened.ListenIP = TypeOpenSession, rec.Domain, rec.ListenIP
	if err := enc.Encode(opened); err != nil {
		return
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.LeaseTimeout))
		var heartbeat Request
		if err := dec.Decode(&heartbeat); err != nil {
			return
		}
		if heartbeat.Type != TypeHeartbeat || requestLeaseID(heartbeat) != leaseID {
			_ = enc.Encode(Response{
				OK:              false,
				Error:           "invalid session heartbeat",
				Version:         s.Version,
				ProtocolVersion: protocol.Version})
			return
		}
		if !requestCompatible(heartbeat) {
			resp := protocolResponse(s.Version, false)
			resp.Error, resp.Sequence = "local daemon runtime protocol is incompatible", heartbeat.Sequence
			_ = enc.Encode(resp)
			return
		}
		resp := protocolResponse(s.Version, true)
		resp.Type, resp.Sequence = TypeHeartbeatAck, heartbeat.Sequence
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}

func (s *Server) releaseSession(sessionID string) {
	if s.HandoffGrace > 0 {
		s.releaseSessionWithGrace(sessionID)
		return
	}
	s.releaseSessionNow(sessionID)
}

func (s *Server) releaseSessionWithGrace(sessionID string) {
	s.mu.Lock()
	session := s.sessions[sessionID]
	if session == nil {
		s.mu.Unlock()
		return
	}
	// Handoff grace preserves the target's domain and forwarding listeners,
	// but a disconnected session's SSH transport is no longer active.
	delete(s.sessions, sessionID)
	if rec := s.targets[session.TargetKey]; rec != nil && rec.Sessions > 0 {
		rec.Sessions--
	}
	targetKey := session.TargetKey
	s.mu.Unlock()
	go func() {
		timer := time.NewTimer(s.HandoffGrace)
		defer timer.Stop()
		<-timer.C
		s.cleanupIdleTarget(targetKey)
	}()
}

func (s *Server) cleanupIdleTarget(key string) {
	var fwd *forward.Manager
	var domainName string
	var domainManager *domain.Manager
	var shutdown bool
	s.mu.Lock()
	if rec := s.targets[key]; rec != nil && rec.Sessions == 0 {
		fwd = s.forwarders[key]
		delete(s.forwarders, key)
		domainName, domainManager = rec.Domain, rec.domainManager
		delete(s.targets, key)
	}
	shutdown = len(s.sessions) == 0 && len(s.targets) == 0
	if shutdown {
		s.draining = true
	}
	s.mu.Unlock()
	if fwd != nil {
		fwd.Stop()
	}
	if domainManager != nil && domainName != "" {
		domainManager.Unregister(domainName)
	}
	if shutdown {
		s.initiateShutdown()
	}
}

func (s *Server) releaseSessionNow(sessionID string) {
	var fwd *forward.Manager
	var domainName string
	var domainManager *domain.Manager
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
				domainName = rec.Domain
				domainManager = rec.domainManager
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
	if domainManager != nil && domainName != "" {
		domainManager.Unregister(domainName)
	}
	if shutdown {
		s.initiateShutdown()
	}
}
