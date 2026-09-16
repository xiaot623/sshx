package remotefs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupMountPointWritesMarkerAndResolvesHierarchy(t *testing.T) {
	base := filepath.Join(t.TempDir(), "session")
	path, err := SetupMountPoint(base, "Users/xiaot")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "Users", "xiaot")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	marker, err := os.ReadFile(filepath.Join(base, ".mount-path"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(marker)) != "Users/xiaot" {
		t.Fatalf("marker = %q", marker)
	}
}

func TestSetupMountPointRejectsTraversal(t *testing.T) {
	base := t.TempDir()
	if _, err := SetupMountPoint(base, "../outside"); err == nil {
		t.Fatal("SetupMountPoint accepted a traversal hierarchy")
	}
}
