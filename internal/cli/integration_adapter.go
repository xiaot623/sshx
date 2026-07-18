package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xiaot623/sshx/internal/config"
	"github.com/xiaot623/sshx/internal/identity"
	"github.com/xiaot623/sshx/internal/integration"
	"github.com/xiaot623/sshx/internal/sshcompat"
)

type activeIntegrationSession struct {
	TargetID    string `json:"targetId"`
	ContextID   string `json:"contextId"`
	SessionID   string `json:"sessionId"`
	ControlPath string `json:"controlPath"`
	PID         int    `json:"pid"`
}

func (r *Runner) runIntegrationAdapter(ctx context.Context, invocation string, args []string) int {
	descriptor, err := integration.DescriptorForInvocation(invocation)
	if err != nil {
		return r.execAdapterCommand(ctx, "ssh", args, r.Stdin)
	}
	if filepath.Base(invocation) == "scp" {
		cleanArgs, noWrap := removeNoWrap(args)
		if noWrap || os.Getenv("SSHX_DISABLE") == "1" {
			return r.execAdapterCommand(ctx, descriptor.SCPPath, cleanArgs, r.Stdin)
		}
		return r.runIntegratedSCP(ctx, descriptor, args)
	}
	parsed := sshcompat.Parse(args)
	if parsed.NoWrap || os.Getenv("SSHX_DISABLE") == "1" || parsed.InfoMode || parsed.Target == "" || hasSSHControlOperation(parsed) {
		return r.execAdapterCommand(ctx, descriptor.SSHPath, parsed.Args, r.Stdin)
	}
	if err := config.EnsureDefault(r.ConfigPath); err != nil {
		r.logIntegration(descriptor.Profile, err)
		return r.execAdapterCommand(ctx, descriptor.SSHPath, parsed.Args, r.Stdin)
	}
	cfg, err := config.Load(r.ConfigPath)
	if err != nil || !cfg.Features.Enabled() {
		if err != nil {
			r.logIntegration(descriptor.Profile, err)
		}
		return r.execAdapterCommand(ctx, descriptor.SSHPath, parsed.Args, r.Stdin)
	}
	connection, err := r.ResolveIdentity(ctx, descriptor.SSHPath, baseSSHArgs(parsed), string(descriptor.Profile))
	if err != nil {
		r.logIntegration(descriptor.Profile, err)
		return r.execAdapterCommand(ctx, descriptor.SSHPath, parsed.Args, r.Stdin)
	}
	remoteHome := remoteServerHome(connection.TargetID)
	contextHome := remoteContextHome(connection.TargetID, connection.ContextID)

	var controlDir, controlPath string
	controlMaster := false
	if session, ok := readIntegrationSession(descriptor.Profile, connection.TargetID); ok {
		controlPath = session.ControlPath
	} else {
		controlDir, controlPath, err = newControlPath(connection.SessionID)
		if err != nil {
			r.logIntegration(descriptor.Profile, err)
			return r.execAdapterCommand(ctx, descriptor.SSHPath, parsed.Args, r.Stdin)
		}
		controlMaster = true
		defer os.RemoveAll(controlDir)
	}

	mainParsedInput := sshcompat.Parse(stripControlOptions(integrationSessionSSHArgs(parsed, connection.ContextID, contextHome)))
	controlOptions := []string{"-o", "ControlMaster=no", "-S", controlPath}
	if controlMaster {
		controlOptions = []string{"-o", "ControlMaster=yes", "-o", "ControlPersist=no", "-S", controlPath}
	}
	mainArgs := insertBeforeTarget(mainParsedInput, controlOptions)

	sidecarParsedInput := sshcompat.Parse(stripAuxiliaryActionOptions(stripControlOptions(parsed.Args)))
	sidecarArgs := insertBeforeTarget(sidecarParsedInput, []string{"-o", "ControlMaster=no", "-o", "ControlPath=" + controlPath, "-o", "ClearAllForwardings=yes"})
	sidecarBase := baseSSHArgs(sshcompat.Parse(sidecarArgs))

	mainCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runIntegrationSidecar(mainCtx, descriptor, cfg, connection, parsed.Target, sidecarBase, remoteHome, controlPath)
	}()

	exitCode := r.execAdapterCommand(mainCtx, descriptor.SSHPath, mainArgs, r.Stdin)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
	return exitCode
}

