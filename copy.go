package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tiziano/fastcopy/internal/fiemap"
	"github.com/tiziano/fastcopy/internal/progress"
)

// entry is one file to copy. Directories are created while walking the tree,
// symlinks are created as we go, and regular files are queued here.
type entry struct {
	src        string
	dst        string
	size       int64
	dev        uint64
	order      uint64 // physical offset of the first extent, used by --fast
	inode      uint64
	mode       os.FileMode
	uid, gid   int
	atime      time.Time
	mtime      time.Time
	hardLinkTo string // destination of an already copied file sharing this inode
}

type copier struct {
	opts      *Options
	prog      *progress.Progress
	hardlinks map[uint64]string
	rootDev   uint64
	mu        sync.Mutex
}

func statT(path string) (syscall.Stat_t, error) {
	var st syscall.Stat_t
	info, err := os.Lstat(path)
	if err != nil {
		return st, err
	}
	if s, ok := info.Sys().(*syscall.Stat_t); ok && s != nil {
		return *s, nil
	}
	return st, fmt.Errorf("%s: cannot read file metadata", path)
}

// walk dispatches on the type of src, mirroring what cp does.
func (c *copier) walk(src, dst string, entries *[]entry) error {
	info, err := os.Lstat(src)
	if err != nil {
		return cpErr("stat", src, err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		if !c.opts.Dereference {
			target, err := os.Readlink(src)
			if err != nil {
				return cpErr("read link", src, err)
			}
			if c.opts.Verbose {
				c.log("%q -> %q", src, dst)
			}
			if err := c.symlink(target, dst); err != nil {
				return err
			}
			return nil
		}
		followed, err := os.Stat(src)
		if err != nil {
			return cpErr("stat", src, err)
		}
		if followed.IsDir() {
			return c.walkDir(src, dst, entries)
		}
		if !followed.Mode().IsRegular() {
			c.warn(fmt.Errorf("%s: skipping special file (%s)", src, followed.Mode()))
			return nil
		}
		c.addFile(entries, src, dst, followed)
		return nil
	}

	if info.IsDir() {
		if !c.opts.Recursive {
			return fmt.Errorf("%s: -r not specified; omitting directory", src)
		}
		return c.walkDir(src, dst, entries)
	}

	if !info.Mode().IsRegular() {
		if c.opts.CopyContents && info.Mode()&(os.ModeDevice|os.ModeNamedPipe) != 0 {
			c.addFile(entries, src, dst, info)
			return nil
		}
		c.warn(fmt.Errorf("%s: skipping special file (%s)", src, info.Mode()))
		return nil
	}

	c.addFile(entries, src, dst, info)
	return nil
}

func (c *copier) walkDir(src, dst string, entries *[]entry) error {
	info, err := os.Stat(src)
	if err != nil {
		return cpErr("stat", src, err)
	}
	st, err := statT(src)
	if err != nil {
		return err
	}
	if err := c.makeDir(dst, info.Mode(), &st); err != nil {
		return cpErr("create", dst, err)
	}
	if c.opts.Verbose {
		c.log("%q -> %q", src, dst)
	}

	items, err := os.ReadDir(src)
	if err != nil {
		return cpErr("read", src, err)
	}
	for _, item := range items {
		childSrc := filepath.Join(src, item.Name())
		childDst := filepath.Join(dst, item.Name())

		if c.opts.OneFileSystem {
			if cst, err := statT(childSrc); err == nil && cst.Dev != c.rootDev {
				c.warn(fmt.Errorf("%s: skipping, on a different file system", childSrc))
				continue
			}
		}
		if err := c.walk(childSrc, childDst, entries); err != nil {
			c.warn(err)
		}
	}
	return nil
}

func (c *copier) addFile(entries *[]entry, src, dst string, info os.FileInfo) {
	c.prog.ScanStep(src)
	st, err := statT(src)
	if err != nil {
		c.warn(err)
		return
	}

	e := entry{
		src:   src,
		dst:   dst,
		size:  info.Size(),
		dev:   st.Dev,
		inode: st.Ino,
		mode:  info.Mode(),
		uid:   int(st.Uid),
		gid:   int(st.Gid),
		atime: timeFromVal(st.Atim),
		mtime: timeFromVal(st.Mtim),
	}

	if c.opts.Fast {
		if off, ok := fiemap.FirstPhysicalOffset(src); ok {
			e.order = off
		}
		if c.opts.Debug {
			c.log("extent %s: physical=%d logical=%d", src, e.order, e.size)
		}
	}

	if c.opts.PreserveLinks || c.opts.Link {
		c.mu.Lock()
		if seen, ok := c.hardlinks[st.Ino]; ok {
			e.hardLinkTo = seen
		} else {
			c.hardlinks[st.Ino] = dst
		}
		c.mu.Unlock()
	}

	*entries = append(*entries, e)
}

// makeParents recreates the leading directories of src under target, which is
// what --parents asks for. cp creates them with the permissions of the source
// directories, and preserves owner/time when asked to.
func (c *copier) makeParents(src, target string) error {
	sub := strings.TrimPrefix(src, "/")
	comps := strings.Split(sub, "/")
	if len(comps) < 2 {
		return nil
	}
	leading := len(comps) - 1 // last component is created by walk()/addFile
	if isDir(src) {
		leading = len(comps)
	}
	for k := 1; k <= leading; k++ {
		srcDir := strings.Join(comps[:k], "/")
		if strings.HasPrefix(src, "/") {
			srcDir = "/" + srcDir
		}
		info, err := os.Stat(srcDir)
		if err != nil {
			return cpErr("stat", srcDir, err)
		}
		st, err := statT(srcDir)
		if err != nil {
			return err
		}
		if err := c.makeDir(filepath.Join(target, strings.Join(comps[:k], "/")), info.Mode(), &st); err != nil {
			return cpErr("create", srcDir, err)
		}
	}
	return nil
}

func (c *copier) makeDir(dst string, mode os.FileMode, st *syscall.Stat_t) error {
	if err := os.Mkdir(dst, mode&0777); err != nil {
		if errors.Is(err, os.ErrExist) {
			info, err := os.Stat(dst)
			if err == nil && info.IsDir() {
				return nil
			}
			return fmt.Errorf("%s exists but is not a directory", dst)
		}
		return err
	}
	if c.opts.PreserveMode {
		if err := os.Chmod(dst, mode&0777); err != nil {
			c.warn(cpErr("chmod", dst, err))
		}
	}
	if c.opts.PreserveOwner {
		if err := os.Chown(dst, int(st.Uid), int(st.Gid)); err != nil {
			c.warn(cpErr("chown", dst, err))
		}
	}
	if c.opts.PreserveTime {
		if err := syscall.UtimesNano(dst, []syscall.Timespec{
			{Sec: st.Atim.Sec, Nsec: st.Atim.Nsec},
			{Sec: st.Mtim.Sec, Nsec: st.Mtim.Nsec},
		}); err != nil {
			c.warn(cpErr("set times", dst, err))
		}
	}
	return nil
}

func (c *copier) symlink(target, dst string) error {
	if err := os.Symlink(target, dst); err != nil {
		if errors.Is(err, os.ErrExist) {
			if c.opts.NoClobber {
				return nil
			}
			if rmErr := os.Remove(dst); rmErr != nil {
				return rmErr
			}
			return os.Symlink(target, dst)
		}
		return err
	}
	return nil
}

// log prints a progress-adjacent message. When the animated bar is on screen it
// goes above the box so the bar keeps its place; otherwise it goes to stderr.
func (c *copier) log(format string, args ...any) {
	if c.prog.Active() {
		c.prog.Line(format, args...)
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func (c *copier) warn(err error) {
	c.log("fastcopy: %v", err)
}

func timeFromVal(ts syscall.Timespec) time.Time {
	return time.Unix(ts.Sec, ts.Nsec)
}

// ---------------------------------------------------------------- copy phase

func (c *copier) run(entries []entry) error {
	if c.opts.Fast {
		sort.SliceStable(entries, func(i, j int) bool {
			if entries[i].dev != entries[j].dev {
				return entries[i].dev < entries[j].dev
			}
			return entries[i].order < entries[j].order
		})
	}

	totalBytes := int64(0)
	for _, e := range entries {
		totalBytes += e.size
	}
	c.prog.StartCopy(len(entries), totalBytes)

	// Hard-link duplicates are copied after everything else so the file they
	// link to is guaranteed to exist, even with several workers or --fast.
	main, links := make([]entry, 0, len(entries)), make([]entry, 0, len(entries))
	for _, e := range entries {
		if e.hardLinkTo != "" {
			links = append(links, e)
		} else {
			main = append(main, e)
		}
	}

	copyAll := func(list []entry) error {
		var firstErr error
		for _, e := range list {
			if err := c.copyOne(e); err != nil {
				c.warn(cpErr("copy", e.src, err))
				if firstErr == nil {
					firstErr = err
				}
			}
		}
		return firstErr
	}

	if c.opts.Jobs <= 1 {
		err := copyAll(main)
		if err2 := copyAll(links); err2 != nil && err == nil {
			err = err2
		}
		return err
	}

	var nextIdx int
	next := func() (entry, bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if nextIdx >= len(main) {
			return entry{}, false
		}
		e := main[nextIdx]
		nextIdx++
		return e, true
	}

	var firstErr error
	errMu := sync.Mutex{}
	var wg sync.WaitGroup
	for w := 0; w < c.opts.Jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				e, ok := next()
				if !ok {
					return
				}
				if err := c.copyOne(e); err != nil {
					c.warn(cpErr("copy", e.src, err))
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	if err := copyAll(links); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (c *copier) copyOne(e entry) error {
	c.prog.Current(e.src)

	if sameFile(e.src, e.dst) {
		return fmt.Errorf("cannot copy a file to itself")
	}

	if e.hardLinkTo != "" {
		err := os.Link(e.hardLinkTo, e.dst)
		if err == nil || errors.Is(err, os.ErrExist) {
			if c.opts.Verbose {
				c.log("%q == %q (hard link)", e.src, e.dst)
			}
			c.prog.AddBytes(e.size)
			c.prog.AddFile()
			return nil
		}
		// With --jobs>1 the file this one links to may not have been copied
		// yet, so fall back to an ordinary copy.
	}

	if c.opts.Link {
		if err := os.Link(e.src, e.dst); err != nil {
			return fmt.Errorf("link: %w", err)
		}
		if c.opts.Verbose {
			c.log("%q -> %q (hard link)", e.src, e.dst)
		}
		c.prog.AddBytes(e.size)
		c.prog.AddFile()
		return nil
	}

	if c.opts.SymbolicLink {
		// cp -s links to the source path exactly as it was given.
		if err := c.symlink(e.src, e.dst); err != nil {
			return fmt.Errorf("symbolic link: %w", err)
		}
		if c.opts.Verbose {
			c.log("%q -> %q (symbolic link)", e.src, e.dst)
		}
		c.prog.AddBytes(e.size)
		c.prog.AddFile()
		return nil
	}

	dstInfo, err := os.Lstat(e.dst)
	exists := err == nil

	if c.opts.RemoveDest {
		if err := os.Remove(e.dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		exists = false
	}

	if exists && !dstInfo.Mode().IsRegular() {
		if dstInfo.IsDir() {
			return fmt.Errorf("cannot overwrite directory %s with a file", e.dst)
		}
		if c.opts.Force {
			if err := os.Remove(e.dst); err != nil {
				return err
			}
			exists = false
		} else {
			return fmt.Errorf("cannot create regular file over %s: %s", e.dst, dstInfo.Mode())
		}
	}

	if exists {
		if c.opts.NoClobber {
			c.prog.AddBytes(e.size)
			c.prog.AddFile()
			return nil
		}
		if c.opts.Update && !dstInfo.ModTime().Before(e.mtime) {
			c.prog.AddBytes(e.size)
			c.prog.AddFile()
			return nil
		}
		if c.opts.Interactive {
			c.log("overwrite %q? [y/N]", e.dst)
			var answer string
			fmt.Scanln(&answer)
			if answer != "y" && answer != "Y" {
				c.prog.AddBytes(e.size)
				c.prog.AddFile()
				return nil
			}
		}
		if c.opts.Backup {
			if err := os.Rename(e.dst, e.dst+c.opts.Suffix); err != nil {
				return fmt.Errorf("backup: %w", err)
			}
			exists = false
		}
	}

	if c.opts.RemoveDest && exists {
		if err := os.Remove(e.dst); err != nil {
			return err
		}
	}

	src, err := os.Open(e.src)
	if err != nil {
		return err
	}
	defer src.Close()

	openFlags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if c.opts.AttributesOnly && exists {
		// cp --attributes-only touches the metadata but leaves existing data.
		openFlags = os.O_WRONLY
	}
	dst, err := os.OpenFile(e.dst, openFlags, e.mode&0777)
	if err != nil && c.opts.Force && errors.Is(err, os.ErrPermission) {
		if rmErr := os.Remove(e.dst); rmErr == nil {
			dst, err = os.OpenFile(e.dst, openFlags, e.mode&0777)
		}
	}
	if err != nil {
		return err
	}

	if !c.opts.AttributesOnly {
		copied, err := copyData(src, dst, c.opts.Buffer, c.opts.Sparse == "always")
		if err != nil {
			dst.Close()
			return err
		}
		c.prog.AddBytes(copied)
	}
	if err := dst.Close(); err != nil {
		return err
	}

	if c.opts.Verbose {
		c.log("%q -> %q", e.src, e.dst)
	}

	if err := applyAttributes(e, c.opts); err != nil {
		c.warn(err)
	}

	c.prog.AddFile()
	return nil
}

func copyData(src io.Reader, dst *os.File, bufsize int, sparse bool) (int64, error) {
	buf := make([]byte, bufsize)
	var total int64
	var hole int64 // zero bytes not written yet

	for {
		n, err := src.Read(buf)
		if n > 0 {
			if sparse && isZero(buf[:n]) {
				// Remember the hole; it can only be turned into a seek once
				// more data follows it.
				hole += int64(n)
			} else {
				if hole > 0 {
					if _, seekErr := dst.Seek(hole, io.SeekCurrent); seekErr != nil {
						return total, seekErr
					}
					total += hole
					hole = 0
				}
				w, writeErr := dst.Write(buf[:n])
				if writeErr != nil {
					return total, writeErr
				}
				total += int64(w)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// A trailing run of zeros has to be written out, otherwise the
				// destination would end up shorter than the source.
				if hole > 0 {
					zeros := make([]byte, 64*1024)
					for rem := hole; rem > 0; {
						n := int64(len(zeros))
						if rem < n {
							n = rem
						}
						w, writeErr := dst.Write(zeros[:n])
						if writeErr != nil {
							return total, writeErr
						}
						total += int64(w)
						rem -= n
					}
				}
				return total, nil
			}
			return total, err
		}
	}
}

func isZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func applyAttributes(e entry, opts *Options) error {
	if opts.PreserveMode {
		if err := os.Chmod(e.dst, e.mode&0777); err != nil {
			return fmt.Errorf("chmod: %w", err)
		}
	}
	if opts.PreserveOwner {
		if err := os.Chown(e.dst, e.uid, e.gid); err != nil {
			return fmt.Errorf("chown: %w", err)
		}
	}
	if opts.PreserveTime {
		if err := syscall.UtimesNano(e.dst, []syscall.Timespec{
			{Sec: e.atime.Unix(), Nsec: int64(e.atime.Nanosecond())},
			{Sec: e.mtime.Unix(), Nsec: int64(e.mtime.Nanosecond())},
		}); err != nil {
			return fmt.Errorf("utimes: %w", err)
		}
	}
	return nil
}

func sameFile(a, b string) bool {
	sa, errA := statT(a)
	sb, errB := statT(b)
	if errA != nil || errB != nil {
		return false
	}
	return sa.Ino == sb.Ino && sa.Dev == sb.Dev
}
