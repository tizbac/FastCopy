package main

import (
	"os"
	"syscall"
)

// syscallStat carries the identity used for hard links, ownership, device
// numbers, --sort=inode and --one-file-system.
type syscallStat struct {
	dev  uint64
	ino  uint64
	uid  uint32
	gid  uint32
	rdev uint64
	nl   uint64
	mode uint32
}

func syscallStatOf(st os.FileInfo) *syscallStat {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || s == nil {
		return nil
	}
	return &syscallStat{
		dev:  uint64(s.Dev),
		ino:  uint64(s.Ino),
		uid:  s.Uid,
		gid:  s.Gid,
		rdev: uint64(s.Rdev),
		nl:   uint64(s.Nlink),
		mode: uint32(s.Mode),
	}
}

// major/minor decode a Linux dev_t (32-bit minor, 8-bit major split).
func major(dev uint64) uint64 {
	return (dev >> 8) & 0xfff
}

func minor(dev uint64) uint64 {
	return (dev & 0xff) | ((dev >> 12) & 0xfff00)
}
