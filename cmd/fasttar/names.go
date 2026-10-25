package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tiziano/fastcopy/internal/fiemap"
	"github.com/tiziano/fastcopy/internal/progress"
)

// entry is one resolved file-name argument of the command line (or of -T).
type entry struct {
	arg  string
	base string // -C directory in effect where the name appeared
	kind cmdKind
}

// member is one archive member while it is being prepared.
type member struct {
	path     string // source path on disk (create side)
	name     string // name stored in the archive
	st       os.FileInfo
	linkname string // symlink target, or archive name of a hard link target
	hard     bool   // this member is a hard link to an earlier one
	order    uint64 // physical offset for --fast (0 when unknown)
}

func (m *member) isDir() bool { return m.st != nil && m.st.IsDir() }

// resolveEntries expands the ordered command list into plain file name
// arguments: -T files are read where they appear, --add-file is taken
// verbatim. Entries added while reading a -T file (a nested -T or a per-line
// -C) are picked up in order, like GNU tar does.
func resolveEntries(o *Options) ([]entry, error) {
	var out []entry
	for i := 0; i < len(o.Cmds); i++ {
		c := o.Cmds[i]
		switch c.kind {
		case cmdName, cmdAddFile:
			out = append(out, entry{arg: c.value, base: c.base, kind: c.kind})
		case cmdFilesFrom:
			names, err := readNamesFrom(o, c.value, c.base)
			if err != nil {
				return nil, err
			}
			out = append(out, names...)
		}
	}
	return out, nil
}

// fileSelectionLong are the options GNU tar accepts on a -T line: only the
// local file name selection and matching options are meaningful there.
func fileSelectionLong(name string) bool {
	switch name {
	case "add-file", "directory", "files-from", "null", "no-null", "unquote",
		"no-unquote", "verbatim-files-from", "no-verbatim-files-from",
		"exclude", "exclude-from", "exclude-caches", "exclude-caches-under",
		"exclude-caches-all", "exclude-tag", "exclude-ignore",
		"exclude-ignore-recursive", "exclude-tag-under", "exclude-tag-all",
		"exclude-vcs", "exclude-vcs-ignores", "exclude-backups",
		"recursion", "no-recursion", "anchored", "no-anchored", "ignore-case",
		"no-ignore-case", "wildcards", "no-wildcards", "wildcards-match-slash",
		"no-wildcards-match-slash":
		return true
	}
	return false
}

// readNamesFrom implements -T: names split on newlines or NULs, with GNU's
// --unquote handling and the "lines that look like options are options" rule.
// base is the -C directory in effect when the file was named; a -C line
// inside the file changes it for the names that follow.
func readNamesFrom(o *Options, file string, base string) ([]entry, error) {
	var data []byte
	var err error
	if file == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		if st, serr := os.Stat(file); serr == nil && st.IsDir() {
			// GNU tar opens a directory, reads nothing from it and carries on.
			return nil, nil
		}
		data, err = os.ReadFile(file)
	}
	if err != nil {
		return nil, &fatalError{msg: fmt.Sprintf("%s: Cannot stat: %s", file, errnoString(err))}
	}
	sep := "\n"
	if o.Null {
		sep = "\x00"
	}
	var out []entry
	lineNo := 0
	for _, line := range strings.Split(string(data), sep) {
		lineNo++
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if o.VerbatimFilesFrom {
			out = append(out, entry{arg: unquote(o, line), base: base, kind: cmdFilesFrom})
			continue
		}
		opt := strings.TrimLeft(line, " \t")
		if opt == "" || !strings.HasPrefix(opt, "-") {
			out = append(out, entry{arg: chopSlash(unquote(o, line)), base: base, kind: cmdFilesFrom})
			continue
		}
		tokens := strings.Fields(opt)
		first := tokens[0]
		known := false
		if strings.HasPrefix(first, "--") {
			name := strings.TrimPrefix(first, "--")
			if i := strings.IndexByte(name, '='); i >= 0 {
				name = name[:i]
			}
			known = fileSelectionLong(name)
		} else if len(first) >= 2 {
			known = strings.IndexByte("CTX", first[1]) >= 0
		}
		if !known {
			errf("%s:%d: unrecognized option", file, lineNo)
			continue
		}
		start := len(o.Cmds)
		sub := &parser{o: o, av: tokens, ind: 0}
		for sub.ind < len(sub.av) {
			arg := sub.av[sub.ind]
			sub.ind++
			var e error
			if strings.HasPrefix(arg, "--") {
				e = sub.long(arg)
			} else {
				e = sub.shorts(arg)
			}
			if e != nil {
				errf("%s:%d: unrecognized option", file, lineNo)
				break
			}
		}
		for _, c := range o.Cmds[start:] {
			if c.kind == cmdDir {
				base = c.value
			}
		}
	}
	return out, nil
}

