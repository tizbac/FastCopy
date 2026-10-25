package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tiziano/fastcopy/internal/progress"
)

// buildSelector turns the file name arguments into a member selector.
func buildSelector(o *Options) (*selector, error) {
	entries, err := resolveEntries(o)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	sel := &selector{opts: o.memberOpts()}
	for _, e := range entries {
		n := e.arg
		if o.Unquote {
			n = unquote(o, n)
		}
		sel.args = append(sel.args, selArg{name: n, base: e.base})
	}
	return sel, nil
}

// memberFunc receives one selected member: its header, the stream of its
// data, the output name, the -C directory in effect and the 512-byte block
// ordinal its header starts in (used by --block-number).
type memberFunc func(r io.Reader, h *tar.Header, name, base, display string, block int64) error

// walkArchive iterates the archive, applying -K, --occurrence, selection,
// --strip-components and --transform, and hands each selected member to fn.
func walkArchive(o *Options, prog *progress.Progress, fn memberFunc) error {
	sel, err := buildSelector(o)
	if err != nil {
		return err
	}
	if sel == nil {
		sel = &selector{opts: o.memberOpts()}
	}
	// GNU tar treats -K as the head of the name list: nothing before the
	// starting member is considered, and from it on every name argument counts.
	if o.StartingFile != "" {
		sel.args = append([]selArg{{name: o.StartingFile, base: o.base}}, sel.args...)
	}
	tl, err := buildTransforms(o.Transform)
	if err != nil {
		return err
	}
	excl, err := exclusions(o)
	if err != nil {
		return err
	}

	in, closeIn, size, err := openRead(o)
	if err != nil {
		return err
	}
	defer closeIn()
	if size > 0 {
		prog.SetTotal(0, size)
	}
	cr := &countReader{r: in}
	r := tar.NewReader(cr)

	occ := map[string]int{}
	found := make([]bool, len(sel.args))
	started := o.StartingFile == ""
	var consumed int64

	recBytes := int64(recordSize(o))
	if recBytes < 512 {
		recBytes = 512
	}
	recBlocks := recBytes / 512
	// GNU tar's read checkpoints count the records it actually pulls from the
	// stream: while extracting every record is read, a non-seekable stream is
	// read through, but a seekable archive listing only reads the records that
	// hold headers plus the terminating record.
	countData := size <= 0 || o.Mode == modeExtract || o.Mode == modeDiff || o.Mode == modeDelete
	var records int64
	var lastRec int64 = -1
	fireRecords := func(n int64) {
		for ; records < n; records++ {
			if o.Checkpoint > 0 && (records+1)%int64(o.Checkpoint) == 0 {
				runCheckpoint(o, records+1, false)
			}
		}
	}

	// nextOff is where the next member's header chain starts in the stream.
	var nextOff int64
	for {
		cr.capAt = nextOff
		cr.cap = cr.cap[:0]
		cr.capOn = true
		h, err := r.Next()
		cr.capOn = false
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%s: %v", archiveName(o), err)
		}
		// archive/tar has just read the member's header block (plus any pax
		// or sparse extension blocks), so cr.n-512 is where that header
		// begins -- the block GNU tar's --block-number reports. GNU's own
		// long-name and long-link entries are reported at the block where
		// their chain starts instead.
		realHeader := cr.n - 512
		block := realHeader / 512
		if len(cr.cap) >= 512 && (cr.cap[156] == 'L' || cr.cap[156] == 'K') {
			block = cr.capAt / 512
		}
		nextOff = realHeader + 512 + (h.Size+511)/512*512
		if countData {
			fireRecords(cr.n / recBytes)
		} else {
			rec := block / recBlocks
			if rec != lastRec {
				fireRecords(records + 1)
			}
			lastRec = rec
		}
		// Roughly track archive consumption for the progress bar.
		consumed += 512 + (h.Size+511)/512*512
		prog.SetBytesDone(consumed)

		// GNU tar skips excluded members in every mode before name matching,
		// on the archive member name (--transform is applied afterwards).
		if !excl.empty() && excl.match(h.Name) {
			continue
		}

		idx := -1
		if !sel.empty() {
			idx = sel.match(h.Name)
			if idx < 0 {
				if o.ShowOmittedDirs && h.Typeflag == tar.TypeDir && o.Verbose > 0 {
					prog.Line("%s", h.Name)
				}
				continue
			}
			if !started {
				if idx != 0 {
					continue // -K: everything before the starting member is out of scope
				}
				started = true
			}
			found[idx] = true
		}

		occ[h.Name]++
		if o.Occurrence > 0 && occ[h.Name] != o.Occurrence {
			continue
		}

		// GNU tar keeps the stored member name for listing and for the verbose
		// extract and diff lines. The name the operation works on is
		// transformed, stripped and relocated (--one-top-level) in extraction
		// and diff, and in any mode when --show-transformed-names is given.
		renamed := o.Mode == modeExtract || o.Mode == modeDiff || o.ShowTransformed
		name := h.Name
		if renamed {
			// GNU transforms the name without the directory's trailing slash
			// and adds the slash back afterwards.
			trailing := ""
			candidate := h.Name
			if h.Typeflag == tar.TypeDir && strings.HasSuffix(candidate, "/") {
				trailing = "/"
				candidate = strings.TrimSuffix(candidate, "/")
			}
			name = tl.apply(candidate) + trailing
			if o.StripComponents > 0 {
				stripped, ok := stripComponents(name, o.StripComponents)
				if !ok {
					// GNU skips members whose name is entirely stripped.
					continue
				}
				name = stripped
			}
			if o.OneTopLevelSet {
				top := o.OneTopLevel
				if top == "" {
					top = defaultTopLevel(o)
				}
				name = strings.TrimSuffix(top, "/") + "/" + strings.TrimPrefix(name, "/")
			}
		}
		if name == "" {
			continue
		}
		display := h.Name
		if o.ShowTransformed {
			display = name
		}

		base := o.base
		if !sel.empty() && idx >= 0 {
			base = sel.args[idx].base
		}

		if err := fn(r, h, name, base, display, block); err != nil {
			return err
		}
		if countData {
			fireRecords(cr.n / recBytes)
		}
	}
	// archive/tar stops after the two all-zero terminating blocks; --block-number
	// reports the first of them like GNU tar does.
	zeroBlock := (cr.n - 1024) / 512
	// GNU tar's --totals reports every byte of the stream, including the
	// trailing zero blocks that archive/tar stops reading at.
	io.Copy(io.Discard, cr)
	totalsRead = cr.n
	if countData {
		fireRecords(cr.n / recBytes)
	} else if cr.n >= 1024 {
		rec := (cr.n - 1024) / recBytes
		if rec != lastRec {
			fireRecords(records + 1)
		}
	}
	if o.BlockNumber && cr.n >= 1024 {
		fmt.Printf("block %d: ** Block of NULs **\n", zeroBlock)
	}
	// GNU tar complains about every name argument that never matched a member.
	for i, a := range sel.args {
		if !found[i] {
			warnWildcards(o, a.name)
			errf("%s: Not found in archive", quoteArg(o, a.name))
		}
	}
	return nil
}

