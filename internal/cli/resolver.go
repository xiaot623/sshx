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

	"github.com/xiaot623/sshx/internal/config"
)

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
