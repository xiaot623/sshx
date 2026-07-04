package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/xiaot/sshx/internal/ports"
	"github.com/xiaot/sshx/internal/protocol"
)

var ErrNoClient = errors.New("no active sshx client bridge session")

type CommandResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

type CommandAllowed func([]string) bool

type ClientOptions struct {
	Ready          chan<- error
	Allow          CommandAllowed
	OnPortObserved func(port int)
}

type Server struct {
	SocketPath       string
	InfoPath         string
	Token            string
	PortScanInterval time.Duration
	IdleTimeout      time.Duration

	mu            sync.Mutex
	clients       []*clientConn
	observedPorts map[int]bool
	lastActive    time.Time
}

type clientConn struct {
	enc *protocol.Encoder
	dec *protocol.Decoder
	c   net.Conn
	mu  sync.Mutex
}

func (s *Server) Serve(ctx context.Context) error {
	if s.SocketPath == "" {
		return errors.New("socket path is required")
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
	s.markActive()
	if err := os.Chmod(s.SocketPath, 0o600); err != nil {
		return err
	}
	if s.InfoPath != "" {
		if err := WriteServerInfo(s.InfoPath, s.SocketPath, s.Token); err != nil {
			return err
		}
	}
	if s.PortScanInterval > 0 {
		go s.observePorts(ctx)
	}
	if s.IdleTimeout > 0 {
		go s.monitorIdle(ctx, ln)
	}

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handleConn(c)
	}
}

func (s *Server) monitorIdle(ctx context.Context, ln net.Listener) {
	interval := time.Second
	if s.IdleTimeout > 0 && s.IdleTimeout/2 < interval {
		interval = s.IdleTimeout / 2
		if interval <= 0 {
			interval = time.Millisecond
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.isIdleExpired() {
				_ = ln.Close()
				return
			}
		}
	}
}

func (s *Server) isIdleExpired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clients) > 0 {
		return false
	}
	return !s.lastActive.IsZero() && time.Since(s.lastActive) >= s.IdleTimeout
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
	for _, port := range portList {
		if s.markPortObserved(port) {
			s.broadcast(protocol.Frame{Type: protocol.TypePortObserved, Host: "localhost", Port: port})
		}
	}
}

func (s *Server) markPortObserved(port int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.observedPorts == nil {
		s.observedPorts = map[int]bool{}
	}
	if s.observedPorts[port] {
		return false
	}
	s.observedPorts[port] = true
	return true
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
	if hello.Type != protocol.TypeHello || hello.ProtocolVersion != protocol.Version {
		_ = enc.Encode(protocol.Frame{Type: protocol.TypeError, Error: "unsupported sshx protocol handshake"})
		_ = c.Close()
		return
	}
	if s.Token != "" && hello.Token != s.Token {
		_ = enc.Encode(protocol.Frame{Type: protocol.TypeError, Error: "invalid sshx server token"})
		_ = c.Close()
		return
	}
	switch hello.Role {
	case protocol.RoleClient:
		cc := &clientConn{enc: enc, dec: dec, c: c}
		s.addClient(cc)
		_ = enc.Encode(protocol.Frame{Type: protocol.TypeCapabilities, Capabilities: []string{"command.exec.batch-stdin"}})
	case protocol.RoleRequester:
		s.handleRequester(c, dec, enc)
	default:
		_ = enc.Encode(protocol.Frame{Type: protocol.TypeError, Error: "unknown bridge role"})
		_ = c.Close()
	}
}

func (s *Server) handleRequester(c net.Conn, dec *protocol.Decoder, enc *protocol.Encoder) {
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
	var lastErr error
	for {
		client := s.pickClient()
		if client == nil {
			msg := ErrNoClient.Error()
			if lastErr != nil {
				msg = lastErr.Error()
			}
			_ = enc.Encode(protocol.Frame{Type: protocol.TypeCommandError, ID: req.ID, Error: msg})
			return
		}
		resp, err := client.roundTrip(req)
		if err != nil {
			lastErr = err
			s.removeClient(client)
			continue
		}
		_ = enc.Encode(resp)
		return
	}
}

func (s *Server) addClient(c *clientConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients = append(s.clients, c)
	s.lastActive = time.Now()
}

func (s *Server) removeClient(c *clientConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, existing := range s.clients {
		if existing == c {
			s.clients = append(s.clients[:i], s.clients[i+1:]...)
			break
		}
	}
	_ = c.c.Close()
	s.lastActive = time.Now()
}

func (s *Server) markActive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActive = time.Now()
}

func (s *Server) pickClient() *clientConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.clients) == 0 {
		return nil
	}
	return s.clients[0]
}

func (c *clientConn) roundTrip(req protocol.Frame) (protocol.Frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enc.Encode(req); err != nil {
		return protocol.Frame{}, err
	}
	resp, err := c.dec.Decode()
	if err != nil {
		return protocol.Frame{}, err
	}
	if resp.ID == "" {
		resp.ID = req.ID
	}
	return resp, nil
}

func (c *clientConn) send(frame protocol.Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc.Encode(frame)
}

func RequestCommand(ctx context.Context, socketPath string, argv []string, stdin []byte, env map[string]string, cwd string, token ...string) (CommandResult, error) {
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
	if err := enc.Encode(protocol.Frame{Type: protocol.TypeHello, ProtocolVersion: protocol.Version, Role: protocol.RoleRequester, Token: firstToken(token)}); err != nil {
		return CommandResult{ExitCode: 1}, err
	}
	id := fmt.Sprintf("req-%d", time.Now().UnixNano())
	if err := enc.Encode(protocol.Frame{
		Type:  protocol.TypeCommandExec,
		ID:    id,
		Argv:  argv,
		Env:   env,
		Cwd:   cwd,
		Stdin: base64.StdEncoding.EncodeToString(stdin),
	}); err != nil {
		return CommandResult{ExitCode: 1}, err
	}
	resp, err := dec.Decode()
	if err != nil {
		return CommandResult{ExitCode: 1}, err
	}
	if resp.Type == protocol.TypeCommandError || resp.Type == protocol.TypeError {
		return CommandResult{ExitCode: 1}, errors.New(resp.Error)
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
	go func() {
		<-ctx.Done()
		_ = c.Close()
	}()
	enc := protocol.NewEncoder(c)
	dec := protocol.NewDecoder(c)
	if err := enc.Encode(protocol.Frame{Type: protocol.TypeHello, ProtocolVersion: protocol.Version, Role: protocol.RoleClient, Token: firstToken(token)}); err != nil {
		signalReady(opts.Ready, err)
		return err
	}
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
			readySignaled = true
			signalReady(opts.Ready, nil)
			continue
		}
		if frame.Type == protocol.TypePortObserved {
			if opts.OnPortObserved != nil && frame.Port > 0 {
				opts.OnPortObserved(frame.Port)
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
		resp := ExecuteLocal(ctx, frame)
		if err := enc.Encode(resp); err != nil {
			return err
		}
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
	cmd := exec.CommandContext(ctx, frame.Argv[0], frame.Argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	if frame.Cwd != "" {
		cmd.Dir = frame.Cwd
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
