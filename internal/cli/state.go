package cli

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiaot623/sshx/internal/identity"
	"github.com/xiaot623/sshx/internal/version"
)

func generateUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32], nil
}

func remoteServerHome(id string) string {
	return "$HOME/.sshx_server/runtimes/" + identity.RuntimeHomeID(id)
}

func remoteContextHome(targetID, contextID string) string {
	return "$HOME/.sshx_server/targets/" + targetID + "/contexts/" + contextID
}

func remoteServerEnvScript(remoteHome string) string {
	return "SSHX_SERVER_HOME=\"" + strings.ReplaceAll(remoteHome, `"`, `\"`) + "\"; SSHX_RUNTIME_ID=" + shellQuote(identity.RuntimeID) + "; export SSHX_SERVER_HOME SSHX_RUNTIME_ID; case \":$PATH:\" in *\":$SSHX_SERVER_HOME:\"*) ;; *) PATH=\"$SSHX_SERVER_HOME:$PATH\" ;; esac; export PATH"
}

func remoteBridgeEnvScript(remoteHome string, session *BridgeSession) string {
	script := remoteServerEnvScript(remoteHome)
	if session == nil {
		return script
	}
	if session.ContextID != "" {
		script += "; SSHX_CONTEXT_ID=" + shellQuote(session.ContextID) + "; export SSHX_CONTEXT_ID"
	}
	if session.SessionID != "" {
		script += "; SSHX_SESSION_ID=" + shellQuote(session.SessionID) + "; export SSHX_SESSION_ID"
	}
	if session.RemoteFS {
		if session.ReadOnly {
			script += "; FS_READ_ONLY=1"
		} else {
			script += "; FS_READ_ONLY=0"
		}
		script += "; SSHX_REMOTE_FS=1; export SSHX_REMOTE_FS FS_READ_ONLY"
	} else {
		script += "; SSHX_REMOTE_FS=0; export SSHX_REMOTE_FS"
	}
	if session.Workspace != "" {
		script += "; SSHX_WORKSPACE=" + shellQuote(session.Workspace)
		if session.MountRoot != "" {
			script += "; SSHX_MOUNT_ROOT=" + shellQuote(session.MountRoot)
		}
		script += "; export SSHX_WORKSPACE SSHX_MOUNT_ROOT"
	}
	return script
}

func integrationContextEnvScript(contextID, contextHome string) string {
	bin := strings.ReplaceAll(contextHome, `"`, `\"`) + "/bin"
	return "SSHX_CONTEXT_ID=" + shellQuote(contextID) + "; PATH=\"" + bin + ":$PATH\"; export SSHX_CONTEXT_ID PATH"
}

type versionState struct {
	CurrentVersion string `json:"current_version"`
	LastVersion    string `json:"last_version,omitempty"`
	UpdatedAt      string `json:"updated_at"`
}

func recordDefaultVersionState(current string) error {
	return recordVersionState(defaultVersionStatePath(), current)
}

func recordVersionState(path, current string) error {
	if current == "" {
		current = "dev"
	}
	var state versionState
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &state)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if state.CurrentVersion != current {
		state.LastVersion = state.CurrentVersion
		state.CurrentVersion = current
	}
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmpPath := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmpPath, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func clientVersion() string {
	if version.Version == "" {
		return "dev"
	}
	return version.Version
}

func normalizeRemoteOS(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "linux":
		return "linux", nil
	case "":
		return "", errors.New("remote OS probe returned empty value")
	default:
		return "", fmt.Errorf("unsupported remote server OS %q", s)
	}
}

func normalizeRemoteArch(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	case "":
		return "", errors.New("remote arch probe returned empty value")
	default:
		return "", fmt.Errorf("unsupported remote arch %q", s)
	}
}

func generateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
