package main

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tiziano/fastcopy/internal/progress"
)

// defaultTopLevel is the directory --one-top-level derives when no name is
// given: the archive file name without its .tar suffix and any compression
// suffix it may carry.
func defaultTopLevel(o *Options) string {
	name := filepath.Base(o.Archive)
	if name == "-" || o.Archive == "-" {
		return "tar"
	}
	for _, suf := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst", ".tar.lz",
		".tar.Z", ".tar", ".tgz", ".tbz2", ".txz", ".tzst"} {
		if strings.HasSuffix(name, suf) {
			return strings.TrimSuffix(name, suf)
		}
	}
	return strings.TrimSuffix(name, ".tar")
}

// splitAction separates an action name from its argument, as GNU tar does for
// --checkpoint-action=echo=TEXT.
func splitAction(a string) (string, string) {
	if i := strings.Index(a, "="); i >= 0 {
		return a[:i], a[i+1:]
	}
	return a, ""
}

// checkCheckpointAction rejects actions GNU tar does not know. Validation
// happens while the options are parsed, before the archive is touched.
func checkCheckpointAction(a string) error {
	name, arg := splitAction(a)
	switch name {
	case "echo", "dot", "bell":
		return nil
	case "sleep":
		if !strings.Contains(a, "=") {
			return &fatalError{msg: fmt.Sprintf("%s: unknown checkpoint action", a)}
		}
		if _, err := strconv.ParseFloat(arg, 64); err != nil {
			return &fatalError{msg: fmt.Sprintf("%s: not a valid timeout", a)}
		}
		return nil
	default:
		return &fatalError{msg: fmt.Sprintf("%s: unknown checkpoint action", a)}
	}
}

// runCheckpoint executes the checkpoint actions GNU tar implements: echo, dot,
// bell and sleep. The text goes to stderr so an archive written to stdout
// stays clean.
func runCheckpoint(o *Options, n int64, writing bool) {
	actions := o.CheckpointAction
	if len(actions) == 0 {
		// GNU tar's default checkpoint action is echo.
		actions = []string{"echo"}
	}
	for _, a := range actions {
		name, arg := splitAction(a)
		switch name {
		case "echo":
			if arg == "" {
				if writing {
					infof("Write checkpoint %d", n)
				} else {
					infof("Read checkpoint %d", n)
				}
			} else {
				infof("%s", arg)
			}
		case "dot":
			// GNU tar's dot action goes to stdout, not to stderr.
			fmt.Print(".")
		case "bell":
			// GNU tar only rings when stdout is a terminal.
			if stdoutIsTTY() {
				fmt.Print("\a")
			}
		case "sleep":
			if d, err := strconv.ParseFloat(arg, 64); err == nil {
				time.Sleep(time.Duration(d * float64(time.Second)))
			}
		}
	}
}

