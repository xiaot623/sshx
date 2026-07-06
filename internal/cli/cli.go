package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/xiaot623/sshx/internal/bridge"
	"github.com/xiaot623/sshx/internal/config"
	"github.com/xiaot623/sshx/internal/locald"
	"github.com/xiaot623/sshx/internal/sshcompat"
	"github.com/xiaot623/sshx/internal/sshconfig"
	"github.com/xiaot623/sshx/internal/version"
)

const reservedLocalMessage = `"local" is reserved for the remote-to-local command bridge. Use it inside an sshx remote session.`

type Runner struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	SSHPath         string
	DockerPath      string
	SSHConfigPath   string
	ConfigPath      string
	RemoteHostsPath string
	Exec            func(context.Context, string, []string) error
	ExecInput       func(context.Context, string, []string, io.Reader) error
	ExecOutput      func(context.Context, string, []string) ([]byte, error)
	DownloadBinary  func(context.Context, string, string) (string, error)
	StartBridge     func(context.Context, string, []string, string) (func(), error)
	EnsureResolver  func(context.Context, config.DomainsFeature) error

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
		Stdin:           stdin,
		Stdout:          stdout,
		Stderr:          stderr,
		SSHPath:         "ssh",
		DockerPath:      "docker",
		SSHConfigPath:   sshconfig.DefaultPath(),
		ConfigPath:      configPath,
		RemoteHostsPath: defaultRemoteHostsPath(),
		Exec:            defaultExec,
		ExecInput:       defaultExecInput,
		ExecOutput:      defaultExecOutput,
	}
	r.DownloadBinary = defaultDownloadBinary
	r.StartBridge = r.defaultStartBridge
	r.EnsureResolver = r.defaultEnsureResolver
	return r
}

