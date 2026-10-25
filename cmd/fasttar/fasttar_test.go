package main

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiziano/fastcopy/internal/progress"
)

func TestWildmatch(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		member  string
		opts    matchOpts
		want    bool
	}{
		{"star", "*.txt", "a.txt", matchOpts{wildcards: true, anchored: true}, true},
		{"star stops at slash", "*.txt", "sub/a.txt", matchOpts{wildcards: true, anchored: true}, false},
		{"star crosses slash", "sub/*.txt", "sub/a.txt", matchOpts{wildcards: true, matchSlash: true, anchored: true}, true},
		{"question mark", "a?.txt", "ab.txt", matchOpts{wildcards: true, anchored: true}, true},
		{"question mark one char", "a?.txt", "abc.txt", matchOpts{wildcards: true, anchored: true}, false},
		{"class", "[abc].txt", "b.txt", matchOpts{wildcards: true, anchored: true}, true},
		{"negated class", "[!abc].txt", "b.txt", matchOpts{wildcards: true, anchored: true}, false},
		{"range", "[a-c].txt", "c.txt", matchOpts{wildcards: true, anchored: true}, true},
		{"escape", `a\*.txt`, "a*.txt", matchOpts{wildcards: true, anchored: true}, true},
		{"ignore case", "*.TXT", "a.txt", matchOpts{wildcards: true, anchored: true, ignoreCase: true}, true},
		{"literal dot", "a.b", "a.b", matchOpts{anchored: true}, true},
		// Non-anchored patterns may start at any component boundary.
		{"unanchored component", "sub", "tree/sub", matchOpts{wildcards: true}, true},
		{"unanchored prefix", "*.o", "tree/a.o", matchOpts{wildcards: true}, true},
		{"unanchored no match", "sub", "tree/subway", matchOpts{wildcards: true}, false},
		{"wildcards cross slash", "sub*file", "sub/file", matchOpts{wildcards: true, matchSlash: true, anchored: true}, true},
		{"wildcards stop at slash", "sub*file", "sub/file", matchOpts{wildcards: true, anchored: true}, false},
	}
	for _, tc := range tests {
		if got := wildmatch(tc.pattern, tc.member, tc.opts); got != tc.want {
			t.Errorf("%s: wildmatch(%q, %q) = %v, want %v", tc.name, tc.pattern, tc.member, got, tc.want)
		}
	}
}

func TestExclSet(t *testing.T) {
	wild := matchOpts{wildcards: true, matchSlash: true}
	tests := []struct {
		name   string
		opts   matchOpts
		pats   []string
		member string
		want   bool
	}{
		{"component", wild, []string{"sub"}, "./sub/file", true},
		{"component prefix", wild, []string{"su*"}, "tree/subway/x", true},
		{"no false component", wild, []string{"sub"}, "./subway/x", false},
		{"leading directory", wild, []string{"./sub"}, "./sub/file", true},
		{"slash pattern", wild, []string{"sub/file"}, "./sub/file", true},
		{"wildcard crosses slash", wild, []string{"sub*file"}, "sub/file", true},
		{"trailing slash does not match", wild, []string{"sub/"}, "./sub/file", false},
		{"anchored", matchOpts{wildcards: true, matchSlash: true, anchored: true}, []string{"sub"}, "./sub/file", false},
		{"anchored leading dir", matchOpts{wildcards: true, matchSlash: true, anchored: true}, []string{"./sub"}, "./sub/file", true},
		{"literal is not a glob", matchOpts{}, []string{"*.txt"}, "./a.txt", false},
		{"literal component", matchOpts{}, []string{"a.txt"}, "./a.txt", true},
		{"literal leading dir", matchOpts{}, []string{"sub"}, "./sub/file", true},
		{"literal ignore case", matchOpts{ignoreCase: true}, []string{"SUB"}, "./sub/file", true},
	}
	for _, tc := range tests {
		e := &exclSet{opts: tc.opts, pats: tc.pats}
		if got := e.match(tc.member); got != tc.want {
			t.Errorf("%s: match(%q) with %v = %v, want %v", tc.name, tc.member, tc.pats, got, tc.want)
		}
	}
}

