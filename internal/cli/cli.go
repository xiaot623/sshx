package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/xiaot/sshx/internal/bridge"
	"github.com/xiaot/sshx/internal/config"
	"github.com/xiaot/sshx/internal/locald"
	"github.com/xiaot/sshx/internal/sshcompat"
)

const reservedLocalMessage = `"local" is reserved for the remote-to-local command bridge. Use it inside an sshx remote session.`

type Runner struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	SSHPath        string
	ConfigPath     string
	Exec           func(context.Context, string, []string) error
	StartBridge    func(context.Context, string, []string) (func(), error)
	EnsureResolver func(context.Context, config.DomainsFeature) error

	commandPolicy config.CommandPolicy
	commandBridge bool
	forwardPorts  bool
	domains       config.DomainsFeature
}

func NewRunner(stdin io.Reader, stdout io.Writer, stderr io.Writer) *Runner {
	configPath := config.DefaultPath()
	if override := os.Getenv("SSHX_CONFIG"); override != "" {
		configPath = override
	}
	r := &Runner{
		Stdin:      stdin,
		Stdout:     stdout,
		Stderr:     stderr,
		SSHPath:    "ssh",
		ConfigPath: configPath,
		Exec:       defaultExec,
	}
	r.StartBridge = r.defaultStartBridge
	r.EnsureResolver = r.defaultEnsureResolver
	return r
}

func (r *Runner) Run(ctx context.Context, args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "server":
			return r.runServer(ctx, args[1:])
		case "bridge-client":
			return r.runBridgeClient(ctx, args[1:])
		case "socket-proxy":
			return r.runSocketProxy(ctx, args[1:])
		case "local-daemon":
			return r.runLocalDaemon(ctx, args[1:])
		case "install-resolver":
			return r.runInstallResolver(args[1:])
		case "forward", "forwrad":
			return r.runForwardList(ctx)
		}
	}

	parsed := sshcompat.Parse(args)
	if parsed.Target == "local" {
		return r.runLocalBridge(ctx, parsed.RemoteCommand)
	}
	if parsed.NoWrap {
		return r.execSSH(ctx, parsed.Args)
	}
	if os.Getenv("SSHX_DISABLE") == "1" || parsed.InfoMode || parsed.Target == "" {
		return r.execSSH(ctx, parsed.Args)
	}

	cfg, err := config.Load(r.ConfigPath)
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx: config error: %v\n", err)
		return 2
	}
	features := cfg.Features
	if !features.Enabled() {
		return r.execSSH(ctx, parsed.Args)
	}
	sshArgs := baseSSHArgs(parsed)
	r.commandPolicy = cfg.Commands
	r.commandBridge = features.CommandBridge
	r.forwardPorts = features.Ports.Auto || features.Domains.Enabled
	r.domains = features.Domains
	if features.Domains.Enabled {
		if err := r.EnsureResolver(ctx, features.Domains); err != nil {
			if cfg.Strict {
				fmt.Fprintf(r.Stderr, "sshx: resolver setup unavailable for %s: %v\n", parsed.Target, err)
				return 1
			}
			fmt.Fprintf(r.Stderr, "sshx: resolver setup skipped: %v\n", err)
		}
	}
	if err := r.ensureRemoteServer(ctx, sshArgs, features); err != nil {
		if cfg.Strict {
			fmt.Fprintf(r.Stderr, "sshx: remote server unavailable for %s: %v\n", parsed.Target, err)
			return 1
		}
	} else if features.CommandBridge || r.forwardPorts {
		stopBridge, err := r.StartBridge(ctx, parsed.Target, sshArgs)
		if err != nil {
			if cfg.Strict {
				fmt.Fprintf(r.Stderr, "sshx: command bridge unavailable for %s: %v\n", parsed.Target, err)
				return 1
			}
		} else {
			defer stopBridge()
		}
	}
	return r.execSSH(ctx, parsed.Args)
}

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
		fmt.Fprintf(r.Stdout, "%d -> %s:%d\n", fwd.LocalPort, fwd.Target, fwd.RemotePort)
	}
	return 0
}