func (r *Runner) Run(ctx context.Context, args []string) int {
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintf(r.Stdout, "sshx %s\n", clientVersion())
		return 0
	}
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
		case "ps":
			return r.runPS(ctx)
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

	if err := config.EnsureDefault(r.ConfigPath); err != nil {
		fmt.Fprintf(r.Stderr, "sshx: config error: %v\n", err)
		return 2
	}
	cfg, err := config.Load(r.ConfigPath)
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx: config error: %v\n", err)
		return 2
	}
	features := cfg.Features
	dockerTarget, dockerMatched, err := r.resolveDockerTarget(ctx, parsed)
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx: docker target: %v\n", err)
		return 1
	}
	if dockerMatched {
		return r.runDocker(ctx, parsed, dockerTarget, cfg)
	}
	if !features.Enabled() {
		return r.execSSH(ctx, parsed.Args)
	}
	if err := recordDefaultVersionState(clientVersion()); err != nil {
		if cfg.Strict {
			fmt.Fprintf(r.Stderr, "sshx: version state unavailable: %v\n", err)
			return 1
		}
		fmt.Fprintf(r.Stderr, "sshx: version state skipped: %v\n", err)
	}
	sshArgs := baseSSHArgs(parsed)
	remoteID, err := remoteIDForTarget(r.RemoteHostsPath, parsed.Target)
	if err != nil {
		if cfg.Strict {
			fmt.Fprintf(r.Stderr, "sshx: remote state unavailable for %s: %v\n", parsed.Target, err)
			return 1
		}
		fmt.Fprintf(r.Stderr, "sshx: remote state skipped for %s: %v\n", parsed.Target, err)
		return r.execSSH(ctx, parsed.Args)
	}
	remoteHome := remoteServerHome(remoteID)
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
	remoteReady := false
	if err := r.ensureRemoteServer(ctx, sshArgs, features, remoteHome); err != nil {
		if cfg.Strict {
			fmt.Fprintf(r.Stderr, "sshx: remote server unavailable for %s: %v\n", parsed.Target, err)
			return 1
		}
	} else if features.CommandBridge || r.forwardPorts {
		stopBridge, err := r.StartBridge(ctx, parsed.Target, sshArgs, remoteHome)
		if err != nil {
			if cfg.Strict {
				fmt.Fprintf(r.Stderr, "sshx: command bridge unavailable for %s: %v\n", parsed.Target, err)
				return 1
			}
		} else {
			remoteReady = true
			defer stopBridge()
		}
	} else {
		remoteReady = true
	}
	if remoteReady {
		return r.execSSH(ctx, sessionSSHArgs(parsed, remoteHome))
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
		fmt.Fprintf(r.Stderr, "sshx ps: docker: %v\n", dockerErr)
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
	if err := recordDefaultVersionState(clientVersion()); err != nil {
		fmt.Fprintf(r.Stderr, "sshx server: version state: %v\n", err)
		return 1
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

func (r *Runner) ensureRemoteServer(ctx context.Context, sshArgs []string, features config.Features, remoteHome string) error {
	if !features.Enabled() {
		return nil
	}
	targetVersion := clientVersion()
	probe, err := r.probeRemote(ctx, sshArgs, remoteHome)
	if err != nil {
		return err
	}
	if probe.Running && probe.ServerVersion == targetVersion {
		return nil
	}
	if probe.BinaryVersion != targetVersion {
		localBinary, err := r.DownloadBinary(ctx, targetVersion, probe.AssetName())
		if err != nil {
			return err
		}
		if err := r.installRemoteBinary(ctx, sshArgs, localBinary, remoteHome); err != nil {
			return err
		}
	}
	start := strings.Join([]string{
		remoteServerEnvScript(remoteHome),
		"mkdir -p \"$SSHX_SERVER_HOME\"",
		"rm -f \"$SSHX_SERVER_HOME/sock\" \"$SSHX_SERVER_HOME/server-info\"",
		"nohup \"$SSHX_SERVER_HOME/sshx\" server --socket \"$SSHX_SERVER_HOME/sock\" --server-info \"$SSHX_SERVER_HOME/server-info\" >\"$SSHX_SERVER_HOME/server.log\" 2>&1 &",
	}, "; ")
	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := r.Exec(startCtx, r.SSHPath, internalSSHArgs(sshArgs, remoteShell(start))); err != nil {
		return err
	}
	verify := strings.Join([]string{
		remoteServerEnvScript(remoteHome),
		"i=0",
		"while [ $i -lt 20 ]; do test -S \"$SSHX_SERVER_HOME/sock\" && test -f \"$SSHX_SERVER_HOME/server-info\" && exit 0; i=$((i+1)); sleep 0.1; done",
		"exit 1",
	}, "; ")
	return r.Exec(startCtx, r.SSHPath, internalSSHArgs(sshArgs, remoteShell(verify)))
}

type remoteProbe struct {
	OS            string
	Arch          string
	BinaryVersion string
	ServerVersion string
	Running       bool
}

func (p remoteProbe) AssetName() string {
	return "sshx-" + p.OS + "-" + p.Arch
}

func (r *Runner) probeRemote(ctx context.Context, sshArgs []string, remoteHome string) (remoteProbe, error) {
	script := strings.Join([]string{
		remoteServerEnvScript(remoteHome),
		"os=$(uname -s 2>/dev/null || true)",
		"arch=$(uname -m 2>/dev/null || true)",
		"ver=",
		"if test -x \"$SSHX_SERVER_HOME/sshx\"; then ver=$(\"$SSHX_SERVER_HOME/sshx\" --version 2>/dev/null | awk '{print $2}' || true); fi",
		"server_ver=",
		"if test -f \"$SSHX_SERVER_HOME/server-info\"; then server_ver=$(sed -n 's/.*\"version\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p' \"$SSHX_SERVER_HOME/server-info\" | head -n 1 || true); fi",
		"running=0",
		"if test -S \"$SSHX_SERVER_HOME/sock\" && test -f \"$SSHX_SERVER_HOME/server-info\"; then running=1; fi",
		"printf '%s\\n%s\\n%s\\n%s\\n%s\\n' \"$os\" \"$arch\" \"$ver\" \"$server_ver\" \"$running\"",
	}, "; ")
	out, err := r.ExecOutput(ctx, r.SSHPath, internalSSHArgs(sshArgs, remoteShell(script)))
	if err != nil {
		return remoteProbe{}, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	lines := make([]string, 0, 5)
	for scanner.Scan() {
		lines = append(lines, strings.TrimSpace(scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		return remoteProbe{}, err
	}
	for len(lines) < 5 {
		lines = append(lines, "")
	}
	osName, err := normalizeRemoteOS(lines[0])
	if err != nil {
		return remoteProbe{}, err
	}
	arch, err := normalizeRemoteArch(lines[1])
	if err != nil {
		return remoteProbe{}, err
	}
	return remoteProbe{
		OS:            osName,
		Arch:          arch,
		BinaryVersion: lines[2],
		ServerVersion: lines[3],
		Running:       lines[4] == "1",
	}, nil
}

func (r *Runner) installRemoteBinary(ctx context.Context, sshArgs []string, localPath string, remoteHome string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	script := strings.Join([]string{
		"set -eu",
		remoteServerEnvScript(remoteHome),
		"mkdir -p \"$SSHX_SERVER_HOME\"",
		"tmp=\"$SSHX_SERVER_HOME/sshx.$$.tmp\"",
		"cat > \"$tmp\"",
		"chmod 755 \"$tmp\"",
		"mv \"$tmp\" \"$SSHX_SERVER_HOME/sshx\"",
	}, "; ")
	return r.ExecInput(ctx, r.SSHPath, sshCommandArgs(sshArgs, remoteShell(script)), f)
}

type dockerContainer struct {
	ID   string
	Name string
}

type dockerTarget struct {
	dockerContainer
	Ref string
}

func (r *Runner) resolveDockerTarget(ctx context.Context, parsed sshcompat.Parsed) (dockerTarget, bool, error) {
	if parsed.Target == "" || explicitSSHTarget(parsed) {
		return dockerTarget{}, false, nil
	}
	hasAlias, err := sshconfig.HasAlias(r.SSHConfigPath, parsed.Target)
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx: ssh config skipped for docker resolution: %v\n", err)
		return dockerTarget{}, false, nil
	}
	if hasAlias {
		return dockerTarget{}, false, nil
	}
	containers, err := r.listDockerContainers(ctx)
	if err != nil {
		return dockerTarget{}, false, nil
	}
	target, ok, err := matchDockerTarget(parsed.Target, containers)
	if err != nil || !ok {
		return dockerTarget{}, ok, err
	}
	return target, true, nil
}

func explicitSSHTarget(parsed sshcompat.Parsed) bool {
	if parsed.TargetIndex > 0 {
		return true
	}
	target := parsed.Target
	host := config.NormalizeTargetHost(target)
	if strings.Contains(target, "@") || strings.HasPrefix(target, "[") || strings.Contains(target, ":") || strings.Contains(host, ".") {
		return true
	}
	if net.ParseIP(host) != nil {
		return true
	}
	return false
}

func (r *Runner) listDockerContainers(ctx context.Context) ([]dockerContainer, error) {
	out, err := r.ExecOutput(ctx, r.DockerPath, []string{"ps", "--no-trunc", "--format", "{{.ID}}\t{{.Names}}"})
	if err != nil {
		return nil, err
	}
	return parseDockerContainers(out), nil
}

func parseDockerContainers(out []byte) []dockerContainer {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	var containers []dockerContainer
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		id := strings.TrimSpace(parts[0])
		name := strings.TrimSpace(parts[1])
		if id == "" || name == "" {
			continue
		}
		containers = append(containers, dockerContainer{ID: id, Name: name})
	}
	return containers
}

func matchDockerTarget(target string, containers []dockerContainer) (dockerTarget, bool, error) {
	var idMatches []dockerContainer
	for _, c := range containers {
		if strings.HasPrefix(c.ID, target) {
			idMatches = append(idMatches, c)
		}
	}
	if len(idMatches) > 1 {
		names := make([]string, 0, len(idMatches))
		for _, c := range idMatches {
			names = append(names, c.Name+"("+shortDockerID(c.ID)+")")
		}
		sort.Strings(names)
		return dockerTarget{}, true, fmt.Errorf("ambiguous container id prefix %q matches %s", target, strings.Join(names, ", "))
	}
	if len(idMatches) == 1 {
		c := idMatches[0]
		return dockerTarget{dockerContainer: c, Ref: c.ID}, true, nil
	}
	for _, c := range containers {
		if c.Name == target {
			return dockerTarget{dockerContainer: c, Ref: c.ID}, true, nil
		}
	}
	return dockerTarget{}, false, nil
}

func shortDockerID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}

func (r *Runner) runDocker(ctx context.Context, parsed sshcompat.Parsed, target dockerTarget, cfg config.Config) int {
	features := cfg.Features
	remoteHome := ""
	remoteReady := false
	if features.CommandBridge {
		if err := recordDefaultVersionState(clientVersion()); err != nil {
			if cfg.Strict {
				fmt.Fprintf(r.Stderr, "sshx: version state unavailable: %v\n", err)
				return 1
			}
			fmt.Fprintf(r.Stderr, "sshx: version state skipped: %v\n", err)
		}
		remoteID, err := remoteIDForTarget(r.RemoteHostsPath, "docker:"+target.ID)
		if err != nil {
			if cfg.Strict {
				fmt.Fprintf(r.Stderr, "sshx: docker state unavailable for %s: %v\n", target.Name, err)
				return 1
			}
			fmt.Fprintf(r.Stderr, "sshx: docker state skipped for %s: %v\n", target.Name, err)
		} else {
			remoteHome = remoteServerHome(remoteID)
			r.commandPolicy = cfg.Commands
			r.commandBridge = features.CommandBridge
			if err := r.ensureDockerServer(ctx, target.Ref, features, remoteHome); err != nil {
				if cfg.Strict {
					fmt.Fprintf(r.Stderr, "sshx: docker server unavailable for %s: %v\n", target.Name, err)
					return 1
				}
				fmt.Fprintf(r.Stderr, "sshx: docker server skipped for %s: %v\n", target.Name, err)
			} else {
				stopBridge, err := r.startDockerBridge(ctx, target.Ref, remoteHome)
				if err != nil {
					if cfg.Strict {
						fmt.Fprintf(r.Stderr, "sshx: docker command bridge unavailable for %s: %v\n", target.Name, err)
						return 1
					}
					fmt.Fprintf(r.Stderr, "sshx: docker command bridge skipped for %s: %v\n", target.Name, err)
				} else {
					remoteReady = true
					defer stopBridge()
				}
			}
		}
	}
	interactive := isInteractiveIO(r.Stdin, r.Stdout)
	if remoteReady {
		return r.execDocker(ctx, dockerSessionArgs(parsed, target.Ref, remoteHome, interactive))
	}
	return r.execDocker(ctx, dockerPlainArgs(parsed, target.Ref, interactive))
}

func (r *Runner) ensureDockerServer(ctx context.Context, container string, features config.Features, remoteHome string) error {
	if !features.CommandBridge {
		return nil
	}
	targetVersion := clientVersion()
	probe, err := r.probeDocker(ctx, container, remoteHome)
	if err != nil {
		return err
	}
	if probe.Running && probe.ServerVersion == targetVersion {
		return nil
	}
	if probe.BinaryVersion != targetVersion {
		localBinary, err := r.DownloadBinary(ctx, targetVersion, probe.AssetName())
		if err != nil {
			return err
		}
		if err := r.installDockerBinary(ctx, container, localBinary, remoteHome); err != nil {
			return err
		}
	}
	start := strings.Join([]string{
		remoteServerEnvScript(remoteHome),
		"mkdir -p \"$SSHX_SERVER_HOME\"",
		"rm -f \"$SSHX_SERVER_HOME/sock\" \"$SSHX_SERVER_HOME/server-info\"",
		"nohup \"$SSHX_SERVER_HOME/sshx\" server --socket \"$SSHX_SERVER_HOME/sock\" --server-info \"$SSHX_SERVER_HOME/server-info\" --port-scan-interval 0 >\"$SSHX_SERVER_HOME/server.log\" 2>&1 &",
	}, "; ")
	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := r.Exec(startCtx, r.DockerPath, dockerInternalExecArgs(container, dockerShell(start)...)); err != nil {
		return err
	}
	verify := strings.Join([]string{
		remoteServerEnvScript(remoteHome),
		"i=0",
		"while [ $i -lt 20 ]; do test -S \"$SSHX_SERVER_HOME/sock\" && test -f \"$SSHX_SERVER_HOME/server-info\" && exit 0; i=$((i+1)); sleep 0.1; done",
		"exit 1",
	}, "; ")
	return r.Exec(startCtx, r.DockerPath, dockerInternalExecArgs(container, dockerShell(verify)...))
}

func (r *Runner) probeDocker(ctx context.Context, container string, remoteHome string) (remoteProbe, error) {
	script := strings.Join([]string{
		remoteServerEnvScript(remoteHome),
		"os=$(uname -s 2>/dev/null || true)",
		"arch=$(uname -m 2>/dev/null || true)",
		"ver=",
		"if test -x \"$SSHX_SERVER_HOME/sshx\"; then ver=$(\"$SSHX_SERVER_HOME/sshx\" --version 2>/dev/null | awk '{print $2}' || true); fi",
		"server_ver=",
		"if test -f \"$SSHX_SERVER_HOME/server-info\"; then server_ver=$(sed -n 's/.*\"version\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p' \"$SSHX_SERVER_HOME/server-info\" | head -n 1 || true); fi",
		"running=0",
		"if test -S \"$SSHX_SERVER_HOME/sock\" && test -f \"$SSHX_SERVER_HOME/server-info\"; then running=1; fi",
		"printf '%s\\n%s\\n%s\\n%s\\n%s\\n' \"$os\" \"$arch\" \"$ver\" \"$server_ver\" \"$running\"",
	}, "; ")
	out, err := r.ExecOutput(ctx, r.DockerPath, dockerInternalExecArgs(container, dockerShell(script)...))
	if err != nil {
		return remoteProbe{}, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	lines := make([]string, 0, 5)
	for scanner.Scan() {
		lines = append(lines, strings.TrimSpace(scanner.Text()))
	}
	if err := scanner.Err(); err != nil {
		return remoteProbe{}, err
	}
	for len(lines) < 5 {
		lines = append(lines, "")
	}
	osName, err := normalizeRemoteOS(lines[0])
	if err != nil {
		return remoteProbe{}, err
	}
	arch, err := normalizeRemoteArch(lines[1])
	if err != nil {
		return remoteProbe{}, err
	}
	return remoteProbe{
		OS:            osName,
		Arch:          arch,
		BinaryVersion: lines[2],
		ServerVersion: lines[3],
		Running:       lines[4] == "1",
	}, nil
}

func (r *Runner) installDockerBinary(ctx context.Context, container string, localPath string, remoteHome string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	script := strings.Join([]string{
		"set -eu",
		remoteServerEnvScript(remoteHome),
		"mkdir -p \"$SSHX_SERVER_HOME\"",
		"tmp=\"$SSHX_SERVER_HOME/sshx.$$.tmp\"",
		"cat > \"$tmp\"",
		"chmod 755 \"$tmp\"",
		"mv \"$tmp\" \"$SSHX_SERVER_HOME/sshx\"",
	}, "; ")
	return r.ExecInput(ctx, r.DockerPath, dockerExecInputArgs(container, dockerShell(script)...), f)
}

func (r *Runner) startDockerBridge(ctx context.Context, container string, remoteHome string) (func(), error) {
	token, err := r.fetchDockerToken(ctx, container, remoteHome)
	if err != nil {
		return nil, err
	}
	bridgeCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(
		bridgeCtx,
		r.DockerPath,
		dockerExecInputArgs(container, dockerShell(remoteServerEnvScript(remoteHome)+"; exec \"$SSHX_SERVER_HOME/sshx\" socket-proxy --socket \"$SSHX_SERVER_HOME/sock\"")...)...,
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
		return nil, errors.New("timed out waiting for docker command bridge handshake")
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

func (r *Runner) fetchDockerToken(ctx context.Context, container string, remoteHome string) (string, error) {
	b, err := r.ExecOutput(ctx, r.DockerPath, dockerInternalExecArgs(container, dockerShell(remoteServerEnvScript(remoteHome)+"; cat \"$SSHX_SERVER_HOME/server-info\"")...))
	if err != nil {
		return "", err
	}
	var info bridge.ServerInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return "", err
	}
	return info.Token, nil
}

func (r *Runner) execDocker(ctx context.Context, args []string) int {
	if err := r.Exec(ctx, r.DockerPath, args); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(r.Stderr, "sshx: exec docker: %v\n", err)
		return 1
	}
	return 0
}

func dockerSessionArgs(parsed sshcompat.Parsed, container string, remoteHome string, interactive bool) []string {
	if len(parsed.RemoteCommand) == 0 {
		return dockerExecArgs(container, interactive, dockerLoginShell(remoteHome)...)
	}
	return dockerExecArgs(container, interactive, dockerExecShell(remoteHome, parsed.RemoteCommand)...)
}

func dockerPlainArgs(parsed sshcompat.Parsed, container string, interactive bool) []string {
	if len(parsed.RemoteCommand) == 0 {
		return dockerExecArgs(container, interactive, defaultDockerShellCommand()...)
	}
	return dockerExecArgs(container, interactive, parsed.RemoteCommand...)
}

func dockerExecArgs(container string, interactive bool, command ...string) []string {
	args := []string{"exec"}
	if interactive {
		args = append(args, "-it")
	} else {
		args = append(args, "-i")
	}
	args = append(args, container)
	args = append(args, command...)
	return args
}

func dockerExecInputArgs(container string, command ...string) []string {
	args := []string{"exec", "-i", container}
	args = append(args, command...)
	return args
}

func dockerInternalExecArgs(container string, command ...string) []string {
	args := []string{"exec", container}
	args = append(args, command...)
	return args
}

func dockerShell(script string) []string {
	return []string{"sh", "-lc", script}
}

func dockerLoginShell(remoteHome string) []string {
	return []string{"sh", "-lc", strings.Join([]string{
		remoteServerEnvScript(remoteHome),
		"mkdir -p \"$SSHX_SERVER_HOME\"",
		defaultDockerShellScript("exec", "-i"),
	}, "\n")}
}

func dockerExecShell(remoteHome string, argv []string) []string {
	if len(argv) == 1 {
		return []string{"sh", "-lc", remoteServerEnvScript(remoteHome) + "; " + argv[0]}
	}
	parts := []string{
		"sh",
		"-lc",
		remoteServerEnvScript(remoteHome) + "; exec \"$@\"",
		"sh",
	}
	parts = append(parts, argv...)
	return parts
}

func defaultDockerShellCommand() []string {
	return []string{"sh", "-lc", defaultDockerShellScript("exec", "-i")}
}

func defaultDockerShellScript(execCmd, interactiveFlag string) string {
	return strings.Join([]string{
		"if [ -n \"$SHELL\" ] && command -v \"$SHELL\" >/dev/null 2>&1; then " + execCmd + " \"$SHELL\" " + interactiveFlag + "; fi",
		"if command -v bash >/dev/null 2>&1; then " + execCmd + " bash " + interactiveFlag + "; fi",
		"if command -v sh >/dev/null 2>&1; then " + execCmd + " sh " + interactiveFlag + "; fi",
		"echo \"sshx: no shell found in container\" >&2",
		"exit 127",
	}, "\n")
}

func isInteractiveIO(stdin io.Reader, stdout io.Writer) bool {
	return isCharacterDevice(stdin) && isCharacterDevice(stdout)
}

func isCharacterDevice(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
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

func defaultExecInput(ctx context.Context, name string, args []string, stdin io.Reader) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func defaultExecOutput(ctx context.Context, name string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

func defaultDownloadBinary(ctx context.Context, targetVersion, assetName string) (string, error) {
	if override := os.Getenv("SSHX_REMOTE_BINARY"); override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", err
		}
		return override, nil
	}
	if targetVersion == "" || targetVersion == "dev" {
		return "", errors.New("remote binary download requires a release version; set SSHX_REMOTE_BINARY for dev builds")
	}
	cachePath := filepath.Join(defaultCacheRoot(), "remote", targetVersion, assetName)
	if info, err := os.Stat(cachePath); err == nil && info.Mode().IsRegular() {
		return cachePath, nil
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
		return "", err
	}
	baseURL := os.Getenv("SSHX_RELEASE_BASE_URL")
	if baseURL == "" {
		baseURL = "https://github.com/xiaot623/sshx/releases/download"
	}
	url := strings.TrimRight(baseURL, "/") + "/v" + targetVersion + "/" + assetName
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("download %s: GitHub returned %s", url, resp.Status)
	}
	tmpPath := fmt.Sprintf("%s.%d.tmp", cachePath, os.Getpid())
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	_, copyErr := io.Copy(tmp, resp.Body)
	closeErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		return "", copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return "", closeErr
	}
	if err := os.Rename(tmpPath, cachePath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return cachePath, nil
}

func (r *Runner) defaultStartBridge(ctx context.Context, target string, sshArgs []string, remoteHome string) (func(), error) {
	token, err := r.fetchRemoteToken(ctx, sshArgs, remoteHome)
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
		sshCommandArgs(sshArgs, remoteShell(remoteServerEnvScript(remoteHome)+"; exec \"$SSHX_SERVER_HOME/sshx\" socket-proxy --socket \"$SSHX_SERVER_HOME/sock\""))...,
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

func (r *Runner) fetchRemoteToken(ctx context.Context, sshArgs []string, remoteHome string) (string, error) {
	cmd := exec.CommandContext(ctx, r.SSHPath, internalSSHArgs(sshArgs, remoteShell(remoteServerEnvScript(remoteHome)+"; cat \"$SSHX_SERVER_HOME/server-info\""))...)
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

func sessionSSHArgs(parsed sshcompat.Parsed, remoteHome string) []string {
	args := baseSSHArgs(parsed)
	if len(args) == 0 {
		return append([]string(nil), parsed.Args...)
	}
	if len(parsed.RemoteCommand) == 0 {
		if hasSSHSessionlessFlag(args) {
			return append([]string(nil), parsed.Args...)
		}
		if !hasSSHDisableTTYFlag(args) && !hasSSHForceTTYFlag(args) {
			args = append([]string{"-t"}, args...)
		}
		return append(args, remoteLoginShell(remoteHome))
	}
	return append(args, remoteExecShell(remoteHome, parsed.RemoteCommand))
}

func remoteLoginShell(remoteHome string) string {
	envLine := remoteServerEnvScript(remoteHome)
	script := strings.Join([]string{
		envLine,
		"mkdir -p \"$SSHX_SERVER_HOME\"",
		"shell=${SHELL:-sh}",
		"name=${shell##*/}",
		"case \"$name\" in",
		"  bash) rc=\"$SSHX_SERVER_HOME/bashrc\"; { printf '%s\\n' " + shellQuote("test -f \"$HOME/.bashrc\" && . \"$HOME/.bashrc\"") + "; printf '%s\\n' " + shellQuote(envLine) + "; } > \"$rc\"; exec \"$shell\" --rcfile \"$rc\" -i ;;",
		"  zsh) zdot=\"$SSHX_SERVER_HOME/zdotdir\"; mkdir -p \"$zdot\"; { printf '%s\\n' " + shellQuote("test -f \"$HOME/.zshrc\" && . \"$HOME/.zshrc\"") + "; printf '%s\\n' " + shellQuote(envLine) + "; } > \"$zdot/.zshrc\"; ZDOTDIR=\"$zdot\" exec \"$shell\" -i ;;",
		"  *) exec \"$shell\" -i ;;",
		"esac",
	}, "\n")
	return remoteShell(script)
}

func remoteExecShell(remoteHome string, argv []string) string {
	if len(argv) == 1 {
		return remoteExecCommandShell(remoteHome, argv[0])
	}
	envLine := remoteServerEnvScript(remoteHome)
	parts := []string{
		"sh",
		"-lc",
		strings.Join([]string{
			envLine,
			"mkdir -p \"$SSHX_SERVER_HOME\"",
			"shell=${SHELL:-sh}",
			"name=${shell##*/}",
			"case \"$name\" in",
			"  bash) err=\"$SSHX_SERVER_HOME/bash-stderr.$$\"; \"$shell\" -ic " + shellQuote(envLine+"; \"$@\"") + " sh \"$@\" 2>\"$err\"; status=$?; sed '/^bash: cannot set terminal process group /d; /^bash: no job control in this shell$/d' \"$err\" >&2; rm -f \"$err\"; exit $status ;;",
			"  zsh) exec \"$shell\" -ic " + shellQuote(envLine+"; \"$@\"") + " sh \"$@\" ;;",
			"  *) \"$@\" ;;",
			"esac",
		}, "\n"),
		"sh",
	}
	parts = append(parts, argv...)
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		quoted = append(quoted, shellQuote(part))
	}
	return strings.Join(quoted, " ")
}

func remoteExecCommandShell(remoteHome string, command string) string {
	envLine := remoteServerEnvScript(remoteHome)
	commandLine := envLine + "; " + command
	script := strings.Join([]string{
		envLine,
		"mkdir -p \"$SSHX_SERVER_HOME\"",
		"shell=${SHELL:-sh}",
		"name=${shell##*/}",
		"case \"$name\" in",
		"  bash) err=\"$SSHX_SERVER_HOME/bash-stderr.$$\"; \"$shell\" -ic " + shellQuote(commandLine) + " 2>\"$err\"; status=$?; sed '/^bash: cannot set terminal process group /d; /^bash: no job control in this shell$/d' \"$err\" >&2; rm -f \"$err\"; exit $status ;;",
		"  zsh) exec \"$shell\" -ic " + shellQuote(commandLine) + " ;;",
		"  *) exec \"$shell\" -lc " + shellQuote(commandLine) + " ;;",
		"esac",
	}, "\n")
	return remoteShell(script)
}

func remoteShell(script string) string {
	return "sh -lc " + shellQuote(script)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func hasSSHSessionlessFlag(args []string) bool {
	for _, arg := range args {
		if shortOptionClusterContains(arg, 'N') || shortOptionClusterContains(arg, 'W') {
			return true
		}
	}
	return false
}

func hasSSHDisableTTYFlag(args []string) bool {
	for _, arg := range args {
		if shortOptionClusterContains(arg, 'T') {
			return true
		}
	}
	return false
}

func hasSSHForceTTYFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-t" || arg == "-tt" {
			return true
		}
	}
	return false
}

func shortOptionClusterContains(arg string, flag byte) bool {
	if len(arg) < 2 || arg[0] != '-' || arg == "--" {
		return false
	}
	if strings.HasPrefix(arg, "-o") || strings.HasPrefix(arg, "-O") {
		return false
	}
	for i := 1; i < len(arg); i++ {
		if arg[i] == flag {
			return true
		}
	}
	return false
}

type remoteHostsState struct {
	Targets map[string]remoteHostState `json:"targets"`
}

type remoteHostState struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func remoteIDForTarget(path, target string) (string, error) {
	if strings.TrimSpace(target) == "" {
		return "", errors.New("remote target is required")
	}
	var state remoteHostsState
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &state); err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if state.Targets == nil {
		state.Targets = make(map[string]remoteHostState)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if entry := state.Targets[target]; entry.ID != "" {
		entry.UpdatedAt = now
		state.Targets[target] = entry
		if err := writeRemoteHostsState(path, state); err != nil {
			return "", err
		}
		return entry.ID, nil
	}
	id, err := generateUUID()
	if err != nil {
		return "", err
	}
	state.Targets[target] = remoteHostState{ID: id, CreatedAt: now, UpdatedAt: now}
	if err := writeRemoteHostsState(path, state); err != nil {
		return "", err
	}
	return id, nil
}

func writeRemoteHostsState(path string, state remoteHostsState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmpPath := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmpPath, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func generateUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32], nil
}

