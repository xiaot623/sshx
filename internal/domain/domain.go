package domain

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
)

type Manager struct {
	suffix  string
	dnsAddr string
	stderr  io.Writer

	dnsConn   *net.UDPConn
	closeOnce sync.Once
}

func NewManager(suffix, dnsAddr string, stderr io.Writer) *Manager {
	return &Manager{
		suffix:  normalizeSuffix(suffix),
		dnsAddr: dnsAddr,
		stderr:  stderr,
	}
}

func (m *Manager) Start(ctx context.Context) error {
	if m.suffix == "" {
		return errors.New("domain suffix is required")
	}
	if err := m.startDNS(ctx); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		m.Stop()
	}()
	return nil
}

func (m *Manager) Stop() {
	m.closeOnce.Do(func() {
		if m.dnsConn != nil {
			_ = m.dnsConn.Close()
		}
	})
}

func (m *Manager) DNSAddr() string {
	if m.dnsConn == nil {
		return ""
	}
	return m.dnsConn.LocalAddr().String()
}

func (m *Manager) NameForTarget(target string) string {
	host := domainSafeHost(targetHost(target))
	if host == "" {
		host = "host"
	}
	return fmt.Sprintf("%s.%s", host, m.suffix)
}

func (m *Manager) startDNS(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp", m.dnsAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	m.dnsConn = conn
	go m.serveDNS(ctx)
	return nil
}

func (m *Manager) serveDNS(ctx context.Context) {
	buf := make([]byte, 1500)
	for {
		n, addr, err := m.dnsConn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		resp, err := buildDNSResponse(buf[:n], m.suffix)
		if err != nil {
			continue
		}
		_, _ = m.dnsConn.WriteToUDP(resp, addr)
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func normalizeSuffix(s string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(s), "."))
}

func targetHost(target string) string {
	target = strings.TrimSpace(target)
	if at := strings.LastIndex(target, "@"); at >= 0 {
		target = target[at+1:]
	}
	if strings.HasPrefix(target, "[") {
		if end := strings.Index(target, "]"); end >= 0 {
			return target[1:end]
		}
	}
	if colon := strings.LastIndex(target, ":"); colon > 0 && !strings.Contains(target[:colon], ":") {
		return target[:colon]
	}
	return target
}

func domainSafeHost(host string) string {
	host = strings.ToLower(strings.Trim(strings.TrimSpace(host), "."))
	var b strings.Builder
	prevDash := false
	for _, r := range host {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.'
		if ok {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(b.String(), "-.")
}

func buildDNSResponse(req []byte, suffix string) ([]byte, error) {
	if len(req) < 12 {
		return nil, errors.New("short dns request")
	}
	name, qEnd, err := parseQName(req, 12)
	if err != nil {
		return nil, err
	}
	if qEnd+4 > len(req) {
		return nil, errors.New("short dns question")
	}
	qtype := binary.BigEndian.Uint16(req[qEnd : qEnd+2])
	answer := normalizeSuffix(name) == normalizeSuffix(suffix) || strings.HasSuffix(normalizeSuffix(name), "."+normalizeSuffix(suffix))
	resp := make([]byte, 0, len(req)+32)
	resp = append(resp, req[:2]...)
	flags := uint16(0x8180)
	if !answer {
		flags = 0x8183
	}
	resp = binary.BigEndian.AppendUint16(resp, flags)
	resp = append(resp, req[4:6]...)
	if answer && (qtype == 1 || qtype == 255) {
		resp = binary.BigEndian.AppendUint16(resp, 1)
	} else {
		resp = binary.BigEndian.AppendUint16(resp, 0)
	}
	resp = binary.BigEndian.AppendUint16(resp, 0)
	resp = binary.BigEndian.AppendUint16(resp, 0)
	resp = append(resp, req[12:qEnd+4]...)
	if answer && (qtype == 1 || qtype == 255) {
		resp = append(resp, 0xc0, 0x0c)
		resp = binary.BigEndian.AppendUint16(resp, 1)
		resp = binary.BigEndian.AppendUint16(resp, 1)
		resp = binary.BigEndian.AppendUint32(resp, 30)
		resp = binary.BigEndian.AppendUint16(resp, 4)
		resp = append(resp, 127, 0, 0, 1)
	}
	return resp, nil
}

func parseQName(msg []byte, offset int) (string, int, error) {
	var labels []string
	for {
		if offset >= len(msg) {
			return "", 0, errors.New("qname overflow")
		}
		l := int(msg[offset])
		offset++
		if l == 0 {
			return strings.Join(labels, "."), offset, nil
		}
		if l&0xc0 != 0 {
			return "", 0, errors.New("compressed qname unsupported in question")
		}
		if offset+l > len(msg) {
			return "", 0, errors.New("qname label overflow")
		}
		labels = append(labels, string(msg[offset:offset+l]))
		offset += l
	}
}
