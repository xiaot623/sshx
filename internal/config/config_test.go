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
  autoForward: true
  remoteFs: true
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
	if !cfg.Strict || !cfg.Features.CommandBridge || !cfg.Features.AutoForward || !cfg.Features.RemoteFS {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if !cfg.Commands.Allows([]string{"uname", "-a"}) {
		t.Fatal("commands should be allowed by default")
	}
	if cfg.Commands.Allows([]string{"rm", "-rf", "/"}) {
		t.Fatal("rm should be denied")
	}
}

func TestEnsureDefaultWritesEmbeddedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".sshx", "config.yaml")
	if err := EnsureDefault(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Strict || !cfg.Features.CommandBridge || !cfg.Features.AutoForward || cfg.Features.RemoteFS {
		t.Fatalf("unexpected default config: %#v", cfg)
	}
}

func TestEnsureDefaultDoesNotOverwriteExistingConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("strict: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDefault(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "strict: true\n" {
		t.Fatalf("config was overwritten: %q", b)
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
	if !(Features{RemoteFS: true}).Enabled() {
		t.Fatal("remote fs should enable features")
	}
}
