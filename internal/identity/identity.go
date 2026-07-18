package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	ContextABI     = "context-v1"
	RuntimeID      = "bridge-v1.mux-v1.remotefs-v2"
	LocalRuntimeID = "locald-v1.forward-v1"
)

type Install struct {
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt"`
}

type Target struct {
	User         string `json:"user"`
	Hostname     string `json:"hostname"`
	Port         int    `json:"port"`
	HostKeyAlias string `json:"hostKeyAlias,omitempty"`
}

type Connection struct {
	ClientInstallID string
	TargetID        string
	ContextID       string
	SessionID       string
	Profile         string
	Target          Target
}

func DefaultInstallPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "sshx-install.json")
	}
	return filepath.Join(home, ".sshx", "install.json")
}

func EnsureInstall(path string) (Install, error) {
	if path == "" {
		path = DefaultInstallPath()
	}
	if install, err := readInstall(path); err == nil {
		return install, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Install{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Install{}, err
	}
	lockFile, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Install{}, err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return Install{}, err
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	if install, err := readInstall(path); err == nil {
		return install, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Install{}, err
	}
	id, err := UUID()
	if err != nil {
		return Install{}, err
	}
	install := Install{ID: id, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	b, err := json.MarshalIndent(install, "", "  ")
	if err != nil {
		return Install{}, err
	}
	b = append(b, '\n')
	if err := atomicWrite(path, b, 0o600); err != nil {
		return Install{}, err
	}
	return install, nil
}

func readInstall(path string) (Install, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Install{}, err
	}
	var install Install
	if err := json.Unmarshal(b, &install); err != nil {
		return Install{}, fmt.Errorf("decode client install identity: %w", err)
	}
	if !validUUID(install.ID) {
		return Install{}, errors.New("client install identity is invalid")
	}
	return install, nil
}

func ResolveTarget(ctx context.Context, sshPath string, args []string) (Target, error) {
	if sshPath == "" {
		sshPath = "ssh"
	}
	probe := ConfigProbeArgs(args)
	if len(probe) == 0 {
		return Target{}, errors.New("SSH target is required")
	}
	cmd := exec.CommandContext(ctx, sshPath, append([]string{"-G"}, probe...)...)
	out, err := cmd.Output()
	if err != nil {
		return Target{}, fmt.Errorf("resolve SSH target with %s -G: %w", sshPath, err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if _, exists := values[key]; !exists {
			values[key] = strings.TrimSpace(value)
		}
	}
	port, err := strconv.Atoi(values["port"])
	if err != nil || port <= 0 || port > 65535 {
		return Target{}, fmt.Errorf("invalid SSH target port %q", values["port"])
	}
	target := Target{
		User:         values["user"],
		Hostname:     strings.ToLower(values["hostname"]),
		Port:         port,
		HostKeyAlias: strings.ToLower(values["hostkeyalias"]),
	}
	if target.HostKeyAlias == "none" {
		target.HostKeyAlias = ""
	}
	if target.User == "" || target.Hostname == "" {
		return Target{}, errors.New("ssh -G did not return user and hostname")
	}
	return target, nil
}

// ConfigProbeArgs keeps SSH configuration selectors and the destination while
// dropping action-specific options. Dynamic forwarding ports must not affect
// TargetID and may not be accepted by every OpenSSH version in -G mode.
func ConfigProbeArgs(args []string) []string {
	valueOptions := map[string]bool{
		"-B": true, "-b": true, "-c": true, "-E": true, "-e": true,
		"-F": true, "-I": true, "-i": true, "-J": true, "-l": true,
		"-m": true, "-o": true, "-p": true, "-w": true,
	}
	actionOptions := map[string]bool{
		"-D": true, "-L": true, "-O": true, "-R": true, "-S": true, "-W": true,
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				out = append(out, args[i+1])
			}
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			out = append(out, arg)
			break
		}
		if actionOptions[arg] {
			i++
			continue
		}
		if len(arg) > 2 && actionOptions[arg[:2]] {
			continue
		}
		if valueOptions[arg] {
			if i+1 < len(args) {
				if arg != "-o" || !isControlOption(args[i+1]) {
					out = append(out, arg, args[i+1])
				}
				i++
			}
			continue
		}
		if len(arg) > 2 && valueOptions[arg[:2]] {
			if arg[:2] != "-o" || !isControlOption(arg[2:]) {
				out = append(out, arg)
			}
			continue
		}
		// Flags that only affect the requested operation are intentionally dropped.
	}
	return out
}

func isControlOption(value string) bool {
	key, _, _ := strings.Cut(value, "=")
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "controlmaster", "controlpath", "controlpersist":
		return true
	default:
		return false
	}
}

func TargetID(target Target) string {
	b, _ := json.Marshal(target)
	return digest("target", string(b))
}

func ContextID(clientInstallID, targetID, profile string) string {
	return digest("context", clientInstallID, targetID, strings.ToLower(profile), ContextABI)
}

// RuntimeHomeID identifies the remote runtime for a target without embedding
// the full target and runtime identities in Unix socket paths.
func RuntimeHomeID(targetID string) string {
	return digest("runtime-home", targetID, RuntimeID)
}

func NewConnection(ctx context.Context, installPath, sshPath string, args []string, profile string) (Connection, error) {
	install, err := EnsureInstall(installPath)
	if err != nil {
		return Connection{}, err
	}
	target, err := ResolveTarget(ctx, sshPath, args)
	if err != nil {
		return Connection{}, err
	}
	targetID := TargetID(target)
	sessionID, err := UUID()
	if err != nil {
		return Connection{}, err
	}
	return Connection{
		ClientInstallID: install.ID,
		TargetID:        targetID,
		ContextID:       ContextID(install.ID, targetID, profile),
		SessionID:       sessionID,
		Profile:         profile,
		Target:          target,
	}, nil
}

func UUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32], nil
}

func digest(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sshx-identity-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
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
	return os.Rename(tmpPath, path)
}
