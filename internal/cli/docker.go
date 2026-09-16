package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/xiaot623/sshx/internal/bridge"
	"github.com/xiaot623/sshx/internal/config"
	"github.com/xiaot623/sshx/internal/identity"
	"github.com/xiaot623/sshx/internal/sshcompat"
	"github.com/xiaot623/sshx/internal/sshconfig"
)

type dockerContainer struct {
	ID   string
	Name string
}

type dockerTarget struct {
	dockerContainer
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
		return dockerTarget{dockerContainer: c}, true, nil
	}
	for _, c := range containers {
		if c.Name == target {
			return dockerTarget{dockerContainer: c}, true, nil
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

func (r *Runner) runDocker(ctx context.Context, parsed sshcompat.Parsed, target dockerTarget, cfg config.Config, timeout time.Duration) int {
	features := cfg.Features
	if features.Proxy {
		if cfg.Strict {
			fmt.Fprintln(r.Stderr, "sshx: proxy does not support Docker targets")
			return 1
		}
		fmt.Fprintln(r.Stderr, "sshx: proxy skipped for Docker target")
	}
	if features.RemoteFS {
		fmt.Fprintln(r.Stderr, "sshx: remoteFs does not support Docker targets yet")
		return 1
	}
	remoteHome := ""
	if features.CommandBridge {
		if err := recordDefaultVersionState(clientVersion()); err != nil {
			if cfg.Strict {
				fmt.Fprintf(r.Stderr, "sshx: version state unavailable: %v\n", err)
				return 1
			}
			fmt.Fprintf(r.Stderr, "sshx: version state skipped: %v\n", err)
		}
		remoteID := identity.TargetID(identity.Target{User: "docker", Hostname: target.ID, Port: 1})
		remoteHome = remoteServerHome(remoteID)
		r.commandPolicy = cfg.Commands
		r.commandBridge = features.CommandBridge
		if err := r.ensureDockerServer(ctx, target.ID, features, remoteHome); err != nil {
			if cfg.Strict {
				fmt.Fprintf(r.Stderr, "sshx: docker server unavailable for %s: %v\n", target.Name, err)
				return 1
			}
			fmt.Fprintf(r.Stderr, "sshx: docker server skipped for %s: %v\n", target.Name, err)
		} else if stopBridge, err := r.startDockerBridge(ctx, target.ID, remoteHome); err != nil {
			if cfg.Strict {
				fmt.Fprintf(r.Stderr, "sshx: docker command bridge unavailable for %s: %v\n", target.Name, err)
				return 1
			}
			fmt.Fprintf(r.Stderr, "sshx: docker command bridge skipped for %s: %v\n", target.Name, err)
		} else {
			defer stopBridge()
			interactive := isInteractiveIO(r.Stdin, r.Stdout)
			return r.execDockerWithTimeout(ctx, dockerSessionArgs(parsed, target.ID, remoteHome, interactive), timeout)
		}
	}
	interactive := isInteractiveIO(r.Stdin, r.Stdout)
	return r.execDockerWithTimeout(ctx, dockerPlainArgs(parsed, target.ID, interactive), timeout)
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

func (r *Runner) execDockerWithTimeout(ctx context.Context, args []string, timeout time.Duration) int {
	return r.execWithTimeout(ctx, r.DockerPath, args, timeout, "exec docker")
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
