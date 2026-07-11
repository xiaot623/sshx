package processlock

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

type Lock struct {
	path string
	file *os.File
}

func Acquire(path string) (*Lock, error) {
	for attempt := 0; attempt < 2; attempt++ {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
				_ = file.Close()
				_ = os.Remove(path)
				return nil, err
			}
			return &Lock{path: path, file: file}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		contents, readErr := os.ReadFile(path)
		pid, parseErr := strconv.Atoi(strings.TrimSpace(string(contents)))
		if readErr == nil && parseErr == nil && pid > 0 {
			if err := syscall.Kill(pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
				return nil, fmt.Errorf("process %d holds %s", pid, path)
			}
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("could not acquire %s", path)
}

func (l *Lock) Release() {
	if l == nil {
		return
	}
	if l.file != nil {
		_ = l.file.Close()
	}
	_ = os.Remove(l.path)
}
