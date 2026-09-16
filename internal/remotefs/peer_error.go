package remotefs

import (
	"context"
	"errors"
	"os"
	"syscall"
)

func attrFrame(attr Attr, err error) wireFrame {
	if err != nil {
		return errorFrame(err)
	}
	return wireFrame{Attr: attr}
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
