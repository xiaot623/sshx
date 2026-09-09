package forward

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestEnsureListensWithoutControlPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := false
	m := NewDynamicManager(ctx, func() (string, []string, string) {
		return "ssh", []string{"host"}, ""
	})
	m.execControl = func(context.Context, string, []string) ([]byte, error) {
		called = true
		return nil, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	f, err := m.Ensure(port, "127.0.0.1", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.listener == nil {
		t.Fatal("empty ControlPath did not bind a local listener")
	}
	if called {
		t.Fatal("empty ControlPath executed ssh")
	}
	probe, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err == nil {
		_ = probe.Close()
		t.Fatal("listen IP:port was not bound")
	}
	m.Stop()
}

func TestEnsureUsesControlForwardWhenControlPathSet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ops [][]string
	m := NewDynamicManager(ctx, func() (string, []string, string) {
		return "ssh", []string{"-t", "-L", "1:localhost:1", "-p", "2222", "host"}, "/tmp/master"
	})
	m.execControl = func(_ context.Context, sshPath string, args []string) ([]byte, error) {
		if sshPath != "ssh" {
			t.Fatalf("sshPath = %q", sshPath)
		}
		ops = append(ops, append([]string(nil), args...))
		return nil, nil
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	f, err := m.Ensure(port, "127.0.0.1", "localhost")
	if err != nil {
		t.Fatal(err)
	}
	if f.listener != nil {
		t.Fatal("ControlPath mode also bound a local listener")
	}
	probe, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatalf("ControlPath mode bound the listen address: %v", err)
	}
	_ = probe.Close()
	if len(ops) != 1 {
		t.Fatalf("ops = %#v", ops)
	}
	got := strings.Join(ops[0], " ")
	wantSpec := LocalForwardSpec("127.0.0.1", port, "127.0.0.1")
	for _, required := range []string{"-S /tmp/master", "-O forward", "-L " + wantSpec, "-p 2222", "host"} {
		if !strings.Contains(got, required) {
			t.Fatalf("forward args lost %q: %s", required, got)
		}
	}
	for _, forbidden := range []string{"-t", "-L 1:localhost:1", "-W"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("forward args retained %q: %s", forbidden, got)
		}
	}

	m.Remove(port)
	if len(ops) != 2 {
		t.Fatalf("ops after remove = %#v", ops)
	}
	cancelGot := strings.Join(ops[1], " ")
	if !strings.Contains(cancelGot, "-O cancel") || !strings.Contains(cancelGot, "-L "+wantSpec) {
		t.Fatalf("cancel args = %s", cancelGot)
	}
	if len(m.List()) != 0 {
		t.Fatalf("list after remove = %#v", m.List())
	}
}

func TestEnsureControlForwardErrorIsReturned(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewDynamicManager(ctx, func() (string, []string, string) {
		return "ssh", []string{"host"}, "/tmp/master"
	})
	m.execControl = func(context.Context, string, []string) ([]byte, error) {
		return []byte("bind: address already in use\n"), errors.New("exit status 1")
	}
	_, err := m.Ensure(8080, "127.64.0.1", "127.0.0.1")
	if err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("err = %v", err)
	}
	if len(m.List()) != 0 {
		t.Fatalf("failed forward was recorded: %#v", m.List())
	}
}

func TestEnsureSamePortDoesNotCancelExistingControlForward(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ops []string
	m := NewDynamicManager(ctx, func() (string, []string, string) {
		return "ssh", []string{"host"}, "/tmp/master"
	})
	m.execControl = func(_ context.Context, _ string, args []string) ([]byte, error) {
		ops = append(ops, strings.Join(args, " "))
		return nil, nil
	}
	if _, err := m.Ensure(8080, "127.64.0.1", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(8080, "127.64.0.1", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || !strings.Contains(ops[0], "-O forward") {
		t.Fatalf("ops = %#v", ops)
	}
	m.Stop()
	if len(ops) != 2 || !strings.Contains(ops[1], "-O cancel") {
		t.Fatalf("ops after stop = %#v", ops)
	}
}

func TestEnsureIPv6RemoteHostSpec(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got string
	m := NewDynamicManager(ctx, func() (string, []string, string) {
		return "ssh", []string{"host"}, "/tmp/master"
	})
	m.execControl = func(_ context.Context, _ string, args []string) ([]byte, error) {
		got = strings.Join(args, " ")
		return nil, nil
	}
	if _, err := m.Ensure(8080, "127.64.0.1", "::1"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "-L 127.64.0.1:8080:[::1]:8080") {
		t.Fatalf("args = %s", got)
	}
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