func remoteServerHome(id string) string {
	return "$HOME/.sshx_server/" + id
}

func remoteServerEnvScript(remoteHome string) string {
	return "SSHX_SERVER_HOME=\"" + strings.ReplaceAll(remoteHome, `"`, `\"`) + "\"; export SSHX_SERVER_HOME; case \":$PATH:\" in *\":$SSHX_SERVER_HOME:\"*) ;; *) PATH=\"$SSHX_SERVER_HOME:$PATH\" ;; esac; export PATH"
}

type versionState struct {
	CurrentVersion string `json:"current_version"`
	LastVersion    string `json:"last_version,omitempty"`
	UpdatedAt      string `json:"updated_at"`
}

func recordDefaultVersionState(current string) error {
	return recordVersionState(defaultVersionStatePath(), current)
}

func recordVersionState(path, current string) error {
	if current == "" {
		current = "dev"
	}
	var state versionState
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &state)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if state.CurrentVersion != current {
		state.LastVersion = state.CurrentVersion
		state.CurrentVersion = current
	}
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmpPath := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmpPath, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func clientVersion() string {
	if version.Version == "" {
		return "dev"
	}
	return version.Version
}

func normalizeRemoteOS(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "linux":
		return "linux", nil
	case "":
		return "", errors.New("remote OS probe returned empty value")
	default:
		return "", fmt.Errorf("unsupported remote server OS %q", s)
	}
}

