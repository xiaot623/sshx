package locald

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaot623/sshx/internal/domain"
	"github.com/xiaot623/sshx/internal/forward"
	"github.com/xiaot623/sshx/internal/loopback"
	"github.com/xiaot623/sshx/internal/protocol"
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
	requireTargetLoopback(t)
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
		if resp.ListenIP != "127.64.0.1" {
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
	requireTargetLoopback(t)
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
	if resp.LocalPort != basePort || resp.ListenIP != "127.64.0.1" {
		t.Fatalf("response = %#v, want target IP port %d", resp, basePort)
	}
}

func TestListPorts(t *testing.T) {
	requireTargetLoopback(t)
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
	if got.Target != "debian" || got.RemotePort != remotePort || got.LocalPort != remotePort || got.ListenIP != "127.64.0.1" {
		t.Fatalf("forward = %#v", got)
	}
}

func TestListPortsIncludesDomainForDirectTarget(t *testing.T) {
	requireTargetLoopback(t)
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
		got.ListenIP != "127.64.0.1" {
		t.Fatalf("forward = %#v", got)
	}
}

func TestMultipleTargetsCanExposeSameRemotePort(t *testing.T) {
	requireTargetLoopback(t)
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

func TestLoopbackPoolMatchesProvisionedRangeAndReusesReleasedAddresses(t *testing.T) {
	s := &Server{targets: map[string]*targetRecord{}}
	for i := 0; i < loopback.Size; i++ {
		ip, err := s.allocateLoopbackIPLocked()
		if err != nil {
			t.Fatalf("allocate address %d: %v", i+1, err)
		}
		if want := loopback.Address(i); ip != want {
			t.Fatalf("address %d = %q, want %q", i+1, ip, want)
		}
		s.targets[itoa(i)] = &targetRecord{ListenIP: ip}
	}

	if ip, err := s.allocateLoopbackIPLocked(); err == nil || ip != "" || !strings.Contains(err.Error(), "pool exhausted") {
		t.Fatalf("allocation beyond pool = %q, %v", ip, err)
	}

	const released = 17
	delete(s.targets, itoa(released))
	ip, err := s.allocateLoopbackIPLocked()
	if err != nil {
		t.Fatalf("reuse released address: %v", err)
	}
	if want := loopback.Address(released); ip != want {
		t.Fatalf("reused address = %q, want %q", ip, want)
	}
}

func TestRemoveTargetPortStopsListingForward(t *testing.T) {
	requireTargetLoopback(t)
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

func TestLastSessionClosesLocalDaemon(t *testing.T) {
	ctx := context.Background()
	socket := shortSocketPath(t)
	dnsAddr := freeUDPAddr(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket, Version: "test-version", LeaseTimeout: 100 * time.Millisecond}).Serve(ctx)
	}()
	waitForSocket(t, socket)
	session, err := OpenSession(ctx, socket, Request{
		SSHPath: "ssh", Target: "debian", DomainSuffix: "it.sshx", DNSAddr: dnsAddr,
		SessionID: "session-1", AppVersion: "test-version",
	}, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("local daemon did not exit after its last session closed")
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still exists: %v", err)
	}
	addr, err := net.ResolveUDPAddr("udp", dnsAddr)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		t.Fatalf("DNS listener was not released: %v", err)
	}
	_ = conn.Close()
}

func TestLocalDaemonWaitsForAllSessions(t *testing.T) {
	ctx := context.Background()
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket, Version: "test-version"}).Serve(ctx)
	}()
	waitForSocket(t, socket)
	var sessions []*Session
	for _, id := range []string{"session-1", "session-2"} {
		session, err := OpenSession(ctx, socket, Request{
			SSHPath: "ssh", Target: "debian", DomainSuffix: "it.sshx", DNSAddr: "127.0.0.1:0",
			SessionID: id, AppVersion: "test-version",
		}, 10*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		sessions = append(sessions, session)
	}
	_ = sessions[0].Close()
	select {
	case err := <-errCh:
		t.Fatalf("daemon exited with another session active: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	_ = sessions[1].Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not exit after all sessions closed")
	}
}

func TestLocalDaemonExpiresSessionWithoutHeartbeat(t *testing.T) {
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket, Version: "test-version", LeaseTimeout: 30 * time.Millisecond}).Serve(context.Background())
	}()
	waitForSocket(t, socket)
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := Request{
		Type: TypeOpenSession, SSHPath: "ssh", Target: "debian", DomainSuffix: "it.sshx", DNSAddr: "127.0.0.1:0",
		SessionID: "session-1", AppVersion: "test-version", ProtocolVersion: protocol.Version,
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		t.Fatal(err)
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil || !resp.OK {
		t.Fatalf("open response = %#v, %v", resp, err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not expire session without heartbeat")
	}
}

func TestLocalDaemonExitsOnVersionChange(t *testing.T) {
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket, Version: "1.0.0"}).Serve(context.Background())
	}()
	waitForSocket(t, socket)
	_, err := OpenSession(context.Background(), socket, Request{
		SSHPath: "ssh", Target: "debian", DomainSuffix: "it.sshx", DNSAddr: "127.0.0.1:0",
		SessionID: "session-1", AppVersion: "2.0.0",
	}, 10*time.Millisecond)
	if err == nil {
		t.Fatal("version-changing session unexpectedly opened")
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not exit after version change")
	}
}

func TestPortMutationRequiresActiveSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket, Version: "test-version"}).Serve(ctx)
	}()
	waitForSocket(t, socket)
	_, err := ClientRequest(ctx, socket, Request{
		Type: TypeEnsureTargetPort, SSHPath: "ssh", Target: "debian", RemotePort: 8080,
		DomainSuffix: "it.sshx", DNSAddr: "127.0.0.1:0",
	})
	if err == nil || !strings.Contains(err.Error(), "active session lease") {
		t.Fatalf("error = %v", err)
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Fatal(err)
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

func requireTargetLoopback(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(loopback.Address(0), "0"))
	if err != nil {
		t.Skipf("target loopback aliases are unavailable: %v", err)
	}
	_ = ln.Close()
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