// chopSlash drops the trailing slash GNU tar strips from -T names.
func chopSlash(s string) string {
	for len(s) > 1 && strings.HasSuffix(s, "/") {
		s = strings.TrimSuffix(s, "/")
	}
	return s
}

// unquote expands the C-style escapes GNU tar honours in -T files.
func unquote(o *Options, s string) string {
	if !o.Unquote || !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '\\':
			b.WriteByte('\\')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			j := i
			for j < len(s) && j < i+3 && s[j] >= '0' && s[j] <= '7' {
				j++
			}
			v, _ := strconv.ParseUint(s[i:j], 8, 8)
			b.WriteByte(byte(v))
			i = j - 1
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// builder turns file name arguments into the member list of a new archive.
type builder struct {
	o       *Options
	prog    *progress.Progress
	excl    *exclSet
	seen    map[string]string // dev:ino -> first archive name
	newer   timeVal
	newerOK bool
	mtime   timeVal
	mtimeOK bool
	readErr error
	files   int
	bytes   int64
}

// timeVal keeps a parsed timestamp together with its resolution.
type timeVal struct {
	t   time.Time
	sub bool // --newer-mtime: compare data change time only (we use mtime)
}

// exclusions builds the exclusion list. It is shared by creation and by the
// reading modes, where GNU tar also honours --exclude.
func exclusions(o *Options) (*exclSet, error) {
	e := &exclSet{opts: o.excludeOpts()}
	e.pats = append(e.pats, o.Excludes...)
	if o.ExcludeBackups {
		e.pats = append(e.pats, backupPats...)
	}
	if o.ExcludeVCS {
		e.pats = append(e.pats, vcsDirs...)
	}
	for _, f := range o.ExcludeFrom {
		pats, err := readPatternFile(o, f)
		if err != nil {
			return nil, err
		}
		e.pats = append(e.pats, pats...)
	}
	return e, nil
}

func newBuilder(o *Options, prog *progress.Progress) *builder {
	b := &builder{
		o:    o,
		prog: prog,
		seen: make(map[string]string),
	}
	excl, err := exclusions(o)
	if err != nil {
		b.readErr = err
		b.excl = &exclSet{opts: o.excludeOpts()}
		return b
	}
	b.excl = excl
	// The threshold date and --mtime were already parsed while the options
	// were read (see dateOption), so the builder only applies them here.
	if o.ThresholdDate && o.ThresholdOK {
		b.newer, b.newerOK = timeVal{t: o.ThresholdTime, sub: o.ThresholdSub}, true
	}
	if o.MtimeOK {
		b.mtime, b.mtimeOK = timeVal{t: o.MtimeTime}, true
	}
	return b
}

func readPatternFile(o *Options, f string) ([]string, error) {
	var data []byte
	var err error
	if f == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(f)
	}
	if err != nil {
		return nil, &fatalError{msg: fmt.Sprintf("%s: %s", f, errnoString(err))}
	}
	sep := "\n"
	if o.Null {
		sep = "\x00"
	}
	var out []string
	for _, p := range strings.Split(string(data), sep) {
		if p != "" {
			out = append(out, unquote(o, p))
		}
	}
	return out, nil
}