func archiveName(o *Options) string {
	if o.Archive == "-" {
		return "stdin"
	}
	return o.Archive
}

// runExtract implements -x.
func runExtract(o *Options, prog *progress.Progress) error {
	x := &extractor{o: o, prog: prog}
	err := walkArchive(o, prog, func(r io.Reader, h *tar.Header, name, base, display string, block int64) error {
		return x.extract(r, h, name, base, display, block)
	})
	x.finish()
	return err
}

type extractor struct {
	o      *Options
	prog   *progress.Progress
	errors int
	dirs   []deferredDir
}

type deferredDir struct {
	path string
	h    *tar.Header
}

// finish applies directory timestamps (and, with --delay-directory-restore,
// their permissions) after every member has been written, so that extracting
// into a directory does not disturb the time the archive recorded for it.
func (x *extractor) finish() {
	for i := len(x.dirs) - 1; i >= 0; i-- {
		d := x.dirs[i]
		if x.o.DelayDirRestore {
			mode := os.FileMode(d.h.Mode).Perm()
			if !x.o.SamePermissions {
				mode &= ^umask()
			}
			os.Chmod(d.path, mode)
			applyOwner(x.o, d.path, d.h)
		}
		if !x.o.Touch {
			os.Chtimes(d.path, d.h.ModTime, d.h.ModTime)
		}
	}
}

