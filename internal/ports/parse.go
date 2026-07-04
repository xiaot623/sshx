package ports

import (
	"bufio"
	"encoding/hex"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
)

var ErrUnsupported = errors.New("port scanning is only supported on Linux servers")

func parseProcNetTCP(data string, ipv6 bool) ([]int, error) {
	seen := map[int]bool{}
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
		host, port, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		if !isLoopbackProcAddress(host, ipv6) {
			continue
		}
		p, err := strconv.ParseInt(port, 16, 32)
		if err != nil || p <= 0 || p > 65535 {
			continue
		}
		seen[int(p)] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	out := make([]int, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Ints(out)
	return out, nil
}

func isLoopbackProcAddress(hexAddr string, ipv6 bool) bool {
	if ipv6 {
		b, err := hex.DecodeString(hexAddr)
		if err != nil || len(b) != net.IPv6len {
			return false
		}
		return net.IP(b).Equal(net.IPv6loopback)
	}
	if len(hexAddr) != 8 {
		return false
	}
	return strings.EqualFold(hexAddr, "0100007F")
}

func mergePorts(groups ...[]int) []int {
	seen := map[int]bool{}
	for _, group := range groups {
		for _, port := range group {
			seen[port] = true
		}
	}
	out := make([]int, 0, len(seen))
	for port := range seen {
		out = append(out, port)
	}
	sort.Ints(out)
	return out
}
