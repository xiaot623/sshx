package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/xiaot623/sshx/internal/config"
	"github.com/xiaot623/sshx/internal/sshcompat"
	"github.com/xiaot623/sshx/internal/sshconfig"
)

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
	EnsureResolver  func(context.Context) error

	commandPolicy config.CommandPolicy
	commandBridge bool
	autoForward   bool
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
	r.autoForward = features.AutoForward
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
		if cfg.Strict {
			fmt.Fprintf(r.Stderr, "sshx: remote server unavailable for %s: %v\n", parsed.Target, err)
			return 1
		}
	} else if features.CommandBridge || features.AutoForward {
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