// collect builds the member list for create/append/update modes.
func (b *builder) collect(entries []entry) ([]member, error) {
	if b.readErr != nil {
		return nil, b.readErr
	}
	var out []member
	for _, e := range entries {
		if err := b.add(e, &out); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 && !b.o.IgnoreFailRead {
		return nil, &usageError{msg: "Cowardly refusing to create an empty archive"}
	}

	switch b.o.Sort {
	case "name":
		sort.SliceStable(out, func(i, j int) bool { return out[i].name < out[j].name })
	case "inode":
		sort.SliceStable(out, func(i, j int) bool {
			si, sj := syscallStatOf(out[i].st), syscallStatOf(out[j].st)
			if si == nil || sj == nil {
				return false
			}
			if si.ino != sj.ino {
				return si.ino < sj.ino
			}
			return si.dev < sj.dev
		})
	}
	if b.o.Fast {
		b.order(out)
	}
	b.files = len(out)
	for i := range out {
		if out[i].st != nil && out[i].st.Mode().IsRegular() {
			b.bytes += out[i].st.Size()
		}
	}
	return out, nil
}

// order sorts regular files by their physical extent so the disk head mostly
// moves forward, exactly like fastcopy --fast. Members without an answer
// (directories, symlinks, tmpfs files) keep their relative position first.
func (b *builder) order(members []member) {
	for i := range members {
		m := &members[i]
		if m.hard || m.st == nil || !m.st.Mode().IsRegular() {
			continue
		}
		off, ok := fiemap.FirstPhysicalOffset(m.path)
		if !ok {
			continue
		}
		m.order = off
		if b.o.Debug {
			b.prog.Line("extent %s: physical=%d size=%d", m.name, off, m.st.Size())
		}
	}
	sort.SliceStable(members, func(i, j int) bool { return members[i].order < members[j].order })
	// A hard link has to follow the file it points at, otherwise even GNU tar
	// cannot extract the archive.
	pos := make(map[string]int, len(members))
	for i := range members {
		pos[members[i].name] = i
	}
	for i := 0; i < len(members); i++ {
		if !members[i].hard {
			continue
		}
		j, ok := pos[members[i].linkname]
		if !ok || j < i {
			continue
		}
		m := members[i]
		copy(members[i:j], members[i+1:j+1])
		members[j] = m
		for k := range members {
			pos[members[k].name] = k
		}
	}
}

// add archives one file-name argument, descending into directories.
func (b *builder) add(e entry, out *[]member) error {
	o := b.o
	path := e.arg
	if !strings.HasPrefix(path, "/") && e.base != "" {
		path = joinBase(e.base, path)
	}
	name := e.arg
	if !o.AbsoluteNames {
		name = strings.TrimPrefix(name, "/")
	}

	st, err := b.stat(path)
	if err != nil {
		b.readFailure(e.arg, "stat", err)
		return nil
	}
	return b.visit(path, name, st, out)
}

func (b *builder) stat(path string) (os.FileInfo, error) {
	if b.o.Dereference {
		return os.Stat(path)
	}
	return os.Lstat(path)
}

// readFailure reports a name that could not be read. GNU tar keeps going with
// the remaining names; --ignore-failed-read demotes the failure to a warning.
func (b *builder) readFailure(name, verb string, err error) {
	msg := errnoString(err)
	if b.o.IgnoreFailRead {
		warnf("%s: Warning: Cannot %s: %s", quoteArg(b.o, name), verb, msg)
		return
	}
	errf("%s: Cannot %s: %s", quoteArg(b.o, name), verb, msg)
}

// excluded applies the global exclusion patterns plus the per-directory ones.
func (b *builder) excluded(name string) bool {
	return b.excl.match(name)
}

// visit adds a single node and, for directories, its children.
func (b *builder) visit(path, name string, st os.FileInfo, out *[]member) error {
	o := b.o
	if !st.IsDir() {
		return b.visitMember(path, name, st, out)
	}

	base := strings.TrimSuffix(name, "/")
	if b.excluded(base) {
		return nil
	}
	mode, tagFile, ok := b.tagAction(path)
	if ok && mode == tagAll {
		return nil
	}
	if err := b.visitMember(path, base+"/", st, out); err != nil {
		return err
	}
	if !o.Recursion {
		return nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		b.readFailure(name, "open", err)
		return nil
	}

	saved := len(b.excl.pats)
	b.pushIgnore(path)
	defer func() { b.excl.pats = b.excl.pats[:saved] }()

	skipContents := ok && (mode == tagCaches || mode == tagUnder)
	for _, de := range entries {
		// --exclude-tag/--exclude-caches keep the tag file itself, while
		// --exclude-tag-under/--exclude-caches-under drop every entry.
		if skipContents && !(mode == tagCaches && de.Name() == tagFile) {
			continue
		}
		child := filepath.Join(path, de.Name())
		cname := base + "/" + de.Name()
		cst, err := b.stat(child)
		if err != nil {
			b.readFailure(cname, "stat", err)
			continue
		}
		if cst.IsDir() && o.OneFileSystem && devOf(st) != devOf(cst) {
			continue
		}
		if err := b.visit(child, cname, cst, out); err != nil {
			return err
		}
	}
	return nil
}

// tagMode is what --exclude-caches* / --exclude-tag* do to a directory.
type tagMode int

const (
	tagNone tagMode = iota
	tagCaches
	tagAll
	tagUnder
)

func (b *builder) tagAction(dir string) (tagMode, string, bool) {
	type rule struct {
		name  string
		mode  tagMode
		cache bool // CACHEDIR.TAG only counts with its standard signature
	}
	var rules []rule
	if b.o.ExcludeCachesAll {
		rules = append(rules, rule{"CACHEDIR.TAG", tagAll, true})
	}
	if b.o.ExcludeCachesUnder {
		rules = append(rules, rule{"CACHEDIR.TAG", tagUnder, true})
	}
	if b.o.ExcludeCaches {
		rules = append(rules, rule{"CACHEDIR.TAG", tagCaches, true})
	}
	for _, n := range b.o.ExcludeTagAll {
		rules = append(rules, rule{n, tagAll, false})
	}
	for _, n := range b.o.ExcludeTagUnder {
		rules = append(rules, rule{n, tagUnder, false})
	}
	for _, n := range b.o.ExcludeTag {
		rules = append(rules, rule{n, tagCaches, false})
	}
	for _, r := range rules {
		if r.name == "" {
			continue
		}
		if r.cache {
			if cacheTagPresent(dir) {
				return r.mode, r.name, true
			}
			continue
		}
		if fileExists(filepath.Join(dir, r.name)) {
			return r.mode, r.name, true
		}
	}
	return tagNone, "", false
}

// cacheTagSignature marks a directory as a cache, per the cache directory tag
// specification GNU tar implements.
const cacheTagSignature = "Signature: 8a477f597d28d172789f06886806bc55"

// cacheTagPresent reports whether dir contains a CACHEDIR.TAG with the
// standard signature; GNU ignores tag files without it.
func cacheTagPresent(dir string) bool {
	f, err := os.Open(filepath.Join(dir, "CACHEDIR.TAG"))
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, len(cacheTagSignature))
	if _, err := io.ReadFull(f, head); err != nil {
		return false
	}
	return string(head) == cacheTagSignature
}

