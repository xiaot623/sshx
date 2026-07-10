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
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xiaot623/sshx/internal/ports"
	"github.com/xiaot623/sshx/internal/processlock"
	"github.com/xiaot623/sshx/internal/protocol"
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
	OnPortObserved    func(port int)
	OnPortGone        func(port int)
	AppVersion        string
	SessionID         string
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
}

const (
	portGoneMissingScans      = 2
	DefaultHeartbeatInterval  = 5 * time.Second
	DefaultHeartbeatTimeout   = 15 * time.Second
	DefaultServerDrainTimeout = 500 * time.Millisecond
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

	mu            sync.Mutex
	clients       []*clientConn
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
	enc       *protocol.Encoder
	dec       *protocol.Decoder
	c         net.Conn
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan protocol.Frame
	lastSeen  time.Time
	closeOnce sync.Once
	done      chan struct{}
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
	if hello.ProtocolVersion != protocol.Version {
		_ = enc.Encode(protocol.Frame{Type: protocol.TypeServerDrain, AppVersion: s.Version, ProtocolVersion: protocol.Version, Error: "sshx protocol version changed"})
		_ = c.Close()
		s.initiateShutdown()
		return
	}
	switch hello.Role {
	case protocol.RoleClient:
		if hello.AppVersion == "" || hello.AppVersion != s.Version {
			_ = enc.Encode(protocol.Frame{Type: protocol.TypeServerDrain, AppVersion: s.Version, ProtocolVersion: protocol.Version, Error: "sshx application version changed"})
			_ = c.Close()
			s.initiateShutdown()
			return
		}
		cc := &clientConn{enc: enc, dec: dec, c: c, pending: map[string]chan protocol.Frame{}, lastSeen: time.Now(), done: make(chan struct{})}
		if !s.addClient(cc) {
			_ = cc.send(protocol.Frame{Type: protocol.TypeServerDrain, ProtocolVersion: protocol.Version, AppVersion: s.Version, Error: "sshx server is draining"})
			cc.close()
			return
		}
		if err := cc.send(protocol.Frame{Type: protocol.TypeCapabilities, ProtocolVersion: protocol.Version, AppVersion: s.Version, Capabilities: []string{"command.exec.batch-stdin", "heartbeat.v1"}}); err != nil {
			s.removeClient(cc)
			return
		}
		s.sendCurrentPorts(cc)
		cc.readLoop(s)
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
		resp, err := client.request(req)
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
	s.clients = append(s.clients, c)
	s.everHadClient = true
	s.lastActive = time.Now()
	return true
}

func (s *Server) removeClient(c *clientConn) {
	s.mu.Lock()
	found := false
	for i, existing := range s.clients {
		if existing == c {
			s.clients = append(s.clients[:i], s.clients[i+1:]...)
			found = true
			break
		}
	}
	if found {
		s.lastActive = time.Now()
	}
	s.mu.Unlock()
	if found {
		c.close()
	}
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
	case <-time.After(30 * time.Second):
		return protocol.Frame{}, errors.New("timed out waiting for bridge command result")
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
			if frame.ProtocolVersion != protocol.Version || frame.AppVersion != s.Version {
				_ = c.send(protocol.Frame{Type: protocol.TypeServerDrain, ProtocolVersion: protocol.Version, AppVersion: s.Version, Error: "sshx version changed"})
				s.initiateShutdown()
				return
			}
			c.pendingMu.Lock()
			c.lastSeen = time.Now()
			c.pendingMu.Unlock()
			if err := c.send(protocol.Frame{Type: protocol.TypeHeartbeatAck, ProtocolVersion: protocol.Version, AppVersion: s.Version, SessionID: frame.SessionID, Sequence: frame.Sequence}); err != nil {
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
	if opts.AppVersion == "" {
		opts.AppVersion = version.Version
	}
	if opts.SessionID == "" {
		opts.SessionID = fmt.Sprintf("bridge-%d", time.Now().UnixNano())
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
	if err := send(protocol.Frame{Type: protocol.TypeHello, ProtocolVersion: protocol.Version, AppVersion: opts.AppVersion, SessionID: opts.SessionID, Role: protocol.RoleClient, Token: firstToken(token)}); err != nil {
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
				if err := send(protocol.Frame{Type: protocol.TypeHeartbeat, ProtocolVersion: protocol.Version, AppVersion: opts.AppVersion, SessionID: opts.SessionID, Sequence: sequence}); err != nil {
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
			if frame.ProtocolVersion != protocol.Version || frame.AppVersion != opts.AppVersion {
				err := errors.New("sshx bridge version changed")
				signalReady(opts.Ready, err)
				return err
			}
			readySignaled = true
			signalReady(opts.Ready, nil)
			continue
		}
		if frame.Type == protocol.TypeHeartbeatAck {
			if frame.ProtocolVersion != protocol.Version || frame.AppVersion != opts.AppVersion {
				return errors.New("sshx bridge heartbeat version changed")
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
			resp := ExecuteLocal(clientCtx, frame)
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
