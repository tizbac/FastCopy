package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tiziano/fastcopy/internal/progress"
)

// totalsWritten is the number of bytes the last writing mode put in the
// archive; --totals reports it.
var totalsWritten int64

// idCache caches user/group lookups so a large tree does not hammer nss.
type idCache struct {
	byID  map[string]string
	byNam map[string]int
}

var ids = &idCache{
	byID:  map[string]string{},
	byNam: map[string]int{},
}

func (c *idCache) name(id int) string {
	k := strconv.Itoa(id)
	if n, ok := c.byID[k]; ok {
		return n
	}
	n := ""
	if u, err := user.LookupId(k); err == nil {
		n = u.Username
	}
	c.byID[k] = n
	return n
}

func (c *idCache) group(id int) string {
	k := strconv.Itoa(id)
	if n, ok := c.byID["g"+k]; ok {
		return n
	}
	n := ""
	if g, err := user.LookupGroupId(k); err == nil {
		n = g.Name
	}
	c.byID["g"+k] = n
	return n
}

// parseID resolves a NAME or numeric id for --owner/--group.
// parseID resolves --owner/--group: GNU tar accepts a numeric id or a name.
// A name it cannot resolve leaves the member's own ownership untouched.
func parseID(s string) (int, string, error) {
	if s == "" {
		return -1, "", nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, "", nil
	}
	u, err := user.Lookup(s)
	if err != nil {
		return -1, "", nil
	}
	n, _ := strconv.Atoi(u.Uid)
	return n, u.Username, nil
}

func parseGID(s string) (int, string, error) {
	if s == "" {
		return -1, "", nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, "", nil
	}
	g, err := user.LookupGroup(s)
	if err != nil {
		return -1, "", nil
	}
	n, _ := strconv.Atoi(g.Gid)
	return n, g.Name, nil
}

var (
	uidOnce sync.Once
	gidOnce sync.Once
	ownUID  int
	ownGID  int
)

func ownIDs() (int, int) {
	uidOnce.Do(func() { ownUID = os.Getuid() })
	gidOnce.Do(func() { ownGID = os.Getgid() })
	return ownUID, ownGID
}

// headerFor converts a collected member into the header fasttar writes.
func headerFor(o *Options, tl transformList, m *member, mtime time.Time, mtimeOK bool) (*tar.Header, error) {
	st := m.st
	// GNU tar transforms the member name without its trailing slash and adds
	// the slash back for directories afterwards.
	name := strings.TrimSuffix(m.name, "/")
	name = tl.apply(name)

	h := &tar.Header{
		Name:    name,
		ModTime: st.ModTime(),
		Format:  goFormat(o),
	}
	if !st.IsDir() {
		h.Name = strings.TrimSuffix(h.Name, "/")
	}

	// permissions
	mode := st.Mode().Perm()
	if o.ModeChange != "" {
		var err error
		mode, err = applySymbolicMode(mode, o.ModeChange, st.IsDir())
		if err != nil {
			return nil, err
		}
	}
	h.Mode = int64(mode)

	// type
	switch {
	case st.IsDir():
		h.Typeflag = tar.TypeDir
		if !strings.HasSuffix(h.Name, "/") {
			h.Name += "/"
		}
		h.Size = 0
	case st.Mode()&os.ModeSymlink != 0 && !o.Dereference:
		h.Typeflag = tar.TypeSymlink
		h.Linkname = m.linkname
		h.Size = 0
	case m.hard:
		h.Typeflag = tar.TypeLink
		h.Linkname = tl.apply(m.linkname)
		h.Size = 0
	case st.Mode().IsRegular() || (st.Mode()&os.ModeSymlink != 0 && o.Dereference):
		h.Typeflag = tar.TypeReg
		h.Size = st.Size()
	case st.Mode()&os.ModeNamedPipe != 0:
		h.Typeflag = tar.TypeFifo
		h.Size = 0
	case st.Mode()&os.ModeDevice != 0:
		if st.Mode()&os.ModeCharDevice != 0 {
			h.Typeflag = tar.TypeChar
		} else {
			h.Typeflag = tar.TypeBlock
		}
		h.Size = 0
		if s := syscallStatOf(st); s != nil {
			h.Devmajor = int64(major(s.rdev))
			h.Devminor = int64(minor(s.rdev))
		}
	default:
		h.Typeflag = tar.TypeReg
		h.Size = st.Size()
	}

	// ownership
	uid, gid := ownIDs()
	if s := syscallStatOf(st); s != nil {
		uid, gid = int(s.uid), int(s.gid)
	}
	if o.Owner != "" {
		n, name, err := parseID(o.Owner)
		if err != nil {
			return nil, err
		}
		if n >= 0 {
			uid = n
			h.Uname = name
		}
	} else if !o.NumericOwner {
		h.Uname = ids.name(uid)
	}
	if o.Group != "" {
		n, name, err := parseGID(o.Group)
		if err != nil {
			return nil, err
		}
		if n >= 0 {
			gid = n
			h.Gname = name
		}
	} else if !o.NumericOwner {
		h.Gname = ids.group(gid)
	}
	h.Uid, h.Gid = uid, gid

	// timestamps
	if mtimeOK {
		if o.ClampMtime {
			if st.ModTime().After(mtime) {
				h.ModTime = mtime
			}
		} else {
			h.ModTime = mtime
		}
	}
	return h, nil
}

