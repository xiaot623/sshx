package processlock

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLockRejectsLiveOwnerAndCanBeReacquired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); err == nil {
		t.Fatal("second process lock acquisition unexpectedly succeeded")
	}
	first.Release()
	second, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	second.Release()
}

func TestLockRemovesStaleOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	if err := os.WriteFile(path, []byte(strconv.Itoa(1<<30)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	lock.Release()
}
