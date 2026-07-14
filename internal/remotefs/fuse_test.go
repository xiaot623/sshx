package remotefs

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
)

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
