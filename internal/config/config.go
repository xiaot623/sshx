package config

import (
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Strict   bool          `yaml:"strict"`
	Features Features      `yaml:"features"`
	Commands CommandPolicy `yaml:"commands"`
}

type Features struct {
	CommandBridge bool `yaml:"commandBridge"`
	AutoForward   bool `yaml:"autoForward"`
	RemoteFS      bool `yaml:"remoteFs"`
}

type CommandPolicy struct {
	Deny []string `yaml:"deny"`
}

//go:embed default_config.yaml
var defaultConfigYAML []byte

func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".sshx", "config.yaml")
}

func EnsureDefault(path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, defaultConfigYAML, 0o600)
}

func Load(path string) (Config, error) {
	if path == "" {
		return Config{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (f Features) Enabled() bool {
	return f.CommandBridge || f.AutoForward || f.RemoteFS
}

func (p CommandPolicy) Allows(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	cmd := filepath.Base(argv[0])
	for _, denied := range p.Deny {
		if denied == cmd || denied == argv[0] {
			return false
		}
	}
	return true
}

func NormalizeTargetHost(target string) string {
	if at := strings.LastIndex(target, "@"); at >= 0 {
		target = target[at+1:]
	}
	if strings.HasPrefix(target, "[") {
		if end := strings.Index(target, "]"); end >= 0 {
			return target[1:end]
		}
	}
	if colon := strings.LastIndex(target, ":"); colon > 0 && !strings.Contains(target[:colon], ":") {
		return target[:colon]
	}
	return target
}
