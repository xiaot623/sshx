package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/xiaot623/sshx/internal/processlock"
)

const (
	settingsKey      = "remote.SSH.path"
	npmManagedMarker = ".npm-managed"
)

type Profile string

const (
	VSCode Profile = "vscode"
	Cursor Profile = "cursor"
)

type Descriptor struct {
	Schema      int     `json:"schema"`
	Profile     Profile `json:"profile"`
	SSHPath     string  `json:"sshPath"`
	SCPPath     string  `json:"scpPath"`
	DriverPath  string  `json:"driverPath"`
	Settings    string  `json:"settingsPath"`
	PreviousSSH string  `json:"previousSSHPath,omitempty"`
	InstalledAt string  `json:"installedAt"`
}

type InstallOptions struct {
	HomeDir    string
	ConfigHome string
	GOOS       string
	Executable string
	NPMManaged bool
	LookPath   func(string) (string, error)
	Run        func(context.Context, string, ...string) ([]byte, error)
}

type InstallResult struct {
	Profile      Profile
	SettingsPath string
	SSHShim      string
}

func ParseProfile(value string) (Profile, error) {
	switch Profile(strings.ToLower(strings.TrimSpace(value))) {
	case VSCode:
		return VSCode, nil
	case Cursor:
		return Cursor, nil
	default:
		return "", fmt.Errorf("unsupported integration %q (expected vscode or cursor)", value)
	}
}

func validProfile(profile Profile) bool {
	value := string(profile)
	if len(value) == 0 || len(value) > 64 || value == "." || value == ".." {
		return false
	}
	for i, r := range value {
		if i == 0 && !isProfileAlphaNumeric(r) {
			return false
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func isProfileAlphaNumeric(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
}

func DefaultRoot(home string) string {
	if override := os.Getenv("SSHX_INTEGRATIONS_DIR"); override != "" {
		return override
	}
	return filepath.Join(home, ".sshx", "integrations")
}

func SettingsPath(profile Profile, opts InstallOptions) (string, error) {
	home := opts.HomeDir
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil || home == "" {
			return "", errors.New("resolve user home directory")
		}
	}
	goos := opts.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	product := "Code"
	if profile == Cursor {
		product = "Cursor"
	}
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", product, "User", "settings.json"), nil
	case "linux":
		configHome := opts.ConfigHome
		if configHome == "" {
			configHome = os.Getenv("XDG_CONFIG_HOME")
		}
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		}
		return filepath.Join(configHome, product, "User", "settings.json"), nil
	default:
		return "", fmt.Errorf("%s integration is unsupported on %s", profile, goos)
	}
}

