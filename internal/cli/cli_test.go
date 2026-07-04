package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xiaot623/sshx/internal/config"
	"github.com/xiaot623/sshx/internal/locald"
	"github.com/xiaot623/sshx/internal/version"
)

type execCall struct {
	name string
	args []string
}

func sameVersionRemoteProbe() []byte {
	return []byte("Linux\nx86_64\n" + version.Version + "\n" + version.Version + "\n1\n")
}

func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

func TestLocalIsReservedWithoutBridgeSocket(t *testing.T) {
	t.Setenv("SSHX_BRIDGE_SOCKET", "")
	var stderr bytes.Buffer
	r := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &stderr)
	r.Exec = func(context.Context, string, []string) error {
		t.Fatal("ssh should not be executed for reserved local target")
		return nil
	}
	code := r.Run(context.Background(), []string{"local", "uname", "-a"})
	if code == 0 {
		t.Fatal("exit code = 0")
	}
	if !IsReservedLocalError(stderr.String()) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestReadLocalBridgeStdinReadsPipedInput(t *testing.T) {
	stdin, err := readLocalBridgeStdin(strings.NewReader("stdin-ok"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stdin) != "stdin-ok" {
		t.Fatalf("stdin = %q", stdin)
	}
}

func TestReadLocalBridgeStdinSkipsCharacterDevice(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stdin, err := readLocalBridgeStdin(f)
	if err != nil {
		t.Fatal(err)
	}
	if stdin != nil {
		t.Fatalf("stdin = %#v, want nil", stdin)
	}
}

func TestVersionFlagPrintsClientVersion(t *testing.T) {
	old := version.Version
	version.Version = "1.2.3-rc.1"
	t.Cleanup(func() { version.Version = old })

	var stdout bytes.Buffer
	r := NewRunner(strings.NewReader(""), &stdout, &bytes.Buffer{})
	code := r.Run(context.Background(), []string{"--version"})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if stdout.String() != "sshx 1.2.3-rc.1\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRecordVersionStateRotatesCurrentToLast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "version-state.json")
	if err := recordVersionState(path, "0.0.1-rc.1"); err != nil {
		t.Fatal(err)
	}
	if err := recordVersionState(path, "0.0.1"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"current_version": "0.0.1"`) ||
		!strings.Contains(string(b), `"last_version": "0.0.1-rc.1"`) {
		t.Fatalf("version state = %s", b)
	}
}

func TestNoWrapDelegatesToSSHWithoutNoWrapFlag(t *testing.T) {
	t.Setenv("SSHX_CONFIG", "")
	var calls []execCall
	r := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	r.ConfigPath = filepath.Join(t.TempDir(), "missing.yaml")
	r.Exec = func(_ context.Context, name string, args []string) error {
		calls = append(calls, execCall{name: name, args: append([]string(nil), args...)})
		return nil
	}
	code := r.Run(context.Background(), []string{"--no-wrap", "-p", "2222", "remote"})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if len(calls) != 1 || calls[0].name != "ssh" || !reflect.DeepEqual(calls[0].args, []string{"-p", "2222", "remote"}) {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestEnsureRemoteServerInstallsClientVersionFromLocalDownload(t *testing.T) {
	old := version.Version
	version.Version = "1.2.3-rc.1"
	t.Cleanup(func() { version.Version = old })

	dir := t.TempDir()
	localBinary := filepath.Join(dir, "sshx-linux-arm64")
	if err := os.WriteFile(localBinary, []byte("binary-data"), 0o755); err != nil {
		t.Fatal(err)
	}

	var downloadedVersion string
	var downloadedAsset string
	var uploaded []byte
	var execCalls []execCall
	r := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	r.ExecOutput = func(context.Context, string, []string) ([]byte, error) {
		return []byte("Linux\naarch64\n1.2.3-rc.0\n1.2.3-rc.0\n1\n"), nil
	}
	r.DownloadBinary = func(_ context.Context, targetVersion, assetName string) (string, error) {
		downloadedVersion = targetVersion
		downloadedAsset = assetName
		return localBinary, nil
	}
	r.ExecInput = func(_ context.Context, name string, args []string, stdin io.Reader) error {
		var err error
		uploaded, err = io.ReadAll(stdin)
		if err != nil {
			return err
		}
		return nil
	}
	r.Exec = func(_ context.Context, name string, args []string) error {
		execCalls = append(execCalls, execCall{name: name, args: append([]string(nil), args...)})
		return nil
	}

	err := r.ensureRemoteServer(context.Background(), []string{"remote"}, config.Features{CommandBridge: true})
	if err != nil {
		t.Fatal(err)
	}
	if downloadedVersion != "1.2.3-rc.1" || downloadedAsset != "sshx-linux-arm64" {
		t.Fatalf("download = %q %q", downloadedVersion, downloadedAsset)
	}
	if string(uploaded) != "binary-data" {
		t.Fatalf("uploaded = %q", uploaded)
	}
	if len(execCalls) != 2 {
		t.Fatalf("exec calls = %#v", execCalls)
	}
}

func TestEnsureRemoteServerRestartsWhenRunningVersionIsUnknown(t *testing.T) {
	old := version.Version
	version.Version = "1.2.3"
	t.Cleanup(func() { version.Version = old })

	var execCalls []execCall
	r := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	r.ExecOutput = func(context.Context, string, []string) ([]byte, error) {
		return []byte("Linux\nx86_64\n1.2.3\n\n1\n"), nil
	}
	r.DownloadBinary = func(context.Context, string, string) (string, error) {
		t.Fatal("binary should not be downloaded when installed binary already matches")
		return "", nil
	}
	r.Exec = func(_ context.Context, name string, args []string) error {
		execCalls = append(execCalls, execCall{name: name, args: append([]string(nil), args...)})
		return nil
	}

	err := r.ensureRemoteServer(context.Background(), []string{"remote"}, config.Features{CommandBridge: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(execCalls) != 2 {
		t.Fatalf("exec calls = %#v", execCalls)
	}
}

func TestConfigPathCanBeOverriddenByEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sshx.yaml")
	t.Setenv("SSHX_CONFIG", path)
	r := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if r.ConfigPath != path {
		t.Fatalf("ConfigPath = %q, want %q", r.ConfigPath, path)
	}
}

func TestInstallResolverPrintsResolverFileWithoutApplying(t *testing.T) {
	var stdout bytes.Buffer
	r := NewRunner(strings.NewReader(""), &stdout, &bytes.Buffer{})
	code := r.Run(context.Background(), []string{"install-resolver", "--suffix", "xiaot.sshx", "--dns-addr", "127.0.0.1:53535"})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "/etc/resolver/xiaot.sshx") ||
		!strings.Contains(out, "nameserver 127.0.0.1") ||
		!strings.Contains(out, "port 53535") {
		t.Fatalf("unexpected resolver output: %q", out)
	}
}

func TestForwardTypoAliasListsForwardedPorts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir, err := os.MkdirTemp("/tmp", "sshx-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "local.sock")
	srv := &locald.Server{SocketPath: socket}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ctx)
	}()
	waitForLocalDaemonSocket(t, socket)
	t.Setenv("SSHX_LOCAL_DAEMON_SOCKET", socket)

	remotePort := freeLocalTCPPort(t)
	resp, err := locald.ClientRequest(ctx, socket, locald.Request{
		Type:       locald.TypeEnsurePort,
		SSHPath:    "ssh",
		Target:     "debian",
		RemotePort: remotePort,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("ensure response = %#v", resp)
	}

	var stdout bytes.Buffer
	r := NewRunner(strings.NewReader(""), &stdout, &bytes.Buffer{})
	code := r.Run(ctx, []string{"forwrad"})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	want := itoa(resp.LocalPort) + " -> debian:" + itoa(remotePort) + "\n"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
}

func TestDisabledGlobalFeaturesDelegateWithoutSideEffects(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
commands:
  deny:
    - rm
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []execCall
	r := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	r.ConfigPath = configPath
	r.Exec = func(_ context.Context, name string, args []string) error {
		calls = append(calls, execCall{name: name, args: append([]string(nil), args...)})
		return nil
	}
	code := r.Run(context.Background(), []string{"remote", "uname", "-a"})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].args, []string{"remote", "uname", "-a"}) {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestGlobalDomainFeatureEnsuresResolver(t *testing.T) {
	isolateHome(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
features:
  domains:
    enabled: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var ensured bool
	r := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	r.ConfigPath = configPath
	r.EnsureResolver = func(_ context.Context, cfg config.DomainsFeature) error {
		ensured = cfg.Enabled
		return nil
	}
	r.ExecOutput = func(context.Context, string, []string) ([]byte, error) {
		return nil, errors.New("remote server unavailable")
	}
	r.Exec = func(_ context.Context, _ string, args []string) error {
		if reflect.DeepEqual(args, []string{"remote"}) {
			return nil
		}
		return errors.New("remote server unavailable")
	}
	code := r.Run(context.Background(), []string{"remote"})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !ensured {
		t.Fatal("resolver was not ensured")
	}
}

func TestGlobalDomainFeatureStartsBridgeWithoutCommandBridge(t *testing.T) {
	isolateHome(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
features:
  domains:
    enabled: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var bridgeStarted bool
	r := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	r.ConfigPath = configPath
	r.EnsureResolver = func(context.Context, config.DomainsFeature) error { return nil }
	r.ExecOutput = func(context.Context, string, []string) ([]byte, error) {
		return sameVersionRemoteProbe(), nil
	}
	r.StartBridge = func(context.Context, string, []string) (func(), error) {
		bridgeStarted = true
		return func() {}, nil
	}
	r.Exec = func(_ context.Context, _ string, args []string) error {
		if strings.Contains(strings.Join(args, " "), "test -S ~/.sshx/sock") {
			return nil
		}
		return nil
	}
	code := r.Run(context.Background(), []string{"remote"})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !bridgeStarted {
		t.Fatal("bridge was not started")
	}
}

func TestInternalSSHUsesOptionsBeforeTargetAndExcludesRemoteCommand(t *testing.T) {
	isolateHome(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
features:
  commandBridge: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []execCall
	var bridgeArgs []string
	var probeArgs []string
	r := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	r.ConfigPath = configPath
	r.ExecOutput = func(_ context.Context, _ string, args []string) ([]byte, error) {
		probeArgs = append([]string(nil), args...)
		return sameVersionRemoteProbe(), nil
	}
	r.StartBridge = func(_ context.Context, _ string, sshArgs []string) (func(), error) {
		bridgeArgs = append([]string(nil), sshArgs...)
		return func() {}, nil
	}
	r.Exec = func(_ context.Context, name string, args []string) error {
		calls = append(calls, execCall{name: name, args: append([]string(nil), args...)})
		return nil
	}
	code := r.Run(context.Background(), []string{"-p", "2222", "-J", "jump", "remote", "uname", "-s"})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	wantBase := []string{"-p", "2222", "-J", "jump", "remote"}
	if !reflect.DeepEqual(bridgeArgs, wantBase) {
		t.Fatalf("bridge args = %#v, want %#v", bridgeArgs, wantBase)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %#v", calls)
	}
	if !reflect.DeepEqual(probeArgs[:len(wantBase)+1], append([]string{"-n"}, wantBase...)) {
		t.Fatalf("internal ssh args = %#v", probeArgs)
	}
	if len(probeArgs) != len(wantBase)+2 || !strings.HasPrefix(probeArgs[len(probeArgs)-1], "sh -lc ") {
		t.Fatalf("internal ssh args = %#v", probeArgs)
	}
	if !reflect.DeepEqual(calls[0].args, []string{"-p", "2222", "-J", "jump", "remote", "uname", "-s"}) {
		t.Fatalf("delegated ssh args = %#v", calls[0].args)
	}
}

func TestQuotedRemoteCommandIsDelegatedButNotUsedForInternalSSH(t *testing.T) {
	isolateHome(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
features:
  commandBridge: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []execCall
	var probeArgs []string
	r := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	r.ConfigPath = configPath
	r.ExecOutput = func(_ context.Context, _ string, args []string) ([]byte, error) {
		probeArgs = append([]string(nil), args...)
		return sameVersionRemoteProbe(), nil
	}
	r.StartBridge = func(context.Context, string, []string) (func(), error) {
		return func() {}, nil
	}
	r.Exec = func(_ context.Context, name string, args []string) error {
		calls = append(calls, execCall{name: name, args: append([]string(nil), args...)})
		return nil
	}
	code := r.Run(context.Background(), []string{"remote", "~/.sshx/bin/sshx local uname -s"})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %#v", calls)
	}
	if strings.Contains(strings.Join(probeArgs, " "), "local uname") {
		t.Fatalf("internal ssh args included quoted remote command: %#v", probeArgs)
	}
	if !reflect.DeepEqual(calls[0].args, []string{"remote", "~/.sshx/bin/sshx local uname -s"}) {
		t.Fatalf("delegated ssh args = %#v", calls[0].args)
	}
}

func TestSSHCommandArgsCanKeepStdioOpen(t *testing.T) {
	got := sshCommandArgs([]string{"-p", "2222", "remote"}, "socket-proxy")
	want := []string{"-p", "2222", "remote", "socket-proxy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ssh command args = %#v, want %#v", got, want)
	}
}

func TestGlobalNonStrictFallsBackToSSHWhenServerUnavailable(t *testing.T) {
	isolateHome(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
features:
  commandBridge: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []execCall
	r := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	r.ConfigPath = configPath
	r.ExecOutput = func(context.Context, string, []string) ([]byte, error) {
		return nil, errors.New("remote server unavailable")
	}
	r.Exec = func(_ context.Context, name string, args []string) error {
		calls = append(calls, execCall{name: name, args: append([]string(nil), args...)})
		return nil
	}
	code := r.Run(context.Background(), []string{"remote"})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].args, []string{"remote"}) {
		t.Fatalf("calls = %#v", calls)
	}
}

func TestGlobalStrictFailsWhenServerUnavailable(t *testing.T) {
	isolateHome(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(configPath, []byte(`
strict: true
features:
  commandBridge: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	var calls []execCall
	r := NewRunner(strings.NewReader(""), &bytes.Buffer{}, &stderr)
	r.ConfigPath = configPath
	r.ExecOutput = func(context.Context, string, []string) ([]byte, error) {
		return nil, errors.New("remote server unavailable")
	}
	r.Exec = func(_ context.Context, name string, args []string) error {
		calls = append(calls, execCall{name: name, args: append([]string(nil), args...)})
		return errors.New("remote server unavailable")
	}
	code := r.Run(context.Background(), []string{"remote"})
	if code == 0 {
		t.Fatal("exit code = 0")
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %#v", calls)
	}
	if !strings.Contains(stderr.String(), "remote server unavailable") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestResolverContentUsesConfiguredDNSAddress(t *testing.T) {
	content, err := resolverContent("127.0.0.1:53535")
	if err != nil {
		t.Fatal(err)
	}
	if content != "nameserver 127.0.0.1\nport 53535\n" {
		t.Fatalf("content = %q", content)
	}
}

func waitForLocalDaemonSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.Dial("unix", path); err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s was not created", path)
}

func freeLocalTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
