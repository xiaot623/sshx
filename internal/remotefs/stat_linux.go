//go:build linux

package remotefs

import (
	"io/fs"
	"syscall"
	"time"
)

func fileInfoToAttr(info fs.FileInfo) Attr {
	stat, _ := info.Sys().(*syscall.Stat_t)
	attr := Attr{
		Size:      uint64(info.Size()),
		Mode:      fileMode(info.Mode()),
		AtimeNano: info.ModTime().UnixNano(),
		MtimeNano: info.ModTime().UnixNano(),
		CtimeNano: info.ModTime().UnixNano(),
		Nlink:     1,
	}
	if stat != nil {
		attr.Ino = stat.Ino
		attr.Blocks = uint64(stat.Blocks)
		attr.Nlink = uint32(stat.Nlink)
		attr.AtimeNano = time.Unix(stat.Atim.Sec, stat.Atim.Nsec).UnixNano()
		attr.MtimeNano = time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec).UnixNano()
		attr.CtimeNano = time.Unix(stat.Ctim.Sec, stat.Ctim.Nsec).UnixNano()
	}
	return attr
}

func attrTimes(info fs.FileInfo) (time.Time, time.Time) {
	stat, _ := info.Sys().(*syscall.Stat_t)
	if stat == nil {
		return info.ModTime(), info.ModTime()
	}
	return time.Unix(stat.Atim.Sec, stat.Atim.Nsec), time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)
}

func statFS(path string) (StatFS, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return StatFS{}, err
	}
	return StatFS{
		Blocks:  stat.Blocks,
		Bfree:   stat.Bfree,
		Bavail:  stat.Bavail,
		Files:   stat.Files,
		Ffree:   stat.Ffree,
		Bsize:   uint32(stat.Bsize),
		NameLen: uint32(stat.Namelen),
		Frsize:  uint32(stat.Frsize),
	}, nil
}

func fileMode(mode fs.FileMode) uint32 {
	out := uint32(mode.Perm())
	switch {
	case mode.IsDir():
		out |= syscall.S_IFDIR
	case mode&fs.ModeSymlink != 0:
		out |= syscall.S_IFLNK
	default:
		out |= syscall.S_IFREG
	}
	return out
}