func umask() os.FileMode {
	m := syscall.Umask(0)
	syscall.Umask(m)
	return os.FileMode(m)
}

func applyOwner(o *Options, path string, h *tar.Header) {
	if !o.SameOwner {
		return
	}
	if err := os.Lchown(path, h.Uid, h.Gid); err != nil && o.Verbose > 0 {
		errf("cannot chown %s: %v", path, err)
	}
}

// extract writes one member to disk (or to stdout / a command).
func (x *extractor) extract(r io.Reader, h *tar.Header, name, base, display string, block int64) error {
	o := x.o
	prog := x.prog
	prog.Current(name)

	if h.Typeflag == tar.TypeXGlobalHeader || h.Typeflag == tar.TypeXHeader {
		return nil
	}

	if o.Verbose > 0 {
		name := quoteName(o, display)
		if o.BlockNumber {
			prog.Line("block %d: %s", block+1, name)
		} else {
			prog.Line("%s", name)
		}
	}

	if h.Typeflag == 'V' {
		if o.Label != "" && !wildmatch(o.Label, h.Name, matchOpts{wildcards: true, matchSlash: true, anchored: true}) {
			return fmt.Errorf("%s does not match volume label %q", h.Name, o.Label)
		}
		return nil
	}

	if o.ToStdout || o.ToCommand != "" {
		return x.toStream(r, h, name)
	}

	if !o.Touch {
		warnFutureTimestamp(quoteName(o, name), h.ModTime)
	}

	dst := outputPath(base, name)

	fi, err := os.Lstat(dst)
	exists := err == nil

	if exists {
		act, aerr := x.overwrite(fi, h, dst, name)
		if act == actError {
			// -k: GNU tar treats the existing target as an open failure on
			// the member and carries on with the rest of the archive.
			x.errors++
			errf("%s: Cannot open: %s", quoteName(o, name), errnoString(aerr))
			prog.AddFile()
			return nil
		}
		if aerr != nil {
			return aerr
		}
		if act == actSkip {
			prog.AddFile()
			return nil
		}
	}

	// GNU tar creates the directories a member needs, even when the
	// directory itself was not selected.
	if h.Typeflag != tar.TypeDir {
		if err := ensureParents(dst); err != nil {
			x.errors++
			errf("cannot create directory for %s: %v", dst, err)
			return nil
		}
	}

	var rerr error
	switch h.Typeflag {
	case tar.TypeDir:
		rerr = x.extractDir(h, dst, exists, fi)
	case tar.TypeSymlink:
		rerr = x.extractSymlink(h, dst, exists)
	case tar.TypeLink:
		rerr = x.extractHardLink(h, dst, name, base, exists)
	case tar.TypeFifo:
		if exists {
			os.Remove(dst)
		}
		if err := syscall.Mkfifo(dst, uint32(h.Mode&0777)); err != nil {
			rerr = x.fail("create fifo", dst, err)
		} else {
			applyOwner(o, dst, h)
		}
	case tar.TypeChar, tar.TypeBlock:
		if exists {
			os.Remove(dst)
		}
		mode := uint32(h.Mode & 0777)
		if h.Typeflag == tar.TypeChar {
			mode |= syscall.S_IFCHR
		} else {
			mode |= syscall.S_IFBLK
		}
		if err := syscall.Mknod(dst, mode, int(mkdev(h.Devmajor, h.Devminor))); err != nil {
			rerr = x.fail("create device", dst, err)
		} else {
			applyOwner(o, dst, h)
		}
	default:
		rerr = x.extractFile(r, h, dst, exists, fi)
	}
	if rerr == nil {
		prog.AddFile()
	}
	return rerr
}

