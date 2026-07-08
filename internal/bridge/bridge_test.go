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

	"github.com/xiaot623/sshx/internal/protocol"
)

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
		errCh <- (&Server{SocketPath: socket, IdleTimeout: 20 * time.Millisecond}).Serve(ctx)
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
	s := &Server{observedPorts: map[int]bool{8080: true}}
	go s.handleConn(serverConn)

	ready := make(chan error, 1)
	observed := make(chan int, 1)
	clientErr := make(chan error, 1)
	go func() {
		clientErr <- RunClientConnWithOptions(ctx, clientConn, ClientOptions{
			Ready: ready,
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
