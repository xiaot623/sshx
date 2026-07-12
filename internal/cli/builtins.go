package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xiaot623/sshx/internal/bridge"
	"github.com/xiaot623/sshx/internal/locald"
	"github.com/xiaot623/sshx/internal/sshconfig"
)

const reservedLocalMessage = `"local" is reserved for the remote-to-local command bridge. Use it inside an sshx remote session.`

func (r *Runner) runForwardList(ctx context.Context) int {
	resp, err := locald.ClientRequest(ctx, defaultLocalDaemonSocketPath(), locald.Request{Type: locald.TypeListPorts})
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx forward: %v\n", err)
		return 1
	}
	if !resp.OK {
		fmt.Fprintf(r.Stderr, "sshx forward: %s\n", resp.Error)
		return 1
	}
	sort.Slice(resp.Forwards, func(i, j int) bool {
		if resp.Forwards[i].Target != resp.Forwards[j].Target {
			return resp.Forwards[i].Target < resp.Forwards[j].Target
		}
		if resp.Forwards[i].LocalPort != resp.Forwards[j].LocalPort {
			return resp.Forwards[i].LocalPort < resp.Forwards[j].LocalPort
		}
		return resp.Forwards[i].RemotePort < resp.Forwards[j].RemotePort
	})
	for _, fwd := range resp.Forwards {
		fmt.Fprintf(r.Stdout, "%s -> %s:%d\n", forwardAddress(fwd), fwd.Target, fwd.RemotePort)
	}
	return 0
}

func (r *Runner) runPS(ctx context.Context) int {
	aliases, err := sshconfig.Aliases(r.SSHConfigPath)
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx ps: ssh config: %v\n", err)
	}
	fmt.Fprintln(r.Stdout, "SSH config")
	for _, alias := range aliases {
		fmt.Fprintf(r.Stdout, "  %s\n", alias)
	}
	fmt.Fprintln(r.Stdout)
	fmt.Fprintln(r.Stdout, "Docker containers")
	containers, dockerErr := r.listDockerContainers(ctx)
	if dockerErr != nil {
		fmt.Fprintf(r.Stdout, "  unavailable: %v\n", dockerErr)
		return 0
	}
	for _, c := range containers {
		fmt.Fprintf(r.Stdout, "  %s\t%s\n", c.Name, shortDockerID(c.ID))
	}
	return 0
}

func forwardAddress(fwd locald.Forwarded) string {
	host := fwd.Domain
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d", host, fwd.LocalPort)
}

func (r *Runner) runServer(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(r.Stderr)
	socketPath := fs.String("socket", defaultSocketPath(), "Unix socket path")
	infoPath := fs.String("server-info", defaultServerInfoPath(), "server-info path")
	token := fs.String("token", "", "server authentication token; generated when empty")
	portScanInterval := fs.Duration("port-scan-interval", 2*time.Second, "localhost port scan interval; 0 disables scanning")
	startupTimeout := fs.Duration("startup-timeout", bridge.DefaultServerStartTimeout, "exit if no bridge client arrives within this duration")
	heartbeatTimeout := fs.Duration("heartbeat-timeout", bridge.DefaultHeartbeatTimeout, "disconnect bridge clients that stop renewing their lease")
	drainTimeout := fs.Duration("drain-timeout", bridge.DefaultServerDrainTimeout, "exit after the last bridge client disconnects")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *token == "" {
		generated, err := generateToken()
		if err != nil {
			fmt.Fprintf(r.Stderr, "sshx server: generate token: %v\n", err)
			return 1
		}
		*token = generated
	}
	if err := recordDefaultVersionState(clientVersion()); err != nil {
		fmt.Fprintf(r.Stderr, "sshx server: version state: %v\n", err)
		return 1
	}
	s := &bridge.Server{
		SocketPath:       *socketPath,
		InfoPath:         *infoPath,
		Token:            *token,
		Version:          clientVersion(),
		PortScanInterval: *portScanInterval,
		StartupTimeout:   *startupTimeout,
		HeartbeatTimeout: *heartbeatTimeout,
		DrainTimeout:     *drainTimeout,
	}
	if err := s.Serve(ctx); err != nil {
		fmt.Fprintf(r.Stderr, "sshx server: %v\n", err)
		return 1
	}
	return 0
}