func hasSSHControlOperation(parsed sshcompat.Parsed) bool {
	for i := 0; i < parsed.TargetIndex; i++ {
		if strings.HasPrefix(parsed.Args[i], "-O") {
			return true
		}
	}
	return false
}

func removeNoWrap(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	found := false
	for _, arg := range args {
		if arg == "--no-wrap" {
			found = true
			continue
		}
		out = append(out, arg)
	}
	return out, found
}

func (r *Runner) runIntegrationSidecar(
	ctx context.Context,
	descriptor integration.Descriptor,
	cfg config.Config,
	connection identity.Connection,
	target string,
	sshArgs []string,
	remoteHome string,
	controlPath string,
) {
	if !waitForPath(ctx, controlPath) {
		r.logIntegration(descriptor.Profile, errors.New("OpenSSH control master did not become ready"))
		return
	}
	registryPath := integrationSessionPath(descriptor.Profile, connection.TargetID, connection.SessionID)
	if err := writeIntegrationSession(registryPath, activeIntegrationSession{
		TargetID: connection.TargetID, ContextID: connection.ContextID, SessionID: connection.SessionID,
		ControlPath: controlPath, PID: os.Getpid(),
	}); err != nil {
		r.logIntegration(descriptor.Profile, err)
	}
	defer removeIntegrationSession(registryPath, controlPath)

	logWriter, closeLog := r.integrationLogWriter(descriptor.Profile)
	defer closeLog()
	sidecarRunner := NewRunner(bytes.NewReader(nil), io.Discard, logWriter)
	sidecarRunner.SSHPath = descriptor.SSHPath
	sidecarRunner.ConfigPath = r.ConfigPath
	sidecarRunner.connection = connection
	sidecarRunner.commandPolicy = cfg.Commands
	sidecarRunner.commandBridge = cfg.Features.CommandBridge
	sidecarRunner.autoForward = cfg.Features.AutoForward
	sidecarRunner.remoteFS = cfg.Features.RemoteFS
	sidecarRunner.integrationSidecar = true

	sidecarFeatures := cfg.Features
	if sidecarRunner.autoForward {
		if err := sidecarRunner.EnsureResolver(ctx); err != nil {
			_, _ = fmt.Fprintf(logWriter, "%s resolver: %v\n", time.Now().UTC().Format(time.RFC3339Nano), err)
			sidecarRunner.autoForward = false
			sidecarFeatures.AutoForward = false
		}
	}
	if err := sidecarRunner.ensureRemoteServer(ctx, sshArgs, sidecarFeatures, remoteHome); err != nil {
		r.logIntegration(descriptor.Profile, fmt.Errorf("remote server: %w", err))
		return
	}
	if err := sidecarRunner.ensureRemoteContextLauncher(ctx, sshArgs, connection, remoteHome, cfg.Features.RemoteFS); err != nil {
		r.logIntegration(descriptor.Profile, fmt.Errorf("context launcher: %w", err))
		return
	}
	sidecar, err := sidecarRunner.StartBridge(ctx, target, sshArgs, remoteHome)
	if err != nil {
		r.logIntegration(descriptor.Profile, fmt.Errorf("sidecar: %w", err))
		return
	}
	defer sidecar.Stop()
	<-ctx.Done()
}

