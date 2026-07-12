//go:build linux

package remotefs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestGoFuseDriverReadWriteRoundTrip(t *testing.T) {
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("/dev/fuse is unavailable: %v", err)
	}
	if _, err := exec.LookPath("fusermount3"); err != nil {
		if _, fallbackErr := exec.LookPath("fusermount"); fallbackErr != nil {
			t.Skip("fusermount is unavailable")
		}
	}
	source := t.TempDir()
	mountpoint := filepath.Join(t.TempDir(), "workspace")
	if err := os.WriteFile(filepath.Join(source, "note.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, err := OpenRootBackend(source)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.CloseBackend()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mount, err := (GoFuseDriver{}).Mount(ctx, mountpoint, backend, MountOptions{})
	if err != nil {
		t.Skipf("FUSE mount is unavailable: %v", err)
	}
	defer func() {
		unmountCtx, unmountCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer unmountCancel()
		_ = mount.Unmount(unmountCtx)
	}()

	data, err := os.ReadFile(filepath.Join(mountpoint, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("content = %q", data)
	}
	f, err := os.OpenFile(filepath.Join(mountpoint, "note.txt"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(" world"); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(mountpoint, "note.txt"), filepath.Join(mountpoint, "renamed.txt")); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(source, "renamed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Fatalf("source content = %q", data)
	}
}
