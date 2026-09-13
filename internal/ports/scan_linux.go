//go:build linux

package ports

import (
	"net"

	psnet "github.com/shirou/gopsutil/v4/net"
)

func ScanLoopbackListeners() ([]Listener, error) {
	// "tcp" returns both IPv4 and IPv6 TCP sockets, including LISTEN.
	conns, err := psnet.Connections("tcp")
	if err != nil {
		return nil, err
	}
	var found []Listener
	for _, c := range conns {
		if c.Status != "LISTEN" {
			continue
		}
		l, ok := listenerFromAddr(net.ParseIP(c.Laddr.IP), int(c.Laddr.Port))
		if !ok {
			continue
		}
		found = append(found, l)
	}
	return mergeListeners(found), nil
}
