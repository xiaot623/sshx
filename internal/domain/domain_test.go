package domain

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestManagerNameForTarget(t *testing.T) {
	m := NewManager("xiaot.sshx", "127.0.0.1:0", io.Discard)
	if got := m.NameForTarget("debian"); got != "debian.xiaot.sshx" {
		t.Fatalf("NameForTarget(debian) = %q", got)
	}
	if got := m.NameForTarget("root@debian:2222"); got != "debian.xiaot.sshx" {
		t.Fatalf("NameForTarget(root@debian:2222) = %q", got)
	}
}

func TestManagerDNSResolvesSuffixToLoopback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager("xiaot.sshx", "127.0.0.1:0", io.Discard)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	conn, err := net.Dial("udp", m.DNSAddr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(dnsQuery("debian.xiaot.sshx")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf[:n], []byte{127, 0, 0, 1}) {
		t.Fatalf("DNS response did not contain loopback A record: %x", buf[:n])
	}
}

func dnsQuery(name string) []byte {
	var b []byte
	b = binary.BigEndian.AppendUint16(b, 0x1234)
	b = binary.BigEndian.AppendUint16(b, 0x0100)
	b = binary.BigEndian.AppendUint16(b, 1)
	b = binary.BigEndian.AppendUint16(b, 0)
	b = binary.BigEndian.AppendUint16(b, 0)
	b = binary.BigEndian.AppendUint16(b, 0)
	for _, label := range strings.Split(name, ".") {
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, 1)
	b = binary.BigEndian.AppendUint16(b, 1)
	return b
}