// countWriter tracks how many bytes reach the archive (for --totals).
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// bufferSize is the read/write buffer size (fasttar --buffer).
func bufferSize(o *Options) int {
	if o.Buffer > 0 {
		return o.Buffer
	}
	return 1 << 20
}

// runCreate implements -c (and, with a different entry point, -r/-u).
func runCreate(o *Options, prog *progress.Progress) error {
	entries, err := resolveEntries(o)
	if err != nil {
		return err
	}
	tl, err := buildTransforms(o.Transform)
	if err != nil {
		return err
	}
	b := newBuilder(o, prog)
	members, err := b.collect(entries)
	if err != nil {
		return err
	}
	if o.CheckLinks {
		reportMissingLinks(o, prog, members)
	}
	return writeArchive(o, prog, members, tl, b, 0)
}

// writeArchive writes members to the output archive. seekTo < 0 means the
// output is a fresh stream; otherwise the archive is truncated there first
// (append and update).
func writeArchive(o *Options, prog *progress.Progress, members []member, tl transformList, b *builder, seekTo int64) error {
	prog.SetTotal(len(members), b.bytes)

	f, err := openWrite(o)
	if err != nil {
		return err
	}
	own := o.Archive != "-"
	if own {
		defer f.Close()
	}
	if seekTo >= 0 {
		if err := f.Truncate(seekTo); err != nil {
			return fmt.Errorf("cannot rewrite %s: %w", o.Archive, err)
		}
		if _, err := f.Seek(seekTo, io.SeekStart); err != nil {
			return err
		}
	}

	bw := bufio.NewWriterSize(f, bufferSize(o))
	// cw counts everything tar.Writer emits, before bufio's buffering, so
	// write checkpoints can fire at the record boundaries GNU tar uses.
	cw := &countWriter{w: bw}
	tw := tar.NewWriter(cw)
	buf := make([]byte, bufferSize(o))
	var wroteDirs []string

	// -V LABEL puts a volume label in the first record of a new archive.
	if o.Label != "" && seekTo == 0 && o.Mode == modeCreate {
		h := &tar.Header{
			Name:     o.Label,
			Typeflag: 'V',
			Format:   goFormat(o),
			ModTime:  now(),
		}
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("cannot write volume label: %w", err)
		}
		if o.Verbose > 0 {
			prog.Line("%s", quoteName(o, o.Label))
		}
		prog.AddFile()
	}
	var nextCP int64
	if o.Checkpoint > 0 {
		nextCP = int64(o.Checkpoint)
	}

	for i := range members {
		m := &members[i]
		name := tl.apply(m.name)
		prog.Current(name)

		h, err := headerFor(o, tl, m, b.mtime.t, b.mtimeOK)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(h); err != nil {
			return fmt.Errorf("cannot write %s: %w", name, err)
		}
		if h.Size > 0 {
			if err := copyFileTo(tw, m.path, buf, prog); err != nil {
				return err
			}
		}
		prog.AddFile()
		if o.Verbose > 0 {
			prog.Line("%s", quoteName(o, name))
		}
		if o.RemoveFiles {
			if m.isDir() {
				wroteDirs = append(wroteDirs, m.path)
			} else if err := os.Remove(m.path); err != nil && o.Verbose > 0 {
				prog.Line("fasttar: cannot remove %s: %v", m.path, err)
			}
		}
		// Checkpoints count archive records, as GNU tar does.
		if o.Checkpoint > 0 {
			rec := int64(recordSize(o))
			for rec > 0 && cw.n/rec >= nextCP {
				runCheckpoint(o, nextCP, true)
				nextCP += int64(o.Checkpoint)
			}
		}
	}

	if err := tw.Close(); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	// GNU tar pads the archive to a whole number of records.
	var padded int64
	if pad := cw.n % int64(recordSize(o)); pad != 0 {
		padded = int64(recordSize(o)) - pad
		if _, err := f.Write(make([]byte, padded)); err != nil {
			return err
		}
	}
	totalsWritten = cw.n + padded
	// The padding record is the last one GNU tar's checkpoints count.
	if o.Checkpoint > 0 {
		rec := int64(recordSize(o))
		for rec > 0 && totalsWritten/rec >= nextCP {
			runCheckpoint(o, nextCP, true)
			nextCP += int64(o.Checkpoint)
		}
	}
	if own {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	if o.RemoveFiles {
		for i := len(wroteDirs) - 1; i >= 0; i-- {
			os.Remove(wroteDirs[i]) // only succeeds when empty
		}
	}
	if o.Verify {
		if err := verifyArchive(o, prog, members); err != nil {
			return err
		}
	}
	return nil
}