// applySymbolicMode implements the --mode CHANGES expression, the same
// syntax chmod accepts: u+rw,go+rX, a-w ...
func applySymbolicMode(mode os.FileMode, expr string, isDir bool) (os.FileMode, error) {
	perms := uint32(mode.Perm())
	for _, clause := range strings.Split(expr, ",") {
		clause = strings.TrimSpace(clause)
		if clause == "" {
			continue
		}
		// A plain octal number behaves like chmod: it replaces the mode.
		if isOctal(clause) {
			n, err := strconv.ParseUint(clause, 8, 32)
			if err != nil {
				return mode, fmt.Errorf("Invalid mode given on option")
			}
			perms = uint32(n) & 07777
			continue
		}
		i := 0
		// who: [ugoa]*
		who := byte(0)
		for i < len(clause) && strings.IndexByte("ugoa", clause[i]) >= 0 {
			who |= 1 << strings.IndexByte("ugoa", clause[i])
			i++
		}
		if who == 0 {
			who = 0b1111 // 'a' when nothing is written
		}
		if i >= len(clause) {
			return mode, fmt.Errorf("invalid --mode expression %q", expr)
		}
		op := clause[i]
		if op != '+' && op != '-' && op != '=' {
			return mode, fmt.Errorf("invalid --mode expression %q", expr)
		}
		i++

		var bits uint32
		var copyFrom byte
		for i < len(clause) {
			c := clause[i]
			i++
			switch c {
			case 'r':
				bits |= 0444
			case 'w':
				bits |= 0222
			case 'x':
				bits |= 0111
			case 'X':
				if isDir || mode&0111 != 0 {
					bits |= 0111
				}
			case 's':
				bits |= 06000
			case 't':
				bits |= 01000
			case 'u', 'g', 'o':
				// chmod's copy form: g=u takes the user bits.
				if copyFrom != 0 {
					return mode, fmt.Errorf("invalid --mode expression %q", expr)
				}
				copyFrom = c
			default:
				return mode, fmt.Errorf("invalid mode character %q in --mode %q", string(c), expr)
			}
		}
		if copyFrom != 0 {
			var src uint32
			switch copyFrom {
			case 'u':
				src = (perms >> 6) & 7
			case 'g':
				src = (perms >> 3) & 7
			case 'o':
				src = perms & 7
			}
			bits |= (src << 6) | (src << 3) | src
		}
		// Apply the requested bits only to the selected users. The special
		// bits follow their class: u+s sets setuid, g+s setgid, o+t sticky.
		var mask, clear uint32
		addClass := func(perm uint32, special uint32) {
			mask |= bits & perm
			if bits&special != 0 {
				mask |= special
			}
			clear |= perm | special
		}
		if who&8 != 0 { // 'a'
			addClass(0700, 04000)
			addClass(0070, 02000)
			addClass(0007, 01000)
		} else {
			if who&1 != 0 {
				addClass(0700, 04000)
			}
			if who&2 != 0 {
				addClass(0070, 02000)
			}
			if who&4 != 0 {
				addClass(0007, 01000)
			}
		}
		switch op {
		case '+':
			perms |= mask
		case '-':
			perms &= ^mask
		case '=':
			perms = (perms & ^clear) | (mask & clear)
		}
	}
	return os.FileMode(perms), nil
}

// isOctal reports whether s is a bare octal number such as "644" or "0755".
func isOctal(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '7' {
			return false
		}
	}
	return true
}

// readable rejects an --exclude-from operand that cannot be opened. GNU tar
// reads these files while parsing the options, so the failure is fatal.
func readable(f string) error {
	if f == "-" {
		return nil
	}
	st, err := os.Stat(f)
	if err != nil {
		return &fatalError{msg: fmt.Sprintf("%s: %s", f, errnoString(err))}
	}
	if st.IsDir() {
		return &fatalError{msg: fmt.Sprintf("%s: Is a directory", f)}
	}
	return nil
}

// isSparseVersion mirrors GNU tar's strtoll-based parse of --sparse-version:
// an optional sign, digits, and an optional ".MINOR". Any leftover character is
// a bad value; GNU does not range-check the numbers themselves.
func isSparseVersion(s string) bool {
	i := 0
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		i++
	}
	digits := false
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits = true
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		return i == len(s)
	}
	return digits && i == len(s)
}

// checkPaxOptions validates the keywords GNU tar accepts for --pax-option.
// fasttar understands none of them, but it rejects the same keywords GNU does
// so the command line surface stays identical.
func checkPaxOptions(v string) error {
	for _, kw := range strings.Split(v, ",") {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		if i := strings.IndexByte(kw, '='); i >= 0 {
			kw = kw[:i]
		}
		switch kw {
		case "delimit", "excluded", "key", "map", "subst":
		default:
			return &usageError{msg: fmt.Sprintf("Keyword %s is unknown or not yet implemented", kw)}
		}
	}
	return nil
}

// warnIfUnsupported prints the list of switches fasttar accepted but could
// not honour, before any work starts, so nobody is surprised later.
func warnIfUnsupported(o *Options, prog *progress.Progress) {
	for _, w := range o.Warned {
		warnf("warning: %s", w)
	}
}

