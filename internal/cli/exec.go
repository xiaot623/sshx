package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func (r *Runner) execSSH(ctx context.Context, args []string) int {
	return r.execSSHWithTimeout(ctx, args, 0)
}

func (r *Runner) execSSHWithTimeout(ctx context.Context, args []string, timeout time.Duration) int {
	commandCtx, cancel := withCommandTimeout(ctx, timeout)
	defer cancel()
	if err := r.Exec(commandCtx, r.SSHPath, args); err != nil {
		if timeout > 0 && errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			fmt.Fprintf(r.Stderr, "sshx: command timed out after %s\n", timeout)
			return 124
		}
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