// copyFileTo streams one member's data into the archive.
func copyFileTo(tw *tar.Writer, path string, buf []byte, prog *progress.Progress) error {
	src, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer src.Close()
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := tw.Write(buf[:n]); werr != nil {
				return werr
			}
			prog.AddBytes(int64(n))
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return fmt.Errorf("cannot read %s: %w", path, rerr)
		}
	}
}

// reportMissingLinks implements -l for create mode: complain when a file's
// hard links were not all archived.
func reportMissingLinks(o *Options, prog *progress.Progress, members []member) {
	counts := map[string]int{}
	links := map[string]string{}
	for i := range members {
		m := &members[i]
		if m.hard {
			counts[m.linkname]++
			links[m.linkname] = m.name
			continue
		}
		if m.st == nil || !m.st.Mode().IsRegular() {
			continue
		}
		if st := syscallStatOf(m.st); st == nil || st.nl <= 1 {
			continue
		}
		counts[m.name]++
	}
	for name, n := range counts {
		for i := range members {
			m := &members[i]
			if m.name != name || m.st == nil {
				continue
			}
			if st := syscallStatOf(m.st); st != nil && int(st.nl) > n {
				prog.Line("fasttar: %s: link not dumped (%d of %d)", name, n, int(st.nl))
			}
		}
	}
}

// runAppend implements -r: append to the end of an existing archive.
func runAppend(o *Options, prog *progress.Progress) error {
	end, err := archiveEndOf(o)
	if err != nil {
		return err
	}
	entries, err := resolveEntries(o)
	if err != nil {
		return err
	}
	tl, err := buildTransforms(o.Transform)
	if err != nil {
		return err
	}
	b := newBuilder(o, prog)
	members, err := b.collect(entries)
	if err != nil {
		return err
	}
	return writeArchive(o, prog, members, tl, b, end)
}

// runUpdate implements -u: append only files newer than the archived copy.
func runUpdate(o *Options, prog *progress.Progress) error {
	old, err := readMemberTimes(o)
	if err != nil {
		return err
	}
	end, err := archiveEndOf(o)
	if err != nil {
		return err
	}
	entries, err := resolveEntries(o)
	if err != nil {
		return err
	}
	tl, err := buildTransforms(o.Transform)
	if err != nil {
		return err
	}
	b := newBuilder(o, prog)
	all, err := b.collect(entries)
	if err != nil {
		return err
	}
	var members []member
	for i := range all {
		m := all[i]
		if m.isDir() {
			// GNU tar never re-adds directory members in update mode.
			continue
		}
		prev, ok := old[tl.apply(m.name)]
		if ok && m.st.ModTime().Unix() <= prev.Unix() {
			continue // archive copy is at least as new (tar stores whole seconds)
		}
		members = append(members, m)
	}
	if len(members) == 0 {
		prog.SetTotal(0, 0)
		return nil
	}
	return writeArchive(o, prog, members, tl, b, end)
}

// runCatenate implements -A: append whole archives to the target.
func runCatenate(o *Options, prog *progress.Progress) error {
	if o.Archive == "-" {
		return fmt.Errorf("-A requires a seekable archive file")
	}
	end, err := archiveEndOf(o)
	if err != nil {
		return err
	}
	entries, err := resolveEntries(o)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("-A requires at least one archive to append")
	}
	f, err := os.OpenFile(o.Archive, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(end); err != nil {
		return err
	}
	if _, err := f.Seek(end, io.SeekStart); err != nil {
		return err
	}
	var total int64
	for _, e := range entries {
		n, err := catenateOne(o, prog, f, e.arg, e.base)
		if err != nil {
			return err
		}
		total += n
	}
	// re-add the end-of-archive marker and padding
	rec := int64(recordSize(o))
	if pad := total % rec; pad != 0 {
		if _, err := f.Write(make([]byte, rec-pad)); err != nil {
			return err
		}
	} else if total%1024 == 0 {
		// catenated archives keep their own end-of-archive blocks; make sure
		// the combined file ends with one.
		if _, err := f.Write(make([]byte, 1024)); err != nil {
			return err
		}
	}
	return nil
}

