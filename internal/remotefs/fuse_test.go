package remotefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type fsyncCounter struct {
	Backend
	fsyncs int
}

func (c *fsyncCounter) Fsync(ctx context.Context, handle uint64) error {
	c.fsyncs++
	return c.Backend.Fsync(ctx, handle)
}

func TestFuseFlushDoesNotFsync(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend, err := OpenRootBackend(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.CloseBackend() })
	counter := &fsyncCounter{Backend: backend}
	handle, _, err := backend.Open(context.Background(), "note.txt", OpenRead|OpenWrite, 0)
	if err != nil {
		t.Fatal(err)
	}
	f := &fuseFile{state: &fuseState{backend: counter}, handle: handle}
	if errno := f.Flush(context.Background()); errno != 0 {
		t.Fatalf("Flush = %v", errno)
	}
	if counter.fsyncs != 0 {
		t.Fatalf("Flush called Fsync %d times", counter.fsyncs)
	}
	if errno := f.Fsync(context.Background(), 0); errno != 0 {
		t.Fatalf("Fsync = %v", errno)
	}
	if counter.fsyncs != 1 {
		t.Fatalf("Fsync calls = %d, want 1", counter.fsyncs)
	}
}

func TestGoFuseMountRetriesFailedUnmount(t *testing.T) {
	calls := 0
	mount := &goFuseMount{
		path: filepath.Join(t.TempDir(), "workspace"),
		unmount: func() error {
			calls++
			if calls == 1 {
				return syscall.EBUSY
			}
			return nil
		},
		done:     make(chan error, 1),
		waitDone: make(chan struct{}),
	}

	if err := mount.Unmount(context.Background()); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("first unmount error = %v, want EBUSY", err)
	}
	select {
	case <-mount.Done():
		t.Fatal("mount finished after failed unmount")
	default:
	}
	if err := mount.Unmount(context.Background()); err != nil {
		t.Fatalf("second unmount: %v", err)
	}
	if calls != 2 {
		t.Fatalf("unmount calls = %d, want 2", calls)
	}
}
