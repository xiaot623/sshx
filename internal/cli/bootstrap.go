package cli

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/xiaot623/sshx/internal/config"
)

type serverBootstrapTransport interface {
	OutputScript(context.Context, string) ([]byte, error)
	ExecScript(context.Context, string) error
	ExecInputScript(context.Context, string, io.Reader) error
}

type serverBootstrapOptions struct {
	Enabled         bool
	DisablePortScan bool
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

func (r *Runner) ensureRemoteServer(ctx context.Context, sshArgs []string, features config.Features, remoteHome string) error {
	return r.ensureBootstrappedServer(ctx, remoteHome, sshServerTransport{r: r, sshArgs: sshArgs}, serverBootstrapOptions{
		Enabled: features.Enabled(),
	})
}

func (r *Runner) ensureDockerServer(ctx context.Context, container string, features config.Features, remoteHome string) error {
	return r.ensureBootstrappedServer(ctx, remoteHome, dockerServerTransport{r: r, container: container}, serverBootstrapOptions{
		Enabled:         features.CommandBridge,
		DisablePortScan: true,
	})
}

func (r *Runner) ensureBootstrappedServer(ctx context.Context, remoteHome string, transport serverBootstrapTransport, opts serverBootstrapOptions) error {
	if !opts.Enabled {
		return nil
	}
	targetVersion := clientVersion()
	probe, err := probeBootstrappedServer(ctx, transport, remoteHome)
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
		if err := installBootstrappedBinary(ctx, transport, localBinary, remoteHome); err != nil {
			return err
		}
	}
	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := transport.ExecScript(startCtx, startServerScript(remoteHome, opts)); err != nil {
		return err
	}
	return transport.ExecScript(startCtx, verifyServerScript(remoteHome))
}

func probeBootstrappedServer(ctx context.Context, transport serverBootstrapTransport, remoteHome string) (remoteProbe, error) {
	out, err := transport.OutputScript(ctx, probeServerScript(remoteHome))
	if err != nil {
		return remoteProbe{}, err
	}
	return parseRemoteProbe(out)
}

func installBootstrappedBinary(ctx context.Context, transport serverBootstrapTransport, localPath string, remoteHome string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return transport.ExecInputScript(ctx, installBinaryScript(remoteHome), f)
}

func parseRemoteProbe(out []byte) (remoteProbe, error) {
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

func probeServerScript(remoteHome string) string {
	return strings.Join([]string{
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
}

func installBinaryScript(remoteHome string) string {
	return strings.Join([]string{
		"set -eu",
		remoteServerEnvScript(remoteHome),
		"mkdir -p \"$SSHX_SERVER_HOME\"",
		"tmp=\"$SSHX_SERVER_HOME/sshx.$$.tmp\"",
		"cat > \"$tmp\"",
		"chmod 755 \"$tmp\"",
		"mv \"$tmp\" \"$SSHX_SERVER_HOME/sshx\"",
	}, "; ")
}

func startServerScript(remoteHome string, opts serverBootstrapOptions) string {
	serverCmd := "nohup \"$SSHX_SERVER_HOME/sshx\" server --socket \"$SSHX_SERVER_HOME/sock\" --server-info \"$SSHX_SERVER_HOME/server-info\""
	if opts.DisablePortScan {
		serverCmd += " --port-scan-interval 0"
	}
	serverCmd += " >\"$SSHX_SERVER_HOME/server.log\" 2>&1 &"
	return strings.Join([]string{
		remoteServerEnvScript(remoteHome),
		"mkdir -p \"$SSHX_SERVER_HOME\"",
		"rm -f \"$SSHX_SERVER_HOME/sock\" \"$SSHX_SERVER_HOME/server-info\"",
		serverCmd,
	}, "; ")
}

func verifyServerScript(remoteHome string) string {
	return strings.Join([]string{
		remoteServerEnvScript(remoteHome),
		"i=0",
		"while [ $i -lt 20 ]; do test -S \"$SSHX_SERVER_HOME/sock\" && test -f \"$SSHX_SERVER_HOME/server-info\" && exit 0; i=$((i+1)); sleep 0.1; done",
		"exit 1",
	}, "; ")
}