// catenateOne copies the raw member blocks of one archive onto dst.
func catenateOne(o *Options, prog *progress.Progress, dst io.Writer, name, base string) (int64, error) {
	path := name
	if !strings.HasPrefix(path, "/") && base != "" {
		path = joinBase(base, path)
	}
	src, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("cannot open %s: %w", path, err)
	}
	defer src.Close()
	end, err := archiveEnd(src, -1)
	if err != nil {
		return 0, err
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.Copy(dst, io.LimitReader(src, end))
	if err != nil {
		return 0, err
	}
	prog.SetTotal(0, n)
	prog.AddBytes(n)
	prog.AddFile()
	if o.Verbose > 0 {
		prog.Line("%s", path)
	}
	return n, nil
}

// runDelete implements --delete: rewrite the archive without the selected
// members.
func runDelete(o *Options, prog *progress.Progress) error {
	sel, err := buildSelector(o)
	if err != nil {
		return err
	}
	tl, err := buildTransforms(o.Transform)
	if err != nil {
		return err
	}
	seen := map[string]int{}
	found := make([]bool, len(sel.args))
	err = rewriteArchive(o, func(name string) (bool, error) {
		if !sel.empty() {
			idx := sel.match(name)
			if idx < 0 {
				return false, nil
			}
			found[idx] = true
		}
		if o.Occurrence > 0 {
			seen[name]++
			return seen[name] == o.Occurrence, nil
		}
		return true, nil
	})
	if err != nil {
		return err
	}
	// GNU tar names every member it was asked to remove that the archive does
	// not contain, and exits 2.
	for i, a := range sel.args {
		if !found[i] {
			warnWildcards(o, a.name)
			errf("%s: Not found in archive", quoteArg(o, a.name))
		}
	}
	_ = tl
	return nil
}

// archiveEndOf locates the end of the current output archive.
func archiveEndOf(o *Options) (int64, error) {
	if o.Archive == "-" {
		return 0, fmt.Errorf("archive must be seekable for this operation: use -f FILE")
	}
	f, err := os.Open(o.Archive)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // append to a new archive starts at zero
		}
		return 0, err
	}
	defer f.Close()
	return archiveEnd(f, -1)
}

// readMemberTimes maps member name -> archived mtime, for -u.
func readMemberTimes(o *Options) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	if _, err := os.Stat(o.Archive); err != nil {
		return out, nil
	}
	f, err := os.Open(o.Archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := tar.NewReader(f)
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("cannot read %s: %w", o.Archive, err)
		}
		out[h.Name] = h.ModTime
	}
	return out, nil
}

// verifyArchive re-reads what we just wrote and compares it with the sources
// (-W/--verify).
func verifyArchive(o *Options, prog *progress.Progress, members []member) error {
	byName := map[string]string{}
	for i := range members {
		byName[members[i].name] = members[i].path
		byName[strings.TrimSuffix(members[i].name, "/")] = members[i].path
	}
	f, err := os.Open(o.Archive)
	if err != nil {
		return err
	}
	defer f.Close()
	r := tar.NewReader(f)
	buf := make([]byte, 1<<20)
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("verify: cannot read %s: %w", o.Archive, err)
		}
		src, ok := byName[h.Name]
		if !ok || h.Size == 0 {
			continue
		}
		sf, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("verify: %s: %w", h.Name, err)
		}
		st, _ := sf.Stat()
		if st.Size() != h.Size {
			sf.Close()
			return fmt.Errorf("verify: %s: size changed while archiving", h.Name)
		}
		for {
			n1, e1 := r.Read(buf)
			if n1 > 0 {
				got := make([]byte, n1)
				if _, e2 := io.ReadFull(sf, got); e2 != nil {
					sf.Close()
					return fmt.Errorf("verify: %s: %v", h.Name, e2)
				}
				if !bytes.Equal(got, buf[:n1]) {
					sf.Close()
					return fmt.Errorf("verify: %s: contents differ", h.Name)
				}
				prog.AddBytes(int64(n1))
			}
			if e1 == io.EOF {
				break
			}
			if e1 != nil {
				sf.Close()
				return e1
			}
		}
		sf.Close()
		if o.Verbose > 0 {
			prog.Line("verify %s: ok", h.Name)
		}
	}
	return nil
}
