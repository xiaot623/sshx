package remotefs

import (
	"context"
	"io"
	"time"
)

const (
	ProtocolVersion = 2
	MaxMetadataSize = 64 << 10
	MaxDataSize     = 256 << 10
	MaxInFlight     = 64
)

// OpenFlags is the platform-independent representation carried by the remote
// filesystem protocol. It must never contain values from os.O_* or syscall.O_*;
// those values differ between Darwin and Linux.
type OpenFlags uint16

const (
	OpenRead OpenFlags = 1 << iota
	OpenWrite
	OpenCreate
	OpenExclusive
	OpenTruncate
	OpenAppend
	OpenSync
)

func (f OpenFlags) Valid() bool {
	const known = OpenRead | OpenWrite | OpenCreate | OpenExclusive | OpenTruncate | OpenAppend | OpenSync
	return f != 0 && f&^known == 0 && f&(OpenRead|OpenWrite) != 0
}

// ErrorCode is a stable wire-level error identity. Local syscall errno values
// are deliberately translated at the protocol boundary because their numeric
// values are operating-system ABI, not protocol data.
type ErrorCode string

const (
	ErrorNone           ErrorCode = ""
	ErrorPermission     ErrorCode = "permission"
	ErrorNotPermitted   ErrorCode = "not_permitted"
	ErrorNotFound       ErrorCode = "not_found"
	ErrorExists         ErrorCode = "exists"
	ErrorBadHandle      ErrorCode = "bad_handle"
	ErrorIsDir          ErrorCode = "is_directory"
	ErrorNotDir         ErrorCode = "not_directory"
	ErrorCrossDev       ErrorCode = "cross_device"
	ErrorBusy           ErrorCode = "busy"
	ErrorTooLarge       ErrorCode = "too_large"
	ErrorUnsupported    ErrorCode = "unsupported"
	ErrorNotImplemented ErrorCode = "not_implemented"
	ErrorInvalid        ErrorCode = "invalid"
	ErrorInterrupted    ErrorCode = "interrupted"
	ErrorTimedOut       ErrorCode = "timed_out"
	ErrorIO             ErrorCode = "io"
)

type Attr struct {
	Ino       uint64 `json:"ino,omitempty"`
	Size      uint64 `json:"size,omitempty"`
	Blocks    uint64 `json:"blocks,omitempty"`
	Mode      uint32 `json:"mode,omitempty"`
	Nlink     uint32 `json:"nlink,omitempty"`
	AtimeNano int64  `json:"atimeNano,omitempty"`
	MtimeNano int64  `json:"mtimeNano,omitempty"`
	CtimeNano int64  `json:"ctimeNano,omitempty"`
}

type DirEntry struct {
	Name string `json:"name"`
	Mode uint32 `json:"mode"`
	Ino  uint64 `json:"ino,omitempty"`
}

type SetAttr struct {
	Mode      *uint32 `json:"mode,omitempty"`
	Size      *uint64 `json:"size,omitempty"`
	AtimeNano *int64  `json:"atimeNano,omitempty"`
	MtimeNano *int64  `json:"mtimeNano,omitempty"`
}

type StatFS struct {
	Blocks  uint64 `json:"blocks,omitempty"`
	Bfree   uint64 `json:"bfree,omitempty"`
	Bavail  uint64 `json:"bavail,omitempty"`
	Files   uint64 `json:"files,omitempty"`
	Ffree   uint64 `json:"ffree,omitempty"`
	Bsize   uint32 `json:"bsize,omitempty"`
	NameLen uint32 `json:"nameLen,omitempty"`
	Frsize  uint32 `json:"frsize,omitempty"`
}

type Backend interface {
	Lookup(context.Context, string) (Attr, error)
	ReadDir(context.Context, string) ([]DirEntry, error)
	Open(context.Context, string, OpenFlags, uint32) (uint64, Attr, error)
	Close(context.Context, uint64) error
	Read(context.Context, uint64, int64, uint32) ([]byte, error)
	Write(context.Context, uint64, int64, []byte) (uint32, error)
	Fsync(context.Context, uint64) error
	Mkdir(context.Context, string, uint32) (Attr, error)
	Unlink(context.Context, string) error
	Rmdir(context.Context, string) error
	Rename(context.Context, string, string) error
	Link(context.Context, string, string) (Attr, error)
	Symlink(context.Context, string, string) (Attr, error)
	Readlink(context.Context, string) (string, error)
	Setattr(context.Context, string, uint64, SetAttr) (Attr, error)
	StatFS(context.Context) (StatFS, error)
	CloseBackend() error
}

type MountOptions struct {
	ReadOnly     bool
	EntryTimeout time.Duration
	AttrTimeout  time.Duration
}

type Mount interface {
	Path() string
	Unmount(context.Context) error
	Done() <-chan error
}

type MountDriver interface {
	Mount(context.Context, string, Backend, MountOptions) (Mount, error)
}

type ReadWriteCloser interface {
	io.Reader
	io.Writer
	io.Closer
}
