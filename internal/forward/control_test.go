package forward

import (
	"strings"
	"testing"
)

func TestControlOperationArgsLocalForward(t *testing.T) {
	args := []string{
		"-t",
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
	spec := LocalForwardSpec("127.64.0.1", 8080, "127.0.0.1")
	got := strings.Join(ControlOperationArgs(args, "/tmp/sshx-master", "forward", "L", spec), " ")
	for _, forbidden := range []string{
		"-t",
		"-D 1081",
		"-L 8080",
		"9000:localhost:90",
		"9100:localhost:91",
		"/tmp/old",
		"ClearAllForwardings=yes",
		"ExitOnForwardFailure=no"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("control operation retained %q: %s", forbidden, got)
		}
	}
	for _, required := range []string{
		"-S /tmp/sshx-master",
		"-O forward",
		"-L 127.64.0.1:8080:127.0.0.1:8080",
		"-p 2222",
		"host"} {
		if !strings.Contains(got, required) {
			t.Fatalf("control operation lost %q: %s", required, got)
		}
	}
	if strings.Contains(got, "-R ") {
		t.Fatalf("local forward used -R: %s", got)
	}
}

func TestLocalForwardSpecFormatsIPv6RemoteHost(t *testing.T) {
	got := LocalForwardSpec("127.64.0.1", 8080, "::1")
	if got != "127.64.0.1:8080:[::1]:8080" {
		t.Fatalf("spec = %q", got)
	}
}

func TestNormalizeRemoteHost(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", "127.0.0.1"},
		{"localhost", "127.0.0.1"},
		{"LOCALHOST", "127.0.0.1"},
		{"127.0.0.1", "127.0.0.1"},
		{"::1", "::1"},
		{"[::1]", "::1"},
	} {
		if got := NormalizeRemoteHost(tc.in); got != tc.want {
			t.Fatalf("NormalizeRemoteHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCleanSSHArgsStripsUserForwardsAndTTY(t *testing.T) {
	got := strings.Join(
		CleanSSHArgs([]string{"-tt", "-L", "80:localhost:80", "-R", "90:localhost:90", "-D", "1080", "-p", "22", "host"}),
		" ")
	if got != "-p 22 host" {
		t.Fatalf("cleaned = %q", got)
	}
}