func mkdev(maj, min int64) uint64 {
	return uint64(maj&0xfff)<<8 | uint64(min&0xff) |
		(uint64(maj)&^0xfff)<<32 | (uint64(min)&^0xff)<<12
}

// act is what the overwrite rules decided to do with an existing target.
type act int

const (
	actOverwrite act = iota
	actSkip
	actError
)

func (x *extractor) overwrite(fi os.FileInfo, h *tar.Header, dst, name string) (act, error) {
	o := x.o
	if o.Interactive {
		fmt.Fprintf(os.Stderr, "overwrite %s? (y/n) ", quoteArg(o, name))
		var a string
		fmt.Fscanln(os.Stdin, &a)
		if strings.HasPrefix(strings.ToLower(a), "y") {
			return actOverwrite, nil
		}
		return actSkip, nil
	}
	if o.SkipOldFiles {
		return actSkip, nil
	}
	if o.KeepOldFiles {
		// GNU tar reports this as an open failure on the member.
		return actError, errors.New("File exists")
	}
	if o.KeepNewerFiles && fi.ModTime().Unix() >= h.ModTime.Unix() {
		infof("Current '%s' is newer or same age", name)
		return actSkip, nil
	}
	if o.KeepDirSymlink && fi.Mode()&os.ModeSymlink != 0 {
		if t, err := os.Stat(dst); err == nil && t.IsDir() {
			return actSkip, nil
		}
	}
	return actOverwrite, nil
}

func (x *extractor) fail(verb, path string, err error) error {
	x.errors++
	errf("cannot %s %s: %v", verb, path, err)
	return nil
}

func (x *extractor) extractDir(h *tar.Header, dst string, exists bool, fi os.FileInfo) error {
	o := x.o
	if exists {
		if fi.Mode()&os.ModeSymlink != 0 {
			if o.Overwrite || o.RecursiveUnlink {
				if err := os.RemoveAll(dst); err != nil {
					return x.fail("remove", dst, err)
				}
				exists = false
			}
		} else if !fi.IsDir() {
			if o.RecursiveUnlink || o.UnlinkFirst {
				if err := os.Remove(dst); err != nil {
					return x.fail("remove", dst, err)
				}
				exists = false
			} else {
				return x.fail("overwrite directory", dst, fmt.Errorf("not a directory"))
			}
		} else if o.RecursiveUnlink {
			if err := os.RemoveAll(dst); err != nil {
				return x.fail("remove", dst, err)
			}
			exists = false
		}
	}

	if !exists {
		mode := os.FileMode(h.Mode).Perm()
		if !o.SamePermissions {
			mode &= ^umask()
		}
		if err := os.MkdirAll(dst, mode); err != nil {
			return x.fail("create directory", dst, err)
		}
		applyOwner(o, dst, h)
	} else if o.OverwriteDir {
		mode := os.FileMode(h.Mode).Perm()
		if !o.SamePermissions {
			mode &= ^umask()
		}
		os.Chmod(dst, mode)
		applyOwner(o, dst, h)
	}
	if !o.DelayDirRestore {
		x.dirs = append(x.dirs, deferredDir{path: dst, h: h})
	} else {
		x.dirs = append(x.dirs, deferredDir{path: dst, h: h})
	}
	return nil
}

func (x *extractor) extractSymlink(h *tar.Header, dst string, exists bool) error {
	o := x.o
	if exists {
		fi, _ := os.Lstat(dst)
		if fi != nil && fi.Mode()&os.ModeSymlink != 0 && o.KeepDirSymlink {
			if t, err := os.Stat(dst); err == nil && t.IsDir() {
				return nil
			}
		}
		if err := os.RemoveAll(dst); err != nil {
			return x.fail("remove", dst, err)
		}
	}
	if err := os.Symlink(h.Linkname, dst); err != nil {
		return x.fail("create symbolic link", dst, err)
	}
	applyOwner(o, dst, h)
	return nil
}

