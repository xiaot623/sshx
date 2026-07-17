package bridge

import (
	"context"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xiaot623/sshx/internal/identity"
	"github.com/xiaot623/sshx/internal/protocol"
	"github.com/xiaot623/sshx/internal/remotefs"
)

func startRemoteFSServer(t *testing.T) (context.Context, string, *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	socket := shortSocketPath(t)
	server := &Server{
		SocketPath:   socket,
		Token:        "secret",
		Version:      "test-version",
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
	return ctx, socket, server
}

func connectRemoteFSPair(t *testing.T, ctx context.Context, socket string, execute func(context.Context, protocol.Frame) protocol.Frame) (*remotefs.Peer, <-chan error) {
	t.Helper()
	controlReady := make(chan error, 1)
	controlErr := make(chan error, 1)
	go func() {
		controlErr <- RunClientConnWithOptions(ctx, mustDialUnix(t, socket), ClientOptions{
			Ready:      controlReady,
			AppVersion: "test-version",
			TargetID:   "target-1",
			ContextID:  "context-1",
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

func waitForRemoteFSPeer(t *testing.T, server *Server, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		server.mu.Lock()
		registered := server.fsPeers[sessionID] != nil
		server.mu.Unlock()
		if registered {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("remote fs peer was not registered")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRemoteFSRejectsLocalToRemoteMountChannel(t *testing.T) {
	ctx, socket, _ := startRemoteFSServer(t)
	clientPeer, _ := connectRemoteFSPair(t, ctx, socket, nil)
	backend, err := remotefs.OpenRootBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := clientPeer.RegisterBackend("local-export", backend); err != nil {
		t.Fatal(err)
	}
	if _, err := clientPeer.CreateMount(ctx, "local-export"); err == nil {
		t.Fatal("server accepted a local-to-remote mount request")
	}
}

func TestRequesterExportsRemoteCwdToExactClientSession(t *testing.T) {
	ctx, socket, server := startRemoteFSServer(t)
	var peerMu sync.RWMutex
	var clientPeer *remotefs.Peer
	var firstMountID string
	execute := func(commandCtx context.Context, frame protocol.Frame) protocol.Frame {
		if !frame.RemoteFS || !frame.MountReadOnly || frame.MountID == "" || frame.SessionID != "session-1" {
			return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: "missing remote fs identity"}
		}
		if firstMountID == "" {
			firstMountID = frame.MountID
		} else if frame.MountID != firstMountID {
			return protocol.Frame{Type: protocol.TypeCommandError, ID: frame.ID, Error: "remote export was not reused"}
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
	waitForRemoteFSPeer(t, server, "session-1")
	remoteRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(remoteRoot, "remote.txt"), []byte("from-remote"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := RequestCommandForSessionWithMountOptions(
			ctx, socket, []string{"read-remote"}, nil, nil, remoteRoot,
			"session-1", true, true, time.Second, "secret",
		)
		if err != nil {
			t.Fatal(err)
		}
		if string(result.Stdout) != "from-remote" {
			t.Fatalf("stdout = %q", result.Stdout)
		}
	}
	server.mu.Lock()
	exports := len(server.fsBackends["session-1"])
	server.mu.Unlock()
	if exports != 1 {
		t.Fatalf("session exports = %d, want one reused export", exports)
	}
}

func TestRequesterRoutesToExactSessionWithMultipleClients(t *testing.T) {
	ctx, socket, _ := startRemoteFSServer(t)
	startClient := func(sessionID string) {
		ready := make(chan error, 1)
		go func() {
			_ = RunClientConnWithOptions(ctx, mustDialUnix(t, socket), ClientOptions{
				Ready: ready, AppVersion: "test-version", TargetID: "target-1", ContextID: "context-1", SessionID: sessionID,
				Allow: func([]string) bool { return true },
				Execute: func(_ context.Context, frame protocol.Frame) protocol.Frame {
					return protocol.Frame{Type: protocol.TypeCommandResult, ID: frame.ID, Stdout: base64.StdEncoding.EncodeToString([]byte(sessionID))}
				},
			}, "secret")
		}()
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
	}
	startClient("session-1")
	startClient("session-2")
	result, err := RequestCommandForSessionWithTimeout(ctx, socket, []string{"which-session"}, nil, nil, "", "session-2", false, time.Second, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "session-2" {
		t.Fatalf("request routed to %q", result.Stdout)
	}
}

func TestRequesterSelectsHealthySessionByContext(t *testing.T) {
	ctx, socket, _ := startRemoteFSServer(t)
	startClient := func(contextID, sessionID string) {
		ready := make(chan error, 1)
		go func() {
			_ = RunClientConnWithOptions(ctx, mustDialUnix(t, socket), ClientOptions{
				Ready: ready, AppVersion: "test-version", TargetID: "target-1", ContextID: contextID, SessionID: sessionID,
				Allow: func([]string) bool { return true },
				Execute: func(_ context.Context, frame protocol.Frame) protocol.Frame {
					return protocol.Frame{Type: protocol.TypeCommandResult, ID: frame.ID, Stdout: base64.StdEncoding.EncodeToString([]byte(sessionID))}
				},
			}, "secret")
		}()
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
	}
	startClient("context-a", "session-a")
	startClient("context-b", "session-b")
	result, err := RequestCommandForContextWithMountOptions(
		ctx, socket, []string{"which-context"}, nil, nil, "", "context-b", "", false, false, time.Second, "secret",
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Stdout) != "session-b" {
		t.Fatalf("context request routed to %q", result.Stdout)
	}
}

func TestRequesterRejectsMismatchedSessionIdentity(t *testing.T) {
	ctx, socket, _ := startRemoteFSServer(t)
	conn := mustDialUnix(t, socket)
	defer conn.Close()
	encoder := protocol.NewEncoder(conn)
	decoder := protocol.NewDecoder(conn)
	if err := encoder.Encode(protocol.Frame{Type: protocol.TypeHello, Role: protocol.RoleRequester, ProtocolVersion: protocol.Version, RuntimeID: identity.RuntimeID, Token: "secret", SessionID: "session-1"}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Encode(protocol.Frame{Type: protocol.TypeCommandExec, ID: "req-1", RequestID: "req-1", Argv: []string{"true"}, SessionID: "session-2"}); err != nil {
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

func TestRemoteFSRejectsDuplicateDataSession(t *testing.T) {
	ctx, socket, _ := startRemoteFSServer(t)
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