func (r *Runner) runServer(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(r.Stderr)
	socketPath := fs.String("socket", defaultSocketPath(), "Unix socket path")
	infoPath := fs.String("server-info", defaultServerInfoPath(), "server-info path")
	token := fs.String("token", "", "server authentication token; generated when empty")
	portScanInterval := fs.Duration("port-scan-interval", 2*time.Second, "localhost port scan interval; 0 disables scanning")
	idleTimeout := fs.Duration("idle-timeout", 10*time.Minute, "exit after this long without bridge clients; 0 disables idle exit")
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
	s := &bridge.Server{SocketPath: *socketPath, InfoPath: *infoPath, Token: *token, PortScanInterval: *portScanInterval, IdleTimeout: *idleTimeout}
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
	s := &locald.Server{SocketPath: *socketPath, Stderr: r.Stderr}
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

func (r *Runner) runLocalBridge(ctx context.Context, argv []string) int {
	socketPath := os.Getenv("SSHX_BRIDGE_SOCKET")
	token := os.Getenv("SSHX_BRIDGE_TOKEN")
	if socketPath == "" && os.Getenv("SSH_CONNECTION") != "" {
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
	result, err := bridge.RequestCommand(ctx, socketPath, argv, stdin, nil, "", token)
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx local: %v\n", err)
		return 1
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

func (r *Runner) ensureRemoteServer(ctx context.Context, sshArgs []string, features config.Features) error {
	if !features.Enabled() {
		return nil
	}
	check := "test -S ~/.sshx/sock && test -f ~/.sshx/server-info"
	if err := r.Exec(ctx, r.SSHPath, internalSSHArgs(sshArgs, remoteShell(check))); err == nil {
		return nil
	}
	start := "mkdir -p ~/.sshx/bin ~/.sshx/run && test -x ~/.sshx/bin/sshx && nohup ~/.sshx/bin/sshx server --socket ~/.sshx/sock --server-info ~/.sshx/server-info >/tmp/sshx-server.log 2>&1 &"
	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := r.Exec(startCtx, r.SSHPath, internalSSHArgs(sshArgs, remoteShell(start))); err != nil {
		return err
	}
	verify := "i=0; while [ $i -lt 20 ]; do test -S ~/.sshx/sock && test -f ~/.sshx/server-info && exit 0; i=$((i+1)); sleep 0.1; done; exit 1"
	return r.Exec(startCtx, r.SSHPath, internalSSHArgs(sshArgs, remoteShell(verify)))
}

func (r *Runner) execSSH(ctx context.Context, args []string) int {
	if err := r.Exec(ctx, r.SSHPath, args); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(r.Stderr, "sshx: exec ssh: %v\n", err)
		return 1
	}
	return 0
}

func defaultExec(ctx context.Context, name string, args []string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (r *Runner) defaultStartBridge(ctx context.Context, target string, sshArgs []string) (func(), error) {
	token, err := r.fetchRemoteToken(ctx, sshArgs)
	if err != nil {
		return nil, err
	}
	localDaemonSocket := defaultLocalDaemonSocketPath()
	if r.forwardPorts {
		if err := r.ensureLocalDaemon(ctx, localDaemonSocket); err != nil {
			return nil, err
		}
	}
	bridgeCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(
		bridgeCtx,
		r.SSHPath,
		sshCommandArgs(sshArgs, remoteShell("exec ~/.sshx/bin/sshx socket-proxy --socket ~/.sshx/sock"))...,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	cmd.Stderr = r.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	conn := bridge.NewReadWriteCloser(stdout, stdin, func() error {
		_ = stdin.Close()
		cancel()
		return nil
	})
	readyCh := make(chan error, 1)
	errCh := make(chan error, 1)
	go func() {
		opts := bridge.ClientOptions{
			Ready: readyCh,
			Allow: func(argv []string) bool {
				return r.commandBridge && r.commandPolicy.Allows(argv)
			},
		}
		if r.forwardPorts {
			opts.OnPortObserved = func(port int) {
				_, err := locald.ClientRequest(bridgeCtx, localDaemonSocket, locald.Request{
					Type:           locald.TypeEnsurePort,
					SSHPath:        r.SSHPath,
					Target:         target,
					SSHArgs:        append([]string(nil), sshArgs...),
					RemotePort:     port,
					DomainsEnabled: r.domains.Enabled,
					DomainSuffix:   domainSuffix(r.domains),
					DNSAddr:        domainDNSAddr(r.domains),
				})
				if err != nil {
					fmt.Fprintf(r.Stderr, "sshx: forward remote port %d: %v\n", port, err)
				}
			}
		}
		errCh <- bridge.RunClientConnWithOptions(bridgeCtx, conn, opts, token)
	}()
	select {
	case err := <-readyCh:
		if err != nil {
			cancel()
			return nil, err
		}
	case err := <-errCh:
		cancel()
		return nil, err
	case <-time.After(2 * time.Second):
		cancel()
		return nil, errors.New("timed out waiting for command bridge handshake")
	}
	stop := func() {
		cancel()
		_ = stdin.Close()
		select {
		case <-errCh:
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
		}
		select {
		case <-waitCh:
		case <-time.After(time.Second):
		}
	}
	return stop, nil
}

func (r *Runner) fetchRemoteToken(ctx context.Context, sshArgs []string) (string, error) {
	cmd := exec.CommandContext(ctx, r.SSHPath, internalSSHArgs(sshArgs, remoteShell("cat ~/.sshx/server-info"))...)
	b, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var info bridge.ServerInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return "", err
	}
	return info.Token, nil
}

func (r *Runner) ensureLocalDaemon(ctx context.Context, socketPath string) error {
	if _, err := locald.ClientRequest(ctx, socketPath, locald.Request{Type: locald.TypePing}); err == nil {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return err
	}
	logPath := filepath.Join(filepath.Dir(socketPath), "local-daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(context.Background(), exe, "local-daemon", "--socket", socketPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if _, err := locald.ClientRequest(ctx, socketPath, locald.Request{Type: locald.TypePing}); err == nil {
			_ = logFile.Close()
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = logFile.Close()
	if lastErr != nil {
		return lastErr
	}
	return errors.New("local daemon did not start")
}

func (r *Runner) defaultEnsureResolver(ctx context.Context, cfg config.DomainsFeature) error {
	if !cfg.Enabled || runtime.GOOS != "darwin" {
		return nil
	}
	suffix := strings.Trim(domainSuffix(cfg), ".")
	if suffix == "" {
		return errors.New("domain suffix is required")
	}
	content, err := resolverContent(domainDNSAddr(cfg))
	if err != nil {
		return err
	}
	path := filepath.Join("/etc/resolver", suffix)
	current, err := os.ReadFile(path)
	if err == nil && string(current) == content {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := writeResolverFile(path, content); err == nil {
		return nil
	}
	return sudoWriteResolverFile(ctx, path, content, r.Stderr)
}

func resolverContent(dnsAddr string) (string, error) {
	host, port, err := splitHostPortDefault(dnsAddr, "53")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("nameserver %s\nport %s\n", host, port), nil
}

func writeResolverFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func sudoWriteResolverFile(ctx context.Context, path, content string, stderr io.Writer) error {
	script := "mkdir -p " + shellQuote(filepath.Dir(path)) +
		" && printf %s " + shellQuote(content) +
		" > " + shellQuote(path)
	cmd := exec.CommandContext(ctx, "sudo", "sh", "-c", script)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func baseSSHArgs(parsed sshcompat.Parsed) []string {
	if parsed.TargetIndex < 0 || parsed.TargetIndex >= len(parsed.Args) {
		return nil
	}
	return append([]string(nil), parsed.Args[:parsed.TargetIndex+1]...)
}

func internalSSHArgs(sshArgs []string, remoteCommand string) []string {
	args := make([]string, 0, len(sshArgs)+2)
	args = append(args, "-n")
	args = append(args, sshCommandArgs(sshArgs, remoteCommand)...)
	return args
}

func sshCommandArgs(sshArgs []string, remoteCommand string) []string {
	args := make([]string, 0, len(sshArgs)+1)
	args = append(args, sshArgs...)
	args = append(args, remoteCommand)
	return args
}

func remoteShell(script string) string {
	return "sh -lc " + shellQuote(script)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func generateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func domainSuffix(cfg config.DomainsFeature) string {
	if cfg.Suffix != "" {
		return cfg.Suffix
	}
	return defaultDomainSuffix()
}

func domainDNSAddr(cfg config.DomainsFeature) string {
	if cfg.DNSAddr != "" {
		return cfg.DNSAddr
	}
	if v := os.Getenv("SSHX_DOMAIN_DNS_ADDR"); v != "" {
		return v
	}
	return defaultDomainDNSAddr()
}

func defaultDomainDNSAddr() string {
	return "127.0.0.1:53535"
}

func defaultDomainSuffix() string {
	user := os.Getenv("USER")
	if user == "" {
		user = "user"
	}
	return user + ".sshx"
}

func splitHostPortDefault(addr, defaultPort string) (string, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err == nil {
		return host, port, nil
	}
	if strings.Count(addr, ":") == 0 {
		return addr, defaultPort, nil
	}
	return "", "", err
}

func defaultSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "sshx.sock")
	}
	return filepath.Join(home, ".sshx", "sock")
}

func defaultLocalDaemonSocketPath() string {
	if override := os.Getenv("SSHX_LOCAL_DAEMON_SOCKET"); override != "" {
		return override
	}
	return locald.DefaultSocketPath()
}

func defaultServerInfoPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "sshx-server-info")
	}
	return filepath.Join(home, ".sshx", "server-info")
}

func IsReservedLocalError(s string) bool {
	return strings.Contains(s, reservedLocalMessage)
}
