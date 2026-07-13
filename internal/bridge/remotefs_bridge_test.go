package bridge

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiaot623/sshx/internal/protocol"
	"github.com/xiaot623/sshx/internal/remotefs"
)

type captureMountDriver struct {
	backend chan remotefs.Backend
	options chan remotefs.MountOptions
}

type blockingMountDriver struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (d *blockingMountDriver) Mount(ctx context.Context, path string, _ remotefs.Backend, _ remotefs.MountOptions) (remotefs.Mount, error) {
	d.calls.Add(1)
	select {
	case <-d.started:
	default:
		close(d.started)
	}
	select {
	case <-d.release:
		return &testMount{path: path, done: make(chan error)}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *captureMountDriver) Mount(_ context.Context, path string, backend remotefs.Backend, options remotefs.MountOptions) (remotefs.Mount, error) {
	d.backend <- backend
	if d.options != nil {
		d.options <- options
	}
	return &testMount{path: path, done: make(chan error)}, nil
}

type testMount struct {
	path string
	done chan error
}

func (m *testMount) Path() string       { return m.path }
func (m *testMount) Done() <-chan error { return m.done }
func (m *testMount) Unmount(context.Context) error {
	select {
	case <-m.done:
	default:
		close(m.done)
	}
	return nil
}

func startRemoteFSServer(t *testing.T, driver remotefs.MountDriver) (context.Context, context.CancelFunc, string, *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	socket := shortSocketPath(t)
	server := &Server{
		SocketPath:   socket,
		Token:        "secret",
		Version:      "test-version",
		MountDriver:  driver,
		DrainTimeout: time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(ctx)
	}()
	waitForSocket(t, socket)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("server: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("server did not stop")
		}
	})
	return ctx, cancel, socket, server
}

func connectRemoteFSPair(t *testing.T, ctx context.Context, socket string, execute func(context.Context, protocol.Frame) protocol.Frame) (*remotefs.Peer, <-chan error) {
	t.Helper()
	controlReady := make(chan error, 1)
	controlErr := make(chan error, 1)
	go func() {
		controlErr <- RunClientConnWithOptions(ctx, mustDialUnix(t, socket), ClientOptions{
			Ready:      controlReady,
			AppVersion: "test-version",
			SessionID:  "session-1",
			Allow:      func([]string) bool { return true },
			Execute:    execute,
		}, "secret")
	}()
	if err := <-controlReady; err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", socket+".fs")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := remotefs.Connect(ctx, conn, "session-1", "secret", remotefs.PeerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	return peer, controlErr
}

