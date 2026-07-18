package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaot623/sshx/internal/identity"
	"github.com/xiaot623/sshx/internal/integration"
	"github.com/xiaot623/sshx/internal/sshcompat"
)

func TestIntegrationSessionWrapsEveryRemoteCommand(t *testing.T) {
	parsed := sshcompat.Parse([]string{"-T", "-D", "4123", "host", "bash", "-s"})
	got := integrationSessionSSHArgs(parsed, "context-id", "$HOME/.sshx/context")
	// Keep the forwarding options and target byte-for-byte before replacing the remote command.
	if len(got) != 5 || strings.Join(got[:4], " ") != "-T -D 4123 host" {
		t.Fatalf("connection args = %#v", got)
	}
	remoteCommand := got[len(got)-1]
	for _, want := range []string{"SSHX_CONTEXT_ID=", "context-id", "$HOME/.sshx/context/bin", `exec "$shell" -c`, "bash -s"} {
		if !strings.Contains(remoteCommand, want) {
			t.Fatalf("missing %q in remote command: %q", want, remoteCommand)
		}
	}
	for _, forbidden := range []string{"VSCODE_AGENT_FOLDER", "SERVER_DATA_DIR"} {
		if strings.Contains(remoteCommand, forbidden) {
			t.Fatalf("remote command contains application-specific variable %s: %q", forbidden, remoteCommand)
		}
	}
}

func TestIntegrationSessionLeavesNonShellActionsUntouched(t *testing.T) {
	for _, args := range [][]string{
		{"-N", "-L", "8080:localhost:80", "host"},
		{"-s", "host", "sftp"},
	} {
		parsed := sshcompat.Parse(args)
		got := integrationSessionSSHArgs(parsed, "context-id", "$HOME/.sshx/context")
		if strings.Join(got, " ") != strings.Join(args, " ") {
			t.Fatalf("args = %q, want %q", strings.Join(got, " "), strings.Join(args, " "))
		}
	}
}

func TestIntegrationRecognizesOpenSSHControlOperations(t *testing.T) {
	for _, args := range [][]string{{"-O", "check", "host"}, {"-Oexit", "host"}, {"host", "echo", "ok"}} {
		parsed := sshcompat.Parse(args)
		got := hasSSHControlOperation(parsed)
		want := strings.HasPrefix(args[0], "-O")
		if got != want {
			t.Fatalf("hasSSHControlOperation(%#v) = %v, want %v", args, got, want)
		}
	}
}

func TestIntegrationSessionWrapperExportsContext(t *testing.T) {
	home := t.TempDir()
	parsed := sshcompat.Parse([]string{"host", `printf '%s' "$SSHX_CONTEXT_ID|$PATH"`})
	got := integrationSessionSSHArgs(parsed, "context-id", "$HOME/.sshx/context")
	cmd := exec.Command("/bin/sh", "-c", got[len(got)-1])
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "SHELL=/bin/sh"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("wrapper failed: %v\n%s", err, out)
	}
	wantPrefix := "context-id|" + filepath.Join(home, ".sshx/context/bin") + ":"
	if !strings.HasPrefix(string(out), wantPrefix) {
		t.Fatalf("wrapper environment = %q, want prefix %q", out, wantPrefix)
	}
}

func TestIntegrationSessionWrapsDefaultLoginShell(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		wantPrefix string
	}{
		{[]string{"host"}, "-t host"},
		{[]string{"-T", "host"}, "-T host"},
	} {
		got := integrationSessionSSHArgs(sshcompat.Parse(tc.args), "context-id", "$HOME/.sshx/context")
		if len(got) < 2 || strings.Join(got[:len(got)-1], " ") != tc.wantPrefix || !strings.Contains(got[len(got)-1], `exec "$shell" -l`) {
			t.Fatalf("wrapped args = %#v", got)
		}
	}
}

func TestInsertBeforeTarget(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-T", "-D", "1234", "host", "bash"}, "-T -D 1234 -S /tmp/master host bash"},
		{[]string{"-T", "--", "host", "bash"}, "-T -S /tmp/master -- host bash"},
	} {
		parsed := sshcompat.Parse(tc.args)
		got := insertBeforeTarget(parsed, []string{"-S", "/tmp/master"})
		if strings.Join(got, " ") != tc.want {
			t.Fatalf("args = %q, want %q", strings.Join(got, " "), tc.want)
		}
	}
}

func TestStripControlOptions(t *testing.T) {
	got := stripControlOptions([]string{"-o", "ControlMaster=auto", "-S/tmp/old", "-p", "22", "host"})
	if strings.Join(got, " ") != "-p 22 host" {
		t.Fatalf("args = %#v", got)
	}
}

