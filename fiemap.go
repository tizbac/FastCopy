package main

import (
	"os"
	"syscall"
	"unsafe"
)

// FS_IOC_FIEMAP = _IOWR('f', 11, struct fiemap)
//
//	= (3 << 30) | ('f' << 8) | 11 | (sizeof(struct fiemap) << 16)
//
// struct fiemap is 32 bytes and the extents are stored inline after it.
const fiemapReq uint64 = (3 << 30) | ('f' << 8) | 11 | (32 << 16)

const extentCount = 64

// FIEMAP extent flags we are not interested in for ordering.
const (
	extLast       = 1 << 0
	extUnknown    = 1 << 1
	extDelalloc   = 1 << 2
	extEncoded    = 1 << 3
	extNotAligned = 1 << 4
	extInlineData = 1 << 5
	extDataTail   = 1 << 6
	extUnwritten  = 1 << 11
)

type fiemapHeader struct {
	Start         uint64
	Length        uint64
	Flags         uint32
	MappedExtents uint32
	ExtentCount   uint32
	Reserved      uint32
}

type fiemapExtent struct {
	Logical   uint64
	Physical  uint64
	Length    uint64
	Reserved1 uint64
	Reserved2 uint64
	Flags     uint32
	Reserved3 [3]uint32
}

type fiemap struct {
	Header  fiemapHeader
	Extents [extentCount]fiemapExtent
}

// firstPhysicalOffset returns the physical byte offset of the first data extent
// of a file. --fast uses it to read files in the order they are laid out on the
// platter, which keeps the heads mostly sequential on rotating media.
func firstPhysicalOffset(path string) (uint64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	var fm fiemap
	fm.Header.Length = ^uint64(0)
	fm.Header.ExtentCount = extentCount

	r, _, errno := syscall.RawSyscall(syscall.SYS_IOCTL, uintptr(f.Fd()), uintptr(fiemapReq), uintptr(unsafe.Pointer(&fm)))
	if errno != 0 || r != 0 || fm.Header.MappedExtents == 0 {
		return 0, false
	}

	for i := 0; i < int(fm.Header.MappedExtents) && i < extentCount; i++ {
		e := fm.Extents[i]
		if e.Physical == 0 {
			continue // hole, tail-packed inode, or not yet allocated
		}
		if e.Flags&(extUnknown|extDelalloc|extEncoded|extInlineData|extDataTail|extUnwritten) != 0 {
			continue
		}
		return e.Physical, true
	}
	return 0, false
}
