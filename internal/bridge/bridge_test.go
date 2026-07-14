package bridge

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaot623/sshx/internal/processlock"
	"github.com/xiaot623/sshx/internal/protocol"
)

func TestServerDoesNotCleanMountsBeforeAcquiringSocketLock(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "bridge.sock")
	mountRoot := filepath.Join(root, "mounts")
	sentinel := filepath.Join(mountRoot, "session-1", "workspace", "sentinel")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("active"), 0o600); err != nil {
		t.Fatal(err)
	}

	lock, err := processlock.Acquire(socket + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	err = (&Server{SocketPath: socket, MountRoot: mountRoot}).Serve(context.Background())
	if err == nil {
		t.Fatal("duplicate server unexpectedly acquired the socket lock")
	}
	if _, statErr := os.Stat(sentinel); statErr != nil {
		t.Fatalf("duplicate server changed an existing mount session: %v", statErr)
	}
}

func TestExecuteLocalPropagatesBatchStdinOutputStderrAndExitCode(t *testing.T) {
	frame := protocol.Frame{
		Type:  protocol.TypeCommandExec,
		ID:    "req-1",
		Argv:  []string{"sh", "-c", "cat; printf err >&2; exit 7"},
		Stdin: base64.StdEncoding.EncodeToString([]byte("input")),
	}
	resp := ExecuteLocal(context.Background(), frame)
	if resp.Type != protocol.TypeCommandResult {
		t.Fatalf("response type = %q error=%q", resp.Type, resp.Error)
	}
	if resp.ExitCode != 7 {
		t.Fatalf("exit code = %d", resp.ExitCode)
	}
	stdout, _ := base64.StdEncoding.DecodeString(resp.Stdout)
	stderr, _ := base64.StdEncoding.DecodeString(resp.Stderr)
	if string(stdout) != "input" || string(stderr) != "err" {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestExecuteLocalHonorsExplicitTimeout(t *testing.T) {
	started := time.Now()
	resp := ExecuteLocal(context.Background(), protocol.Frame{
		Type:          protocol.TypeCommandExec,
		ID:            "req-timeout",
		Argv:          []string{"sh", "-c", "sleep 5"},
		TimeoutMillis: 20,
	})
	if resp.Type != protocol.TypeCommandError || !strings.Contains(resp.Error, "timed out after 20ms") {
		t.Fatalf("response = %#v", resp)
	}
	if resp.ExitCode != 124 {
		t.Fatalf("exit code = %d", resp.ExitCode)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestServerReturnsClearErrorWithoutClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket}).Serve(ctx)
	}()
	waitForSocket(t, socket)

	_, err := RequestCommand(context.Background(), socket, []string{"uname"}, nil, nil, "")
	if !errors.Is(err, ErrNoClient) && (err == nil || !strings.Contains(err.Error(), ErrNoClient.Error())) {
		t.Fatalf("error = %v, want ErrNoClient", err)
	}
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
}

func TestServerForwardsCommandToClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket}).Serve(ctx)
	}()
	waitForSocket(t, socket)

	clientErr := make(chan error, 1)
	go func() {
		clientErr <- RunClient(ctx, socket)
	}()

	var result CommandResult
	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		result, err = RequestCommand(context.Background(), socket, []string{"sh", "-c", "cat; printf err >&2; exit 7"}, []byte("input"), nil, "")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("RequestCommand: %v", err)
	}
	if result.ExitCode != 7 || string(result.Stdout) != "input" || string(result.Stderr) != "err" {
		t.Fatalf("result = %#v", result)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
	select {
	case <-clientErr:
	case <-time.After(time.Second):
		t.Fatal("client did not stop")
	}
}

