package ports

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"net"
	"sort"
	"strconv"
	"strings"
)

// Listener is a loopback or wildcard TCP listen entry that sshx can auto-forward.
type Listener struct {
	Port        int
	ConnectHost string // "127.0.0.1" or "::1"
}

// minForwardablePort is the lowest port sshx will auto-forward. Ports below
// 1024 are privileged: unprivileged users cannot bind them locally, and
// forwarding them (e.g. sshd on :22) is rarely useful — the session is
// already established over SSH. Manual `sshx forward add` is unaffected since
// it bypasses the scanner.
const minForwardablePort = 1024

// connectHost returns the address sshx should dial on the remote when
// forwarding this listen entry: "127.0.0.1" for IPv4 loopback/wildcard and
// IPv6 unspecified; "::1" for IPv6 loopback. Empty if not forwardable.
func connectHost(hexAddr string) string {
	ip, ok := parseProcNetIP(hexAddr)
	if !ok {
		return ""
	}
	if len(ip) == net.IPv6len {
		if ip.IsLoopback() {
			return "::1"
		}
		if ip.IsUnspecified() {
			return "127.0.0.1"
		}
		return ""
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return "127.0.0.1"
	}
	return ""
}

// parseProcNetIP decodes a /proc/net/{tcp,tcp6} local_address hex field.
// The kernel prints each 32-bit word with %08X of a native-endian uint32, so
// each 4-byte group is converted from host endian to network order before
// being treated as net.IP.
func parseProcNetIP(hexAddr string) (net.IP, bool) {
	b, err := hex.DecodeString(hexAddr)
	if err != nil {
		return nil, false
	}
	want := len(b)
	if want != net.IPv4len && want != net.IPv6len {
		return nil, false
	}
	ip := make(net.IP, want)
	for i := 0; i < want; i += 4 {
		word := binary.NativeEndian.Uint32(b[i : i+4])
		binary.BigEndian.PutUint32(ip[i:i+4], word)
	}
	return ip, true
}

func parseProcNetTCPListeners(data string) ([]Listener, error) {
	byPort := map[int]Listener{}
	scanner := bufio.NewScanner(strings.NewReader(data))
	first := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			if strings.HasPrefix(line, "sl") {
				continue
			}
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if fields[3] != "0A" {
			continue
		}
		hexAddr, port, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		host := connectHost(hexAddr)
		if host == "" {
			continue
		}
		p, err := strconv.ParseInt(port, 16, 32)
		if err != nil || p < minForwardablePort || p > 65535 {
			continue
		}
		byPort[int(p)] = Listener{Port: int(p), ConnectHost: host}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return listenersFromMap(byPort), nil
}

func mergeListeners(groups ...[]Listener) []Listener {
	byPort := map[int]Listener{}
	for _, group := range groups {
		for _, l := range group {
			existing, ok := byPort[l.Port]
			if !ok {
				byPort[l.Port] = l
				continue
			}
			if existing.ConnectHost == "::1" && l.ConnectHost == "127.0.0.1" {
				byPort[l.Port] = l
			}
		}
	}
	return listenersFromMap(byPort)
}

func listenersFromMap(byPort map[int]Listener) []Listener {
	out := make([]Listener, 0, len(byPort))
	for _, l := range byPort {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}
