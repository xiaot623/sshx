package remotefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type GoFuseDriver struct{}

type goFuseMount struct {
	path        string
	server      *fuse.Server
	done        chan error
	waitDone    chan struct{}
	unmountOnce sync.Once
	finishOnce  sync.Once
	result      error
}

type fuseState struct {
	backend Backend
	options MountOptions

	statMu      sync.Mutex
	stat        StatFS
	statExpires time.Time
}

type fuseNode struct {
	fs.Inode
	state *fuseState
}

type fuseFile struct {
	state  *fuseState
	path   string
	handle uint64
	once   sync.Once
}

func (GoFuseDriver) Mount(ctx context.Context, path string, backend Backend, options MountOptions) (Mount, error) {
	if backend == nil {
		return nil, errors.New("remote fs backend is required")
	}
	if options.EntryTimeout <= 0 {
		options.EntryTimeout = 500 * time.Millisecond
	}
	if options.AttrTimeout <= 0 {
		options.AttrTimeout = 500 * time.Millisecond
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return nil, err
	}
	state := &fuseState{backend: backend, options: options}
	root := &fuseNode{state: state}
	mountOptions := fuse.MountOptions{
		Name:               "sshx",
		FsName:             "sshx",
		MaxWrite:           128 << 10,
		MaxReadAhead:       64 << 10,
		MaxBackground:      16,
		DisableXAttrs:      true,
		DisableReadDirPlus: false,
		SyncRead:           runtime.GOOS == "darwin",
	}
	if options.ReadOnly {
		mountOptions.Options = append(mountOptions.Options, "ro")
	}
	if runtime.GOOS == "darwin" {
		mountOptions.Options = append(mountOptions.Options, "daemon_timeout=0")
	}
	server, err := fs.Mount(path, root, &fs.Options{
		MountOptions: mountOptions,
		EntryTimeout: &options.EntryTimeout,
		AttrTimeout:  &options.AttrTimeout,
	})
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	mount := &goFuseMount{
		path:     path,
		server:   server,
		done:     make(chan error, 1),
		waitDone: make(chan struct{}),
	}
	go func() {
		server.Wait()
		mount.finish(nil)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = mount.Unmount(context.Background())
		case <-mount.waitDone:
		}
	}()
	return mount, nil
}

func (m *goFuseMount) Path() string       { return m.path }
func (m *goFuseMount) Done() <-chan error { return m.done }

