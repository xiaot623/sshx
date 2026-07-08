package locald

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiaot623/sshx/internal/domain"
	"github.com/xiaot623/sshx/internal/forward"
)

func TestPing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket}).Serve(ctx)
	}()
	waitForSocket(t, socket)
	resp, err := ClientRequest(ctx, socket, Request{Type: TypePing})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("response = %#v", resp)
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

func TestDomainManagerCanBeSharedByRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := shortSocketPath(t)
	s := &Server{SocketPath: socket}
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Serve(ctx)
	}()
	waitForSocket(t, socket)

	dnsAddr := freeUDPAddr(t)
	remotePort := freeTCPPort(t)
	for i := 0; i < 2; i++ {
		resp := s.handle(ctx, Request{
			Type:         TypeEnsureTargetPort,
			SSHPath:      "ssh",
			Target:       "debian",
			RemotePort:   remotePort,
			DomainSuffix: "it.sshx",
			DNSAddr:      dnsAddr,
		})
		if !resp.OK {
			t.Fatalf("ensure response = %#v", resp)
		}
		if resp.LocalPort != remotePort {
			t.Fatalf("local port = %d, want %d", resp.LocalPort, remotePort)
		}
		if resp.Domain != "debian.it.sshx" {
			t.Fatalf("domain = %q", resp.Domain)
		}
		if resp.ListenIP != "127.64.0.2" {
			t.Fatalf("listen IP = %q", resp.ListenIP)
		}
	}
	if len(s.domains) != 1 {
		t.Fatalf("domains = %#v", s.domains)
	}

	cancel()
	select {
	case <-errCh:
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
}

func TestDomainForwardUsesTargetIPWhenLocalhostPortIsOccupied(t *testing.T) {
	basePort := freeConsecutiveTCPPorts(t)
	occupied, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(basePort)))
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{
		SocketPath:     shortSocketPath(t),
		forwarders:     map[string]*forward.Manager{},
		forwardRecords: map[string]map[int]forwardRecord{},
		domains:        map[string]*domain.Manager{},
		Stderr:         io.Discard,
	}
	resp := s.handle(ctx, Request{
		Type:         TypeEnsureTargetPort,
		SSHPath:      "ssh",
		Target:       "debian",
		RemotePort:   basePort,
		DomainSuffix: "it.sshx",
		DNSAddr:      "127.0.0.1:0",
	})
	if !resp.OK {
		t.Fatalf("ensure response = %#v", resp)
	}
	if resp.LocalPort != basePort || resp.ListenIP != "127.64.0.2" {
		t.Fatalf("response = %#v, want target IP port %d", resp, basePort)
	}
}

func TestListPorts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{
		SocketPath:     shortSocketPath(t),
		forwarders:     map[string]*forward.Manager{},
		forwardRecords: map[string]map[int]forwardRecord{},
		domains:        map[string]*domain.Manager{},
		Stderr:         io.Discard,
	}
	remotePort := freeTCPPort(t)
	ensure := s.handle(ctx, Request{
		Type:         TypeEnsureTargetPort,
		SSHPath:      "ssh",
		Target:       "debian",
		RemotePort:   remotePort,
		DomainSuffix: "it.sshx",
		DNSAddr:      "127.0.0.1:0",
	})
	if !ensure.OK {
		t.Fatalf("ensure response = %#v", ensure)
	}
	resp := s.handle(ctx, Request{Type: TypeListPorts})
	if !resp.OK || len(resp.Forwards) != 1 {
		t.Fatalf("list response = %#v", resp)
	}
	got := resp.Forwards[0]
	if got.Target != "debian" || got.RemotePort != remotePort || got.LocalPort != remotePort || got.ListenIP != "127.64.0.2" {
		t.Fatalf("forward = %#v", got)
	}
}

func TestListPortsIncludesDomainForDirectTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{
		SocketPath:     shortSocketPath(t),
		forwarders:     map[string]*forward.Manager{},
		forwardRecords: map[string]map[int]forwardRecord{},
		domains:        map[string]*domain.Manager{},
		Stderr:         io.Discard,
	}
	remotePort := freeTCPPort(t)
	ensure := s.handle(ctx, Request{
		Type:         TypeEnsureTargetPort,
		SSHPath:      "ssh",
		Target:       "debian@192.168.1.100",
		RemotePort:   remotePort,
		DomainSuffix: "it.sshx",
		DNSAddr:      "127.0.0.1:0",
	})
	if !ensure.OK {
		t.Fatalf("ensure response = %#v", ensure)
	}
	resp := s.handle(ctx, Request{Type: TypeListPorts})
	if !resp.OK || len(resp.Forwards) != 1 {
		t.Fatalf("list response = %#v", resp)
	}
	got := resp.Forwards[0]
	if got.Target != "debian@192.168.1.100" ||
		got.Domain != "debian-192-168-1-100.it.sshx" ||
		got.RemotePort != remotePort ||
		got.LocalPort != remotePort ||
		got.ListenIP != "127.64.0.2" {
		t.Fatalf("forward = %#v", got)
	}
}