// pushIgnore loads the --exclude-ignore patterns of one directory; the caller
// restores the previous length when it returns.
func (b *builder) pushIgnore(dir string) {
	if b.o.ExcludeVCSIgnores {
		for _, f := range []string{".cvsignore", ".gitignore", ".bzrignore", ".hgignore"} {
			if pats, err := readVCSIgnore(filepath.Join(dir, f)); err == nil {
				b.excl.pats = append(b.excl.pats, pats...)
			}
		}
	}
	for _, f := range b.o.ExcludeIgnore {
		if pats, err := readPatternFile(b.o, filepath.Join(dir, f)); err == nil {
			b.excl.pats = append(b.excl.pats, pats...)
		}
	}
	for _, f := range b.o.ExcludeIgnoreRecursi {
		if pats, err := readPatternFile(b.o, filepath.Join(dir, f)); err == nil {
			b.excl.pats = append(b.excl.pats, pats...)
		}
	}
}

// readVCSIgnore reads the patterns of one VCS ignore file the way GNU tar
// does for --exclude-vcs-ignores: comments and negations are dropped, and
// .cvsignore lists several space separated patterns per line.
func readVCSIgnore(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fields := false
	if filepath.Base(path) == ".cvsignore" {
		fields = true
	}
	var pats []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		if fields {
			pats = append(pats, strings.Fields(line)...)
		} else {
			pats = append(pats, line)
		}
	}
	return pats, nil
}

// visitMember decides whether a node becomes a member and appends it.
func (b *builder) visitMember(path, name string, st os.FileInfo, out *[]member) error {
	o := b.o

	// Exclusion always looks at the name without its trailing slash.
	if b.excluded(strings.TrimSuffix(name, "/")) {
		return nil
	}
	if b.newerOK && !st.ModTime().After(b.newer.t) {
		return nil
	}

	m := member{path: path, name: name, st: st}

	switch {
	case st.IsDir():
		// directory member, nothing else to decide
	case st.Mode()&os.ModeSymlink != 0 && !o.Dereference:
		target, err := os.Readlink(path)
		if err != nil {
			b.readFailure(name, "stat", err)
			return nil
		}
		m.linkname = target
	case st.Mode().IsRegular() || (st.Mode()&os.ModeSymlink != 0 && o.Dereference):
		if !o.HardDereference && nlOf(st) > 1 {
			if key, ok := fileKey(st); ok {
				if prev, dup := b.seen[key]; dup {
					m.hard = true
					m.linkname = prev
					if idx := indexOf(*out, prev); idx >= 0 {
						m.order = (*out)[idx].order
					}
				} else {
					b.seen[key] = name
				}
			}
		}
	default:
		// fifo, device, socket: archived as-is
	}

	*out = append(*out, m)
	return nil
}

func indexOf(members []member, name string) int {
	for i := range members {
		if members[i].name == name {
			return i
		}
	}
	return -1
}