// GNU tar distinguishes three kinds of diagnostics: plain information,
// warnings (which GNU prints but does not count towards the exit status) and
// errors (exit status 2). The error counter lets main() reproduce GNU's final
// summary line.
var diagErrors, diagWarnings int

// errf reports an error on stderr, exactly like GNU tar, so that the data on
// stdout stays pipeable while the progress box is running.
func errf(format string, args ...any) {
	diagErrors++
	fmt.Fprintf(os.Stderr, "fasttar: "+format+"\n", args...)
}

// warnf reports a warning. GNU tar's warnings never change the exit status, so
// the counter here is only for diagnostics/tests.
func warnf(format string, args ...any) {
	diagWarnings++
	fmt.Fprintf(os.Stderr, "fasttar: "+format+"\n", args...)
}

// infof prints a note that has no effect on the exit status.
func infof(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fasttar: "+format+"\n", args...)
}

// quoteArg renders a file name the way GNU tar does in its messages.
func quoteArg(o *Options, s string) string { return quoteName(o, s) }

// warnFutureTimestamp is GNU tar's note that a member being extracted carries
// a modification time that has not happened yet.
func warnFutureTimestamp(name string, t time.Time) {
	n := now()
	if !t.After(n) {
		return
	}
	d := t.Sub(n)
	warnf("%s: time stamp %s is %d.%09d s in the future", name,
		t.Format("2006-01-02 15:04:05"), int64(d.Seconds()), int64(d.Nanoseconds())%1000000000)
}

// logInfo prints a short informational line when the operation produced one.
func logInfo(o *Options, prog *progress.Progress, format string, args ...any) {
	if o.Verbose > 0 {
		prog.Line(format, args...)
	}
}

// ensureParents creates the directory chain a member lives in. GNU tar does
// this with mode 0777 & ~umask for directories the archive does not mention.
func ensureParents(dst string) error {
	dir := filepath.Dir(dst)
	if dir == "" || dir == "." {
		return nil
	}
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		return nil
	}
	return os.MkdirAll(dir, os.FileMode(0777)&^umask())
}

type countReader struct {
	r io.Reader
	n int64
	// capAt/cap let walkArchive inspect the first block of the next member's
	// header chain (GNU tar reports that block for its long-name entries).
	capAt int64
	cap   []byte
	capOn bool
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if c.capOn && int64(n) > 0 && c.n < c.capAt+512 && c.n+int64(n) > c.capAt {
		from := int64(0)
		if c.capAt > c.n {
			from = c.capAt - c.n
		}
		to := c.capAt + 512 - c.n
		if to > int64(n) {
			to = int64(n)
		}
		if to > from {
			c.cap = append(c.cap, p[from:to]...)
		}
	}
	c.n += int64(n)
	return n, err
}

// now is kept as a variable so tests can freeze the clock.
var now = time.Now

// totalsRead is the number of bytes a reading mode pulled out of the archive;
// --totals reports it.
var totalsRead int64

// warningNames is the set GNU tar accepts for --warning.
var warningNames = []string{
	"all", "alone-zero-block", "bad-dumpdir", "cachedir", "contiguous-cast",
	"file-changed", "file-ignored", "file-removed", "file-shrank",
	"file-unchanged", "filename-with-nuls", "ignore-archive", "ignore-newer",
	"new-directory", "rename-directory", "symlink-cast", "timestamp",
	"unknown-cast", "unknown-keyword", "xdev", "decompress-program",
	"existing-file", "xattr-write", "record-size", "failed-read",
	"missing-zero-blocks", "verbose",
}

// restrictedArg mirrors GNU tar's argp check for options that accept one of a
// fixed set of values: the bad value, the option, and the list of accepted
// ones. An empty value is reported as "ambiguous". There is no "Try --help"
// hint on this path and the status is 2.
func restrictedArg(name, val string, valid []string) error {
	for _, v := range valid {
		if v == val {
			return nil
		}
	}
	if val == "" {
		return &argRangeError{
			msg:  fmt.Sprintf("ambiguous argument '' for '%s'", name),
			vals: valid,
		}
	}
	return &argRangeError{
		msg:  fmt.Sprintf("invalid argument '%s' for '%s'", val, name),
		vals: valid,
	}
}

