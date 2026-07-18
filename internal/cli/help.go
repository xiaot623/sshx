package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

const sshxHelp = `sshx - OpenSSH-compatible SSH wrapper

USAGE
  sshx [--no-wrap] [ssh-options] destination [command [argument ...]]
  sshx destination --timeout=<duration> command [argument ...]
  sshx <command> [arguments]

SSHX OPTIONS
  --help                       Show sshx help followed by OpenSSH help
  --version                    Show the sshx version
  --no-wrap                    Bypass sshx features and invoke OpenSSH directly
  --timeout=<duration>         Limit a remote or bridged command; place it after
                               destination (examples: 30, 500ms, 2m)

SSHX COMMANDS
  forward                      List active automatic port forwards
  ps                           List SSH config hosts and running Docker containers
  integrate install [-y] <vscode|cursor>
                               Install an editor Remote SSH integration
  install-resolver [options]   Print or install the macOS resolver configuration

SPECIAL TARGET
  local <command>              From an sshx remote session, run a command on the
                               connected client; reserved on the client itself

ENVIRONMENT
  SSHX_CONFIG=<path>           Override the config file (~/.sshx/config.yaml)
  SSHX_DISABLE=1               Bypass sshx features and invoke OpenSSH directly
  COMMANDBRIDGE=0|1            Override the commandBridge config feature
  AUTOFORWARD=0|1              Override the autoForward config feature
  REMOTEFS=0|1                 Override the remoteFs config feature
  SSHX_DOMAIN_DNS_ADDR=<addr>  Override the local DNS listener address
                               (default: 127.0.0.1:53535)
  SSHX_CACHE_DIR=<path>        Override the local download/cache directory
  SSHX_RELEASE_BASE_URL=<url>  Override the remote binary release origin
  SSHX_REMOTE_BINARY=<path>    Use a local remote-side binary (development)
  SSHX_INTEGRATIONS_DIR=<path> Override the editor integration directory
  SSHX_LOCAL_DAEMON_SOCKET=<path>
                               Override the local daemon Unix socket

RUNTIME ENVIRONMENT (SET BY SSHX)
  SSHX_CONTEXT_ID              Current application/session routing identity
  SSHX_REMOTE_FS               1 when RemoteFS is active; otherwise 0
  SSHX_REMOTE_CWD              Source working directory for a bridged command
  SSHX_WORKSPACE               Mounted workspace path in a RemoteFS session
  SSHX_MOUNT_ROOT              Root of the mounted tree in a RemoteFS session

NOTES
  OpenSSH options such as -F, -o, -J, -L, and -p are passed through.
  A destination matching a running Docker container uses docker exec automatically.
  XDG_CACHE_HOME and XDG_CONFIG_HOME are honored when their paths are applicable.

OPENSSH HELP
`

func (r *Runner) runHelp(ctx context.Context) int {
	fmt.Fprint(r.Stdout, sshxHelp)

	// OpenSSH has no portable help flag. Invoking it without a destination prints
	// its platform-specific usage without the distracting "illegal option" error
	// produced by `ssh --help` on macOS. That usage path exits non-zero by design.
	output, err := r.ExecCombined(ctx, r.SSHPath, nil)
	_, _ = r.Stdout.Write(output)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return 0
		}
		fmt.Fprintf(r.Stderr, "sshx: exec ssh help: %v\n", err)
		return 1
	}
	return 0
}