// fileKey is the identity used to spot hard links.
func fileKey(st os.FileInfo) (string, bool) {
	s := syscallStatOf(st)
	if s == nil {
		return "", false
	}
	return fmt.Sprintf("%d:%d", s.dev, s.ino), true
}

// nlOf returns the link count of a file, or 1 when it is unknown.
func nlOf(st os.FileInfo) uint64 {
	if s := syscallStatOf(st); s != nil {
		return s.nl
	}
	return 1
}

func devOf(st os.FileInfo) uint64 {
	s := syscallStatOf(st)
	if s == nil {
		return 0
	}
	return s.dev
}

// parseDate understands the formats GNU tar's --newer/--mtime accept, plus
// "now", "today", "yesterday" and relative expressions like "+2 days".
func parseDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty date")
	}
	if t, ok := fileMtime(s); ok {
		return t, nil
	}
	now := time.Now()
	lower := strings.ToLower(s)
	switch lower {
	case "now", "today":
		return now, nil
	case "yesterday":
		return now.AddDate(0, 0, -1), nil
	case "tomorrow":
		return now.AddDate(0, 0, 1), nil
	}
	// GNU's date parser reads "@SECONDS[.FRACTION]" as a Unix timestamp.
	if strings.HasPrefix(s, "@") {
		sec, err := strconv.ParseFloat(s[1:], 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid date %q", s)
		}
		whole, frac := math.Modf(sec)
		return time.Unix(int64(whole), int64(frac*1e9)), nil
	}
	// relative: "+N unit", "N unit", "N unit ago", "+1 week" ...
	fields := strings.Fields(s)
	if len(fields) == 2 || len(fields) == 3 {
		if n, err := strconv.Atoi(fields[0]); err == nil {
			ago := false
			if len(fields) == 3 {
				if !strings.EqualFold(fields[2], "ago") {
					return time.Time{}, fmt.Errorf("invalid date %q", s)
				}
				ago = true
			}
			if t, ok := relativeDate(now, fields[1], n, ago); ok {
				return t, nil
			}
			return time.Time{}, fmt.Errorf("invalid date %q", s)
		}
	}

	layouts := []string{
		"2006-01-02 15:04:05.999999999 -0700",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"01/02/2006 15:04:05",
		"01/02/2006 15:04",
		"01/02/2006",
		"1/2/2006",
		"02/01/2006",
		"2006-01-02T15:04:05Z07:00",
		time.RFC3339Nano,
		"Mon Jan 2 15:04:05 2006",
		"Mon Jan 02 15:04:05 2006",
		"Jan 2 15:04:05 2006",
		"2 Jan 15:04:05 2006",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid date %q", s)
}

// relativeDate applies "N unit[s] [ago]" offsets, which is how GNU's date
// parser understands "yesterday", "1 day ago" and "+1 week".
func relativeDate(now time.Time, unit string, n int, ago bool) (time.Time, bool) {
	if ago {
		n = -n
	}
	switch strings.ToLower(unit) {
	case "second", "seconds", "sec", "secs":
		return now.Add(time.Duration(n) * time.Second), true
	case "minute", "minutes", "min", "mins":
		return now.Add(time.Duration(n) * time.Minute), true
	case "hour", "hours", "hr", "hrs":
		return now.Add(time.Duration(n) * time.Hour), true
	case "day", "days":
		return now.AddDate(0, 0, n), true
	case "week", "weeks", "fortnight", "fortnights":
		return now.AddDate(0, 0, 7*n), true
	case "month", "months":
		return now.AddDate(0, n, 0), true
	case "year", "years":
		return now.AddDate(n, 0, 0), true
	}
	return time.Time{}, false
}

// fileMtime implements GNU's "DATE-OR-FILE": if the argument names a file, its
// modification time is used.
func fileMtime(s string) (time.Time, bool) {
	if strings.ContainsAny(s, " \t:") {
		// Dates like "2026-10-25 12:00" must not be treated as paths.
		if st, err := os.Stat(s); err == nil {
			return st.ModTime(), true
		}
		return time.Time{}, false
	}
	if st, err := os.Stat(s); err == nil && !st.IsDir() {
		return st.ModTime(), true
	}
	return time.Time{}, false
}

// backupSuffix returns the suffix used by --backup (simple backups).
func backupSuffix(o *Options) string {
	if o.Suffix != "" {
		return o.Suffix
	}
	if e := getenvDefault("SIMPLE_BACKUP_SUFFIX", "~"); e != "" {
		return e
	}
	return "~"
}

func getenvDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
