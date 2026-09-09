package remotefs

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
)

func SetupMountPoint(base, hierarchy string) (string, error) {
	clean, err := CleanMountPath(hierarchy)
	if err != nil {
		return "", err
	}
	path := filepath.Join(base, filepath.FromSlash(clean))
	_ = syscall.Unmount(path, 0)
	if err := os.RemoveAll(path); err != nil {
		return "", err
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(base, ".mount-path"), []byte(clean+"\n"), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// MountLocal sets up a mount point under base and mounts backend there.
// ctx is the mount lifetime: cancel it only when the mount should go away.
func MountLocal(ctx context.Context, driver MountDriver, base, hierarchy string, backend Backend, options MountOptions) (Mount, error) {
	path, err := SetupMountPoint(base, hierarchy)
	if err != nil {
		return nil, err
	}
	if driver == nil {
		driver = GoFuseDriver{}
	}
	mount, err := driver.Mount(ctx, path, backend, options)
	if err != nil {
		_ = os.RemoveAll(base)
		return nil, err
	}
	return mount, nil
}