func (r *Runner) runIntegratedSCP(ctx context.Context, descriptor integration.Descriptor, args []string) int {
	identityArgs := scpIdentityArgs(args)
	if len(identityArgs) > 0 {
		connection, err := r.ResolveIdentity(ctx, descriptor.SSHPath, identityArgs, string(descriptor.Profile))
		if err == nil {
			if session, ok := readIntegrationSession(descriptor.Profile, connection.TargetID); ok {
				withControl := append([]string{"-o", "ControlMaster=no", "-o", "ControlPath=" + session.ControlPath}, args...)
				return r.execAdapterCommand(ctx, descriptor.SCPPath, withControl, r.Stdin)
			}
		}
	}
	return r.execAdapterCommand(ctx, descriptor.SCPPath, args, r.Stdin)
}

func (r *Runner) execAdapterCommand(ctx context.Context, path string, args []string, stdin io.Reader) int {
	cmd := exec.Command(path, args...)
	cmd.Stdout = r.Stdout
	cmd.Stderr = r.Stderr
	var childStdin io.WriteCloser
	if stdin != nil {
		var err error
		childStdin, err = cmd.StdinPipe()
		if err != nil {
			return 1
		}
	}
	if err := cmd.Start(); err != nil {
		if childStdin != nil {
			_ = childStdin.Close()
		}
		return 1
	}
	stdinDone := make(chan struct{})
	if childStdin == nil {
		close(stdinDone)
	} else {
		go func() {
			_, _ = io.Copy(childStdin, stdin)
			_ = childStdin.Close()
			close(stdinDone)
		}()
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			if cause, ok := context.Cause(ctx).(interface{ processSignal() os.Signal }); ok {
				_ = cmd.Process.Signal(cause.processSignal())
			} else {
				_ = cmd.Process.Kill()
			}
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	// The source reader can remain open after OpenSSH exits (for example a TTY
	// or a caller-owned pipe), so stop our copy goroutine without delaying exit.
	if closer, ok := stdin.(interface{ CloseWithError(error) error }); ok {
		_ = closer.CloseWithError(io.ErrClosedPipe)
	}
	select {
	case <-stdinDone:
	case <-time.After(time.Second):
	}
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
	}
	return 1
}

func insertBeforeTarget(parsed sshcompat.Parsed, options []string) []string {
	if parsed.TargetIndex < 0 || parsed.TargetIndex > len(parsed.Args) {
		return append([]string(nil), parsed.Args...)
	}
	insertAt := parsed.TargetIndex
	if insertAt > 0 && parsed.Args[insertAt-1] == "--" {
		insertAt--
	}
	out := make([]string, 0, len(parsed.Args)+len(options))
	out = append(out, parsed.Args[:insertAt]...)
	out = append(out, options...)
	out = append(out, parsed.Args[insertAt:]...)
	return out
}

