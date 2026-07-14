package remotefs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveExportLayoutPreservesHomeHierarchy(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "Users", "xiaot")
	cwd := filepath.Join(home, "workspace", "sshx")
	layout, err := ResolveExportLayout(home, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if layout.RootPath != home || layout.RelativeCwd != filepath.Join("workspace", "sshx") || layout.MountPath != "Users/xiaot" {
		t.Fatalf("layout = %#v", layout)
	}
	mountRoot := filepath.Join(string(filepath.Separator), "remote", "mounts", "session", filepath.FromSlash(layout.MountPath))
	workspace, err := WorkspacePathBelow(mountRoot, filepath.ToSlash(layout.RelativeCwd))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(string(filepath.Separator), "remote", "mounts", "session", "Users", "xiaot", "workspace", "sshx")
	if workspace != want {
		t.Fatalf("workspace = %q, want %q", workspace, want)
	}
}

func TestResolveExportLayoutFallsBackOutsideHome(t *testing.T) {
	layout, err := ResolveExportLayout("/home/xiaot", "/srv/project")
	if err != nil {
		t.Fatal(err)
	}
	if layout.RootPath != "/srv/project" || layout.RelativeCwd != "." || layout.MountPath != "srv/project" {
		t.Fatalf("layout = %#v", layout)
	}
}

func TestMountPathsRejectTraversal(t *testing.T) {
	for _, value := range []string{"", ".", "../home", "/Users/xiaot", `Users\xiaot`} {
		if _, err := MountPathBelow(t.TempDir(), value); err == nil {
			t.Fatalf("MountPathBelow accepted %q", value)
		}
	}
	for _, value := range []string{"", "../workspace", "/workspace", `workspace\repo`} {
		if _, err := WorkspacePathBelow(t.TempDir(), value); err == nil {
			t.Fatalf("WorkspacePathBelow accepted %q", value)
		}
	}
}

func TestRootBackendExcludesManagedMountTree(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, ".sshx_server", "id", "mounts")
	if err := os.MkdirAll(managed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managed, "recursive"), []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, err := OpenRootBackendExcluding(root, managed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.CloseBackend() })
	entries, err := backend.ReadDir(context.Background(), filepath.Join(".sshx_server", "id"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name == "mounts" {
			t.Fatal("managed mount tree was visible")
		}
	}
	if _, err := backend.Lookup(context.Background(), filepath.Join(".sshx_server", "id", "mounts", "recursive")); err == nil {
		t.Fatal("managed mount tree was directly accessible")
	}
}
