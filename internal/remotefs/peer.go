package remotefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
)

type PeerOptions struct {
	OnMount   func(context.Context, *Peer, string, string, MountOptions) (string, error)
	OnUnmount func(context.Context, string) error
}

type Peer struct {
	conn      ReadWriteCloser
	sessionID string
	opts      PeerOptions

	writeMu sync.Mutex
	nextID  atomic.Uint64

	pendingMu sync.Mutex
	pending   map[uint64]chan wireFrame

	activeMu sync.Mutex
	active   map[uint64]context.CancelFunc

	backendMu     sync.RWMutex
	backends      map[string]Backend
	backendClosed bool

	mountMu     sync.Mutex
	mounts      map[string]struct{}
	mountClosed bool

	sem       chan struct{}
	outgoing  chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	errMu     sync.Mutex
	err       error
}

func Connect(ctx context.Context, conn ReadWriteCloser, sessionID, token string, opts PeerOptions) (*Peer, error) {
	if sessionID == "" {
		return nil, errors.New("remote fs sessionId is required")
	}
	if err := writeWireFrame(conn, wireFrame{Type: frameHello, Version: ProtocolVersion, SessionID: sessionID, Token: token}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	response, err := readWireFrame(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if response.Type != frameHelloOK {
		_ = conn.Close()
		if response.Error != "" {
			return nil, errors.New(response.Error)
		}
		return nil, fmt.Errorf("unexpected remote fs handshake %q", response.Type)
	}
	if response.Version != ProtocolVersion {
		_ = conn.Close()
		return nil, fmt.Errorf("remote fs protocol mismatch: server=%d client=%d", response.Version, ProtocolVersion)
	}
	return startPeer(ctx, conn, sessionID, opts, 1), nil
}

func Accept(ctx context.Context, conn ReadWriteCloser, validate func(sessionID, token string) error, opts PeerOptions) (*Peer, error) {
	hello, err := readWireFrame(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if hello.Type != frameHello || hello.SessionID == "" {
		_ = writeWireFrame(conn, wireFrame{Type: frameResponse, Error: "invalid remote fs hello"})
		_ = conn.Close()
		return nil, errors.New("invalid remote fs hello")
	}
	if hello.Version != ProtocolVersion {
		_ = writeWireFrame(conn, wireFrame{Type: frameResponse, Version: ProtocolVersion, Error: "remote fs protocol version changed"})
		_ = conn.Close()
		return nil, errors.New("remote fs protocol version changed")
	}
	if validate != nil {
		if err := validate(hello.SessionID, hello.Token); err != nil {
			_ = writeWireFrame(conn, wireFrame{Type: frameResponse, Error: err.Error()})
			_ = conn.Close()
			return nil, err
		}
	}
	if err := writeWireFrame(conn, wireFrame{Type: frameHelloOK, Version: ProtocolVersion, SessionID: hello.SessionID}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return startPeer(ctx, conn, hello.SessionID, opts, 0), nil
}

func startPeer(ctx context.Context, conn ReadWriteCloser, sessionID string, opts PeerOptions, requestIDBase uint64) *Peer {
	p := &Peer{
		conn:      conn,
		sessionID: sessionID,
		opts:      opts,
		pending:   map[uint64]chan wireFrame{},
		active:    map[uint64]context.CancelFunc{},
		backends:  map[string]Backend{},
		mounts:    map[string]struct{}{},
		sem:       make(chan struct{}, MaxInFlight),
		outgoing:  make(chan struct{}, MaxInFlight),
		done:      make(chan struct{}),
	}
	// Client requests use odd IDs and server requests use even IDs. Frame type
	// already disambiguates requests from responses, while split ID spaces also
	// make cancellation and diagnostics unambiguous during full-duplex traffic.
	p.nextID.Store(requestIDBase)
	go p.readLoop(ctx)
	go func() {
		select {
		case <-ctx.Done():
			p.close(ctx.Err())
		case <-p.done:
		}
	}()
	return p
}

func (p *Peer) SessionID() string     { return p.sessionID }
func (p *Peer) Done() <-chan struct{} { return p.done }

func (p *Peer) Err() error {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	return p.err
}

func (p *Peer) Close() error {
	p.close(nil)
	return p.Err()
}

func (p *Peer) RegisterBackend(mountID string, backend Backend) error {
	if mountID == "" || backend == nil {
		return errors.New("mountId and backend are required")
	}
	p.backendMu.Lock()
	defer p.backendMu.Unlock()
	if p.backendClosed {
		return io.ErrClosedPipe
	}
	if _, exists := p.backends[mountID]; exists {
		return fmt.Errorf("remote fs export %q already exists", mountID)
	}
	p.backends[mountID] = backend
	return nil
}

func (p *Peer) UnregisterBackend(mountID string) error {
	p.backendMu.Lock()
	backend := p.backends[mountID]
	delete(p.backends, mountID)
	p.backendMu.Unlock()
	if backend == nil {
		return nil
	}
	return backend.CloseBackend()
}

func (p *Peer) CreateMount(ctx context.Context, mountID string) (string, error) {
	return p.CreateMountAt(ctx, mountID, "workspace")
}

func (p *Peer) CreateMountAt(ctx context.Context, mountID, mountPath string) (string, error) {
	return p.CreateMountAtWithOptions(ctx, mountID, mountPath, MountOptions{})
}

func (p *Peer) CreateMountAtWithOptions(ctx context.Context, mountID, mountPath string, options MountOptions) (string, error) {
	response, err := p.request(ctx, wireFrame{MountID: mountID, MountPath: mountPath, ReadOnly: options.ReadOnly, Op: "mount.create"})
	if err != nil {
		return "", err
	}
	return response.MountPath, nil
}

func (p *Peer) ReleaseMount(ctx context.Context, mountID string) error {
	_, err := p.request(ctx, wireFrame{MountID: mountID, Op: "mount.release"})
	return err
}

func (p *Peer) RemoteBackend(mountID string) Backend {
	return &remoteBackend{peer: p, mountID: mountID}
}

func (p *Peer) request(ctx context.Context, request wireFrame) (wireFrame, error) {
	select {
	case p.outgoing <- struct{}{}:
		defer func() { <-p.outgoing }()
	case <-ctx.Done():
		return wireFrame{}, ctx.Err()
	case <-p.done:
		if err := p.Err(); err != nil {
			return wireFrame{}, err
		}
		return wireFrame{}, io.EOF
	}
	id := p.nextID.Add(2)
	request.Type = frameRequest
	request.ID = id
	responseCh := make(chan wireFrame, 1)
	p.pendingMu.Lock()
	p.pending[id] = responseCh
	p.pendingMu.Unlock()
	defer func() {
		p.pendingMu.Lock()
		delete(p.pending, id)
		p.pendingMu.Unlock()
	}()
	if err := p.send(request); err != nil {
		return wireFrame{}, err
	}
	select {
	case response := <-responseCh:
		if response.ErrorCode != ErrorNone {
			return response, errorFromCode(response.ErrorCode)
		}
		if response.Error != "" {
			return response, errors.New(response.Error)
		}
		return response, nil
	case <-ctx.Done():
		cancelFrame := wireFrame{Type: frameCancel, ID: id}
		if request.Op == "mount.create" {
			// A canceled caller does not take ownership of a mount even if the
			// successful response crossed the cancellation on the wire.
			cancelFrame.MountID = request.MountID
		}
		_ = p.send(cancelFrame)
		return wireFrame{}, ctx.Err()
	case <-p.done:
		if err := p.Err(); err != nil {
			return wireFrame{}, err
		}
		return wireFrame{}, io.EOF
	}
}

func (p *Peer) send(frame wireFrame) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	select {
	case <-p.done:
		return io.ErrClosedPipe
	default:
		return writeWireFrame(p.conn, frame)
	}
}

func (p *Peer) readLoop(ctx context.Context) {
	for {
		frame, err := readWireFrame(p.conn)
		if err != nil {
			p.close(err)
			return
		}
		switch frame.Type {
		case frameResponse:
			p.pendingMu.Lock()
			ch := p.pending[frame.ID]
			p.pendingMu.Unlock()
			if ch != nil {
				select {
				case ch <- frame:
				default:
				}
			}
		case frameCancel:
			p.activeMu.Lock()
			cancel := p.active[frame.ID]
			p.activeMu.Unlock()
			if cancel != nil {
				cancel()
			}
			if frame.MountID != "" && p.opts.OnUnmount != nil {
				p.mountMu.Lock()
				delete(p.mounts, frame.MountID)
				p.mountMu.Unlock()
				go func(mountID string) {
					_ = p.opts.OnUnmount(context.Background(), mountID)
				}(frame.MountID)
			}
		case frameRequest:
			select {
			case p.sem <- struct{}{}:
				requestCtx, cancel := context.WithCancel(ctx)
				p.activeMu.Lock()
				p.active[frame.ID] = cancel
				p.activeMu.Unlock()
				go func(request wireFrame) {
					defer func() {
						cancel()
						p.activeMu.Lock()
						delete(p.active, request.ID)
						p.activeMu.Unlock()
						<-p.sem
					}()
					response := p.handleRequest(requestCtx, request)
					if requestCtx.Err() != nil {
						return
					}
					response.Type = frameResponse
					response.ID = request.ID
					p.sendResponse(response)
				}(frame)
			default:
				p.sendResponse(wireFrame{Type: frameResponse, ID: frame.ID, ErrorCode: ErrorBusy, Error: "too many remote fs requests"})
			}
		default:
			p.close(fmt.Errorf("unexpected remote fs frame %q", frame.Type))
			return
		}
	}
}

func (p *Peer) sendResponse(response wireFrame) {
	err := p.send(response)
	if errors.Is(err, ErrFrameTooLarge) {
		err = p.send(wireFrame{
			Type:      frameResponse,
			ID:        response.ID,
			ErrorCode: ErrorTooLarge,
			Error:     err.Error(),
		})
	}
	if err != nil {
		p.close(err)
	}
}

func (p *Peer) handleRequest(ctx context.Context, request wireFrame) wireFrame {
	if request.Op == "mount.create" {
		if p.opts.OnMount == nil {
			return errorFrame(syscall.ENOTSUP)
		}
		path, err := p.opts.OnMount(ctx, p, request.MountID, request.MountPath, MountOptions{ReadOnly: request.ReadOnly})
		if err != nil {
			return errorFrame(err)
		}
		p.mountMu.Lock()
		if p.mountClosed || ctx.Err() != nil {
			p.mountMu.Unlock()
			if p.opts.OnUnmount != nil {
				_ = p.opts.OnUnmount(context.Background(), request.MountID)
			}
			if err := ctx.Err(); err != nil {
				return errorFrame(err)
			}
			return errorFrame(io.ErrClosedPipe)
		}
		p.mounts[request.MountID] = struct{}{}
		p.mountMu.Unlock()
		return wireFrame{MountPath: path}
	}
	if request.Op == "mount.release" {
		if p.opts.OnUnmount != nil {
			if err := p.opts.OnUnmount(ctx, request.MountID); err != nil {
				return errorFrame(err)
			}
		}
		p.mountMu.Lock()
		delete(p.mounts, request.MountID)
		p.mountMu.Unlock()
		return wireFrame{}
	}

	p.backendMu.RLock()
	backend := p.backends[request.MountID]
	p.backendMu.RUnlock()
	if backend == nil {
		return errorFrame(syscall.ENOENT)
	}
	switch request.Op {
	case "lookup":
		attr, err := backend.Lookup(ctx, request.Path)
		return attrFrame(attr, err)
	case "readdir":
		entries, err := backend.ReadDir(ctx, request.Path)
		if err != nil {
			return errorFrame(err)
		}
		return wireFrame{Entries: entries}
	case "open":
		if !request.OpenFlags.Valid() {
			return errorFrame(syscall.EINVAL)
		}
		handle, attr, err := backend.Open(ctx, request.Path, request.OpenFlags, request.Mode)
		if err != nil {
			return errorFrame(err)
		}
		return wireFrame{Handle: handle, Attr: attr}
	case "close":
		return errorFrameOrEmpty(backend.Close(ctx, request.Handle))
	case "read":
		data, err := backend.Read(ctx, request.Handle, request.Offset, request.Size)
		if err != nil {
			return errorFrame(err)
		}
		return wireFrame{Data: data, Size: uint32(len(data))}
	case "write":
		size, err := backend.Write(ctx, request.Handle, request.Offset, request.Data)
		if err != nil {
			return errorFrame(err)
		}
		return wireFrame{Size: size}
	case "fsync":
		return errorFrameOrEmpty(backend.Fsync(ctx, request.Handle))
	case "mkdir":
		attr, err := backend.Mkdir(ctx, request.Path, request.Mode)
		return attrFrame(attr, err)
	case "unlink":
		return errorFrameOrEmpty(backend.Unlink(ctx, request.Path))
	case "rmdir":
		return errorFrameOrEmpty(backend.Rmdir(ctx, request.Path))
	case "rename":
		return errorFrameOrEmpty(backend.Rename(ctx, request.Path, request.Path2))
	case "link":
		attr, err := backend.Link(ctx, request.Path, request.Path2)
		return attrFrame(attr, err)
	case "symlink":
		attr, err := backend.Symlink(ctx, request.Target, request.Path)
		return attrFrame(attr, err)
	case "readlink":
		target, err := backend.Readlink(ctx, request.Path)
		if err != nil {
			return errorFrame(err)
		}
		return wireFrame{Target: target}
	case "setattr":
		attr, err := backend.Setattr(ctx, request.Path, request.Handle, request.Change)
		return attrFrame(attr, err)
	case "statfs":
		stat, err := backend.StatFS(ctx)
		if err != nil {
			return errorFrame(err)
		}
		return wireFrame{StatFS: stat}
	default:
		return errorFrame(syscall.ENOSYS)
	}
}

func attrFrame(attr Attr, err error) wireFrame {
	if err != nil {
		return errorFrame(err)
	}
	return wireFrame{Attr: attr}
}

func errorFrameOrEmpty(err error) wireFrame {
	if err == nil {
		return wireFrame{}
	}
	return errorFrame(err)
}

func errorFrame(err error) wireFrame {
	if err == nil {
		return wireFrame{}
	}
	return wireFrame{ErrorCode: errorCodeOf(err), Error: err.Error()}
}

func errorCodeOf(err error) ErrorCode {
	errno := errnoOf(err)
	switch errno {
	case 0:
		return ErrorNone
	case syscall.EPERM:
		return ErrorNotPermitted
	case syscall.EACCES:
		return ErrorPermission
	case syscall.ENOENT:
		return ErrorNotFound
	case syscall.EEXIST:
		return ErrorExists
	case syscall.EBADF:
		return ErrorBadHandle
	case syscall.EISDIR:
		return ErrorIsDir
	case syscall.ENOTDIR:
		return ErrorNotDir
	case syscall.EXDEV:
		return ErrorCrossDev
	case syscall.EBUSY:
		return ErrorBusy
	case syscall.E2BIG:
		return ErrorTooLarge
	case syscall.ENOTSUP:
		return ErrorUnsupported
	case syscall.ENOSYS:
		return ErrorNotImplemented
	case syscall.EINVAL:
		return ErrorInvalid
	case syscall.EINTR:
		return ErrorInterrupted
	case syscall.ETIMEDOUT:
		return ErrorTimedOut
	default:
		return ErrorIO
	}
}

func errorFromCode(code ErrorCode) error {
	switch code {
	case ErrorNotPermitted:
		return syscall.EPERM
	case ErrorPermission:
		return syscall.EACCES
	case ErrorNotFound:
		return syscall.ENOENT
	case ErrorExists:
		return syscall.EEXIST
	case ErrorBadHandle:
		return syscall.EBADF
	case ErrorIsDir:
		return syscall.EISDIR
	case ErrorNotDir:
		return syscall.ENOTDIR
	case ErrorCrossDev:
		return syscall.EXDEV
	case ErrorBusy:
		return syscall.EBUSY
	case ErrorTooLarge:
		return syscall.E2BIG
	case ErrorUnsupported:
		return syscall.ENOTSUP
	case ErrorNotImplemented:
		return syscall.ENOSYS
	case ErrorInvalid:
		return syscall.EINVAL
	case ErrorInterrupted:
		return syscall.EINTR
	case ErrorTimedOut:
		return syscall.ETIMEDOUT
	case ErrorIO, ErrorNone:
		return syscall.EIO
	default:
		return syscall.EIO
	}
}

func errnoOf(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		if errors.As(pathErr.Err, &errno) {
			return errno
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return syscall.EINTR
	case errors.Is(err, context.DeadlineExceeded):
		return syscall.ETIMEDOUT
	case errors.Is(err, os.ErrNotExist):
		return syscall.ENOENT
	case errors.Is(err, os.ErrPermission):
		return syscall.EACCES
	case errors.Is(err, os.ErrExist):
		return syscall.EEXIST
	default:
		return syscall.EIO
	}
}

func (p *Peer) close(err error) {
	p.closeOnce.Do(func() {
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
			p.errMu.Lock()
			p.err = err
			p.errMu.Unlock()
		}
		_ = p.conn.Close()

		p.activeMu.Lock()
		for _, cancel := range p.active {
			cancel()
		}
		p.active = map[uint64]context.CancelFunc{}
		p.activeMu.Unlock()

		p.backendMu.Lock()
		backends := p.backends
		p.backends = map[string]Backend{}
		p.backendClosed = true
		p.backendMu.Unlock()
		p.mountMu.Lock()
		p.mountClosed = true
		mountIDs := make([]string, 0, len(p.mounts))
		for mountID := range p.mounts {
			mountIDs = append(mountIDs, mountID)
		}
		p.mounts = map[string]struct{}{}
		p.mountMu.Unlock()

		close(p.done)

		for _, backend := range backends {
			_ = backend.CloseBackend()
		}
		if p.opts.OnUnmount != nil {
			for _, mountID := range mountIDs {
				_ = p.opts.OnUnmount(context.Background(), mountID)
			}
		}
	})
}

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
