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
	"github.com/xiaot623/sshx/internal/identity"
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

func TestTargetsWithCollidingPrefixesGetUniqueDomains(t *testing.T) {
	requireTargetLoopback(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{SocketPath: shortSocketPath(t), Stderr: io.Discard}
	remotePort := freeTCPPort(t)

	var domains []string
	for _, target := range []string{"foo_bar", "foo-bar", "foo bar"} {
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
			t.Fatalf("ensure %q response = %#v", target, resp)
		}
		domains = append(domains, resp.Domain)
	}

	want := []string{"foo-bar.it.sshx", "foo-bar-1.it.sshx", "foo-bar-2.it.sshx"}
	for i := range want {
		if domains[i] != want[i] {
			t.Fatalf("domain %d = %q, want %q", i, domains[i], want[i])
		}
	}
}

func TestSameTargetLabelWithDifferentSSHArgsGetsUniqueDomains(t *testing.T) {
	requireTargetLoopback(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{SocketPath: shortSocketPath(t), Stderr: io.Discard}
	remotePort := freeTCPPort(t)

	var domains []string
	for _, port := range []string{"22", "2222"} {
		resp := s.handle(ctx, Request{
			Type:         TypeEnsureTargetPort,
			SSHPath:      "ssh",
			Target:       "debian",
			SSHArgs:      []string{"-p", port, "debian"},
			RemotePort:   remotePort,
			DomainSuffix: "it.sshx",
			DNSAddr:      "127.0.0.1:0",
		})
		if !resp.OK {
			t.Fatalf("ensure SSH port %s response = %#v", port, resp)
		}
		domains = append(domains, resp.Domain)
	}
	if domains[0] != "debian.it.sshx" || domains[1] != "debian-1.it.sshx" {
		t.Fatalf("domains = %#v", domains)
	}
}

func TestSameTargetConnectionReusesDomain(t *testing.T) {
	requireTargetLoopback(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{Stderr: io.Discard}
	req := Request{
		SSHPath:      "ssh",
		Target:       "foo_bar",
		SSHArgs:      []string{"-p", "2222", "foo_bar"},
		DomainSuffix: "it.sshx",
		DNSAddr:      "127.0.0.1:0",
	}

	first, err := s.ensureTarget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ensureTarget(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatal("same SSH target connection created a second target record")
	}
	if first.Domain != "foo-bar.it.sshx" || second.Domain != first.Domain || second.ListenIP != first.ListenIP {
		t.Fatalf("target records = %#v and %#v", first, second)
	}
	if len(s.targets) != 1 {
		t.Fatalf("target record count = %d, want 1", len(s.targets))
	}
}

func TestTargetDomainIsReleasedWithoutUnregisteringCollidingTarget(t *testing.T) {
	requireTargetLoopback(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Server{
		Stderr:   io.Discard,
		sessions: map[string]*sessionRecord{},
	}
	baseReq := Request{
		SSHPath:      "ssh",
		Target:       "foo_bar",
		SSHArgs:      []string{"foo_bar"},
		DomainSuffix: "it.sshx",
		DNSAddr:      "127.0.0.1:0",
	}
	collidingReq := baseReq
	collidingReq.Target = "foo-bar"
	collidingReq.SSHArgs = []string{"foo-bar"}

	first, err := s.ensureTarget(ctx, baseReq)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ensureTarget(ctx, collidingReq)
	if err != nil {
		t.Fatal(err)
	}
	first.Sessions = 1
	second.Sessions = 1
	firstKey := requestKey(baseReq.SSHPath, baseReq.SSHArgs)
	secondKey := requestKey(collidingReq.SSHPath, collidingReq.SSHArgs)
	s.sessions["first"] = &sessionRecord{ID: "first", TargetKey: firstKey}
	s.sessions["second"] = &sessionRecord{ID: "second", TargetKey: secondKey}

	s.releaseSession("first")
	if s.targets[secondKey] != second {
		t.Fatal("releasing first target removed the colliding target")
	}

	reused, err := s.ensureTarget(ctx, baseReq)
	if err != nil {
		t.Fatal(err)
	}
	if reused.Domain != "foo-bar.it.sshx" {
		t.Fatalf("reused domain = %q", reused.Domain)
	}

	thirdReq := baseReq
	thirdReq.Target = "foo bar"
	thirdReq.SSHArgs = []string{"foo bar"}
	third, err := s.ensureTarget(ctx, thirdReq)
	if err != nil {
		t.Fatal(err)
	}
	if third.Domain != "foo-bar-2.it.sshx" {
		t.Fatalf("third domain = %q; colliding target domain was unexpectedly released", third.Domain)
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
		SessionID: "session-1", AppVersion: "test-version", RuntimeID: identity.LocalRuntimeID, ProtocolVersion: protocol.Version,
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

func TestLocalDaemonAllowsDifferentAppVersionsInOneRuntime(t *testing.T) {
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket, Version: "1.0.0"}).Serve(context.Background())
	}()
	waitForSocket(t, socket)
	session, err := OpenSession(context.Background(), socket, Request{
		SSHPath: "ssh", Target: "debian", DomainSuffix: "it.sshx", DNSAddr: "127.0.0.1:0",
		SessionID: "session-1", AppVersion: "2.0.0",
	}, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("open different app version: %v", err)
	}
	_ = session.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not exit after the final lease")
	}
}

func TestLocalDaemonHandoffGraceAcceptsReplacementLease(t *testing.T) {
	socket := shortSocketPath(t)
	errCh := make(chan error, 1)
	go func() {
		errCh <- (&Server{SocketPath: socket, Version: "test-version", HandoffGrace: 80 * time.Millisecond}).Serve(context.Background())
	}()
	waitForSocket(t, socket)
	open := func(leaseID string) *Session {
		session, err := OpenSession(context.Background(), socket, Request{
			SSHPath: "ssh", Target: "debian", TargetID: "stable-target", DomainSuffix: "it.sshx", DNSAddr: "127.0.0.1:0",
			LeaseID: leaseID, AppVersion: "test-version",
		}, 10*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	first := open("lease-1")
	_ = first.Close()
	time.Sleep(20 * time.Millisecond)
	second := open("lease-2")
	// Let the first lease's grace timer fire. The replacement must keep the
	// target resources and daemon alive.
	time.Sleep(100 * time.Millisecond)
	if _, err := ClientRequest(context.Background(), socket, Request{Type: TypePing}); err != nil {
		t.Fatalf("daemon exited during handoff: %v", err)
	}
	_ = second.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not exit after replacement lease grace")
	}
}

func TestLocalDaemonHandoffGraceWaitsForAllTargets(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const grace = 300 * time.Millisecond
	socket := shortSocketPath(t)
	s := &Server{SocketPath: socket, Version: "test-version", HandoffGrace: grace}
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.Serve(ctx)
	}()
	waitForSocket(t, socket)
	open := func(leaseID, targetID, target string) *Session {
		session, err := OpenSession(ctx, socket, Request{
			SSHPath: "ssh", Target: target, TargetID: targetID, DomainSuffix: "it.sshx", DNSAddr: "127.0.0.1:0",
			LeaseID: leaseID, AppVersion: "test-version",
		}, 10*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	first := open("lease-1", "target-1", "debian-1")
	second := open("lease-2", "target-2", "debian-2")
	_ = first.Close()
	time.Sleep(grace / 2)
	_ = second.Close()

	// The first target's grace expires while the second target is still
	// waiting for a replacement lease. The daemon must remain available.
	deadline := time.Now().Add(grace)
	for {
		s.mu.Lock()
		_, firstExists := s.targets["target-1"]
		_, secondExists := s.targets["target-2"]
		s.mu.Unlock()
		if !firstExists && secondExists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first target was not cleaned before second target's grace expired")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := ClientRequest(ctx, socket, Request{Type: TypePing}); err != nil {
		t.Fatalf("daemon exited during another target's handoff grace: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not exit after all target grace periods expired")
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
