package remotefs

import "context"

type remoteBackend struct {
	peer    *Peer
	mountID string
}

func (b *remoteBackend) call(ctx context.Context, frame wireFrame) (wireFrame, error) {
	frame.MountID = b.mountID
	return b.peer.request(ctx, frame)
}

func (b *remoteBackend) Lookup(ctx context.Context, path string) (Attr, error) {
	response, err := b.call(ctx, wireFrame{Op: "lookup", Path: path})
	return response.Attr, err
}

func (b *remoteBackend) ReadDir(ctx context.Context, path string) ([]DirEntry, error) {
	response, err := b.call(ctx, wireFrame{Op: "readdir", Path: path})
	return response.Entries, err
}

func (b *remoteBackend) Open(ctx context.Context, path string, flags OpenFlags, mode uint32) (uint64, Attr, error) {
	response, err := b.call(ctx, wireFrame{Op: "open", Path: path, OpenFlags: flags, Mode: mode})
	return response.Handle, response.Attr, err
}

func (b *remoteBackend) Close(ctx context.Context, handle uint64) error {
	_, err := b.call(ctx, wireFrame{Op: "close", Handle: handle})
	return err
}

func (b *remoteBackend) Read(ctx context.Context, handle uint64, offset int64, size uint32) ([]byte, error) {
	response, err := b.call(ctx, wireFrame{Op: "read", Handle: handle, Offset: offset, Size: size})
	return response.Data, err
}

func (b *remoteBackend) Write(ctx context.Context, handle uint64, offset int64, data []byte) (uint32, error) {
	response, err := b.call(ctx, wireFrame{Op: "write", Handle: handle, Offset: offset, Data: data})
	return response.Size, err
}

func (b *remoteBackend) Fsync(ctx context.Context, handle uint64) error {
	_, err := b.call(ctx, wireFrame{Op: "fsync", Handle: handle})
	return err
}

func (b *remoteBackend) Mkdir(ctx context.Context, path string, mode uint32) (Attr, error) {
	response, err := b.call(ctx, wireFrame{Op: "mkdir", Path: path, Mode: mode})
	return response.Attr, err
}

func (b *remoteBackend) Unlink(ctx context.Context, path string) error {
	_, err := b.call(ctx, wireFrame{Op: "unlink", Path: path})
	return err
}

func (b *remoteBackend) Rmdir(ctx context.Context, path string) error {
	_, err := b.call(ctx, wireFrame{Op: "rmdir", Path: path})
	return err
}

func (b *remoteBackend) Rename(ctx context.Context, oldPath, newPath string) error {
	_, err := b.call(ctx, wireFrame{Op: "rename", Path: oldPath, Path2: newPath})
	return err
}

func (b *remoteBackend) Link(ctx context.Context, oldPath, newPath string) (Attr, error) {
	response, err := b.call(ctx, wireFrame{Op: "link", Path: oldPath, Path2: newPath})
	return response.Attr, err
}

func (b *remoteBackend) Symlink(ctx context.Context, target, path string) (Attr, error) {
	response, err := b.call(ctx, wireFrame{Op: "symlink", Target: target, Path: path})
	return response.Attr, err
}

func (b *remoteBackend) Readlink(ctx context.Context, path string) (string, error) {
	response, err := b.call(ctx, wireFrame{Op: "readlink", Path: path})
	return response.Target, err
}

func (b *remoteBackend) Setattr(ctx context.Context, path string, handle uint64, change SetAttr) (Attr, error) {
	response, err := b.call(ctx, wireFrame{Op: "setattr", Path: path, Handle: handle, Change: change})
	return response.Attr, err
}

func (b *remoteBackend) StatFS(ctx context.Context) (StatFS, error) {
	response, err := b.call(ctx, wireFrame{Op: "statfs"})
	return response.StatFS, err
}

func (b *remoteBackend) CloseBackend() error { return nil }
