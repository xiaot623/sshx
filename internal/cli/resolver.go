package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func (r *Runner) defaultEnsureResolver(ctx context.Context) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	suffix := strings.Trim(domainSuffix(), ".")
	if suffix == "" {
		return errors.New("domain suffix is required")
	}
	content, err := resolverContent(domainDNSAddr())
	if err != nil {
		return err
	}
	path := filepath.Join("/etc/resolver", suffix)

	// Resolver file: try a direct write first (works when already root or
	// /etc/resolver is writable); only escalate to sudo if needed.
	resolverOK := false
	if current, rerr := os.ReadFile(path); rerr == nil && string(current) == content {
		resolverOK = true
	} else if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return rerr
	} else if werr := writeResolverFile(path, content); werr == nil {
		resolverOK = true
	}

	// Loopback aliases: provision the 127.64.0.x pool so per-target forwards can
	// bind. Idempotent; returns nil when the whole pool already exists.
	aliasCmds := loopbackAliasCommands()

	if resolverOK && len(aliasCmds) == 0 {
		return nil
	}

	// Batch whatever still needs root into a single sudo sh -c so the user sees
	// at most one password prompt for the whole first-time setup.
	var cmds []string
	if !resolverOK {
		cmds = append(cmds, resolverWriteScript(filepath.Dir(path), path, content))
	}
	cmds = append(cmds, aliasCmds...)
	return sudoRun(ctx, joinShellCommands(cmds), r.Stderr)
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

// resolverWriteScript builds the shell command that writes /etc/resolver/<suffix>.
func resolverWriteScript(dir, path, content string) string {
	return "mkdir -p " + shellQuote(dir) +
		" && printf %s " + shellQuote(content) +
		" > " + shellQuote(path)
}

func sudoRun(ctx context.Context, script string, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "sudo", "sh", "-c", script)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
