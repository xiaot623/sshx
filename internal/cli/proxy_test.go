package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAllocatedProxyPort(t *testing.T) {
	if port, err := parseAllocatedPort([]byte("43123\n")); err != nil || port != 43123 {
		t.Fatalf("port = %d, %v", port, err)
	}
	for _, output := range []string{"", "not-a-port", "70000"} {
		if _, err := parseAllocatedPort([]byte(output)); err == nil {
			t.Fatalf("output %q was accepted", output)
		}
	}
}

func TestProxyControlOperationDoesNotRepeatUserForwards(t *testing.T) {
	args := []string{
		"-D", "1081",
		"-L", "8080:localhost:80",
		"-R", "9000:localhost:90",
		"-o", "ControlMaster=yes",
		"-o", "ControlPath=/tmp/old",
		"-o", "ClearAllForwardings=yes",
		"-o", "RemoteForward=9100:localhost:91",
		"-oExitOnForwardFailure=no",
		"-p", "2222",
		"host",
	}
	got := strings.Join(sshControlOperationArgs(args, "/tmp/sshx-master", "forward", "127.0.0.1:0:127.0.0.1:4567"), " ")
	for _, forbidden := range []string{"-D 1081", "-L 8080", "9000:localhost:90", "9100:localhost:91", "/tmp/old", "ClearAllForwardings=yes", "ExitOnForwardFailure=no"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("control operation retained %q: %s", forbidden, got)
		}
	}
	for _, required := range []string{"-S /tmp/sshx-master", "-O forward", "-R 127.0.0.1:0:127.0.0.1:4567", "-p 2222", "host"} {
		if !strings.Contains(got, required) {
			t.Fatalf("control operation lost %q: %s", required, got)
		}
	}
}

func TestProxyEnvironmentOverridesAndExtendsNoProxy(t *testing.T) {
	script := (proxyEnvironment{
		HTTP:  "http://sshx:secret@127.0.0.1:41000",
		SOCKS: "socks5h://sshx:secret@127.0.0.1:41000",
	}).script()
	merged := "internal,lower,localhost,127.0.0.1,::1"
	for _, tc := range []struct {
		name   string
		prefix string
		want   string
	}{
		{name: "both forms", prefix: "NO_PROXY=internal; no_proxy=lower", want: merged},
		{name: "uppercase only", prefix: "NO_PROXY=internal.example; unset no_proxy", want: "internal.example,localhost,127.0.0.1,::1"},
		{name: "lowercase only", prefix: "unset NO_PROXY; no_proxy=internal.example", want: "internal.example,localhost,127.0.0.1,::1"},
		{name: "neither form", prefix: "unset NO_PROXY; unset no_proxy", want: "localhost,127.0.0.1,::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := tc.prefix + "; " + script + `; printf '%s\n' "$HTTP_PROXY|$HTTPS_PROXY|$ALL_PROXY|$NO_PROXY|$no_proxy"`
			output, err := os.ReadFile(runShellToFile(t, command))
			if err != nil {
				t.Fatal(err)
			}
			got := string(output)
			for _, want := range []string{
				"http://sshx:secret@127.0.0.1:41000",
				"socks5h://sshx:secret@127.0.0.1:41000",
				tc.want + "|" + tc.want,
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("environment output missing %q: %q", want, got)
				}
			}
		})
	}
}

func TestIntegrationProxyWaitScriptIsSessionScoped(t *testing.T) {
	script := integrationProxyWaitScript("$HOME/.sshx/context", "session-a", true)
	for _, want := range []string{"session-a.env", "session-a.error", "-lt 60", "sleep 0.05", "exit 1"} {
		if !strings.Contains(script, want) {
			t.Fatalf("wait script missing %q: %s", want, script)
		}
	}
	if strings.Contains(script, "session-b") {
		t.Fatalf("wait script mixed sessions: %s", script)
	}
}

func TestStartProxyTunnelCreatesAndCancelsDynamicForward(t *testing.T) {
	for _, name := range []string{"SSHX_PROXY_URL", "ALL_PROXY", "all_proxy", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	var operations [][]string
	runner := NewRunner(strings.NewReader(""), ioDiscard{}, ioDiscard{})
	runner.ExecOutput = func(_ context.Context, _ string, args []string) ([]byte, error) {
		operations = append(operations, append([]string(nil), args...))
		if strings.Contains(strings.Join(args, " "), "-O forward") {
			return []byte("42123\n"), nil
		}
		return nil, nil
	}
	tunnel, err := runner.startProxyTunnel(context.Background(), []string{"-p", "2222", "host"}, "/tmp/master")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tunnel.environment.HTTP, "127.0.0.1:42123") || !strings.Contains(tunnel.environment.SOCKS, "127.0.0.1:42123") {
		t.Fatalf("environment = %#v", tunnel.environment)
	}
	tunnel.Close()
	if len(operations) != 2 {
		t.Fatalf("operations = %#v", operations)
	}
	if got := strings.Join(operations[1], " "); !strings.Contains(got, "-O cancel") || !strings.Contains(got, "127.0.0.1:0:127.0.0.1:") {
		t.Fatalf("cancel operation = %s", got)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func runShellToFile(t *testing.T, command string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "output")
	full := command + " > " + shellQuote(path)
	cmd := defaultExec
	if err := cmd(t.Context(), "/bin/sh", []string{"-c", full}); err != nil {
		t.Fatal(err)
	}
	return path
}
