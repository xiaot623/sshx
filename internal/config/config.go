package config

import (
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
	CommandBridge bool           `yaml:"commandBridge"`
	Ports         PortsFeature   `yaml:"ports"`
	Domains       DomainsFeature `yaml:"domains"`
}

type PortsFeature struct {
	Auto bool `yaml:"auto"`
}

type DomainsFeature struct {
	Enabled bool   `yaml:"enabled"`
	Suffix  string `yaml:"suffix"`
	DNSAddr string `yaml:"dnsAddr"`
}

type CommandPolicy struct {
	Deny []string `yaml:"deny"`
}

func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".sshx", "config.yaml")
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
	return f.CommandBridge || f.Ports.Auto || f.Domains.Enabled
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
