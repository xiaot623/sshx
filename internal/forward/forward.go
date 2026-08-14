package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync"
	"time"
)

type Manager struct {
	ctx         context.Context
	cancel      context.CancelFunc
	transport   func() (string, []string, string, bool)
	stderr      io.Writer
	execControl func(context.Context, string, []string) ([]byte, error)

	mu       sync.Mutex
	byRemote map[int]*Forward
	stopped  bool
}

type Forward struct {
	RemotePort  int
	LocalPort   int
	ListenIP    string
	RemoteHost  string
	spec        string
	sshPath     string
	sshArgs     []string
	controlPath string
	listener    net.Listener
}

type Entry struct {
	RemotePort int
	LocalPort  int
	ListenIP   string
}

func NewManager(ctx context.Context, sshPath string, sshArgs []string, stderr io.Writer) *Manager {
	return NewDynamicManager(ctx, func() (string, []string, string, bool) {
		return sshPath, append([]string(nil), sshArgs...), "", sshPath != ""
	}, stderr)
}

func NewDynamicManager(ctx context.Context, transport func() (string, []string, string, bool), stderr io.Writer) *Manager {
	managerCtx, cancel := context.WithCancel(ctx)
	return &Manager{
		ctx:         managerCtx,
		cancel:      cancel,
		transport:   transport,
		stderr:      stderr,
		execControl: execControlOutput,
		byRemote:    map[int]*Forward{},
	}
}

func (m *Manager) Ensure(remotePort int, listenIP string, remoteHost string) (*Forward, error) {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil, errors.New("forward manager is stopped")
	}
	if f := m.byRemote[remotePort]; f != nil {
		m.mu.Unlock()
		return f, nil
	}
	m.mu.Unlock()

	if listenIP == "" {
		return nil, errors.New("listen IP is required")
	}
	remoteHost = NormalizeRemoteHost(remoteHost)
	sshPath, sshArgs, controlPath, ok := m.transport()

	var f *Forward
	if controlPath != "" {
		if !ok || sshPath == "" {
			return nil, errors.New("ssh control transport is unavailable")
		}
		spec := LocalForwardSpec(listenIP, remotePort, remoteHost)
		if err := m.controlOp(m.ctx, sshPath, sshArgs, controlPath, "forward", spec); err != nil {
			m.mu.Lock()
			existing := m.byRemote[remotePort]
			m.mu.Unlock()
			if existing != nil {
				return existing, nil
			}
			return nil, err
		}
		f = &Forward{
			RemotePort:  remotePort,
			LocalPort:   remotePort,
			ListenIP:    listenIP,
			RemoteHost:  remoteHost,
			spec:        spec,
			sshPath:     sshPath,
			sshArgs:     append([]string(nil), sshArgs...),
			controlPath: controlPath,
		}
	} else {
		ln, err := net.Listen("tcp", net.JoinHostPort(listenIP, fmt.Sprint(remotePort)))
		if err != nil {
			return nil, err
		}
		f = &Forward{RemotePort: remotePort, LocalPort: remotePort, ListenIP: listenIP, RemoteHost: remoteHost, listener: ln}
	}

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		m.drop(f)
		return nil, errors.New("forward manager is stopped")
	}
	if existing := m.byRemote[remotePort]; existing != nil {
		m.mu.Unlock()
		m.abandon(f)
		return existing, nil
	}
	m.byRemote[remotePort] = f
	m.mu.Unlock()
	return f, nil
}

func (m *Manager) List() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := make([]Entry, 0, len(m.byRemote))
	for _, f := range m.byRemote {
		entries = append(entries, Entry{RemotePort: f.RemotePort, LocalPort: f.LocalPort, ListenIP: f.ListenIP})
	}
	return entries
}

func (m *Manager) Remove(remotePort int) {
	m.mu.Lock()
	f := m.byRemote[remotePort]
	delete(m.byRemote, remotePort)
	m.mu.Unlock()
	if f != nil {
		m.drop(f)
	}
}

func (m *Manager) Stop() {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	forwards := make([]*Forward, 0, len(m.byRemote))
	for _, f := range m.byRemote {
		forwards = append(forwards, f)
	}
	m.byRemote = map[int]*Forward{}
	m.mu.Unlock()
	for _, f := range forwards {
		m.drop(f)
	}
	m.cancel()
}

func (m *Manager) drop(f *Forward) {
	m.abandon(f)
	if f == nil || f.controlPath == "" || f.spec == "" || f.sshPath == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = m.controlOp(ctx, f.sshPath, f.sshArgs, f.controlPath, "cancel", f.spec)
}

// abandon releases an in-process listener without cancelling an OpenSSH
// forward. Used when a racing Ensure lost the registration race: the winner
// already owns the same -L spec, and -O cancel would tear it down.
func (m *Manager) abandon(f *Forward) {
	if f == nil || f.listener == nil {
		return
	}
	_ = f.listener.Close()
	f.listener = nil
}

func (m *Manager) controlOp(ctx context.Context, sshPath string, sshArgs []string, controlPath, operation, spec string) error {
	args := ControlOperationArgs(sshArgs, controlPath, operation, "L", spec)
	execControl := m.execControl
	if execControl == nil {
		execControl = execControlOutput
	}
	output, err := execControl(ctx, sshPath, args)
	if err != nil {
		return controlForwardError(output, err)
	}
	return nil
}

func execControlOutput(ctx context.Context, sshPath string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, sshPath, args...)
	return cmd.CombinedOutput()
}