func Install(ctx context.Context, profile Profile, opts InstallOptions) (InstallResult, error) {
	settingsPath, err := SettingsPath(profile, opts)
	if err != nil {
		return InstallResult{}, err
	}
	home := opts.HomeDir
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return InstallResult{}, err
		}
	}
	executable := opts.Executable
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return InstallResult{}, err
		}
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return InstallResult{}, err
	}
	lookPath := opts.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	run := opts.Run
	if run == nil {
		run = func(ctx context.Context, path string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, path, args...).CombinedOutput()
		}
	}
	original, err := os.ReadFile(settingsPath)
	settingsExisted := err == nil
	settingsMode := os.FileMode(0o600)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return InstallResult{}, err
	}
	if !settingsExisted {
		original = []byte("{}\n")
	} else if info, statErr := os.Stat(settingsPath); statErr == nil {
		settingsMode = info.Mode().Perm()
	}
	settingsSource := original
	if len(bytes.TrimSpace(settingsSource)) == 0 {
		settingsSource = []byte("{}\n")
	}
	previous, _, err := StringProperty(settingsSource, settingsKey)
	if err != nil {
		return InstallResult{}, fmt.Errorf("read %s: %w", settingsPath, err)
	}
	root := DefaultRoot(home)
	profileRoot := filepath.Join(root, string(profile))
	if err := os.MkdirAll(root, 0o700); err != nil {
		return InstallResult{}, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return InstallResult{}, err
	}
	lock, err := processlock.Acquire(filepath.Join(root, "."+string(profile)+".install.lock"))
	if err != nil {
		return InstallResult{}, fmt.Errorf("another %s integration install is active: %w", profile, err)
	}
	defer lock.Release()
	oldDescriptor, _ := ReadDescriptor(filepath.Join(profileRoot, "integration.json"))
	upstreamSSH := previous
	recordedPrevious := previous
	if samePath(upstreamSSH, filepath.Join(profileRoot, "bin", "ssh")) {
		upstreamSSH = oldDescriptor.SSHPath
		if oldDescriptor.PreviousSSH != "" {
			recordedPrevious = oldDescriptor.PreviousSSH
		}
	}
	upstreamSSH, err = findOpenSSH(ctx, upstreamSSH, executable, lookPath, run)
	if err != nil {
		return InstallResult{}, err
	}
	upstreamSCP, err := findSCP(upstreamSSH, executable, lookPath)
	if err != nil {
		return InstallResult{}, err
	}

	staging, err := os.MkdirTemp(root, "."+string(profile)+"-install-*")
	if err != nil {
		return InstallResult{}, err
	}
	defer os.RemoveAll(staging)
	binDir := filepath.Join(staging, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return InstallResult{}, err
	}
	for _, name := range []string{"ssh", "scp"} {
		if err := os.Symlink(executable, filepath.Join(binDir, name)); err != nil {
			return InstallResult{}, err
		}
	}
	descriptor := Descriptor{
		Schema:      1,
		Profile:     profile,
		SSHPath:     upstreamSSH,
		SCPPath:     upstreamSCP,
		DriverPath:  executable,
		Settings:    settingsPath,
		PreviousSSH: recordedPrevious,
		InstalledAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := WriteDescriptor(filepath.Join(staging, "integration.json"), descriptor); err != nil {
		return InstallResult{}, err
	}
	if opts.NPMManaged {
		if err := atomicWrite(filepath.Join(staging, npmManagedMarker), []byte("1\n"), 0o600); err != nil {
			return InstallResult{}, err
		}
	}
	if err := validateProfile(staging, executable, descriptor); err != nil {
		return InstallResult{}, err
	}
	if err := selfCheck(ctx, filepath.Join(binDir, "ssh"), filepath.Join(binDir, "scp"), upstreamSSH, upstreamSCP, run); err != nil {
		return InstallResult{}, err
	}

	finalSSH := filepath.Join(profileRoot, "bin", "ssh")
	patched, err := SetStringProperty(settingsSource, settingsKey, finalSSH)
	if err != nil {
		return InstallResult{}, fmt.Errorf("update %s: %w", settingsPath, err)
	}
	backup := profileRoot + fmt.Sprintf(".backup-%d", os.Getpid())
	_ = os.RemoveAll(backup)
	if _, statErr := os.Lstat(profileRoot); statErr == nil {
		if err := os.Rename(profileRoot, backup); err != nil {
			return InstallResult{}, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return InstallResult{}, statErr
	}
	committed := false
	settingsCommitted := false
	defer func() {
		if committed {
			return
		}
		if settingsCommitted {
			if settingsExisted {
				_ = atomicWrite(settingsPath, original, settingsMode)
			} else {
				_ = os.Remove(settingsPath)
			}
		}
		_ = os.RemoveAll(profileRoot)
		if _, err := os.Lstat(backup); err == nil {
			_ = os.Rename(backup, profileRoot)
		}
	}()
	if err := os.Rename(staging, profileRoot); err != nil {
		return InstallResult{}, err
	}
	if err := validateProfile(profileRoot, executable, descriptor); err != nil {
		return InstallResult{}, err
	}
	if err := atomicWrite(settingsPath, patched, settingsMode); err != nil {
		return InstallResult{}, err
	}
	settingsCommitted = true
	installedSettings, readErr := os.ReadFile(settingsPath)
	if readErr != nil {
		return InstallResult{}, readErr
	}
	if value, found, err := StringProperty(installedSettings, settingsKey); err != nil || !found || !samePath(value, finalSSH) {
		if err == nil {
			err = errors.New("remote.SSH.path verification failed")
		}
		return InstallResult{}, err
	}
	committed = true
	_ = os.RemoveAll(backup)
	return InstallResult{Profile: profile, SettingsPath: settingsPath, SSHShim: finalSSH}, nil
}

func DescriptorForInvocation(invocation string) (Descriptor, error) {
	if !strings.ContainsRune(invocation, os.PathSeparator) {
		if resolved, err := exec.LookPath(invocation); err == nil {
			invocation = resolved
		}
	}
	abs, err := filepath.Abs(invocation)
	if err != nil {
		return Descriptor{}, err
	}
	return ReadDescriptor(filepath.Join(filepath.Dir(filepath.Dir(abs)), "integration.json"))
}

func ReadDescriptor(path string) (Descriptor, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Descriptor{}, err
	}
	var descriptor Descriptor
	if err := json.Unmarshal(b, &descriptor); err != nil {
		return Descriptor{}, err
	}
	if descriptor.Schema != 1 || !validProfile(descriptor.Profile) || descriptor.SSHPath == "" || descriptor.SCPPath == "" {
		return Descriptor{}, errors.New("invalid sshx integration descriptor")
	}
	return descriptor, nil
}

