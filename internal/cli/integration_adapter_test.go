package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaot623/sshx/internal/integration"
	"github.com/xiaot623/sshx/internal/sshcompat"
)

func TestTransformVSCodeBootstrap(t *testing.T) {
	script := []byte("#!/bin/sh\nVSCODE_AGENT_FOLDER=$HOME/.vscode-server\necho ready\n")
	got, ok := transformBootstrap(integration.VSCode, script, "context", "$HOME/.sshx/context")
	if !ok {
		t.Fatal("script was not recognized")
	}
	for _, want := range []string{"VSCODE_AGENT_FOLDER=\"${VSCODE_AGENT_FOLDER%/}/sshx/context\"", "SSHX_CONTEXT_ID='context'", "$HOME/.sshx/context/bin:$PATH"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"SSHX_SESSION_ID", "SSHX_BRIDGE_SOCKET", "SSHX_BRIDGE_TOKEN", "SSHX_RUNTIME_ID", "SSHX_SERVER_HOME", "SSHX_REMOTE_FS"} {
		if strings.Contains(string(got), forbidden) {
			t.Fatalf("bootstrap leaked %s into the application terminal environment:\n%s", forbidden, got)
		}
	}
}

func TestTransformCursorRequiresBothAnchors(t *testing.T) {
	if _, ok := transformBootstrap(integration.Cursor, []byte("VSCODE_AGENT_FOLDER=x\n"), "c", "h"); ok {
		t.Fatal("Cursor script without SERVER_DATA_DIR was accepted")
	}
	script := []byte("SERVER_DATA_DIR=$HOME/.cursor-server\nVSCODE_AGENT_FOLDER=$SERVER_DATA_DIR\n")
	got, ok := transformBootstrap(integration.Cursor, script, "c", "h")
	if !ok || !strings.Contains(string(got), "SERVER_DATA_DIR=\"${SERVER_DATA_DIR%/}/sshx/c\"") {
		t.Fatalf("Cursor transform = %v\n%s", ok, got)
	}
}

func TestSupportedRemoteSSHScriptFixtures(t *testing.T) {
	fixtures := []struct {
		profile integration.Profile
		path    string
	}{
		{integration.VSCode, "testdata/vscode-remote-ssh-0.122.0/local-server.sh"},
		{integration.VSCode, "testdata/vscode-remote-ssh-0.122.0/exec-server.sh"},
		{integration.Cursor, "testdata/cursor-remote-ssh-1.1.10/dynamic-forward.sh"},
		{integration.Cursor, "testdata/cursor-remote-ssh-1.1.10/socket-forward.sh"},
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture.path), func(t *testing.T) {
			script, err := os.ReadFile(fixture.path)
			if err != nil {
				t.Fatal(err)
			}
			transformed, ok := transformBootstrap(fixture.profile, script, "context-id", "$HOME/.sshx/context")
			if !ok {
				t.Fatal("fixture was not recognized")
			}
			if !strings.Contains(string(transformed), "/sshx/context-id") || !strings.Contains(string(transformed), "SSHX_CONTEXT_ID='context-id'") {
				t.Fatalf("fixture was not isolated and injected:\n%s", transformed)
			}
		})
	}
}

func TestInsertBeforeTarget(t *testing.T) {
	parsed := sshcompat.Parse([]string{"-T", "-D", "1234", "host", "bash"})
	got := insertBeforeTarget(parsed, []string{"-S", "/tmp/master"})
	want := "-T -D 1234 -S /tmp/master host bash"
	if strings.Join(got, " ") != want {
		t.Fatalf("args = %q, want %q", strings.Join(got, " "), want)
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

func TestUnknownBootstrapIsExactPassthroughEvenWithStrictConfig(t *testing.T) {
	isolateHome(t)
	root := filepath.Join(t.TempDir(), "vscode")
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	upstream := filepath.Join(t.TempDir(), "ssh")
	script := `#!/bin/sh
if [ "${1:-}" = "-G" ]; then
  printf '%s\n' 'user alice' 'hostname example.com' 'port 22' 'hostkeyalias none'
  exit 0
fi
printf 'args:'
for arg in "$@"; do printf '<%s>' "$arg"; done
printf '\nstdin:'
cat
`
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
	input := "#!/bin/sh\necho not-a-vscode-bootstrap\n"
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	runner := NewRunner(strings.NewReader(input), stdout, stderr)
	runner.InvocationPath = invocation
	runner.ConfigPath = configPath
	code := runner.Run(context.Background(), []string{"-T", "host", "bash"})
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
	}
	want := "args:<-T><host><bash>\nstdin:" + input
	if stdout.String() != want {
		t.Fatalf("passthrough = %q, want %q", stdout.String(), want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("adapter polluted stderr: %q", stderr.String())
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
