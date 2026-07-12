package remotefs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type RootBackend struct {
	root      *os.Root
	rootPath  string
	next      atomic.Uint64
	mu        sync.Mutex
	handles   map[uint64]*os.File
	closeOnce sync.Once
}

func OpenRootBackend(path string) (*RootBackend, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, err
	}
	b := &RootBackend{root: root, rootPath: absolute, handles: map[uint64]*os.File{}}
	b.next.Store(1)
	return b, nil
}

func cleanPath(name string) (string, error) {
	if name == "" || name == "." {
		return ".", nil
	}
	if filepath.IsAbs(name) {
		return "", syscall.EPERM
	}
	name = filepath.Clean(name)
	if name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
		return "", syscall.EPERM
	}
	return name, nil
}

func isVisibleMode(mode fs.FileMode) bool {
	return mode.IsRegular() || mode.IsDir() || mode&os.ModeSymlink != 0
}

func (b *RootBackend) Lookup(_ context.Context, name string) (Attr, error) {
	name, err := cleanPath(name)
	if err != nil {
		return Attr{}, err
	}
	info, err := b.root.Lstat(name)
	if err != nil {
		return Attr{}, err
	}
	if !isVisibleMode(info.Mode()) {
		return Attr{}, syscall.EPERM
	}
	return fileInfoToAttr(info), nil
}

func (b *RootBackend) ReadDir(_ context.Context, name string) ([]DirEntry, error) {
	name, err := cleanPath(name)
	if err != nil {
		return nil, err
	}
	f, err := b.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !isVisibleMode(info.Mode()) {
			continue
		}
		attr := fileInfoToAttr(info)
		out = append(out, DirEntry{Name: entry.Name(), Mode: attr.Mode, Ino: attr.Ino})
	}
	return out, nil
}

func (b *RootBackend) Open(_ context.Context, name string, flags OpenFlags, mode uint32) (uint64, Attr, error) {
	name, err := cleanPath(name)
	if err != nil {
		return 0, Attr{}, err
	}
	localFlags, err := localOpenFlags(flags)
	if err != nil {
		return 0, Attr{}, err
	}
	f, err := b.root.OpenFile(name, localFlags, fs.FileMode(mode&0o777))
	if err != nil {
		return 0, Attr{}, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return 0, Attr{}, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return 0, Attr{}, syscall.EPERM
	}
	id := b.next.Add(1)
	b.mu.Lock()
	b.handles[id] = f
	b.mu.Unlock()
	return id, fileInfoToAttr(info), nil
}

func localOpenFlags(flags OpenFlags) (int, error) {
	if !flags.Valid() {
		return 0, syscall.EINVAL
	}
	local := 0
	switch flags & (OpenRead | OpenWrite) {
	case OpenRead:
		local = os.O_RDONLY
	case OpenWrite:
		local = os.O_WRONLY
	case OpenRead | OpenWrite:
		local = os.O_RDWR
	default:
		return 0, syscall.EINVAL
	}
	if flags&OpenCreate != 0 {
		local |= os.O_CREATE
	}
	if flags&OpenExclusive != 0 {
		local |= os.O_EXCL
	}
	if flags&OpenTruncate != 0 {
		local |= os.O_TRUNC
	}
	if flags&OpenSync != 0 {
		local |= os.O_SYNC
	}
	// FUSE supplies the authoritative write offset for append operations.
	// Opening with O_APPEND would make os.File.WriteAt fail, so OpenAppend is
	// intentionally represented on the wire but not applied to the source fd.
	return local, nil
}

func (b *RootBackend) file(handle uint64) (*os.File, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f := b.handles[handle]
	if f == nil {
		return nil, syscall.EBADF
	}
	return f, nil
}

func (b *RootBackend) Close(_ context.Context, handle uint64) error {
	b.mu.Lock()
	f := b.handles[handle]
	delete(b.handles, handle)
	b.mu.Unlock()
	if f == nil {
		return syscall.EBADF
	}
	return f.Close()
}

func (b *RootBackend) Read(_ context.Context, handle uint64, offset int64, size uint32) ([]byte, error) {
	if size > MaxDataSize {
		size = MaxDataSize
	}
	f, err := b.file(handle)
	if err != nil {
		return nil, err
	}
	data := make([]byte, size)
	n, err := f.ReadAt(data, offset)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return data[:n], err
}

func (b *RootBackend) Write(_ context.Context, handle uint64, offset int64, data []byte) (uint32, error) {
	if len(data) > MaxDataSize {
		return 0, syscall.E2BIG
	}
	f, err := b.file(handle)
	if err != nil {
		return 0, err
	}
	n, err := f.WriteAt(data, offset)
	return uint32(n), err
}