func TestClientDeniesCommandByPolicy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket}).Serve(ctx)
	}()
	waitForSocket(t, socket)

	clientErr := make(chan error, 1)
	go func() {
		clientErr <- RunClientConnReadyPolicy(ctx, mustDialUnix(t, socket), nil, func(argv []string) bool {
			return false
		})
	}()

	var err error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, err = RequestCommand(context.Background(), socket, []string{"sh", "-c", "echo nope"}, nil, nil, "")
		if err != nil && strings.Contains(err.Error(), "command denied") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err == nil || !strings.Contains(err.Error(), "command denied") {
		t.Fatalf("error = %v, want policy denial", err)
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
	select {
	case <-clientErr:
	case <-time.After(time.Second):
		t.Fatal("client did not stop")
	}
}

func TestServerExitsAfterIdleTimeout(t *testing.T) {
	ctx := context.Background()
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket, StartupTimeout: 20 * time.Millisecond}).Serve(ctx)
	}()
	waitForSocket(t, socket)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not exit after idle timeout")
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("socket still exists or unexpected stat error: %v", err)
	}
}

func TestPortScanDiffDebouncesGonePorts(t *testing.T) {
	s := &Server{}
	observed, gone := s.applyPortScan([]int{8080})
	if len(observed) != 1 || observed[0] != 8080 || len(gone) != 0 {
		t.Fatalf("first scan observed=%v gone=%v", observed, gone)
	}
	observed, gone = s.applyPortScan(nil)
	if len(observed) != 0 || len(gone) != 0 {
		t.Fatalf("first miss observed=%v gone=%v", observed, gone)
	}
	observed, gone = s.applyPortScan(nil)
	if len(observed) != 0 || len(gone) != 1 || gone[0] != 8080 {
		t.Fatalf("second miss observed=%v gone=%v", observed, gone)
	}
	observed, gone = s.applyPortScan([]int{8080})
	if len(observed) != 1 || observed[0] != 8080 || len(gone) != 0 {
		t.Fatalf("restart observed=%v gone=%v", observed, gone)
	}
}

func TestNewClientReceivesCurrentPortSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverConn, clientConn := net.Pipe()
	s := &Server{Version: "test-version", observedPorts: map[int]bool{8080: true}}
	go s.handleConn(serverConn)

	ready := make(chan error, 1)
	observed := make(chan int, 1)
	clientErr := make(chan error, 1)
	go func() {
		clientErr <- RunClientConnWithOptions(ctx, clientConn, ClientOptions{
			Ready:      ready,
			AppVersion: "test-version",
			OnPortObserved: func(port int) {
				observed <- port
			},
		})
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("client did not become ready")
	}
	select {
	case port := <-observed:
		if port != 8080 {
			t.Fatalf("observed port = %d", port)
		}
	case <-time.After(time.Second):
		t.Fatal("client did not receive current port snapshot")
	}
	cancel()
	select {
	case <-clientErr:
	case <-time.After(time.Second):
		t.Fatal("client did not stop")
	}
}

func TestServerExitsAfterLastBridgeClientDisconnects(t *testing.T) {
	serverCtx := context.Background()
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket, Version: "test-version", DrainTimeout: 20 * time.Millisecond}).Serve(serverCtx)
	}()
	waitForSocket(t, socket)

	clientCtx, cancelClient := context.WithCancel(context.Background())
	ready := make(chan error, 1)
	go func() {
		_ = RunClientConnWithOptions(clientCtx, mustDialUnix(t, socket), ClientOptions{
			Ready:             ready,
			AppVersion:        "test-version",
			HeartbeatInterval: 5 * time.Millisecond,
			HeartbeatTimeout:  50 * time.Millisecond,
		})
	}()
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	cancelClient()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not exit after its last bridge client disconnected")
	}
}

func TestServerExpiresClientWithoutHeartbeat(t *testing.T) {
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{
			SocketPath:       socket,
			Version:          "test-version",
			HeartbeatTimeout: 30 * time.Millisecond,
			DrainTimeout:     10 * time.Millisecond,
		}).Serve(context.Background())
	}()
	waitForSocket(t, socket)
	conn := mustDialUnix(t, socket)
	defer conn.Close()
	enc := protocol.NewEncoder(conn)
	dec := protocol.NewDecoder(conn)
	if err := enc.Encode(protocol.Frame{Type: protocol.TypeHello, Role: protocol.RoleClient, ProtocolVersion: protocol.Version, AppVersion: "test-version", SessionID: "lease-test"}); err != nil {
		t.Fatal(err)
	}
	if frame, err := dec.Decode(); err != nil || frame.Type != protocol.TypeCapabilities {
		t.Fatalf("capabilities = %#v, %v", frame, err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not expire a bridge client without heartbeat")
	}
}