func (m *goFuseMount) Unmount(ctx context.Context) error {
	var unmountErr error
	m.unmountOnce.Do(func() {
		unmountErr = m.server.Unmount()
		if unmountErr != nil {
			m.finish(unmountErr)
		}
	})
	if unmountErr != nil {
		return unmountErr
	}
	select {
	case <-m.waitDone:
		return m.result
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *goFuseMount) finish(err error) {
	m.finishOnce.Do(func() {
		m.result = err
		_ = os.Remove(m.path)
		m.done <- err
		close(m.done)
		close(m.waitDone)
	})
}

func (n *fuseNode) relPath() string {
	path := n.EmbeddedInode().Path(nil)
	if path == "" {
		return "."
	}
	return filepath.Clean(path)
}

func (n *fuseNode) child(attr Attr) *fs.Inode {
	child := &fuseNode{state: n.state}
	return n.NewInode(context.Background(), child, fs.StableAttr{Mode: attr.Mode & syscall.S_IFMT, Ino: attr.Ino})
}

func (n *fuseNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	attr, err := n.state.backend.Lookup(ctx, filepath.Join(n.relPath(), name))
	if err != nil {
		return nil, errnoOf(err)
	}
	fillEntry(out, attr, n.state.options)
	return n.child(attr), 0
}

func (n *fuseNode) Getattr(ctx context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	attr, err := n.state.backend.Lookup(ctx, n.relPath())
	if err != nil {
		return errnoOf(err)
	}
	fillAttr(&out.Attr, attr)
	out.SetTimeout(n.state.options.AttrTimeout)
	return 0
}

func (n *fuseNode) Setattr(ctx context.Context, file fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	var change SetAttr
	if mode, ok := in.GetMode(); ok {
		change.Mode = &mode
	}
	if size, ok := in.GetSize(); ok {
		change.Size = &size
	}
	if atime, ok := in.GetATime(); ok {
		nanos := atime.UnixNano()
		change.AtimeNano = &nanos
	}
	if mtime, ok := in.GetMTime(); ok {
		nanos := mtime.UnixNano()
		change.MtimeNano = &nanos
	}
	handle := uint64(0)
	if opened, ok := file.(*fuseFile); ok {
		handle = opened.handle
	}
	attr, err := n.state.backend.Setattr(ctx, n.relPath(), handle, change)
	if err != nil {
		return errnoOf(err)
	}
	fillAttr(&out.Attr, attr)
	out.SetTimeout(n.state.options.AttrTimeout)
	return 0
}

func (n *fuseNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	openFlags, err := portableOpenFlags(flags)
	if err != nil {
		return nil, 0, errnoOf(err)
	}
	handle, _, err := n.state.backend.Open(ctx, n.relPath(), openFlags, 0)
	if err != nil {
		return nil, 0, errnoOf(err)
	}
	return &fuseFile{state: n.state, path: n.relPath(), handle: handle}, fuse.FOPEN_DIRECT_IO, 0
}

func (n *fuseNode) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	path := filepath.Join(n.relPath(), name)
	openFlags, err := portableOpenFlags(flags)
	if err != nil {
		return nil, nil, 0, errnoOf(err)
	}
	openFlags |= OpenCreate
	handle, attr, err := n.state.backend.Open(ctx, path, openFlags, mode)
	if err != nil {
		return nil, nil, 0, errnoOf(err)
	}
	fillEntry(out, attr, n.state.options)
	return n.child(attr), &fuseFile{state: n.state, path: path, handle: handle}, fuse.FOPEN_DIRECT_IO, 0
}

func portableOpenFlags(flags uint32) (OpenFlags, error) {
	var portable OpenFlags
	switch flags & uint32(syscall.O_ACCMODE) {
	case uint32(syscall.O_RDONLY):
		portable |= OpenRead
	case uint32(syscall.O_WRONLY):
		portable |= OpenWrite
	case uint32(syscall.O_RDWR):
		portable |= OpenRead | OpenWrite
	default:
		return 0, syscall.EINVAL
	}
	if flags&uint32(syscall.O_CREAT) != 0 {
		portable |= OpenCreate
	}
	if flags&uint32(syscall.O_EXCL) != 0 {
		portable |= OpenExclusive
	}
	if flags&uint32(syscall.O_TRUNC) != 0 {
		portable |= OpenTruncate
	}
	if flags&uint32(syscall.O_APPEND) != 0 {
		portable |= OpenAppend
	}
	if syncFlag := uint32(syscall.O_SYNC); syncFlag != 0 && flags&syncFlag == syncFlag {
		portable |= OpenSync
	}
	return portable, nil
}

func (n *fuseNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	entries, err := n.state.backend.ReadDir(ctx, n.relPath())
	if err != nil {
		return nil, errnoOf(err)
	}
	out := make([]fuse.DirEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, fuse.DirEntry{Name: entry.Name, Mode: entry.Mode, Ino: entry.Ino})
	}
	return fs.NewListDirStream(out), 0
}

func (n *fuseNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	attr, err := n.state.backend.Mkdir(ctx, filepath.Join(n.relPath(), name), mode)
	if err != nil {
		return nil, errnoOf(err)
	}
	fillEntry(out, attr, n.state.options)
	return n.child(attr), 0
}

func (n *fuseNode) Unlink(ctx context.Context, name string) syscall.Errno {
	return errnoOf(n.state.backend.Unlink(ctx, filepath.Join(n.relPath(), name)))
}

func (n *fuseNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return errnoOf(n.state.backend.Rmdir(ctx, filepath.Join(n.relPath(), name)))
}