func (x *extractor) extractHardLink(h *tar.Header, dst, name, base string, exists bool) error {
	o := x.o
	candidates := []string{
		outputPath(base, h.Linkname),
		outputPath(base, filepath.Join(filepath.Dir(strings.TrimSuffix(name, "/")), h.Linkname)),
	}
	target := ""
	for _, c := range candidates {
		if _, err := os.Lstat(c); err == nil {
			target = c
			break
		}
	}
	if target == "" {
		x.errors++
		errf("%s: Cannot hard link to '%s': No such file or directory",
			quoteName(o, name), h.Linkname)
		return nil
	}
	if exists {
		os.RemoveAll(dst)
	}
	if err := os.Link(target, dst); err != nil {
		return x.fail("hard link", dst, err)
	}
	applyOwner(o, dst, h)
	return nil
}

func (x *extractor) extractFile(r io.Reader, h *tar.Header, dst string, exists bool, fi os.FileInfo) error {
	o := x.o
	prog := x.prog

	if exists {
		if fi.Mode()&os.ModeSymlink != 0 || fi.IsDir() {
			if err := os.RemoveAll(dst); err != nil {
				return x.fail("remove", dst, err)
			}
			exists = false
		} else if o.UnlinkFirst {
			if err := os.Remove(dst); err != nil {
				return x.fail("remove", dst, err)
			}
			exists = false
		}
	}
	_ = exists

	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode).Perm())
	if err != nil {
		return x.fail("create", dst, err)
	}
	sparse := h.Typeflag == 'S' || hasPaxSparse(h)
	if err := copyExtract(f, r, h.Size, sparse, prog); err != nil {
		f.Close()
		return x.fail("write", dst, err)
	}
	f.Close()

	mode := os.FileMode(h.Mode).Perm()
	if !o.SamePermissions {
		mode &= ^umask()
	}
	os.Chmod(dst, mode)
	applyOwner(o, dst, h)
	if !o.Touch {
		os.Chtimes(dst, h.ModTime, h.ModTime)
	}
	return nil
}

func hasPaxSparse(h *tar.Header) bool {
	for k := range h.PAXRecords {
		if strings.HasPrefix(k, "GNU.sparse.") {
			return true
		}
	}
	return false
}