func TestServerExitsOnVersionChange(t *testing.T) {
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket, Version: "1.0.0"}).Serve(context.Background())
	}()
	waitForSocket(t, socket)
	conn := mustDialUnix(t, socket)
	enc := protocol.NewEncoder(conn)
	dec := protocol.NewDecoder(conn)
	if err := enc.Encode(protocol.Frame{Type: protocol.TypeHello, Role: protocol.RoleClient, ProtocolVersion: protocol.Version, AppVersion: "2.0.0", SessionID: "version-test"}); err != nil {
		t.Fatal(err)
	}
	frame, err := dec.Decode()
	if err != nil || frame.Type != protocol.TypeServerDrain {
		t.Fatalf("drain frame = %#v, %v", frame, err)
	}
	_ = conn.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not exit after version change")
	}
}

func TestUnauthenticatedVersionChangeDoesNotStopServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket, Token: "secret", Version: "1.0.0"}).Serve(ctx)
	}()
	waitForSocket(t, socket)
	conn := mustDialUnix(t, socket)
	enc := protocol.NewEncoder(conn)
	dec := protocol.NewDecoder(conn)
	if err := enc.Encode(protocol.Frame{Type: protocol.TypeHello, Role: protocol.RoleClient, Token: "wrong", ProtocolVersion: protocol.Version + 1, AppVersion: "2.0.0"}); err != nil {
		t.Fatal(err)
	}
	frame, err := dec.Decode()
	if err != nil || frame.Type != protocol.TypeError {
		t.Fatalf("error frame = %#v, %v", frame, err)
	}
	_ = conn.Close()
	select {
	case err := <-errCh:
		t.Fatalf("server exited after unauthenticated version change: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatContinuesDuringCommandExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket, Version: "test-version", HeartbeatTimeout: 40 * time.Millisecond}).Serve(ctx)
	}()
	waitForSocket(t, socket)
	ready := make(chan error, 1)
	go func() {
		_ = RunClientConnWithOptions(ctx, mustDialUnix(t, socket), ClientOptions{
			Ready:             ready,
			AppVersion:        "test-version",
			HeartbeatInterval: 5 * time.Millisecond,
			HeartbeatTimeout:  30 * time.Millisecond,
		})
	}()
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	result, err := RequestCommand(ctx, socket, []string{"sh", "-c", "sleep 0.1; printf ok"}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "ok" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	cancel()
	<-errCh
}

func TestCommandTimeoutDoesNotRemoveHealthyBridgeClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket}).Serve(ctx)
	}()
	waitForSocket(t, socket)

	ready := make(chan error, 1)
	go func() {
		_ = RunClientConnWithOptions(ctx, mustDialUnix(t, socket), ClientOptions{Ready: ready})
	}()
	if err := <-ready; err != nil {
		t.Fatal(err)
	}

	timedOut, err := RequestCommandWithTimeout(ctx, socket, []string{"sh", "-c", "sleep 5"}, nil, nil, "", 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out after 20ms") {
		t.Fatalf("timeout error = %v", err)
	}
	if timedOut.ExitCode != 124 {
		t.Fatalf("exit code = %d", timedOut.ExitCode)
	}

	result, err := RequestCommand(ctx, socket, []string{"sh", "-c", "printf still-connected"}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "still-connected" {
		t.Fatalf("stdout = %q", result.Stdout)
	}

	cancel()
	<-errCh
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(path); err == nil && st.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s was not created", path)
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sshx-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func mustDialUnix(t *testing.T, socket string) io.ReadWriteCloser {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}
