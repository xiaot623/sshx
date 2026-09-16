package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/xiaot623/sshx/internal/identity"
	"github.com/xiaot623/sshx/internal/protocol"
	"github.com/xiaot623/sshx/internal/remotefs"
	"github.com/xiaot623/sshx/internal/sshcompat"
)

type stubMountDriver struct {
	calls int
}

type stubMount struct {
	path string
	done chan error
}

func (d *stubMountDriver) Mount(_ context.Context, path string, _ remotefs.Backend, _ remotefs.MountOptions) (remotefs.Mount, error) {
	d.calls++
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	return &stubMount{path: path, done: make(chan error)}, nil
}

func (m *stubMount) Path() string       { return m.path }
func (m *stubMount) Done() <-chan error { return m.done }
func (m *stubMount) Unmount(context.Context) error {
	select {
	case <-m.done:
	default:
		close(m.done)
	}
	return nil
}

func TestRemoteMountManagerExecuteRequiresPriorOnMount(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	manager := newRemoteMountManager("session-1", false)
	defer manager.Close()
	if manager.initErr != nil {
		t.Fatal(manager.initErr)
	}
	resp := manager.Execute(context.Background(), protocol.Frame{
		Type:      protocol.TypeCommandExec,
		ID:        "1",
		SessionID: "session-1",
		MountID:   "export-1",
		MountPath: "Users/xiaot",
		Cwd:       ".",
		Argv:      []string{"true"},
		RemoteFS:  true,
	})
	if resp.Type != protocol.TypeCommandError || !strings.Contains(resp.Error, "not available") {
		t.Fatalf("response = %#v", resp)
	}
}

func TestRemoteMountManagerOnMountThenExecute(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	manager := newRemoteMountManager("session-1", false)
	defer manager.Close()
	if manager.initErr != nil {
		t.Fatal(manager.initErr)
	}
	driver := &stubMountDriver{}
	manager.driver = driver
	peer := &remotefs.Peer{}
	mountPath, err := manager.OnMount(context.Background(), peer, "export-1", "Users/xiaot", remotefs.MountOptions{})
	if err != nil {
		t.Fatal(err)
	}
	again, err := manager.OnMount(context.Background(), peer, "export-1", "Users/xiaot", remotefs.MountOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again != mountPath {
		t.Fatalf("idempotent OnMount path = %q, want %q", again, mountPath)
	}
	if driver.calls != 1 {
		t.Fatalf("Mount calls = %d, want 1", driver.calls)
	}
	workspace := filepath.Join(mountPath, "workspace", "sshx")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("from-remote"), 0o600); err != nil {
		t.Fatal(err)
	}
	resp := manager.Execute(context.Background(), protocol.Frame{
		Type:      protocol.TypeCommandExec,
		ID:        "1",
		SessionID: "session-1",
		MountID:   "export-1",
		MountPath: "Users/xiaot",
		Cwd:       "workspace/sshx",
		Argv:      []string{"cat", "note.txt"},
		RemoteFS:  true,
	})
	if resp.Type != protocol.TypeCommandResult {
		t.Fatalf("response = %#v", resp)
	}
	stdout, err := base64.StdEncoding.DecodeString(resp.Stdout)
	if err != nil {
		t.Fatal(err)
	}
	if string(stdout) != "from-remote" {
		t.Fatalf("stdout = %q", stdout)
	}
	if err := manager.OnUnmount(context.Background(), "export-1"); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnUnmount(context.Background(), "export-1"); err != nil {
		t.Fatal(err)
	}
}

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

func TestRemoteFSMountStartupReclaimsOnlyStaleSessions(t *testing.T) {
	runtimeRoot := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
	root := localReverseMountsRoot()
	staleExport := filepath.Join(root, "stale-session", "export-1")
	if err := os.MkdirAll(filepath.Join(staleExport, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleExport, ".mount-path"), []byte("workspace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	activeRoot := filepath.Join(root, "active-session")
	if err := os.MkdirAll(activeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	activeLease, err := os.OpenFile(filepath.Join(activeRoot, ".lease"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer activeLease.Close()
	if err := syscall.Flock(int(activeLease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(activeLease.Fd()), syscall.LOCK_UN)

	manager := newRemoteMountManager("current-session", false)
	defer manager.Close()
	if manager.initErr != nil {
		t.Fatal(manager.initErr)
	}
	if _, err := os.Stat(filepath.Join(root, "stale-session")); !os.IsNotExist(err) {
		t.Fatalf("stale session was not reclaimed: %v", err)
	}
	if _, err := os.Stat(activeRoot); err != nil {
		t.Fatalf("active session was changed: %v", err)
	}
}

func TestIntegrationRemoteFSSessionDoesNotExportLocalWorkspaceToRemote(t *testing.T) {
	parsed := sshcompat.Parse([]string{"remote", "sh", "-c", "cat note.txt"})
	session := &BridgeSession{SessionID: "session-1", ContextID: "context-1", RemoteFS: true, ReadOnly: true}
	args := sessionSSHArgsForBridge(parsed, "$HOME/.sshx_server/id", session)
	command := args[len(args)-1]
	for _, want := range []string{
		"SSHX_SESSION_ID",
		"SSHX_CONTEXT_ID",
		"SSHX_REMOTE_FS=1",
		"FS_READ_ONLY=1",
		"cat note.txt",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("remote command %q does not contain %q", command, want)
		}
	}
	if strings.Contains(command, "SSHX_WORKSPACE") || strings.Contains(command, "SSHX_MOUNT_ROOT") {
		t.Fatalf("remote command still exports a local workspace: %q", command)
	}
}

func TestDirectRemoteFSSessionExportsLocalWorkspaceToRemote(t *testing.T) {
	parsed := sshcompat.Parse([]string{"remote", "sh", "-c", "cat note.txt"})
	session := &BridgeSession{
		SessionID: "session-1",
		ContextID: "context-1",
		RemoteFS:  true,
		MountRoot: "/tmp/mounts/session-1/Users/xiaot",
		Workspace: "/tmp/mounts/session-1/Users/xiaot/workspace/sshx",
		ReadOnly:  true,
	}
	args := sessionSSHArgsForBridge(parsed, "$HOME/.sshx_server/id", session)
	command := args[len(args)-1]
	for _, want := range []string{
		"SSHX_MOUNT_ROOT",
		"SSHX_WORKSPACE",
		"cd -- \"$SSHX_WORKSPACE\"",
		"cat note.txt",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("remote command %q does not contain %q", command, want)
		}
	}
}

func TestRemoteFSInteractiveShellKeepsRemoteHome(t *testing.T) {
	parsed := sshcompat.Parse([]string{"remote"})
	session := &BridgeSession{SessionID: "session-1", ContextID: "context-1", RemoteFS: true}
	args := sessionSSHArgsForBridge(parsed, "$HOME/.sshx_server/id", session)
	command := args[len(args)-1]
	if !strings.Contains(command, "SSHX_REMOTE_FS=1") {
		t.Fatalf("interactive command = %q", command)
	}
	if strings.Contains(command, "SSHX_WORKSPACE") {
		t.Fatalf("interactive shell received a local workspace: %q", command)
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
	var executed bool
	runner := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	runner.ConfigPath = configPath
	runner.ResolveIdentity = func(context.Context, string, []string, string) (identity.Connection, error) {
		return identity.Connection{}, errors.New("identity state unavailable")
	}
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