func TestRemoteFSSessionMountsClientExportOnServer(t *testing.T) {
	driver := &captureMountDriver{backend: make(chan remotefs.Backend, 1), options: make(chan remotefs.MountOptions, 1)}
	ctx, _, socket, _ := startRemoteFSServer(t, driver)
	clientPeer, _ := connectRemoteFSPair(t, ctx, socket, nil)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "local.txt"), []byte("from-local"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, err := remotefs.OpenRootBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientPeer.RegisterBackend("workspace", backend); err != nil {
		t.Fatal(err)
	}
	path, err := clientPeer.CreateMountAtWithOptions(ctx, "workspace", "Users/xiaot", remotefs.MountOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(filepath.ToSlash(path), "/session-1/Users/xiaot") {
		t.Fatalf("mount path = %q", path)
	}
	if options := <-driver.options; !options.ReadOnly {
		t.Fatal("server mount was not read-only")
	}
	var mountedBackend remotefs.Backend
	select {
	case mountedBackend = <-driver.backend:
	case <-time.After(time.Second):
		t.Fatal("mount driver was not called")
	}
	handle, _, err := mountedBackend.Open(ctx, "local.txt", remotefs.OpenRead, 0)
	if err != nil {
		t.Fatal(err)
	}
	data, err := mountedBackend.Read(ctx, handle, 0, 32)
	if err != nil {
		t.Fatal(err)
	}
	_ = mountedBackend.Close(ctx, handle)
	if string(data) != "from-local" {
		t.Fatalf("mounted data = %q", data)
	}
	if err := clientPeer.ReleaseMount(ctx, "workspace"); err != nil {
		t.Fatal(err)
	}
}

func TestRequesterRejectsCwdUnderMountRoot(t *testing.T) {
	ctx, _, socket, server := startRemoteFSServer(t, &captureMountDriver{backend: make(chan remotefs.Backend, 1)})
	deadline := time.Now().Add(time.Second)
	for server.MountRoot == "" {
		if time.Now().After(deadline) {
			t.Fatal("MountRoot was not initialized")
		}
		time.Sleep(time.Millisecond)
	}
	mounted := filepath.Join(server.MountRoot, "session-1", "workspace")
	if err := os.MkdirAll(mounted, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := RequestCommandForSessionWithTimeout(
		ctx,
		socket,
		[]string{"true"},
		nil,
		nil,
		mounted,
		"session-1",
		true,
		time.Second,
		"secret",
	)
	if err == nil {
		t.Fatal("expected mounted cwd to be rejected")
	}
	if !strings.Contains(err.Error(), ErrMountedCwd.Error()) {
		t.Fatalf("error = %v", err)
	}
}

func TestRequesterExportsRemoteCwdToExactClientSession(t *testing.T) {
	ctx, _, socket, server := startRemoteFSServer(t, &captureMountDriver{backend: make(chan remotefs.Backend, 1)})
	var peerMu sync.RWMutex
	var clientPeer *remotefs.Peer
	execute := func(commandCtx context.Context, frame protocol.Frame) protocol.Frame {
		if !frame.RemoteFS || !frame.MountReadOnly || frame.MountID == "" || frame.SessionID != "session-1" {
			return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: "missing remote fs identity"}
		}
		peerMu.RLock()
		peer := clientPeer
		peerMu.RUnlock()
		if peer == nil {
			return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: "remote fs peer unavailable"}
		}
		backend := peer.RemoteBackend(frame.MountID)
		handle, _, err := backend.Open(commandCtx, "remote.txt", remotefs.OpenRead, 0)
		if err != nil {
			return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: err.Error()}
		}
		data, err := backend.Read(commandCtx, handle, 0, 32)
		_ = backend.Close(commandCtx, handle)
		if err != nil {
			return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: err.Error()}
		}
		return protocol.Frame{Type: protocol.TypeCommandResult, ID: frame.ID, Stdout: base64.StdEncoding.EncodeToString(data)}
	}
	peer, _ := connectRemoteFSPair(t, ctx, socket, execute)
	peerMu.Lock()
	clientPeer = peer
	peerMu.Unlock()
	deadline := time.Now().Add(time.Second)
	for {
		server.mu.Lock()
		registered := server.fsPeers["session-1"] != nil
		server.mu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("remote fs peer was not registered")
		}
		time.Sleep(time.Millisecond)
	}
	remoteRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(remoteRoot, "remote.txt"), []byte("from-remote"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := RequestCommandForSessionWithMountOptions(
		ctx,
		socket,
		[]string{"read-remote"},
		nil,
		nil,
		remoteRoot,
		"session-1",
		true,
		true,
		time.Second,
		"secret",
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "from-remote" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
}

func TestRequesterRoutesToExactSessionWithMultipleClients(t *testing.T) {
	ctx, _, socket, _ := startRemoteFSServer(t, &captureMountDriver{backend: make(chan remotefs.Backend, 1)})
	startClient := func(sessionID string) {
		ready := make(chan error, 1)
		go func() {
			_ = RunClientConnWithOptions(ctx, mustDialUnix(t, socket), ClientOptions{
				Ready:      ready,
				AppVersion: "test-version",
				SessionID:  sessionID,
				Allow:      func([]string) bool { return true },
				Execute: func(_ context.Context, frame protocol.Frame) protocol.Frame {
					return protocol.Frame{
						Type:   protocol.TypeCommandResult,
						ID:     frame.ID,
						Stdout: base64.StdEncoding.EncodeToString([]byte(sessionID)),
					}
				},
			}, "secret")
		}()
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
	}
	startClient("session-1")
	startClient("session-2")

	result, err := RequestCommandForSessionWithTimeout(
		ctx, socket, []string{"which-session"}, nil, nil, "", "session-2", false, time.Second, "secret",
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "session-2" {
		t.Fatalf("request routed to %q", result.Stdout)
	}
}

func TestRequesterRejectsMismatchedSessionIdentity(t *testing.T) {
	ctx, _, socket, _ := startRemoteFSServer(t, &captureMountDriver{backend: make(chan remotefs.Backend, 1)})
	conn := mustDialUnix(t, socket)
	defer conn.Close()
	encoder := protocol.NewEncoder(conn)
	decoder := protocol.NewDecoder(conn)
	if err := encoder.Encode(protocol.Frame{
		Type:            protocol.TypeHello,
		Role:            protocol.RoleRequester,
		ProtocolVersion: protocol.Version,
		Token:           "secret",
		SessionID:       "session-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(protocol.Frame{
		Type:      protocol.TypeCommandExec,
		ID:        "req-1",
		Argv:      []string{"true"},
		SessionID: "session-2",
	}); err != nil {
		t.Fatal(err)
	}
	response, err := decoder.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if response.Type != protocol.TypeCommandError || response.Error == "" {
		t.Fatalf("response = %#v", response)
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	default:
	}
}

func TestRemoteFSRejectsConcurrentMountsForOneSession(t *testing.T) {
	driver := &blockingMountDriver{started: make(chan struct{}), release: make(chan struct{})}
	ctx, _, socket, _ := startRemoteFSServer(t, driver)
	clientPeer, _ := connectRemoteFSPair(t, ctx, socket, nil)
	firstResult := make(chan error, 1)
	go func() {
		_, err := clientPeer.CreateMount(ctx, "first")
		firstResult <- err
	}()
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("first mount did not start")
	}
	if _, err := clientPeer.CreateMount(ctx, "second"); err == nil {
		t.Fatal("concurrent mount unexpectedly succeeded")
	}
	close(driver.release)
	if err := <-firstResult; err != nil {
		t.Fatal(err)
	}
	if calls := driver.calls.Load(); calls != 1 {
		t.Fatalf("mount driver calls = %d", calls)
	}
	if err := clientPeer.ReleaseMount(ctx, "first"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteFSCancelsInFlightMount(t *testing.T) {
	driver := &blockingMountDriver{started: make(chan struct{}), release: make(chan struct{})}
	ctx, _, socket, server := startRemoteFSServer(t, driver)
	clientPeer, _ := connectRemoteFSPair(t, ctx, socket, nil)
	mountCtx, mountCancel := context.WithCancel(ctx)
	defer mountCancel()
	result := make(chan error, 1)
	go func() {
		_, err := clientPeer.CreateMount(mountCtx, "workspace")
		result <- err
	}()
	select {
	case <-driver.started:
	case <-time.After(time.Second):
		t.Fatal("mount did not start")
	}
	mountCancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CreateMount error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CreateMount did not return after cancel")
	}
	deadline := time.Now().Add(time.Second)
	for {
		server.mu.Lock()
		mounting := server.fsMounting["session-1"]
		server.mu.Unlock()
		if !mounting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fsMounting still set after canceled mount")
		}
		time.Sleep(time.Millisecond)
	}
	close(driver.release)
	if _, err := clientPeer.CreateMount(ctx, "workspace"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteFSRejectsDuplicateDataSession(t *testing.T) {
	ctx, _, socket, _ := startRemoteFSServer(t, &captureMountDriver{backend: make(chan remotefs.Backend, 1)})
	first, _ := connectRemoteFSPair(t, ctx, socket, nil)
	if first == nil {
		t.Fatal("first peer is nil")
	}
	conn, err := net.Dial("unix", socket+".fs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remotefs.Connect(ctx, conn, "session-1", "secret", remotefs.PeerOptions{}); err == nil {
		t.Fatal("duplicate data session unexpectedly connected")
	}
}

func TestRemoteFSCleansStaleManagedDirectories(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(root, "session-1")
	mountPath := filepath.Join(sessionPath, "Users", "xiaot")
	if err := os.MkdirAll(mountPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionPath, ".mount-path"), []byte("Users/xiaot\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountPath, "stale"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &Server{MountRoot: root}
	server.cleanupStaleMounts()
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Fatalf("stale session still exists: %v", err)
	}
}
