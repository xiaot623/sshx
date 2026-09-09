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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xiaot623/sshx/internal/identity"
	"github.com/xiaot623/sshx/internal/protocol"
	"github.com/xiaot623/sshx/internal/version"
)

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
	OnPortObserved    func(host string, port int)
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
	return RunClientConnWithOptions(ctx, c, ClientOptions{})
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
		opts.Capabilities = defaultCapabilities
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
	execute := opts.Execute
	if execute == nil {
		execute = ExecuteLocal
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
				opts.OnPortObserved(frame.Host, frame.Port)
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
			if err := send(protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: "command denied by sshx policy"}); err != nil {
				return err
			}
			continue
		}
		go func(frame protocol.Frame) {
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