func (r *Runner) runBridgeClient(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("bridge-client", flag.ContinueOnError)
	fs.SetOutput(r.Stderr)
	socketPath := fs.String("socket", os.Getenv("SSHX_BRIDGE_SOCKET"), "remote sshx server socket path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *socketPath == "" {
		fmt.Fprintln(r.Stderr, "sshx bridge-client: --socket or SSHX_BRIDGE_SOCKET is required")
		return 2
	}
	if err := bridge.RunClient(ctx, *socketPath); err != nil {
		fmt.Fprintf(r.Stderr, "sshx bridge-client: %v\n", err)
		return 1
	}
	return 0
}

func (r *Runner) runSocketProxy(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("socket-proxy", flag.ContinueOnError)
	fs.SetOutput(r.Stderr)
	socketPath := fs.String("socket", os.Getenv("SSHX_BRIDGE_SOCKET"), "remote sshx server socket path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *socketPath == "" {
		fmt.Fprintln(r.Stderr, "sshx socket-proxy: --socket or SSHX_BRIDGE_SOCKET is required")
		return 2
	}
	if err := bridge.SocketProxy(ctx, *socketPath, r.Stdin, r.Stdout); err != nil {
		fmt.Fprintf(r.Stderr, "sshx socket-proxy: %v\n", err)
		return 1
	}
	return 0
}

func (r *Runner) runLocalDaemon(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("local-daemon", flag.ContinueOnError)
	fs.SetOutput(r.Stderr)
	socketPath := fs.String("socket", defaultLocalDaemonSocketPath(), "local daemon Unix socket path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s := &locald.Server{SocketPath: *socketPath, Stderr: r.Stderr, Version: clientVersion()}
	if err := s.Serve(ctx); err != nil {
		fmt.Fprintf(r.Stderr, "sshx local-daemon: %v\n", err)
		return 1
	}
	return 0
}

func (r *Runner) runInstallResolver(args []string) int {
	fs := flag.NewFlagSet("install-resolver", flag.ContinueOnError)
	fs.SetOutput(r.Stderr)
	suffix := fs.String("suffix", defaultDomainSuffix(), "domain suffix")
	dnsAddr := fs.String("dns-addr", defaultDomainDNSAddr(), "DNS listener address")
	apply := fs.Bool("apply", false, "write /etc/resolver entry instead of printing it")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	host, port, err := splitHostPortDefault(*dnsAddr, "53")
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx install-resolver: %v\n", err)
		return 2
	}
	content := fmt.Sprintf("nameserver %s\nport %s\n", host, port)
	path := filepath.Join("/etc/resolver", strings.Trim(*suffix, "."))
	if !*apply {
		fmt.Fprintf(r.Stdout, "# %s\n%s", path, content)
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(r.Stderr, "sshx install-resolver: %v\n", err)
		return 1
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		fmt.Fprintf(r.Stderr, "sshx install-resolver: %v\n", err)
		return 1
	}
	return 0
}

func (r *Runner) runLocalBridge(ctx context.Context, argv []string, timeout time.Duration) int {
	socketPath := os.Getenv("SSHX_BRIDGE_SOCKET")
	token := os.Getenv("SSHX_BRIDGE_TOKEN")
	if socketPath == "" && (os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSHX_SERVER_HOME") != "") {
		if info, err := bridge.ReadServerInfo(defaultServerInfoPath()); err == nil {
			socketPath = info.Address
			token = info.Token
		}
	}
	if socketPath == "" {
		fmt.Fprintln(r.Stderr, reservedLocalMessage)
		return 2
	}
	if len(argv) == 0 {
		fmt.Fprintln(r.Stderr, "sshx local: command is required")
		return 2
	}
	stdin, err := readLocalBridgeStdin(r.Stdin)
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx local: read stdin: %v\n", err)
		return 1
	}
	if token == "" {
		if info, err := bridge.ReadServerInfo(defaultServerInfoPath()); err == nil {
			token = info.Token
		}
	}
	sessionID := os.Getenv("SSHX_SESSION_ID")
	remoteFS := os.Getenv("SSHX_REMOTE_FS") == "1"
	cwd := ""
	if remoteFS {
		cwd, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(r.Stderr, "sshx local: current directory: %v\n", err)
			return 1
		}
	}
	result, err := bridge.RequestCommandForSessionWithTimeout(ctx, socketPath, argv, stdin, nil, cwd, sessionID, remoteFS, timeout, token)
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx local: %v\n", err)
		return result.ExitCode
	}
	_, _ = r.Stdout.Write(result.Stdout)
	_, _ = r.Stderr.Write(result.Stderr)
	return result.ExitCode
}

func readLocalBridgeStdin(stdin io.Reader) ([]byte, error) {
	if f, ok := stdin.(*os.File); ok {
		info, err := f.Stat()
		if err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return nil, nil
		}
	}
	return io.ReadAll(stdin)
}