func TestSidecarReusesMasterWithoutRepeatingActionForwards(t *testing.T) {
	original := []string{"-T", "-D", "4123", "-L/tmp/code.sock:localhost:22", "-R", "8080:localhost:80", "-p", "2222", "host", "bash"}
	mainInput := sshcompat.Parse(stripControlOptions(original))
	mainArgs := insertBeforeTarget(mainInput, []string{"-o", "ControlMaster=yes", "-S", "/tmp/master"})
	mainJoined := strings.Join(mainArgs, " ")
	for _, preserved := range []string{"-D 4123", "-L/tmp/code.sock:localhost:22", "-R 8080:localhost:80"} {
		if !strings.Contains(mainJoined, preserved) {
			t.Fatalf("main args lost %q: %s", preserved, mainJoined)
		}
	}
	sidecarInput := sshcompat.Parse(stripAuxiliaryActionOptions(mainInput.Args))
	sidecarArgs := insertBeforeTarget(sidecarInput, []string{"-o", "ControlMaster=no", "-o", "ControlPath=/tmp/master", "-o", "ClearAllForwardings=yes"})
	sidecarJoined := strings.Join(sidecarArgs, " ")
	for _, forbidden := range []string{"-D", "4123", "-L", "code.sock", "-R", "8080:localhost:80", "ControlMaster=yes"} {
		if strings.Contains(sidecarJoined, forbidden) {
			t.Fatalf("sidecar args retained %q: %s", forbidden, sidecarJoined)
		}
	}
	for _, required := range []string{"-T", "-p 2222", "ControlMaster=no", "ControlPath=/tmp/master", "ClearAllForwardings=yes", "host bash"} {
		if !strings.Contains(sidecarJoined, required) {
			t.Fatalf("sidecar args lost %q: %s", required, sidecarJoined)
		}
	}
}

func TestSCPRemoteTarget(t *testing.T) {
	got := scpIdentityArgs([]string{"-P", "2222", "local", "alice@example.com:/tmp/a"})
	if strings.Join(got, " ") != "-p 2222 alice@example.com" {
		t.Fatalf("identity args = %#v", got)
	}
}

func TestSCPWithDifferentRemoteHostsIsAmbiguous(t *testing.T) {
	if got := scpIdentityArgs([]string{"alice@one:/tmp/a", "bob@two:/tmp/b"}); got != nil {
		t.Fatalf("ambiguous scp identity args = %#v", got)
	}
}

func TestIntegrationStartsOpenSSHBeforeStdinEOF(t *testing.T) {
	isolateHome(t)
	root := filepath.Join(t.TempDir(), "vscode")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	upstream := filepath.Join(t.TempDir(), "ssh")
	artifacts := t.TempDir()
	ready := filepath.Join(artifacts, "ready")
	received := filepath.Join(artifacts, "stdin")
	script := "#!/bin/sh\n: > " + shellQuote(ready) + "\ncat > " + shellQuote(received) + "\n"
	if err := os.WriteFile(upstream, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	invocation := filepath.Join(binDir, "ssh")
	if err := os.Symlink("/does/not/matter", invocation); err != nil {
		t.Fatal(err)
	}
	if err := integration.WriteDescriptor(filepath.Join(root, "integration.json"), integration.Descriptor{
		Schema: 1, Profile: integration.VSCode, SSHPath: upstream, SCPPath: upstream, DriverPath: "/does/not/matter",
	}); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte("strict: true\nfeatures:\n  commandBridge: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	runner := NewRunner(reader, stdout, stderr)
	runner.InvocationPath = invocation
	runner.ConfigPath = configPath
	runner.ResolveIdentity = func(context.Context, string, []string, string) (identity.Connection, error) {
		return identity.Connection{TargetID: "target-id", ContextID: "context-id", SessionID: "12345678-1234-4234-8234-123456789abc"}, nil
	}
	result := make(chan int, 1)
	go func() {
		result <- runner.Run(context.Background(), []string{"-T", "host", "bash"})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("OpenSSH was not started while integration stdin remained open")
		}
		time.Sleep(5 * time.Millisecond)
	}
	input := []byte("echo ready\n")
	if _, err := writer.Write(input); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-result:
		if code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("integration did not finish after stdin closed")
	}
	got, err := os.ReadFile(received)
	if err != nil || !bytes.Equal(got, input) {
		t.Fatalf("upstream stdin = %q, %v; want %q", got, err, input)
	}
}

func TestAdapterForwardsSignalToOpenSSHProcess(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancelCause(context.Background())
	runner := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	result := make(chan int, 1)
	go func() {
		result <- runner.execAdapterCommand(ctx, "/bin/sh", []string{"-c", "trap 'exit 42' TERM; : > " + shellQuote(ready) + "; while :; do :; done"}, strings.NewReader(""))
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel(processSignalCause{signal: syscall.SIGTERM})
	select {
	case code := <-result:
		if code != 42 {
			t.Fatalf("exit = %d, want trapped signal exit 42", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("signal was not forwarded")
	}
}

func TestAdapterReturnsWhenOpenSSHExitsWithoutReadingBootstrap(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	runner := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	result := make(chan int, 1)
	go func() {
		result <- runner.execAdapterCommand(context.Background(), "/bin/sh", []string{"-c", "exit 37"}, reader)
	}()
	select {
	case code := <-result:
		if code != 37 {
			t.Fatalf("exit = %d, want 37", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("adapter remained blocked on gated bootstrap stdin after OpenSSH exited")
	}
}