func TestReadVCSIgnore(t *testing.T) {
	dir := t.TempDir()
	git := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(git, []byte("# comment\n*.o\n\n!keep.o\r\nbuild/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pats, err := readVCSIgnore(git)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"*.o", "build/"}
	if strings.Join(pats, ",") != strings.Join(want, ",") {
		t.Errorf("readVCSIgnore = %v, want %v", pats, want)
	}

	cvs := filepath.Join(dir, ".cvsignore")
	if err := os.WriteFile(cvs, []byte("a.o b.o\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pats, err = readVCSIgnore(cvs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pats, ",") != "a.o,b.o" {
		t.Errorf("readVCSIgnore(.cvsignore) = %v", pats)
	}
}

func TestParseTransform(t *testing.T) {
	tests := []struct {
		expr  string
		name  string
		want  string
		error bool
	}{
		{expr: "s/^tfx/Z/", name: "tfx/a", want: "Z/a"},
		{expr: "s/a/b/", name: "aaa", want: "baa"},
		{expr: "s/a/b/g", name: "aaa", want: "bbb"},
		{expr: "s/a/b/2", name: "aaa", want: "aba"},
		{expr: "s/A/x/i", name: "aA", want: "xA"},
		{expr: "s|/|_|g", name: "a/b/c", want: "a_b_c"},
		// GNU tar uses POSIX basic regular expressions: groups and operators
		// must be escaped, plain parentheses are literal.
		{expr: `s/\(a\+\)/[\1]/`, name: "aa", want: "[aa]"},
		{expr: "s/(a+)/X/", name: "aa", want: "aa"},
		{expr: "s/(a+)/X/", name: "(a+)", want: "X"},
		{expr: `s/a\+/X/`, name: "aaa", want: "X"},
		{expr: "s/a+/X/", name: "a+", want: "X"},
		{expr: `s/a\{2\}/X/`, name: "aaa", want: "Xa"},
		{expr: `s/[[:digit:]]/X/g`, name: "a1b2", want: "aXbX"},
		{expr: "", name: "unchanged", want: "unchanged"},
		// GNU tar's parser requires both delimiters and rejects unknown flags.
		{expr: "s/a", error: true},
		{expr: "s/a/b", error: true},
		{expr: "s/a/b/Q", error: true},
	}
	for _, tc := range tests {
		tr, err := parseTransform(tc.expr)
		if tc.error {
			if err == nil {
				t.Errorf("parseTransform(%q) succeeded, want an error", tc.expr)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTransform(%q): %v", tc.expr, err)
			continue
		}
		if got := tr.apply(tc.name); got != tc.want {
			t.Errorf("parseTransform(%q).apply(%q) = %q, want %q", tc.expr, tc.name, got, tc.want)
		}
	}
}

func TestParseDate(t *testing.T) {
	tm, err := parseDate("2020-01-01")
	if err != nil {
		t.Fatalf("parseDate: %v", err)
	}
	if y, m, d := tm.Date(); y != 2020 || m != time.January || d != 1 {
		t.Errorf("parseDate(2020-01-01) = %v", tm)
	}
	if h, m, s := tm.Clock(); h != 0 || m != 0 || s != 0 {
		t.Errorf("parseDate(2020-01-01) time = %02d:%02d:%02d, want midnight", h, m, s)
	}

	tm, err = parseDate("@0")
	if err != nil || !tm.Equal(time.Unix(0, 0)) {
		t.Errorf("parseDate(@0) = %v, %v", tm, err)
	}

	tm, err = parseDate("now")
	if err != nil || time.Since(tm) > time.Minute {
		t.Errorf("parseDate(now) = %v, %v", tm, err)
	}

	tm, err = parseDate("1 day ago")
	if err != nil || time.Since(tm) > 26*time.Hour || time.Since(tm) < 23*time.Hour {
		t.Errorf("parseDate(1 day ago) = %v, %v", tm, err)
	}

	tm, err = parseDate("+1 week")
	if err != nil || time.Until(tm) < 6*24*time.Hour {
		t.Errorf("parseDate(+1 week) = %v, %v", tm, err)
	}

	if _, err := parseDate("not a date"); err == nil {
		t.Error("parseDate(not a date) succeeded")
	}
	// GNU's date parser does not know this word and substitutes a sentinel.
	if _, err := parseDate("epoch"); err == nil {
		t.Error("parseDate(epoch) succeeded")
	}
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		mode    opMode
		wantErr string // substring of the error message, empty means success
		kind    any    // expected error wrapper type
	}{
		{name: "create", args: []string{"-cf", "a.tar", "x"}, mode: modeCreate},
		{name: "list", args: []string{"-tf", "a.tar"}, mode: modeList},
		{name: "extract", args: []string{"-xf", "a.tar"}, mode: modeExtract},
		{name: "diff", args: []string{"-df", "a.tar"}, mode: modeDiff},
		{name: "same mode twice", args: []string{"-c", "-c", "-f", "a.tar", "x"}, mode: modeCreate},
		{name: "no mode", args: []string{"-f", "a.tar"},
			wantErr: "You must specify one of the '-Acdtrux', '--delete' or '--test-label' options",
			kind:    &usageError{}},
		{name: "two modes", args: []string{"-c", "-x", "-f", "a.tar"},
			wantErr: "You may not specify more than one", kind: &usageError{}},
		{name: "bad blocking factor", args: []string{"-b", "0", "-cf", "a.tar", "x"},
			wantErr: "0: Invalid blocking factor", kind: &usageError{}},
		{name: "bad format", args: []string{"-H", "bogus", "-cf", "a.tar", "x"},
			wantErr: "bogus: Invalid archive format", kind: &usageError{}},
		{name: "bad sort", args: []string{"--sort=bogus", "-tf", "a.tar"},
			wantErr: "invalid argument 'bogus' for '--sort'", kind: &argRangeError{}},
		{name: "compression option", args: []string{"-z", "-cf", "a.tar", "x"}, mode: modeCreate},
		{name: "old files conflict", args: []string{"--overwrite", "--keep-old-files", "-xf", "a.tar"},
			wantErr: "'--keep-old-files' cannot be used with '--overwrite'", kind: &usageError{}},
		{name: "two threshold dates", args: []string{"--newer=x", "--newer=y", "-tf", "a.tar"},
			wantErr: "More than one threshold date", kind: &usageError{}},
		{name: "unreadable exclude file", args: []string{"-X", filepath.Join(t.TempDir(), "missing"), "-cf", "a.tar", "x"},
			wantErr: "No such file or directory", kind: &fatalError{}},
		{name: "checkpoint not an integer", args: []string{"--checkpoint=abc", "-tf", "a.tar"},
			wantErr: "--checkpoint value is not an integer", kind: &fatalError{}},
	}
	for _, tc := range tests {
		o, err := parseArgs(tc.args)
		if tc.name == "compression option" {
			// -z is accepted by the parser; main refuses it before writing.
			if err != nil || !o.CompressionAsked {
				t.Errorf("%s: err=%v CompressionAsked=%v", tc.name, err, o != nil && o.CompressionAsked)
			}
			continue
		}
		if tc.wantErr == "" && tc.kind == nil {
			if err != nil {
				t.Errorf("%s: parseArgs(%v): %v", tc.name, tc.args, err)
				continue
			}
			if o.Mode != tc.mode {
				t.Errorf("%s: mode = %v, want %v", tc.name, o.Mode, tc.mode)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: parseArgs(%v) succeeded, want an error", tc.name, tc.args)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: error = %q, want it to contain %q", tc.name, err, tc.wantErr)
		}
		switch tc.kind.(type) {
		case *usageError:
			var e *usageError
			if !errors.As(err, &e) {
				t.Errorf("%s: error type %T, want *usageError", tc.name, err)
			}
		case *argRangeError:
			var e *argRangeError
			if !errors.As(err, &e) {
				t.Errorf("%s: error type %T, want *argRangeError", tc.name, err)
			}
		case *fatalError:
			var e *fatalError
			if !errors.As(err, &e) {
				t.Errorf("%s: error type %T, want *fatalError", tc.name, err)
			}
		case *unsupportedError:
			var e *unsupportedError
			if !errors.As(err, &e) {
				t.Errorf("%s: error type %T, want *unsupportedError", tc.name, err)
			}
		}
	}
}

func TestParseArgsValues(t *testing.T) {
	o, err := parseArgs([]string{
		"-c", "-f", "a.tar", "-v", "--format=pax", "--exclude=*.o",
		"--exclude-from", "/dev/null", "--owner=0", "--group=0",
		"--numeric-owner", "--totals", "--wildcards", "-C", "/tmp", "x",
	})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if o.Format != "pax" {
		t.Errorf("Format = %q, want pax", o.Format)
	}
	if len(o.Excludes) != 1 || o.Excludes[0] != "*.o" {
		t.Errorf("Excludes = %v", o.Excludes)
	}
	if o.Verbose != 1 || !o.NumericOwner || !o.Totals || !o.Wildcards {
		t.Errorf("boolean flags not applied: %+v", o)
	}
	if o.Owner != "0" || o.Group != "0" {
		t.Errorf("Owner/Group = %q/%q", o.Owner, o.Group)
	}
	if len(o.Cmds) == 0 {
		t.Fatal("no commands recorded")
	}
	last := o.Cmds[len(o.Cmds)-1]
	if last.kind != cmdName || last.value != "x" || last.base != "/tmp" {
		t.Errorf("last command = %+v, want name x in /tmp", last)
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0B"},
		{1, "1B"},
		{1023, "1023B"},
		{1024, "1.0KiB"},
		{1536, "1.5KiB"},
		{10239, "10KiB"}, // at ten units the decimal disappears
		{1024 * 1024, "1.0MiB"},
		{1258291, "1.2MiB"}, // rounded up
		{2 * 1024 * 1024, "2.0MiB"},
		{20981760, "21MiB"},
	}
	for _, tc := range tests {
		if got := formatBytes(tc.n); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestApplySymbolicMode(t *testing.T) {
	tests := []struct {
		mode  os.FileMode
		expr  string
		isDir bool
		want  os.FileMode
	}{
		{0, "u+rw", false, 0600},
		{0, "go+r", false, 0044},
		{0777, "a-x", false, 0666},
		{0644, "u+x,go-w", false, 0744},
		{0644, "a=r", false, 0444},
		{0644, "755", false, 0755},
		{0644, "u+X", false, 0644}, // X adds x only for directories or already-executable files
		{0644, "u+X", true, 0744},
	}
	for _, tc := range tests {
		got, err := applySymbolicMode(tc.mode, tc.expr, tc.isDir)
		if err != nil {
			t.Errorf("applySymbolicMode(%v, %q): %v", tc.mode, tc.expr, err)
			continue
		}
		if got != tc.want {
			t.Errorf("applySymbolicMode(%v, %q) = %v, want %v", tc.mode, tc.expr, got, tc.want)
		}
	}
	for _, bad := range []string{"ugly", "q+r", "u+q"} {
		if _, err := applySymbolicMode(0644, bad, false); err == nil {
			t.Errorf("applySymbolicMode(%q) succeeded, want an error", bad)
		}
	}
}

func TestStripComponents(t *testing.T) {
	tests := []struct {
		name string
		n    int
		want string
		ok   bool
	}{
		{"a/b/c", 0, "a/b/c", true},
		{"a/b/c", 1, "b/c", true},
		{"a/b/c", 2, "c", true},
		{"a/b/c", 3, "", false},
		{"./a/b", 1, "a/b", true},
		{"a/b/", 1, "b/", true},
	}
	for _, tc := range tests {
		got, ok := stripComponents(tc.name, tc.n)
		if got != tc.want || ok != tc.ok {
			t.Errorf("stripComponents(%q, %d) = %q, %v; want %q, %v", tc.name, tc.n, got, ok, tc.want, tc.ok)
		}
	}
}

func TestIsSparseVersion(t *testing.T) {
	for _, s := range []string{"0", "3", "99", "-3", "+5", "1.5", "2.", ".5"} {
		if !isSparseVersion(s) {
			t.Errorf("isSparseVersion(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "x", "1a", "1.2.3", "-"} {
		if isSparseVersion(s) {
			t.Errorf("isSparseVersion(%q) = true, want false", s)
		}
	}
}

// TestCreateAndList drives the create path and reads the result back with the
// standard library's tar reader, then checks the listing writer's selection.
func TestCreateAndList(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	small := []byte("hello\n")
	big := []byte(strings.Repeat("x", 5000))
	if err := os.WriteFile(filepath.Join(src, "a.txt"), small, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.bin"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(src, "a.txt"), filepath.Join(src, "hard")); err != nil {
		t.Fatal(err)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	if err := os.Chdir(src); err != nil {
		t.Fatal(err)
	}

	arch := filepath.Join(dir, "out.tar")
	o, err := parseArgs([]string{"-cf", arch, "--mtime=2020-01-01", "a.txt", "sub", "link", "hard"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	prog := progress.New(io.Discard, false)
	if err := runCreate(o, prog); err != nil {
		t.Fatalf("runCreate: %v", err)
	}
	prog.Finish()

	f, err := os.Open(arch)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Size()%int64(recordSize(o)) != 0 {
		t.Errorf("archive size %d is not a multiple of the record size %d", st.Size(), recordSize(o))
	}

	tr := tar.NewReader(f)
	got := map[string]*tar.Header{}
	var contents = map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading archive: %v", err)
		}
		got[h.Name] = h
		if h.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatal(err)
			}
			contents[h.Name] = string(data)
		}
	}
	for _, name := range []string{"a.txt", "sub/", "sub/b.bin", "link", "hard"} {
		if got[name] == nil {
			t.Errorf("member %q missing from archive", name)
		}
	}
	if contents["a.txt"] != string(small) {
		t.Errorf("a.txt contents = %q", contents["a.txt"])
	}
	if contents["sub/b.bin"] != string(big) {
		t.Errorf("sub/b.bin contents truncated")
	}
	if got["link"] != nil && (got["link"].Typeflag != tar.TypeSymlink || got["link"].Linkname != "a.txt") {
		t.Errorf("link header = %+v", got["link"])
	}
	if got["hard"] != nil && (got["hard"].Typeflag != tar.TypeLink || got["hard"].Linkname != "a.txt") {
		t.Errorf("hard link header = %+v", got["hard"])
	}
	if h := got["a.txt"]; h != nil && !h.ModTime.Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.Local)) {
		t.Errorf("a.txt mtime = %v, want the --mtime value", h.ModTime)
	}

	// The listing selects the same members, in sorted order.
	listPath := filepath.Join(dir, "list.txt")
	lo, err := parseArgs([]string{"-tf", arch, "--index-file=" + listPath})
	if err != nil {
		t.Fatalf("parseArgs -t: %v", err)
	}
	if err := runList(lo, progress.New(io.Discard, false)); err != nil {
		t.Fatalf("runList: %v", err)
	}
	data, err := os.ReadFile(listPath)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		listed = append(listed, line)
	}
	want := []string{"a.txt", "sub/", "sub/b.bin", "link", "hard"}
	if strings.Join(listed, ",") != strings.Join(want, ",") {
		t.Errorf("listing = %v, want %v", listed, want)
	}

	// A named member is the only thing extracted.
	out := filepath.Join(dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	xo, err := parseArgs([]string{"-xf", arch, "-C", out, "sub/b.bin"})
	if err != nil {
		t.Fatalf("parseArgs -x: %v", err)
	}
	if err := runExtract(xo, progress.New(io.Discard, false)); err != nil {
		t.Fatalf("runExtract: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(out, "sub", "b.bin")); err != nil || string(b) != string(big) {
		t.Errorf("extracted sub/b.bin = %v, %v", len(b), err)
	}
	if _, err := os.Stat(filepath.Join(out, "a.txt")); !os.IsNotExist(err) {
		t.Errorf("unselected member a.txt was extracted")
	}
}
