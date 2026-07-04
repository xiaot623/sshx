package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGlobalFeaturesAndCommandPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte(`
strict: true
features:
  commandBridge: true
  ports:
    auto: true
  domains:
    enabled: true
commands:
  deny:
    - rm
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Strict || !cfg.Features.CommandBridge || !cfg.Features.Ports.Auto || !cfg.Features.Domains.Enabled {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if !cfg.Commands.Allows([]string{"uname", "-a"}) {
		t.Fatal("commands should be allowed by default")
	}
	if cfg.Commands.Allows([]string{"rm", "-rf", "/"}) {
		t.Fatal("rm should be denied")
	}
}

func TestNormalizeTargetHost(t *testing.T) {
	cases := map[string]string{
		"remote":              "remote",
		"user@remote":         "remote",
		"user@example.com:22": "example.com",
		"[::1]:22":            "::1",
	}
	for in, want := range cases {
		if got := NormalizeTargetHost(in); got != want {
			t.Fatalf("NormalizeTargetHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFeaturesEnabled(t *testing.T) {
	if (Features{}).Enabled() {
		t.Fatal("empty features should be disabled")
	}
	if !(Features{CommandBridge: true}).Enabled() {
		t.Fatal("command bridge should enable features")
	}
}
