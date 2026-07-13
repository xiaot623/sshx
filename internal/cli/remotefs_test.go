package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaot623/sshx/internal/remotefs"
	"github.com/xiaot623/sshx/internal/sshcompat"
)

func TestLocalReverseMountsRootDetectsNestedCwd(t *testing.T) {
	root := localReverseMountsRoot()
	nested := filepath.Join(root, "session-1", "req-1", "workspace")
	within, err := remotefs.PathWithin(root, nested)
	if err != nil {
		t.Fatal(err)
	}
	if !within {
		t.Fatalf("expected %q within %q", nested, root)
	}
	outside := t.TempDir()
	within, err = remotefs.PathWithin(root, outside)
	if err != nil {
		t.Fatal(err)
	}
	if within {
		t.Fatalf("did not expect %q within %q", outside, root)
	}
}

func TestRemoteFSSessionCommandUsesMountedWorkspace(t *testing.T) {
	parsed := sshcompat.Parse([]string{"remote", "sh", "-c", "cat note.txt"})
	session := &BridgeSession{SessionID: "session-1", MountRoot: "/tmp/mounts/session-1/Users/xiaot", Workspace: "/tmp/mounts/session-1/Users/xiaot/workspace/sshx", ReadOnly: true}
	args := sessionSSHArgsForBridge(parsed, "$HOME/.sshx_server/id", session)
	command := args[len(args)-1]
	for _, want := range []string{
		"SSHX_SESSION_ID",
		"SSHX_WORKSPACE",
		"SSHX_MOUNT_ROOT",
		"SSHX_REMOTE_FS=1",
		"FS_READ_ONLY=1",
		`cd -- "$SSHX_WORKSPACE"`,
		"cat note.txt",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("remote command %q does not contain %q", command, want)
		}
	}
}

func TestRemoteFSInteractiveShellKeepsRemoteHome(t *testing.T) {
	parsed := sshcompat.Parse([]string{"remote"})
	session := &BridgeSession{SessionID: "session-1", MountRoot: "/tmp/mounts/session-1/Users/xiaot", Workspace: "/tmp/mounts/session-1/Users/xiaot/workspace/sshx"}
	args := sessionSSHArgsForBridge(parsed, "$HOME/.sshx_server/id", session)
	command := args[len(args)-1]
	if !strings.Contains(command, "SSHX_WORKSPACE") {
		t.Fatalf("interactive command = %q", command)
	}
	if strings.Contains(command, `cd -- "$SSHX_WORKSPACE"`) {
		t.Fatalf("interactive shell changed directory: %q", command)
	}
}

func TestRemoteFSFailureNeverFallsBackToPlainSSH(t *testing.T) {
	isolateHome(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
strict: false
features:
  commandBridge: false
  autoForward: false
  remoteFs: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var executed bool
	stderr := &bytes.Buffer{}
	runner := NewRunner(strings.NewReader(""), &bytes.Buffer{}, stderr)
	runner.ConfigPath = configPath
	runner.ExecOutput = func(context.Context, string, []string) ([]byte, error) {
		return sameVersionRemoteProbe(), nil
	}
	runner.StartBridge = func(context.Context, string, []string, string) (*BridgeSession, error) {
		return nil, errors.New("FUSE unavailable")
	}
	runner.Exec = func(context.Context, string, []string) error {
		executed = true
		return nil
	}
	code := runner.Run(context.Background(), []string{"user@remote", "true"})
	if code == 0 {
		t.Fatal("remoteFs failure returned success")
	}
	if executed {
		t.Fatal("remoteFs failure fell back to plain SSH")
	}
	if !strings.Contains(stderr.String(), "FUSE unavailable") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRemoteFSStateFailureNeverFallsBackToPlainSSH(t *testing.T) {
	isolateHome(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("features:\n  remoteFs: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	var executed bool
	runner := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	runner.ConfigPath = configPath
	runner.RemoteHostsPath = filepath.Join(blockedParent, "hosts.json")
	runner.Exec = func(context.Context, string, []string) error {
		executed = true
		return nil
	}
	if code := runner.Run(context.Background(), []string{"user@remote", "true"}); code == 0 {
		t.Fatal("remote state failure returned success")
	}
	if executed {
		t.Fatal("remote state failure fell back to plain SSH")
	}
}

func TestRemoteFSServerBootstrapFailureNeverFallsBackToPlainSSH(t *testing.T) {
	isolateHome(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("features:\n  remoteFs: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var executed bool
	runner := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	runner.ConfigPath = configPath
	runner.ExecOutput = func(context.Context, string, []string) ([]byte, error) {
		return nil, errors.New("remote probe failed")
	}
	runner.Exec = func(context.Context, string, []string) error {
		executed = true
		return nil
	}
	if code := runner.Run(context.Background(), []string{"user@remote", "true"}); code == 0 {
		t.Fatal("bootstrap failure returned success")
	}
	if executed {
		t.Fatal("bootstrap failure fell back to plain SSH")
	}
}