func TestMultipleTargetsCanExposeSameRemotePort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{SocketPath: shortSocketPath(t), Stderr: io.Discard}
	remotePort := freeTCPPort(t)
	for _, target := range []string{"debian", "ubuntu"} {
		resp := s.handle(ctx, Request{
			Type:         TypeEnsureTargetPort,
			SSHPath:      "ssh",
			Target:       target,
			SSHArgs:      []string{target},
			RemotePort:   remotePort,
			DomainSuffix: "it.sshx",
			DNSAddr:      "127.0.0.1:0",
		})
		if !resp.OK {
			t.Fatalf("ensure %s response = %#v", target, resp)
		}
	}
	resp := s.handle(ctx, Request{Type: TypeListPorts})
	if !resp.OK || len(resp.Forwards) != 2 {
		t.Fatalf("list response = %#v", resp)
	}
	if resp.Forwards[0].ListenIP == resp.Forwards[1].ListenIP {
		t.Fatalf("targets shared listen IP: %#v", resp.Forwards)
	}
	for _, fwd := range resp.Forwards {
		if fwd.RemotePort != remotePort || fwd.LocalPort != remotePort {
			t.Fatalf("forward = %#v", fwd)
		}
	}
}

func TestRemoveTargetPortStopsListingForward(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{SocketPath: shortSocketPath(t), Stderr: io.Discard}
	remotePort := freeTCPPort(t)
	req := Request{
		SSHPath:      "ssh",
		Target:       "debian",
		RemotePort:   remotePort,
		DomainSuffix: "it.sshx",
		DNSAddr:      "127.0.0.1:0",
	}
	ensure := s.handle(ctx, withType(req, TypeEnsureTargetPort))
	if !ensure.OK {
		t.Fatalf("ensure response = %#v", ensure)
	}
	remove := s.handle(ctx, withType(req, TypeRemoveTargetPort))
	if !remove.OK {
		t.Fatalf("remove response = %#v", remove)
	}
	resp := s.handle(ctx, Request{Type: TypeListPorts})
	if !resp.OK || len(resp.Forwards) != 0 {
		t.Fatalf("list response = %#v", resp)
	}
}

func TestUnregisterTargetCleansAfterLastReference(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{SocketPath: shortSocketPath(t), Stderr: io.Discard}
	remotePort := freeTCPPort(t)
	req := Request{
		SSHPath:      "ssh",
		Target:       "debian",
		RemotePort:   remotePort,
		DomainSuffix: "it.sshx",
		DNSAddr:      "127.0.0.1:0",
	}
	for i := 0; i < 2; i++ {
		if resp := s.handle(ctx, withType(req, TypeRegisterTarget)); !resp.OK {
			t.Fatalf("register response = %#v", resp)
		}
	}
	if resp := s.handle(ctx, withType(req, TypeEnsureTargetPort)); !resp.OK {
		t.Fatalf("ensure response = %#v", resp)
	}
	if resp := s.handle(ctx, withType(req, TypeUnregisterTarget)); !resp.OK {
		t.Fatalf("first unregister response = %#v", resp)
	}
	list := s.handle(ctx, Request{Type: TypeListPorts})
	if !list.OK || len(list.Forwards) != 1 {
		t.Fatalf("list after first unregister = %#v", list)
	}
	if resp := s.handle(ctx, withType(req, TypeUnregisterTarget)); !resp.OK {
		t.Fatalf("second unregister response = %#v", resp)
	}
	list = s.handle(ctx, Request{Type: TypeListPorts})
	if !list.OK || len(list.Forwards) != 0 {
		t.Fatalf("list after second unregister = %#v", list)
	}
}

func waitForSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", path); err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s was not created", path)
}

func withType(req Request, typ string) Request {
	req.Type = typ
	return req
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sshx-locald-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func freeConsecutiveTCPPorts(t *testing.T) int {
	t.Helper()
	for port := 20000; port < 60000; port++ {
		ln1, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
		if err != nil {
			continue
		}
		ln2, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port+1)))
		if err == nil {
			_ = ln2.Close()
			_ = ln1.Close()
			return port
		}
		_ = ln1.Close()
	}
	t.Fatal("no consecutive TCP ports available")
	return 0
}

func freeUDPAddr(t *testing.T) string {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.LocalAddr().String()
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
