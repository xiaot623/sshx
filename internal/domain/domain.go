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

	mu        sync.RWMutex
	records   map[string]net.IP
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

func (m *Manager) Register(name string, ip net.IP) error {
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("domain record %s requires an IPv4 address", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.records == nil {
		m.records = map[string]net.IP{}
	}
	key := normalizeSuffix(name)
	if _, exists := m.records[key]; exists {
		return fmt.Errorf("domain record %s is already registered", name)
	}
	m.records[key] = append(net.IP(nil), ip4...)
	return nil
}

func (m *Manager) RegisterTarget(target string, ip net.IP) (string, error) {
	ip4 := ip.To4()
	if ip4 == nil {
		return "", fmt.Errorf("target %s requires an IPv4 address", target)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.records == nil {
		m.records = map[string]net.IP{}
	}

	prefix := TargetPrefix(target)
	for suffix := 0; ; suffix++ {
		candidatePrefix := prefix
		if suffix > 0 {
			candidatePrefix = fmt.Sprintf("%s-%d", prefix, suffix)
		}
		name := fmt.Sprintf("%s.%s", candidatePrefix, m.suffix)
		key := normalizeSuffix(name)
		if _, exists := m.records[key]; exists {
			continue
		}
		m.records[key] = append(net.IP(nil), ip4...)
		return name, nil
	}
}

func (m *Manager) Unregister(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.records, normalizeSuffix(name))
}

func (m *Manager) lookup(name string) (net.IP, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ip, ok := m.records[normalizeSuffix(name)]
	return append(net.IP(nil), ip...), ok
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
		name, qEnd, err := parseQName(buf[:n], 12)
		if err != nil {
			continue
		}
		if qEnd+4 > n {
			continue
		}
		qtype := binary.BigEndian.Uint16(buf[qEnd : qEnd+2])
		ip, found := m.lookup(name)
		resp := buildDNSResponse(buf[:n], qEnd, qtype, ip, found)
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

func TargetPrefix(target string) string {
	host := strings.ToLower(strings.Trim(strings.TrimSpace(target), "."))
	var b strings.Builder
	prevDash := false
	for _, r := range host {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
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
	prefix := strings.Trim(b.String(), "-")
	if prefix == "" {
		return "host"
	}
	return prefix
}

func buildDNSResponse(req []byte, qEnd int, qtype uint16, ip net.IP, found bool) []byte {
	ip4 := ip.To4()
	answer := found && ip4 != nil
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
		resp = append(resp, ip4...)
	}
	return resp
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
