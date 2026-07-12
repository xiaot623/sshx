package remotefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRootBackendReadWriteRenameAndMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	backend, err := OpenRootBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.CloseBackend()

	ctx := context.Background()
	attr, err := backend.Lookup(ctx, "note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if attr.Size != 5 || attr.Mode&syscall.S_IFMT != syscall.S_IFREG {
		t.Fatalf("unexpected attr: %#v", attr)
	}
	handle, _, err := backend.Open(ctx, "note.txt", uint32(os.O_RDWR), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Rename(ctx, "note.txt", "renamed.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Write(ctx, handle, 5, []byte(" world")); err != nil {
		t.Fatal(err)
	}
	if err := backend.Fsync(ctx, handle); err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(ctx, handle); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Read(ctx, handle, 0, 1); !errors.Is(err, syscall.EBADF) {
		t.Fatalf("closed handle read error = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "renamed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello world" {
		t.Fatalf("content = %q", got)
	}
}

func TestRootBackendRejectsEscapesAndSpecialFiles(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo is unavailable: %v", err)
	}
	backend, err := OpenRootBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.CloseBackend()

	ctx := context.Background()
	attr, err := backend.Lookup(ctx, "escape")
	if err != nil {
		t.Fatal(err)
	}
	if attr.Mode&syscall.S_IFMT != syscall.S_IFLNK {
		t.Fatalf("symlink mode = %#o", attr.Mode)
	}
	if _, _, err := backend.Open(ctx, "escape", uint32(os.O_RDONLY), 0); err == nil {
		t.Fatal("opening an escaping symlink succeeded")
	}
	if _, err := backend.Lookup(ctx, "../secret.txt"); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("parent escape error = %v", err)
	}
	if _, err := backend.Lookup(ctx, "pipe"); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("special file error = %v", err)
	}
	if _, err := backend.Symlink(ctx, outside, "created-escape"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backend.Open(ctx, "created-escape", uint32(os.O_RDONLY), 0); err == nil {
		t.Fatal("opening a newly-created escaping symlink succeeded")
	}
}

func TestRootBackendDirectoryAndSymlinkOperations(t *testing.T) {
	root := t.TempDir()
	backend, err := OpenRootBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.CloseBackend()
	ctx := context.Background()

	if _, err := backend.Mkdir(ctx, "dir", 0o750); err != nil {
		t.Fatal(err)
	}
	handle, _, err := backend.Open(ctx, "dir/file", uint32(os.O_CREATE|os.O_RDWR), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(ctx, handle); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Symlink(ctx, "file", "dir/link"); err != nil {
		t.Fatal(err)
	}
	target, err := backend.Readlink(ctx, "dir/link")
	if err != nil {
		t.Fatal(err)
	}
	if target != "file" {
		t.Fatalf("target = %q", target)
	}
	linkHandle, _, err := backend.Open(ctx, "dir/link", uint32(os.O_RDONLY), 0)
	if err != nil {
		t.Fatalf("opening in-root symlink: %v", err)
	}
	if err := backend.Close(ctx, linkHandle); err != nil {
		t.Fatal(err)
	}
	entries, err := backend.ReadDir(ctx, "dir")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %#v", entries)
	}
	if err := backend.Unlink(ctx, "dir/link"); err != nil {
		t.Fatal(err)
	}
	if err := backend.Unlink(ctx, "dir/file"); err != nil {
		t.Fatal(err)
	}
	if err := backend.Rmdir(ctx, "dir"); err != nil {
		t.Fatal(err)
	}
}

func TestErrnoMappingIsStable(t *testing.T) {
	tests := []struct {
		err  error
		want syscall.Errno
	}{
		{syscall.EBADF, syscall.EBADF},
		{os.ErrNotExist, syscall.ENOENT},
		{os.ErrPermission, syscall.EACCES},
		{os.ErrExist, syscall.EEXIST},
		{context.Canceled, syscall.EINTR},
		{context.DeadlineExceeded, syscall.ETIMEDOUT},
		{errors.New("unknown"), syscall.EIO},
	}
	for _, test := range tests {
		if got := errnoOf(test.err); got != test.want {
			t.Fatalf("errnoOf(%v) = %v, want %v", test.err, got, test.want)
		}
	}
}
