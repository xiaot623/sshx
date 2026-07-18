package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/xiaot623/sshx/internal/config"
	"github.com/xiaot623/sshx/internal/identity"
	"github.com/xiaot623/sshx/internal/integration"
	"github.com/xiaot623/sshx/internal/sshcompat"
	"github.com/xiaot623/sshx/internal/sshconfig"
)

type BridgeSession struct {
	SessionID string
	ContextID string
	RemoteFS  bool
	MountRoot string
	Workspace string
	ReadOnly  bool
	Done      <-chan struct{}
	stop      func()
	stopOnce  sync.Once
}

func (s *BridgeSession) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.stop != nil {
			s.stop()
		}
	})
}

func (s *BridgeSession) CommandContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if s == nil || s.Done == nil {
		return ctx, cancel
	}
	go func() {
		select {
		case <-s.Done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

type Runner struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	InvocationPath  string
	SSHPath         string
	DockerPath      string
	SSHConfigPath   string
	ConfigPath      string
	Exec            func(context.Context, string, []string) error
	ExecInput       func(context.Context, string, []string, io.Reader) error
	ExecOutput      func(context.Context, string, []string) ([]byte, error)
	DownloadBinary  func(context.Context, string, string) (string, error)
	StartBridge     func(context.Context, string, []string, string) (*BridgeSession, error)
	EnsureResolver  func(context.Context) error
	ResolveIdentity func(context.Context, string, []string, string) (identity.Connection, error)

	commandPolicy      config.CommandPolicy
	commandBridge      bool
	autoForward        bool
	remoteFS           bool
	integrationSidecar bool
	connection         identity.Connection
}

func NewRunner(stdin io.Reader, stdout io.Writer, stderr io.Writer) *Runner {
	configPath := config.DefaultPath()
	if override := os.Getenv("SSHX_CONFIG"); override != "" {
		configPath = override
	}
	r := &Runner{
		Stdin:         stdin,
		Stdout:        stdout,
		Stderr:        stderr,
		SSHPath:       "ssh",
		DockerPath:    "docker",
		SSHConfigPath: sshconfig.DefaultPath(),
		ConfigPath:    configPath,
		Exec:          defaultExec,
		ExecInput:     defaultExecInput,
		ExecOutput:    defaultExecOutput,
	}
	r.DownloadBinary = defaultDownloadBinary
	r.StartBridge = r.defaultStartBridge
	r.EnsureResolver = r.defaultEnsureResolver
	r.ResolveIdentity = func(ctx context.Context, sshPath string, args []string, profile string) (identity.Connection, error) {
		return identity.NewConnection(ctx, identity.DefaultInstallPath(), sshPath, args, profile)
	}
	return r
}

func (r *Runner) Run(ctx context.Context, args []string) int {
	invocation := r.InvocationPath
	if invocation == "" {
		invocation = "sshx"
	}
	switch filepath.Base(invocation) {
	case "ssh", "scp":
		if _, err := integration.DescriptorForInvocation(invocation); err == nil {
			return r.runIntegrationAdapter(ctx, invocation, args)
		}
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintf(r.Stdout, "sshx %s\n", clientVersion())
		return 0
	}
	if len(args) > 0 {
		switch args[0] {
		case "integrate":
			return r.runIntegrate(ctx, args[1:])
		case "runtime-id":
			fmt.Fprintln(r.Stdout, identity.RuntimeID)
			return 0
		case "server":
			return r.runServer(ctx, args[1:])
		case "bridge-client":
			return r.runBridgeClient(ctx, args[1:])
		case "socket-proxy":
			return r.runSocketProxy(ctx, args[1:])
		case "mux-proxy":
			return r.runMuxProxy(ctx, args[1:])
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
	timeout, err := commandTimeout(&parsed)
	if err != nil {
		fmt.Fprintf(r.Stderr, "sshx: %v\n", err)
		return 2
	}
	if parsed.Target == "local" {
		return r.runLocalBridge(ctx, parsed.RemoteCommand, timeout)
	}
	if parsed.NoWrap {
		return r.execSSHWithTimeout(ctx, parsed.Args, timeout)
	}
	if os.Getenv("SSHX_DISABLE") == "1" || parsed.InfoMode || parsed.Target == "" {
		return r.execSSHWithTimeout(ctx, parsed.Args, timeout)
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
		return r.runDocker(ctx, parsed, dockerTarget, cfg, timeout)
	}
	if !features.Enabled() {
		return r.execSSHWithTimeout(ctx, parsed.Args, timeout)
	}
	if err := recordDefaultVersionState(clientVersion()); err != nil {
		if cfg.Strict {
			fmt.Fprintf(r.Stderr, "sshx: version state unavailable: %v\n", err)
			return 1
		}
		fmt.Fprintf(r.Stderr, "sshx: version state skipped: %v\n", err)
	}
	sshArgs := baseSSHArgs(parsed)
	connection, err := r.ResolveIdentity(ctx, r.SSHPath, sshArgs, "cli")
	if err != nil {
		if cfg.Strict || features.RemoteFS {
			fmt.Fprintf(r.Stderr, "sshx: target identity unavailable for %s: %v\n", parsed.Target, err)
			return 1
		}
		fmt.Fprintf(r.Stderr, "sshx: target identity skipped for %s: %v\n", parsed.Target, err)
		return r.execSSHWithTimeout(ctx, parsed.Args, timeout)
	}
	r.connection = connection
	remoteHome := remoteServerHome(connection.TargetID)
	r.commandPolicy = cfg.Commands
	r.commandBridge = features.CommandBridge
	r.autoForward = features.AutoForward
	r.remoteFS = features.RemoteFS
	if features.AutoForward {
		if err := r.EnsureResolver(ctx); err != nil {
			if cfg.Strict {
				fmt.Fprintf(r.Stderr, "sshx: resolver setup unavailable for %s: %v\n", parsed.Target, err)
				return 1
			}
			fmt.Fprintf(r.Stderr, "sshx: resolver setup skipped: %v\n", err)
		}
	}
	remoteReady := false
	if err := r.ensureRemoteServer(ctx, sshArgs, features, remoteHome); err != nil {
		if cfg.Strict || features.RemoteFS {
			fmt.Fprintf(r.Stderr, "sshx: remote server unavailable for %s: %v\n", parsed.Target, err)
			return 1
		}
	} else if features.CommandBridge || features.AutoForward || features.RemoteFS {
		bridgeSession, err := r.StartBridge(ctx, parsed.Target, sshArgs, remoteHome)
		if err != nil {
			if cfg.Strict || features.RemoteFS {
				fmt.Fprintf(r.Stderr, "sshx: command bridge unavailable for %s: %v\n", parsed.Target, err)
				return 1
			}
		} else {
			remoteReady = true
			defer bridgeSession.Stop()
			commandCtx, cancel := bridgeSession.CommandContext(ctx)
			defer cancel()
			return r.execSSHWithTimeout(commandCtx, sessionSSHArgsForBridge(parsed, remoteHome, bridgeSession), timeout)
		}
	} else {
		remoteReady = true
	}
	if remoteReady {
		return r.execSSHWithTimeout(ctx, sessionSSHArgs(parsed, remoteHome), timeout)
	}
	return r.execSSHWithTimeout(ctx, parsed.Args, timeout)
}
