package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/xiaot623/sshx/internal/identity"
	"github.com/xiaot623/sshx/internal/protocol"
	"github.com/xiaot623/sshx/internal/version"
)

type ServerInfo struct {
	Protocol    string `json:"protocol"`
	Address     string `json:"address"`
	Token       string `json:"token,omitempty"`
	Version     string `json:"version,omitempty"`
	RuntimeID   string `json:"runtimeId,omitempty"`
	ProtocolMin int    `json:"protocolMin,omitempty"`
	ProtocolMax int    `json:"protocolMax,omitempty"`
	PID         int    `json:"pid,omitempty"`
}

func WriteServerInfo(path, socketPath, token, appVersion string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if appVersion == "" {
		appVersion = version.Version
	}
	b, err := json.MarshalIndent(ServerInfo{Protocol: "unix", Address: socketPath, Token: token, Version: appVersion, RuntimeID: identity.RuntimeID, ProtocolMin: protocol.MinVersion, ProtocolMax: protocol.MaxVersion, PID: os.Getpid()}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o600)
}

func ReadServerInfo(path string) (ServerInfo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ServerInfo{}, err
	}
	var info ServerInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return ServerInfo{}, err
	}
	return info, nil
}
