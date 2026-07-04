package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type ServerInfo struct {
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Token    string `json:"token,omitempty"`
}

func WriteServerInfo(path, socketPath, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(ServerInfo{Protocol: "unix", Address: socketPath, Token: token}, "", "  ")
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
