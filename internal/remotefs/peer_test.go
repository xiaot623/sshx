package remotefs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func peerPair(t *testing.T, clientOptions, serverOptions PeerOptions) (*Peer, *Peer) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serverCh := make(chan *Peer, 1)
	errCh := make(chan error, 1)
	go func() {
		peer, err := Accept(ctx, serverConn, func(sessionID, token string) error {
			if sessionID != "session-1" || token != "token-1" {
				return syscall.EACCES
			}
			return nil
		}, serverOptions)
		if err != nil {
			errCh <- err
			return
		}
		serverCh <- peer
	}()
	client, err := Connect(ctx, clientConn, "session-1", "token-1", clientOptions)
	if err != nil {
		t.Fatal(err)
	}
	var server *Peer
	select {
	case server = <-serverCh:
	case err := <-errCh:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("timed out accepting peer")
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

func TestPeerProxiesFilesystemOperationsBothDirections(t *testing.T) {
	client, server := peerPair(t, PeerOptions{}, PeerOptions{})
	clientRoot := t.TempDir()
	serverRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(clientRoot, "client.txt"), []byte("client"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverRoot, "server.txt"), []byte("server"), 0o600); err != nil {
		t.Fatal(err)
	}
	clientBackend, err := OpenRootBackend(clientRoot)
	if err != nil {
		t.Fatal(err)
	}
	serverBackend, err := OpenRootBackend(serverRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RegisterBackend("client-root", clientBackend); err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterBackend("server-root", serverBackend); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	serverView := server.RemoteBackend("client-root")
	handle, _, err := serverView.Open(ctx, "client.txt", uint32(os.O_RDWR), 0)
	if err != nil {
		t.Fatal(err)
	}
	data, err := serverView.Read(ctx, handle, 0, 32)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "client" {
		t.Fatalf("client data = %q", data)
	}
	if _, err := serverView.Write(ctx, handle, 6, []byte("-updated")); err != nil {
		t.Fatal(err)
	}
	if err := serverView.Close(ctx, handle); err != nil {
		t.Fatal(err)
	}

	clientView := client.RemoteBackend("server-root")
	handle, _, err = clientView.Open(ctx, "server.txt", uint32(os.O_RDONLY), 0)
	if err != nil {
		t.Fatal(err)
	}
	data, err = clientView.Read(ctx, handle, 0, 32)
	if err != nil {
		t.Fatal(err)
	}
	_ = clientView.Close(ctx, handle)
	if string(data) != "server" {
		t.Fatalf("server data = %q", data)
	}

	got, err := os.ReadFile(filepath.Join(clientRoot, "client.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "client-updated" {
		t.Fatalf("updated data = %q", got)
	}
}

type blockingBackend struct {
	Backend
	canceled chan struct{}
	once     chan struct{}
}

type largeDirectoryBackend struct {
	Backend
}

func (b *largeDirectoryBackend) ReadDir(context.Context, string) ([]DirEntry, error) {
	entries := make([]DirEntry, 5000)
	for i := range entries {
		entries[i] = DirEntry{Name: string(bytes.Repeat([]byte{'x'}, 32)), Mode: syscall.S_IFREG | 0o600}
	}
	return entries, nil
}

func (b *blockingBackend) Read(ctx context.Context, _ uint64, _ int64, _ uint32) ([]byte, error) {
	select {
	case <-ctx.Done():
		close(b.canceled)
		return nil, ctx.Err()
	case <-b.once:
		return nil, nil
	}
}

func TestPeerPropagatesRequestCancellation(t *testing.T) {
	client, server := peerPair(t, PeerOptions{}, PeerOptions{})
	base, err := OpenRootBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blocking := &blockingBackend{Backend: base, canceled: make(chan struct{}), once: make(chan struct{})}
	if err := server.RegisterBackend("blocked", blocking); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = client.RemoteBackend("blocked").Read(ctx, 1, 0, 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read error = %v", err)
	}
	select {
	case <-blocking.canceled:
	case <-time.After(time.Second):
		t.Fatal("remote request was not canceled")
	}
}

func TestPeerHandlesConcurrentRequests(t *testing.T) {
	client, server := peerPair(t, PeerOptions{}, PeerOptions{})
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "value.txt"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, err := OpenRootBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterBackend("concurrent", backend); err != nil {
		t.Fatal(err)
	}
	remote := client.RemoteBackend("concurrent")
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle, _, err := remote.Open(context.Background(), "value.txt", uint32(os.O_RDONLY), 0)
			if err != nil {
				errs <- err
				return
			}
			data, readErr := remote.Read(context.Background(), handle, 0, 16)
			closeErr := remote.Close(context.Background(), handle)
			if readErr != nil {
				errs <- readErr
			} else if closeErr != nil {
				errs <- closeErr
			} else if string(data) != "value" {
				errs <- errors.New("unexpected concurrent read")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestPeerReturnsE2BIGForOversizedResponseWithoutHanging(t *testing.T) {
	client, server := peerPair(t, PeerOptions{}, PeerOptions{})
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "value.txt"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := OpenRootBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	backend := &largeDirectoryBackend{Backend: base}
	if err := server.RegisterBackend("large", backend); err != nil {
		t.Fatal(err)
	}
	remote := client.RemoteBackend("large")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := remote.ReadDir(ctx, "."); !errors.Is(err, syscall.E2BIG) {
		t.Fatalf("large readdir error = %v", err)
	}
	if _, err := remote.Lookup(ctx, "value.txt"); err != nil {
		t.Fatalf("peer was not usable after E2BIG: %v", err)
	}
}

func TestPeerHandlesConcurrentRequestsInBothDirections(t *testing.T) {
	client, server := peerPair(t, PeerOptions{}, PeerOptions{})
	clientRoot := t.TempDir()
	serverRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(clientRoot, "value.txt"), []byte("client"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serverRoot, "value.txt"), []byte("server"), 0o600); err != nil {
		t.Fatal(err)
	}
	clientBackend, err := OpenRootBackend(clientRoot)
	if err != nil {
		t.Fatal(err)
	}
	serverBackend, err := OpenRootBackend(serverRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.RegisterBackend("client", clientBackend); err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterBackend("server", serverBackend); err != nil {
		t.Fatal(err)
	}
	read := func(backend Backend, want string) error {
		handle, _, err := backend.Open(context.Background(), "value.txt", uint32(os.O_RDONLY), 0)
		if err != nil {
			return err
		}
		data, readErr := backend.Read(context.Background(), handle, 0, 16)
		closeErr := backend.Close(context.Background(), handle)
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if string(data) != want {
			return errors.New("unexpected full-duplex read")
		}
		return nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for range 32 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- read(client.RemoteBackend("server"), "server")
		}()
		go func() {
			defer wg.Done()
			errs <- read(server.RemoteBackend("client"), "client")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPeerMountLifecycleCallbacks(t *testing.T) {
	unmounted := make(chan string, 1)
	client, _ := peerPair(t, PeerOptions{}, PeerOptions{
		OnMount: func(_ context.Context, _ *Peer, mountID string) (string, error) {
			return "/tmp/" + mountID, nil
		},
		OnUnmount: func(_ context.Context, mountID string) error {
			unmounted <- mountID
			return nil
		},
	})
	path, err := client.CreateMount(context.Background(), "workspace")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/tmp/workspace" {
		t.Fatalf("mount path = %q", path)
	}
	if err := client.ReleaseMount(context.Background(), "workspace"); err != nil {
		t.Fatal(err)
	}
	select {
	case mountID := <-unmounted:
		if mountID != "workspace" {
			t.Fatalf("mount id = %q", mountID)
		}
	case <-time.After(time.Second):
		t.Fatal("unmount callback was not called")
	}
}

func TestPeerRejectsExportsAfterClose(t *testing.T) {
	client, _ := peerPair(t, PeerOptions{}, PeerOptions{})
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	backend, err := OpenRootBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer backend.CloseBackend()
	if err := client.RegisterBackend("late", backend); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("register after close error = %v", err)
	}
}

func TestWireFrameRoundTripAndLimits(t *testing.T) {
	frame := wireFrame{Type: frameRequest, ID: 42, Op: "write", MountID: "workspace", Data: []byte("hello")}
	var buffer bytes.Buffer
	if err := writeWireFrame(&buffer, frame); err != nil {
		t.Fatal(err)
	}
	got, err := readWireFrame(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != frame.ID || got.Op != frame.Op || !bytes.Equal(got.Data, frame.Data) {
		t.Fatalf("round trip = %#v", got)
	}
	frame.Data = make([]byte, MaxDataSize+1)
	if err := writeWireFrame(&buffer, frame); err == nil {
		t.Fatal("oversized data frame succeeded")
	}
	frame.Data = nil
	frame.MountID = string(bytes.Repeat([]byte{'x'}, MaxMetadataSize))
	if err := writeWireFrame(&buffer, frame); err == nil {
		t.Fatal("oversized metadata frame succeeded")
	}
}

func FuzzReadWireFrame(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 2, 0, 0, 0, 0, '{', '}'})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = readWireFrame(bytes.NewReader(data))
	})
}
