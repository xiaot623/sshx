package identity

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestEnsureInstallIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.json")
	first, err := EnsureInstall(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureInstall(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.ID == "" {
		t.Fatalf("install IDs = %q, %q", first.ID, second.ID)
	}
}

func TestTargetAndContextIDs(t *testing.T) {
	target := Target{User: "root", Hostname: "example.com", Port: 22}
	if TargetID(target) != TargetID(target) {
		t.Fatal("TargetID is not stable")
	}
	if ContextID("install", TargetID(target), "vscode") == ContextID("install", TargetID(target), "cursor") {
		t.Fatal("profiles must have distinct ContextIDs")
	}
}

func TestConfigProbeArgsDropsActionOptions(t *testing.T) {
	got := ConfigProbeArgs([]string{"-T", "-D", "43210", "-L8080:localhost:80", "-o", "ControlPath=/tmp/x", "-p", "2222", "host", "bash"})
	want := []string{"-p", "2222", "host"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ConfigProbeArgs = %#v, want %#v", got, want)
	}
}

func TestResolveTarget(t *testing.T) {
	dir := t.TempDir()
	ssh := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' 'user alice' 'hostname Example.COM' 'port 2222' 'hostkeyalias Alias.EXAMPLE'\n"
	if err := os.WriteFile(ssh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	target, err := ResolveTarget(context.Background(), ssh, []string{"-D", "1234", "alias"})
	if err != nil {
		t.Fatal(err)
	}
	want := Target{User: "alice", Hostname: "example.com", Port: 2222, HostKeyAlias: "alias.example"}
	if !reflect.DeepEqual(target, want) {
		t.Fatalf("target = %#v, want %#v", target, want)
	}
}