func stripControlOptions(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-S" {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-S") && len(arg) > 2 {
			continue
		}
		if arg == "-o" && i+1 < len(args) {
			if isControlSSHOption(args[i+1]) {
				i++
				continue
			}
			out = append(out, arg, args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(arg, "-o") && len(arg) > 2 && isControlSSHOption(arg[2:]) {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func stripAuxiliaryActionOptions(args []string) []string {
	valueActions := map[string]bool{"-D": true, "-L": true, "-R": true, "-W": true}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if valueActions[arg] {
			i++
			continue
		}
		if len(arg) > 2 && valueActions[arg[:2]] {
			continue
		}
		if arg == "-N" || arg == "-f" {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func isControlSSHOption(value string) bool {
	key, _, _ := strings.Cut(value, "=")
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "controlmaster", "controlpath", "controlpersist":
		return true
	default:
		return false
	}
}

func newControlPath(sessionID string) (string, string, error) {
	root := filepath.Join(os.TempDir(), "sshx-"+strconv.Itoa(os.Getuid()), "c")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", "", err
	}
	dir, err := os.MkdirTemp(root, sessionID[:8]+"-")
	if err != nil {
		return "", "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", err
	}
	return dir, filepath.Join(dir, "m"), nil
}

func waitForPath(ctx context.Context, path string) bool {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func integrationSessionsRoot(profile integration.Profile) string {
	return filepath.Join(os.TempDir(), "sshx-"+strconv.Itoa(os.Getuid()), "integrations", string(profile), "sessions")
}

func integrationSessionPath(profile integration.Profile, targetID, sessionID string) string {
	return filepath.Join(integrationSessionsRoot(profile), targetID, sessionID+".json")
}

func writeIntegrationSession(path string, session activeIntegrationSession) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(session)
	if err != nil {
		return err
	}
	tmp := path + fmt.Sprintf(".%d.tmp", os.Getpid())
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readIntegrationSession(profile integration.Profile, targetID string) (activeIntegrationSession, bool) {
	dir := filepath.Join(integrationSessionsRoot(profile), targetID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return activeIntegrationSession{}, false
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].IsDir() || !strings.HasSuffix(entries[i].Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entries[i].Name())
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			continue
		}
		var session activeIntegrationSession
		if json.Unmarshal(b, &session) != nil || session.TargetID != targetID || session.ControlPath == "" {
			_ = os.Remove(path)
			continue
		}
		if session.PID <= 0 || syscall.Kill(session.PID, 0) != nil {
			_ = os.Remove(path)
			continue
		}
		if info, statErr := os.Stat(session.ControlPath); statErr != nil || info.Mode()&os.ModeSocket == 0 {
			_ = os.Remove(path)
			continue
		}
		return session, true
	}
	_ = os.Remove(dir)
	return activeIntegrationSession{}, false
}

func removeIntegrationSession(path, controlPath string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var session activeIntegrationSession
	if json.Unmarshal(b, &session) == nil && session.ControlPath == controlPath {
		_ = os.Remove(path)
	}
}

func scpIdentityArgs(args []string) []string {
	valueOptions := map[string]bool{"-c": true, "-D": true, "-F": true, "-i": true, "-J": true, "-l": true, "-o": true, "-P": true, "-S": true, "-X": true}
	sshOptions := make([]string, 0, len(args))
	remoteTargets := map[string]bool{}
	remoteTarget := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if valueOptions[arg] {
			if i+1 < len(args) {
				value := args[i+1]
				switch arg {
				case "-P":
					sshOptions = append(sshOptions, "-p", value)
				case "-F", "-i", "-J", "-o":
					sshOptions = append(sshOptions, arg, value)
				}
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		colon := strings.Index(arg, ":")
		if colon <= 0 {
			continue
		}
		target := arg[:colon]
		if strings.HasPrefix(target, "[") && strings.HasSuffix(target, "]") {
			target = strings.Trim(target, "[]")
		}
		if target != "" && !strings.Contains(target, "/") {
			remoteTargets[target] = true
			remoteTarget = target
		}
	}
	if len(remoteTargets) != 1 {
		return nil
	}
	return append(sshOptions, remoteTarget)
}

func (r *Runner) logIntegration(profile integration.Profile, err error) {
	if err == nil {
		return
	}
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		return
	}
	path := filepath.Join(integration.DefaultRoot(home), string(profile), "integration.log")
	dir := filepath.Dir(path)
	if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
		return
	}
	if chmodErr := os.Chmod(dir, 0o700); chmodErr != nil {
		return
	}
	f, openErr := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if openErr != nil {
		return
	}
	defer f.Close()
	if chmodErr := f.Chmod(0o600); chmodErr != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "%s %v\n", time.Now().UTC().Format(time.RFC3339Nano), err)
}

func (r *Runner) integrationLogWriter(profile integration.Profile) (io.Writer, func()) {
	home, err := os.UserHomeDir()
	if err != nil {
		return io.Discard, func() {}
	}
	path := filepath.Join(integration.DefaultRoot(home), string(profile), "integration.log")
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return io.Discard, func() {}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return io.Discard, func() {}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return io.Discard, func() {}
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return io.Discard, func() {}
	}
	return f, func() { _ = f.Close() }
}
