package ports

import (
	"net"
	"sort"
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

// listenerFromAddr applies sshx auto-forward policy to a TCP bind address.
// The caller is responsible for keeping only LISTEN sockets. Port must be in
// [1024, 65535], and the bind must be loopback or unspecified (wildcard).
// IPv6 loopback uses ConnectHost "::1"; IPv6 unspecified and all IPv4
// loopback/unspecified binds use "127.0.0.1".
func listenerFromAddr(ip net.IP, port int) (Listener, bool) {
	if port < minForwardablePort || port > 65535 || ip == nil {
		return Listener{}, false
	}
	if v4 := ip.To4(); v4 != nil {
		if v4.IsLoopback() || v4.IsUnspecified() {
			return Listener{Port: port, ConnectHost: "127.0.0.1"}, true
		}
		return Listener{}, false
	}
	if ip.IsLoopback() {
		return Listener{Port: port, ConnectHost: "::1"}, true
	}
	if ip.IsUnspecified() {
		return Listener{Port: port, ConnectHost: "127.0.0.1"}, true
	}
	return Listener{}, false
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