// copyExtract writes member data. Sparse members get holes instead of the zero
// runs, which is what GNU tar produces too.
func copyExtract(dst *os.File, src io.Reader, size int64, sparse bool, prog *progress.Progress) error {
	buf := make([]byte, 1<<20)
	var dataOff, logical int64

	for {
		n, err := src.Read(buf)
		if n > 0 {
			if sparse && allZero(buf[:n]) {
				logical += int64(n)
				prog.AddBytes(int64(n))
			} else {
				if logical != dataOff {
					// flush the pending hole
					if _, serr := dst.Seek(logical, io.SeekStart); serr != nil {
						return serr
					}
					dataOff = logical
				}
				k, werr := dst.Write(buf[:n])
				if werr != nil {
					return werr
				}
				dataOff += int64(k)
				logical += int64(n - k)
				logical += int64(k)
				prog.AddBytes(int64(n))
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if dataOff < size {
		if err := dst.Truncate(size); err != nil {
			return err
		}
	}
	return nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// toStream implements -O and --to-command.
func (x *extractor) toStream(r io.Reader, h *tar.Header, name string) error {
	o := x.o
	prog := x.prog
	if o.ToStdout {
		buf := make([]byte, 1<<20)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				if _, werr := os.Stdout.Write(buf[:n]); werr != nil {
					return werr
				}
				prog.AddBytes(int64(n))
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}
		prog.AddFile()
		return nil
	}
	cmd := exec.Command("sh", "-c", o.ToCommand)
	cmd.Stdin = r
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"TAR_FILENAME="+name,
		fmt.Sprintf("TAR_MODE=%04o", h.Mode&07777),
		fmt.Sprintf("TAR_TYPE=%c", h.Typeflag),
		fmt.Sprintf("TAR_SIZE=%d", h.Size),
		fmt.Sprintf("TAR_UID=%d", h.Uid),
		fmt.Sprintf("TAR_GID=%d", h.Gid),
		"TAR_UNAME="+h.Uname,
		"TAR_GNAME="+h.Gname,
		"TAR_MTIME="+h.ModTime.Format(time.RFC3339),
	)
	if err := cmd.Run(); err != nil && !o.IgnoreCmdError {
		return x.fail("run "+o.ToCommand, "", err)
	}
	prog.AddFile()
	return nil
}

// runList implements -t.
func runList(o *Options, prog *progress.Progress) error {
	lw := newListWriter(o, prog)
	err := walkArchive(o, prog, func(_ io.Reader, h *tar.Header, name, base, display string, block int64) error {
		prog.Current(name)
		prog.AddFile()
		return lw.write(h, display, block)
	})
	if err != nil {
		return err
	}
	return lw.close()
}

// runDiff implements -d: compare the archive with the file system. GNU tar
// reports every difference it finds on stderr, in the order mode, uid, gid,
// modification time, size, contents.
func runDiff(o *Options, prog *progress.Progress) error {
	diffs := 0
	diff := func(name, msg string, args ...any) {
		// GNU tar writes the differences to stdout.
		prog.Line("%s: %s", quoteName(o, name), fmt.Sprintf(msg, args...))
		diffs++
	}
	err := walkArchive(o, prog, func(r io.Reader, h *tar.Header, name, base, display string, block int64) error {
		prog.Current(name)
		if o.Verbose > 0 {
			name := quoteName(o, display)
			if o.BlockNumber {
				prog.Line("block %d: %s", block+1, name)
			} else {
				prog.Line("%s", name)
			}
		}
		// GNU tar reports differences under the name without a directory's
		// trailing slash.
		dname := strings.TrimSuffix(name, "/")
		dst := outputPath(base, name)
		switch h.Typeflag {
		case tar.TypeSymlink:
			tgt, err := os.Readlink(dst)
			if err != nil {
				errf("%s: Cannot readlink: %s", quoteName(o, dname), errnoString(err))
				return nil
			}
			if tgt != h.Linkname {
				diff(dname, "Symlink differs")
			}
		case tar.TypeLink:
			a, err1 := os.Stat(dst)
			b, err2 := os.Stat(outputPath(base, h.Linkname))
			if err1 != nil || err2 != nil {
				warnf("%s: Warning: Cannot stat: %s", quoteName(o, dname), errnoString(firstErr(err1, err2)))
				return nil
			}
			if !sameInode(a, b) {
				diff(dname, "Not linked to %s", quoteArg(o, h.Linkname))
			}
		case tar.TypeDir:
			st, err := os.Stat(dst)
			if err != nil {
				warnf("%s: Warning: Cannot stat: %s", quoteName(o, dname), errnoString(err))
				return nil
			}
			if !st.IsDir() {
				diff(dname, "File type differs")
				return nil
			}
			if modeOf(st) != uint32(h.Mode)&07777 {
				diff(dname, "Mode differs")
			}
		default:
			st, err := os.Stat(dst)
			if err != nil {
				warnf("%s: Warning: Cannot stat: %s", quoteName(o, dname), errnoString(err))
				return nil
			}
			if !typeMatches(h.Typeflag, st) {
				diff(dname, "File type differs")
				return nil
			}
			if modeOf(st) != uint32(h.Mode)&07777 {
				diff(dname, "Mode differs")
			}
			if h.Typeflag == tar.TypeReg || h.Typeflag == 'S' || h.Typeflag == '7' {
				if s := syscallStatOf(st); s != nil {
					if int(s.uid) != h.Uid {
						diff(dname, "Uid differs")
					}
					if int(s.gid) != h.Gid {
						diff(dname, "Gid differs")
					}
				}
				if !timeEqual(st.ModTime(), h.ModTime) {
					diff(dname, "Mod time differs")
				}
				if st.Size() != h.Size {
					diff(dname, "Size differs")
				} else {
					same, err := streamEqualsFile(r, dst, h.Size)
					if err != nil {
						errf("%s: Cannot read: %s", quoteName(o, dname), errnoString(err))
					} else if !same {
						diff(dname, "Contents differ")
					}
				}
			}
		}
		prog.AddFile()
		return nil
	})
	if err != nil {
		return err
	}
	if diffs > 0 {
		return &differenceError{n: diffs}
	}
	return nil
}

// streamEqualsFile compares the archive member data with the file on disk.
func streamEqualsFile(r io.Reader, path string, size int64) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	bufA := make([]byte, 1<<20)
	bufB := make([]byte, 1<<20)
	for {
		na, ea := readFull(r, bufA)
		nb, eb := readFull(f, bufB)
		if na > 0 || nb > 0 {
			if na != nb || string(bufA[:na]) != string(bufB[:nb]) {
				return false, nil
			}
		}
		if ea == io.EOF && eb == io.EOF {
			return true, nil
		}
		if ea != nil && ea != io.EOF {
			return false, ea
		}
		if eb != nil && eb != io.EOF {
			return false, eb
		}
		if ea == io.EOF || eb == io.EOF {
			return false, nil // one side ended first
		}
	}
}

func readFull(r io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		k, err := r.Read(buf[n:])
		n += k
		if err != nil {
			if err == io.EOF {
				return n, io.EOF
			}
			return n, err
		}
		if k == 0 {
			break
		}
	}
	return n, nil
}

