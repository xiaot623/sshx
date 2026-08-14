package cli

import (
	"bytes"
	"context"
	"errors"
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
	got := strings.Join(sshControlOperationArgs(args, "/tmp/sshx-master", "forward", "R", "127.0.0.1:0"), " ")
	for _, forbidden := range []string{"-D 1081", "-L 8080", "9000:localhost:90", "9100:localhost:91", "/tmp/old", "ClearAllForwardings=yes", "ExitOnForwardFailure=no", "127.0.0.1:0:"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("control operation retained %q: %s", forbidden, got)
		}
	}
	for _, required := range []string{"-S /tmp/sshx-master", "-O forward", "-R 127.0.0.1:0", "-p 2222", "host"} {
		if !strings.Contains(got, required) {
			t.Fatalf("control operation lost %q: %s", required, got)
		}
	}
}

func TestControlOperationArgsLocalForwardUsesDashL(t *testing.T) {
	args := []string{"-t", "-L", "8080:localhost:80", "-p", "2222", "host"}
	got := strings.Join(sshControlOperationArgs(args, "/tmp/sshx-master", "forward", "L", "127.64.0.1:8080:127.0.0.1:8080"), " ")
	for _, required := range []string{"-S /tmp/sshx-master", "-O forward", "-L 127.64.0.1:8080:127.0.0.1:8080", "-p 2222", "host"} {
		if !strings.Contains(got, required) {
			t.Fatalf("local control operation lost %q: %s", required, got)
		}
	}
	for _, forbidden := range []string{"-t", "-L 8080:localhost:80", "-R "} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("local control operation retained %q: %s", forbidden, got)
		}
	}
}

func TestOwnControlMasterForAutoForwardWithoutProxy(t *testing.T) {
	for _, tc := range []struct {
		autoForward, useProxy, sidecar, want bool
	}{
		{true, false, false, true},
		{false, true, false, true},
		{true, true, false, true},
		{true, false, true, false},
		{false, true, true, false},
		{false, false, false, false},
	} {
		if got := ownControlMaster(tc.autoForward, tc.useProxy, tc.sidecar); got != tc.want {
			t.Fatalf("ownControlMaster(autoForward=%v, proxy=%v, sidecar=%v) = %v, want %v", tc.autoForward, tc.useProxy, tc.sidecar, got, tc.want)
		}
	}
}

func TestControlMasterArgsAppliedForAutoForward(t *testing.T) {
	got := strings.Join(controlMasterArgs([]string{"-t", "-L", "8080:localhost:80", "-p", "2222", "host"}, "/tmp/sshx-master"), " ")
	for _, required := range []string{"ControlMaster=yes", "ControlPersist=no", "-S /tmp/sshx-master", "ClearAllForwardings=yes", "-p 2222", "host"} {
		if !strings.Contains(got, required) {
			t.Fatalf("control master args lost %q: %s", required, got)
		}
	}
	for _, forbidden := range []string{"-t", "-L 8080:localhost:80"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("control master args retained %q: %s", forbidden, got)
		}
	}
}

func TestProxyEnvironmentOverridesAndExtendsNoProxy(t *testing.T) {
	script := (proxyEnvironment{
		URL: "socks5h://127.0.0.1:41000",
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
				"socks5h://127.0.0.1:41000|socks5h://127.0.0.1:41000|socks5h://127.0.0.1:41000",
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

func TestSkipOptionalProxyContinuesUnlessStrict(t *testing.T) {
	err := errors.New("create reverse proxy forwarding: remote port forwarding failed")

	var stderr bytes.Buffer
	runner := NewRunner(strings.NewReader(""), ioDiscard{}, &stderr)
	if !runner.skipOptionalProxy("host", err) {
		t.Fatal("non-strict mode aborted optional proxy failure")
	}
	if !strings.Contains(stderr.String(), "sshx: proxy skipped for host") || !strings.Contains(stderr.String(), err.Error()) {
		t.Fatalf("stderr = %q", stderr.String())
	}

	stderr.Reset()
	runner.strict = true
	if runner.skipOptionalProxy("host", err) {
		t.Fatal("strict mode skipped required proxy failure")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestStartProxyTunnelCreatesAndCancelsDynamicForward(t *testing.T) {
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
	if tunnel.environment.URL != "socks5h://127.0.0.1:42123" {
		t.Fatalf("environment = %#v", tunnel.environment)
	}
	if strings.Contains(tunnel.environment.script(), "@") {
		t.Fatalf("proxy environment contains credentials: %s", tunnel.environment.script())
	}
	tunnel.Close()
	if len(operations) != 2 {
		t.Fatalf("operations = %#v", operations)
	}
	forwarded := strings.Join(operations[0], " ")
	if !strings.Contains(forwarded, "-O forward") || !strings.Contains(forwarded, "-R 127.0.0.1:0") {
		t.Fatalf("forward operation = %s", forwarded)
	}
	if strings.Contains(forwarded, "127.0.0.1:0:") {
		t.Fatalf("forward spec included a destination: %s", forwarded)
	}
	canceled := strings.Join(operations[1], " ")
	if !strings.Contains(canceled, "-O cancel") || !strings.Contains(canceled, "-R 127.0.0.1:0") {
		t.Fatalf("cancel operation = %s", canceled)
	}
	if strings.Contains(canceled, "127.0.0.1:0:") {
		t.Fatalf("cancel spec included a destination: %s", canceled)
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