func (n *fuseNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if flags != 0 {
		return syscall.ENOTSUP
	}
	parent, ok := newParent.(*fuseNode)
	if !ok {
		return syscall.EXDEV
	}
	return errnoOf(n.state.backend.Rename(
		ctx,
		filepath.Join(n.relPath(), name),
		filepath.Join(parent.relPath(), newName),
	))
}

func (n *fuseNode) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	oldNode, ok := target.(*fuseNode)
	if !ok {
		return nil, syscall.EXDEV
	}
	attr, err := n.state.backend.Link(ctx, oldNode.relPath(), filepath.Join(n.relPath(), name))
	if err != nil {
		return nil, errnoOf(err)
	}
	fillEntry(out, attr, n.state.options)
	return n.child(attr), 0
}

func (n *fuseNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	attr, err := n.state.backend.Symlink(ctx, target, filepath.Join(n.relPath(), name))
	if err != nil {
		return nil, errnoOf(err)
	}
	fillEntry(out, attr, n.state.options)
	return n.child(attr), 0
}

func (n *fuseNode) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	target, err := n.state.backend.Readlink(ctx, n.relPath())
	if err != nil {
		return nil, errnoOf(err)
	}
	return []byte(target), 0
}

func (n *fuseNode) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	n.state.statMu.Lock()
	defer n.state.statMu.Unlock()
	if time.Now().After(n.state.statExpires) {
		stat, err := n.state.backend.StatFS(ctx)
		if err != nil {
			return errnoOf(err)
		}
		n.state.stat = stat
		n.state.statExpires = time.Now().Add(2 * time.Second)
	}
	stat := n.state.stat
	out.Blocks = stat.Blocks
	out.Bfree = stat.Bfree
	out.Bavail = stat.Bavail
	out.Files = stat.Files
	out.Ffree = stat.Ffree
	out.Bsize = stat.Bsize
	out.NameLen = stat.NameLen
	out.Frsize = stat.Frsize
	return 0
}

func (f *fuseFile) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	size := len(dest)
	if size > MaxDataSize {
		size = MaxDataSize
	}
	data, err := f.state.backend.Read(ctx, f.handle, off, uint32(size))
	if err != nil {
		return nil, errnoOf(err)
	}
	return fuse.ReadResultData(data), 0
}

func (f *fuseFile) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	size, err := f.state.backend.Write(ctx, f.handle, off, data)
	return size, errnoOf(err)
}

func (f *fuseFile) Flush(ctx context.Context) syscall.Errno {
	return errnoOf(f.state.backend.Fsync(ctx, f.handle))
}

func (f *fuseFile) Fsync(ctx context.Context, _ uint32) syscall.Errno {
	return errnoOf(f.state.backend.Fsync(ctx, f.handle))
}

func (f *fuseFile) Release(ctx context.Context) syscall.Errno {
	var err error
	f.once.Do(func() {
		err = f.state.backend.Close(ctx, f.handle)
	})
	return errnoOf(err)
}

func fillEntry(out *fuse.EntryOut, attr Attr, options MountOptions) {
	fillAttr(&out.Attr, attr)
	out.SetEntryTimeout(options.EntryTimeout)
	out.SetAttrTimeout(options.AttrTimeout)
}

func fillAttr(out *fuse.Attr, attr Attr) {
	atime := time.Unix(0, attr.AtimeNano)
	mtime := time.Unix(0, attr.MtimeNano)
	ctime := time.Unix(0, attr.CtimeNano)
	out.Ino = attr.Ino
	out.Size = attr.Size
	out.Blocks = attr.Blocks
	out.Atime = uint64(atime.Unix())
	out.Atimensec = uint32(atime.Nanosecond())
	out.Mtime = uint64(mtime.Unix())
	out.Mtimensec = uint32(mtime.Nanosecond())
	out.Ctime = uint64(ctime.Unix())
	out.Ctimensec = uint32(ctime.Nanosecond())
	out.Mode = attr.Mode
	out.Nlink = attr.Nlink
	out.Owner.Uid = uint32(os.Getuid())
	out.Owner.Gid = uint32(os.Getgid())
	out.Blksize = 4096
}