func (b *RootBackend) Fsync(_ context.Context, handle uint64) error {
	f, err := b.file(handle)
	if err != nil {
		return err
	}
	return f.Sync()
}

func (b *RootBackend) Mkdir(_ context.Context, name string, mode uint32) (Attr, error) {
	name, err := cleanPath(name)
	if err != nil {
		return Attr{}, err
	}
	if err := b.root.Mkdir(name, fs.FileMode(mode&0o777)); err != nil {
		return Attr{}, err
	}
	info, err := b.root.Lstat(name)
	if err != nil {
		return Attr{}, err
	}
	return fileInfoToAttr(info), nil
}

func (b *RootBackend) Unlink(_ context.Context, name string) error {
	name, err := cleanPath(name)
	if err != nil {
		return err
	}
	info, err := b.root.Lstat(name)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return syscall.EISDIR
	}
	return b.root.Remove(name)
}

func (b *RootBackend) Rmdir(_ context.Context, name string) error {
	name, err := cleanPath(name)
	if err != nil {
		return err
	}
	info, err := b.root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return syscall.ENOTDIR
	}
	return b.root.Remove(name)
}

func (b *RootBackend) Rename(_ context.Context, oldName, newName string) error {
	oldName, err := cleanPath(oldName)
	if err != nil {
		return err
	}
	newName, err = cleanPath(newName)
	if err != nil {
		return err
	}
	return b.root.Rename(oldName, newName)
}

func (b *RootBackend) Link(_ context.Context, oldName, newName string) (Attr, error) {
	oldName, err := cleanPath(oldName)
	if err != nil {
		return Attr{}, err
	}
	newName, err = cleanPath(newName)
	if err != nil {
		return Attr{}, err
	}
	if err := b.root.Link(oldName, newName); err != nil {
		return Attr{}, err
	}
	info, err := b.root.Lstat(newName)
	if err != nil {
		return Attr{}, err
	}
	return fileInfoToAttr(info), nil
}

func (b *RootBackend) Symlink(_ context.Context, target, newName string) (Attr, error) {
	newName, err := cleanPath(newName)
	if err != nil {
		return Attr{}, err
	}
	if err := b.root.Symlink(target, newName); err != nil {
		return Attr{}, err
	}
	info, err := b.root.Lstat(newName)
	if err != nil {
		return Attr{}, err
	}
	return fileInfoToAttr(info), nil
}

func (b *RootBackend) Readlink(_ context.Context, name string) (string, error) {
	name, err := cleanPath(name)
	if err != nil {
		return "", err
	}
	return b.root.Readlink(name)
}

func (b *RootBackend) Setattr(_ context.Context, name string, handle uint64, change SetAttr) (Attr, error) {
	name, err := cleanPath(name)
	if err != nil {
		return Attr{}, err
	}
	var f *os.File
	if handle != 0 {
		f, err = b.file(handle)
		if err != nil {
			return Attr{}, err
		}
	}
	if change.Size != nil {
		if f != nil {
			err = f.Truncate(int64(*change.Size))
		} else {
			var opened *os.File
			opened, err = b.root.OpenFile(name, os.O_WRONLY, 0)
			if err == nil {
				err = opened.Truncate(int64(*change.Size))
				_ = opened.Close()
			}
		}
		if err != nil {
			return Attr{}, err
		}
	}
	if change.Mode != nil {
		if f != nil {
			err = f.Chmod(fs.FileMode(*change.Mode & 0o777))
		} else {
			err = b.root.Chmod(name, fs.FileMode(*change.Mode&0o777))
		}
		if err != nil {
			return Attr{}, err
		}
	}
	if change.AtimeNano != nil || change.MtimeNano != nil {
		info, statErr := b.root.Stat(name)
		if statErr != nil {
			return Attr{}, statErr
		}
		atime, mtime := attrTimes(info)
		if change.AtimeNano != nil {
			atime = time.Unix(0, *change.AtimeNano)
		}
		if change.MtimeNano != nil {
			mtime = time.Unix(0, *change.MtimeNano)
		}
		if err := b.root.Chtimes(name, atime, mtime); err != nil {
			return Attr{}, err
		}
	}
	info, err := b.root.Lstat(name)
	if err != nil {
		return Attr{}, err
	}
	return fileInfoToAttr(info), nil
}

func (b *RootBackend) StatFS(_ context.Context) (StatFS, error) {
	return statFS(b.rootPath)
}

func (b *RootBackend) CloseBackend() error {
	var first error
	b.closeOnce.Do(func() {
		b.mu.Lock()
		handles := b.handles
		b.handles = map[uint64]*os.File{}
		b.mu.Unlock()
		for _, f := range handles {
			if err := f.Close(); err != nil && first == nil {
				first = err
			}
		}
		if err := b.root.Close(); err != nil && first == nil {
			first = err
		}
	})
	return first
}