func normalizeRemoteArch(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	case "":
		return "", errors.New("remote arch probe returned empty value")
	default:
		return "", fmt.Errorf("unsupported remote server arch %q", s)
	}
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
	if serverHome := os.Getenv("SSHX_SERVER_HOME"); serverHome != "" {
		return filepath.Join(serverHome, "sock")
	}
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

func defaultCacheRoot() string {
	if override := os.Getenv("SSHX_CACHE_DIR"); override != "" {
		return override
	}
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "sshx")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "sshx-cache")
	}
	return filepath.Join(home, ".cache", "sshx")
}

func defaultVersionStatePath() string {
	if serverHome := os.Getenv("SSHX_SERVER_HOME"); serverHome != "" {
		return filepath.Join(serverHome, "version-state.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "sshx-version-state.json")
	}
	return filepath.Join(home, ".sshx", "version-state.json")
}

func defaultRemoteHostsPath() string {
	if override := os.Getenv("SSHX_REMOTE_HOSTS"); override != "" {
		return override
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "sshx-remote-hosts.json")
	}
	return filepath.Join(home, ".sshx", "remote-hosts.json")
}

func defaultServerInfoPath() string {
	if serverHome := os.Getenv("SSHX_SERVER_HOME"); serverHome != "" {
		return filepath.Join(serverHome, "server-info")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "sshx-server-info")
	}
	return filepath.Join(home, ".sshx", "server-info")
}

func IsReservedLocalError(s string) bool {
	return strings.Contains(s, reservedLocalMessage)
}