// formatBytes renders a byte count the way GNU tar's --totals line does: the
// value is expressed in the largest base-1024 unit it reaches, rounded up.
// GNU's human_readable keeps one decimal place while the scaled value is below
// ten units ("1.2MiB") and prints whole units above that ("21MiB").
func formatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const letters = "KMGTPE"
	exp := 0
	for exp < 6 && n >= 1<<(10*(exp+1)) {
		exp++
	}
	if exp == 0 {
		return fmt.Sprintf("%dB", n)
	}
	scale := int64(1) << (10 * exp)
	q, r := n/scale, n%scale
	tenths := q*10 + int64((uint64(r)*10+uint64(scale)-1)/uint64(scale))
	if tenths < 100 {
		return fmt.Sprintf("%d.%d%ciB", tenths/10, tenths%10, letters[exp-1])
	}
	if r != 0 {
		q++
	}
	return fmt.Sprintf("%d%ciB", q, letters[exp-1])
}

// stdoutIsTTY reports whether standard output is a terminal. GNU tar's bell
// checkpoint action rings only in that case.
func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// reportTextualDates prints GNU tar's verbose note explaining how each date
// option string was interpreted. GNU prints it whenever the run is verbose,
// which includes plain listing.
func reportTextualDates(o *Options) {
	if o.Verbose == 0 && o.Mode != modeList {
		return
	}
	for _, d := range o.TextualDates {
		treated := tartime(o, d.t)
		if d.raw != treated {
			warnf("Option %s: Treating date '%s' as %s", d.option, d.raw, treated)
		}
	}
}

// tartime renders a timestamp the way GNU tar's date diagnostics do: local
// time (or UTC with --utc), with the nanosecond fraction trimmed of trailing
// zeros.
func tartime(o *Options, t time.Time) string {
	if o.UTC {
		t = t.UTC()
	}
	s := t.Format("2006-01-02 15:04:05")
	if ns := t.Nanosecond(); ns != 0 {
		s += "." + strings.TrimRight(fmt.Sprintf("%09d", ns), "0")
	}
	return s
}

// errnoString renders an error the way GNU tar renders strerror(errno): the
// libc message with the first letter capitalised.
func errnoString(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		s := errno.Error()
		if s == "" {
			return "Error"
		}
		return strings.ToUpper(s[:1]) + s[1:]
	}
	if err == nil {
		return "Success"
	}
	return err.Error()
}

func firstErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// modeOf returns the permission bits (including setuid/setgid/sticky) of a
// file, in the octal form tar stores them in.
func modeOf(st os.FileInfo) uint32 {
	if s := syscallStatOf(st); s != nil {
		return uint32(s.mode & 07777)
	}
	m := st.Mode()
	perm := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		perm |= 04000
	}
	if m&os.ModeSetgid != 0 {
		perm |= 02000
	}
	if m&os.ModeSticky != 0 {
		perm |= 01000
	}
	return perm
}

// typeMatches answers whether the file on disk is of the kind the archive says.
func typeMatches(typeflag byte, st os.FileInfo) bool {
	switch typeflag {
	case tar.TypeDir:
		return st.IsDir()
	case tar.TypeSymlink:
		return st.Mode()&os.ModeSymlink != 0
	case tar.TypeFifo:
		return st.Mode()&os.ModeNamedPipe != 0
	case tar.TypeChar:
		return st.Mode()&os.ModeCharDevice != 0
	case tar.TypeBlock:
		return st.Mode()&os.ModeDevice != 0 && st.Mode()&os.ModeCharDevice == 0
	default:
		return st.Mode().IsRegular()
	}
}

// timeEqual compares two timestamps the way tar stores them: whole seconds.
func timeEqual(a, b time.Time) bool { return a.Unix() == b.Unix() }

func sameInode(a, b os.FileInfo) bool {
	sa, sb := syscallStatOf(a), syscallStatOf(b)
	if sa == nil || sb == nil {
		return false
	}
	return sa.dev == sb.dev && sa.ino == sb.ino
}
