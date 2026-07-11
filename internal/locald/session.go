package locald

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/xiaot623/sshx/internal/protocol"
)

type Session struct {
	conn      net.Conn
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once

	Domain   string
	ListenIP string
}

func OpenSession(ctx context.Context, socketPath string, req Request, heartbeatInterval time.Duration) (*Session, error) {
	if req.SessionID == "" || req.AppVersion == "" {
		return nil, errors.New("sessionId and appVersion are required")
	}
	if heartbeatInterval <= 0 {
		heartbeatInterval = DefaultHeartbeatInterval
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	req.Type = TypeOpenSession
	req.ProtocolVersion = protocol.Version
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(bufio.NewReader(conn))
	if err := enc.Encode(req); err != nil {
		_ = conn.Close()
		return nil, err
	}
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if !resp.OK {
		_ = conn.Close()
		return nil, errors.New(resp.Error)
	}
	if resp.Version != req.AppVersion || resp.ProtocolVersion != protocol.Version {
		_ = conn.Close()
		return nil, errors.New("local daemon version changed during session open")
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &Session{conn: conn, cancel: cancel, done: make(chan struct{}), Domain: resp.Domain, ListenIP: resp.ListenIP}
	go s.heartbeatLoop(sessionCtx, enc, dec, req, heartbeatInterval)
	return s, nil
}

func (s *Session) heartbeatLoop(ctx context.Context, enc *json.Encoder, dec *json.Decoder, req Request, interval time.Duration) {
	defer close(s.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var sequence uint64
	for {
		select {
		case <-ctx.Done():
			_ = s.conn.Close()
			return
		case <-ticker.C:
			sequence++
			heartbeat := Request{Type: TypeHeartbeat, SessionID: req.SessionID, AppVersion: req.AppVersion, ProtocolVersion: protocol.Version, Sequence: sequence}
			if err := enc.Encode(heartbeat); err != nil {
				_ = s.conn.Close()
				return
			}
			_ = s.conn.SetReadDeadline(time.Now().Add(DefaultLeaseTimeout))
			var resp Response
			if err := dec.Decode(&resp); err != nil || !resp.OK || resp.Type != TypeHeartbeatAck || resp.Sequence != sequence || resp.Version != req.AppVersion || resp.ProtocolVersion != protocol.Version {
				_ = s.conn.Close()
				return
			}
			_ = s.conn.SetReadDeadline(time.Time{})
		}
	}
}

func (s *Session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.cancel()
		err = s.conn.Close()
		<-s.done
	})
	return err
}

func (s *Session) Done() <-chan struct{} {
	return s.done
}
