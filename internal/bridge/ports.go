package bridge

import (
	"context"
	"sort"
	"time"

	"github.com/xiaot623/sshx/internal/ports"
	"github.com/xiaot623/sshx/internal/protocol"
)

func (s *Server) observePorts(ctx context.Context) {
	ticker := time.NewTicker(s.PortScanInterval)
	defer ticker.Stop()
	s.scanAndBroadcastPorts()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scanAndBroadcastPorts()
		}
	}
}

func (s *Server) scanAndBroadcastPorts() {
	listeners, err := ports.ScanLoopbackListeners()
	if err != nil {
		return
	}
	portList := make([]int, 0, len(listeners))
	hostByPort := make(map[int]string, len(listeners))
	for _, l := range listeners {
		portList = append(portList, l.Port)
		hostByPort[l.Port] = l.ConnectHost
	}
	observed, gone := s.applyPortScan(portList)
	for _, port := range observed {
		s.broadcast(protocol.Frame{Type: protocol.TypePortObserved, Host: hostByPort[port], Port: port})
	}
	for _, port := range gone {
		s.broadcast(protocol.Frame{Type: protocol.TypePortGone, Port: port})
	}
}

func (s *Server) applyPortScan(portList []int) ([]int, []int) {
	current := map[int]bool{}
	for _, port := range portList {
		if port > 0 {
			current[port] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.observedPorts == nil {
		s.observedPorts = map[int]bool{}
	}
	if s.portMisses == nil {
		s.portMisses = map[int]int{}
	}
	var observed []int
	for port := range current {
		if !s.observedPorts[port] {
			observed = append(observed, port)
		}
		s.observedPorts[port] = true
		delete(s.portMisses, port)
	}
	var gone []int
	for port := range s.observedPorts {
		if current[port] {
			continue
		}
		s.portMisses[port]++
		if s.portMisses[port] >= portGoneMissingScans {
			delete(s.observedPorts, port)
			delete(s.portMisses, port)
			gone = append(gone, port)
		}
	}
	sort.Ints(observed)
	sort.Ints(gone)
	return observed, gone
}

func (s *Server) currentPorts() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	ports := make([]int, 0, len(s.observedPorts))
	for port := range s.observedPorts {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

func (s *Server) sendCurrentPorts(client *clientConn) {
	hostByPort := map[int]string{}
	if listeners, err := ports.ScanLoopbackListeners(); err == nil {
		for _, l := range listeners {
			hostByPort[l.Port] = l.ConnectHost
		}
	}
	for _, port := range s.currentPorts() {
		host := hostByPort[port]
		if host == "" {
			host = "127.0.0.1"
		}
		_ = client.send(protocol.Frame{Type: protocol.TypePortObserved, Host: host, Port: port})
	}
}

func (s *Server) broadcast(frame protocol.Frame) {
	s.mu.Lock()
	clients := append([]*clientConn(nil), s.clients...)
	s.mu.Unlock()
	for _, client := range clients {
		_ = client.send(frame)
	}
}
