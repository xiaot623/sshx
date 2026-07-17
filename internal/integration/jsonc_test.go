package integration

import (
	"strings"
	"testing"
)

func TestSetStringPropertyPreservesJSONC(t *testing.T) {
	src := []byte("{\n  // keep this\n  \"editor.fontSize\": 14,\n  \"remote.SSH.path\": \"/old/ssh\",\n}\n")
	out, err := SetStringProperty(src, settingsKey, "/new/ssh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "// keep this") || !strings.Contains(string(out), "\"editor.fontSize\": 14") {
		t.Fatalf("unrelated JSONC changed:\n%s", out)
	}
	got, found, err := StringProperty(out, settingsKey)
	if err != nil || !found || got != "/new/ssh" {
		t.Fatalf("path = %q, %v, %v", got, found, err)
	}
}

func TestSetStringPropertyAddsSetting(t *testing.T) {
	src := []byte("{\n  \"editor.fontSize\": 14, // keep trailing comma comment\n}\n")
	out, err := SetStringProperty(src, settingsKey, "/new/ssh")
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := StringProperty(out, settingsKey)
	if err != nil || !found || got != "/new/ssh" {
		t.Fatalf("path = %q, %v, %v\n%s", got, found, err, out)
	}
	if strings.Contains(string(out), ",\n,\n") {
		t.Fatalf("added a duplicate comma:\n%s", out)
	}
}

func TestSettingsPaths(t *testing.T) {
	mac, err := SettingsPath(VSCode, InstallOptions{HomeDir: "/Users/alice", GOOS: "darwin"})
	if err != nil || mac != "/Users/alice/Library/Application Support/Code/User/settings.json" {
		t.Fatalf("mac path = %q, %v", mac, err)
	}
	linux, err := SettingsPath(Cursor, InstallOptions{HomeDir: "/home/alice", ConfigHome: "/cfg", GOOS: "linux"})
	if err != nil || linux != "/cfg/Cursor/User/settings.json" {
		t.Fatalf("linux path = %q, %v", linux, err)
	}
}