func WriteDescriptor(path string, descriptor Descriptor) error {
	b, err := json.MarshalIndent(descriptor, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'), 0o600)
}

func findOpenSSH(ctx context.Context, preferred, executable string, lookPath func(string) (string, error), run func(context.Context, string, ...string) ([]byte, error)) (string, error) {
	candidates := []string{preferred}
	if path, err := lookPath("ssh"); err == nil {
		candidates = append(candidates, path)
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		candidate, err := unwrapIntegrationClient(candidate, false)
		if err != nil {
			continue
		}
		abs, err := filepath.Abs(candidate)
		if err != nil || seen[abs] || samePath(abs, executable) {
			continue
		}
		seen[abs] = true
		if info, err := os.Stat(abs); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		out, _ := run(ctx, abs, "-V")
		if bytes.Contains(out, []byte("OpenSSH")) {
			return abs, nil
		}
	}
	return "", errors.New("a usable OpenSSH client was not found")
}

func findSCP(sshPath, executable string, lookPath func(string) (string, error)) (string, error) {
	candidates := []string{filepath.Join(filepath.Dir(sshPath), "scp")}
	if path, err := lookPath("scp"); err == nil {
		candidates = append(candidates, path)
	}
	for _, candidate := range candidates {
		candidate, err := unwrapIntegrationClient(candidate, true)
		if err != nil {
			continue
		}
		abs, err := filepath.Abs(candidate)
		if err != nil || samePath(abs, executable) {
			continue
		}
		if info, err := os.Stat(abs); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return abs, nil
		}
	}
	return "", errors.New("a usable OpenSSH scp client was not found")
}

func unwrapIntegrationClient(path string, scp bool) (string, error) {
	seen := map[string]bool{}
	for depth := 0; depth < 8; depth++ {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		if seen[abs] {
			return "", errors.New("recursive sshx integration shim")
		}
		seen[abs] = true
		descriptor, err := DescriptorForInvocation(abs)
		if err != nil {
			return abs, nil
		}
		if scp {
			path = descriptor.SCPPath
		} else {
			path = descriptor.SSHPath
		}
		if path == "" {
			return "", errors.New("sshx integration shim has no upstream client")
		}
	}
	return "", errors.New("sshx integration shim chain is too deep")
}

func validateProfile(root, executable string, descriptor Descriptor) error {
	for _, dir := range []string{root, filepath.Join(root, "bin")} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			return fmt.Errorf("integration directory permissions are invalid: %s", dir)
		}
	}
	for _, name := range []string{"ssh", "scp"} {
		path := filepath.Join(root, "bin", name)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("integration %s shim is not a symlink", name)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !samePath(resolved, executable) {
			return fmt.Errorf("integration %s shim does not invoke the current sshx binary", name)
		}
	}
	descriptorPath := filepath.Join(root, "integration.json")
	info, err := os.Stat(descriptorPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("integration descriptor permissions are invalid")
	}
	installed, err := ReadDescriptor(descriptorPath)
	if err != nil || installed != descriptor {
		return errors.New("integration descriptor invocation chain is invalid")
	}
	return nil
}

func selfCheck(ctx context.Context, sshShim, scpShim, upstreamSSH, upstreamSCP string, run func(context.Context, string, ...string) ([]byte, error)) error {
	want, _ := run(ctx, upstreamSSH, "-V")
	got, _ := run(ctx, sshShim, "-V")
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		return fmt.Errorf("SSH shim self-check failed: got %q, want %q", bytes.TrimSpace(got), bytes.TrimSpace(want))
	}
	wantSCP, _ := run(ctx, upstreamSCP)
	gotSCP, _ := run(ctx, scpShim)
	if !bytes.Equal(bytes.TrimSpace(gotSCP), bytes.TrimSpace(wantSCP)) {
		return errors.New("SCP shim self-check failed")
	}
	return nil
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	if infoA, err := os.Stat(aa); err == nil {
		if infoB, err := os.Stat(bb); err == nil {
			return os.SameFile(infoA, infoB)
		}
	}
	return aa == bb
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sshx-settings-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
