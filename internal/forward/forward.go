package forward

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
)

type Manager struct {
	ctx     context.Context
	sshPath string
	sshArgs []string
	stderr  io.Writer

	mu       sync.Mutex
	byRemote map[int]*Forward
}

type Forward struct {
	RemotePort int
	LocalPort  int
	listener   net.Listener
}

type Entry struct {
	RemotePort int
	LocalPort  int
}

func NewManager(ctx context.Context, sshPath string, sshArgs []string, stderr io.Writer) *Manager {
	return &Manager{
		ctx:      ctx,
		sshPath:  sshPath,
		sshArgs:  append([]string(nil), sshArgs...),
		stderr:   stderr,
		byRemote: map[int]*Forward{},
	}
}

func (m *Manager) Ensure(remotePort int, preferRemoteLocalPort bool) (*Forward, error) {
	m.mu.Lock()
	if f := m.byRemote[remotePort]; f != nil {
		m.mu.Unlock()
		return f, nil
	}
	m.mu.Unlock()

	ln, err := listenPreferredPort(remotePort, preferRemoteLocalPort)
	if err != nil {
		return nil, err
	}
	localPort := ln.Addr().(*net.TCPAddr).Port
	f := &Forward{RemotePort: remotePort, LocalPort: localPort, listener: ln}

	m.mu.Lock()
	if existing := m.byRemote[remotePort]; existing != nil {
		m.mu.Unlock()
		_ = ln.Close()
		return existing, nil
	}
	m.byRemote[remotePort] = f
	m.mu.Unlock()

	go m.acceptLoop(f)
	return f, nil
}

func listenPreferredPort(remotePort int, preferRemoteLocalPort bool) (net.Listener, error) {
	if preferRemoteLocalPort {
		var lastErr error
		for localPort := remotePort; localPort <= 65535; localPort++ {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
			if err == nil {
				return ln, nil
			}
			lastErr = err
		}
		return nil, fmt.Errorf("no local port available at or above %d: %w", remotePort, lastErr)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", remotePort))
	if err == nil {
		return ln, nil
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

func (m *Manager) List() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := make([]Entry, 0, len(m.byRemote))
	for _, f := range m.byRemote {
		entries = append(entries, Entry{RemotePort: f.RemotePort, LocalPort: f.LocalPort})
	}
	return entries
}

func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.byRemote {
		_ = f.listener.Close()
	}
	m.byRemote = map[int]*Forward{}
}

func (m *Manager) acceptLoop(f *Forward) {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go m.handleConn(conn, f.RemotePort)
	}
}

func (m *Manager) handleConn(conn net.Conn, remotePort int) {
	defer conn.Close()
	args := make([]string, 0, len(m.sshArgs)+2)
	args = append(args, "-W", fmt.Sprintf("127.0.0.1:%d", remotePort))
	args = append(args, m.sshArgs...)
	cmd := exec.CommandContext(m.ctx, m.sshPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return
	}
	cmd.Stderr = m.stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(stdin, conn)
		_ = stdin.Close()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, stdout)
		done <- struct{}{}
	}()
	<-done
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}