func sameFileStat(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	ia := syscallStatOf(sa)
	ib := syscallStatOf(sb)
	if ia == nil || ib == nil {
		return false, fmt.Errorf("no inode info")
	}
	return ia.dev == ib.dev && ia.ino == ib.ino, nil
}

// archiveLabel reads the first record of the archive and reports its volume
// label, and whether one is present at all.
func archiveLabel(o *Options) (string, bool, error) {
	in, closeIn, _, err := openRead(o)
	if err != nil {
		return "", false, err
	}
	defer closeIn()
	r := tar.NewReader(in)
	for {
		h, err := r.Next()
		if err == io.EOF {
			return "", false, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("%s: %v", archiveName(o), err)
		}
		if h.Typeflag == 'V' {
			return h.Name, true, nil
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		return "", false, nil
	}
}

// warnWildcards is GNU tar's reminder that a member argument looks like a glob
// even though member names are matched literally by default.
func warnWildcards(o *Options, name string) {
	if o.Wildcards || o.NoWildcards {
		return
	}
	if strings.ContainsAny(name, "*?[") {
		warnf("Pattern matching characters used in file names")
		warnf("Use --wildcards to enable pattern matching, or --no-wildcards to suppress this warning")
	}
}

// checkVolumeLabel is what -V LABEL means for every mode other than -c: the
// archive must carry a matching label, and GNU tar treats a mismatch as fatal.
func checkVolumeLabel(o *Options) error {
	label, found, err := archiveLabel(o)
	if err != nil {
		return err
	}
	warnWildcards(o, o.Label)
	if !found {
		errf("Archive not labeled to match '%s'", o.Label)
		errf("Error is not recoverable: exiting now")
		return &statusError{code: 2}
	}
	if !wildmatch(o.Label, label, matchOpts{wildcards: true, matchSlash: true, anchored: true}) {
		errf("Volume '%s' does not match '%s'", label, o.Label)
		errf("Error is not recoverable: exiting now")
		return &statusError{code: 2}
	}
	return nil
}

// runTestLabel implements --test-label.
func runTestLabel(o *Options, prog *progress.Progress) error {
	entries, err := resolveEntries(o)
	if err != nil {
		return err
	}
	pattern := ""
	if len(entries) > 0 {
		pattern = entries[0].arg
	}
	label, found, err := archiveLabel(o)
	if err != nil {
		return err
	}
	if pattern == "" {
		if found {
			prog.Line("%s", label)
		}
		return nil
	}
	warnWildcards(o, pattern)
	// GNU tar matches the label against the name arguments with the same
	// literal-by-default rule it uses for member names, and says nothing on a
	// mismatch: only the exit status changes.
	match := pattern == label
	if o.Wildcards {
		match = wildmatch(pattern, label, o.memberOpts())
	}
	if !found || !match {
		return &statusError{code: 1}
	}
	return nil
}
