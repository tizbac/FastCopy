package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	cases := map[string]int{
		"1":    1,
		"1K":   1024,
		"64k":  65536,
		"4M":   4194304,
		"1G":   1073741824,
		"10":   10,
		"512":  512,
		"8MiB": 8388608,
	}
	for in, want := range cases {
		got, err := parseSize(in)
		if err != nil {
			t.Fatalf("parseSize(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := parseSize("abc"); err == nil {
		t.Error("parseSize(\"abc\") should fail")
	}
}

func TestFlagClustering(t *testing.T) {
	opts, args, err := parseArgs([]string{"-rpv", "-a", "--jobs=4", "src", "dst"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !opts.Recursive || !opts.Verbose || !opts.PreserveTime || !opts.PreserveLinks {
		t.Errorf("clustered flags not parsed: %+v", opts)
	}
	if opts.Jobs != 4 {
		t.Errorf("--jobs=4 -> %d", opts.Jobs)
	}
	if len(args) != 2 || args[0] != "src" || args[1] != "dst" {
		t.Errorf("positional args = %v", args)
	}
}

func TestStripTrailingSlashes(t *testing.T) {
	opts, args, err := parseArgs([]string{"--strip-trailing-slashes", "a/", "b/", "dst"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !opts.StripSlashes {
		t.Fatal("StripSlashes not set")
	}
	if args[0] != "a" || args[1] != "b" || args[2] != "dst" {
		t.Errorf("args = %v, want [a b dst]", args)
	}
}

func TestPreserveList(t *testing.T) {
	opts, _, err := parseArgs([]string{"--preserve=mode,timestamps", "-s", "a", "b"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !opts.PreserveMode || !opts.PreserveTime || opts.PreserveOwner {
		t.Errorf("--preserve=mode,timestamps -> %+v", opts)
	}
	if _, _, err := parseArgs([]string{"--preserve=bogus", "a", "b"}); err == nil {
		t.Error("bogus --preserve attribute should fail")
	}
}

func TestMakeParents(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "a", "b"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a", "b", "leaf.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "dst")
	if err := os.Mkdir(dst, 0755); err != nil {
		t.Fatal(err)
	}

	opts := defaultOptions()
	c := &copier{opts: &opts, prog: NewProgress(os.Stdout, false), hardlinks: map[uint64]string{}}
	leaf := filepath.Join(src, "a", "b", "leaf.txt")
	if err := c.makeParents(leaf, dst); err != nil {
		t.Fatalf("makeParents: %v", err)
	}
	// cp --parents strips the leading slash of an absolute source and recreates
	// the rest of the path under the destination.
	chain := strings.TrimSuffix(strings.TrimPrefix(leaf, "/"), "/leaf.txt")
	for k := 1; k <= len(strings.Split(chain, "/")); k++ {
		want := filepath.Join(dst, strings.Join(strings.Split(chain, "/")[:k], "/"))
		if _, err := os.Stat(want); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
}

func TestSparseCopyKeepsLogicalSize(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src.bin")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(4<<20, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	c := &copier{opts: &opts, prog: NewProgress(os.Stdout, false), hardlinks: map[uint64]string{}}
	e := entry{
		src:   src,
		dst:   filepath.Join(root, "dst.bin"),
		size:  info.Size(),
		mode:  info.Mode(),
		mtime: info.ModTime(),
		atime: time.Now(),
	}
	if err := c.copyOne(e); err != nil {
		t.Fatalf("copyOne: %v", err)
	}
	got, err := os.Stat(e.dst)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size() != info.Size() {
		t.Errorf("destination size %d != source size %d", got.Size(), info.Size())
	}
	if got.Mode() != info.Mode() {
		t.Errorf("destination mode %v != source mode %v", got.Mode(), info.Mode())
	}
}

func TestDirectoryNeedsRecursive(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0755); err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	c := &copier{opts: &opts, prog: NewProgress(os.Stderr, false), hardlinks: map[uint64]string{}}
	var entries []entry
	if err := c.walk(filepath.Join(root, "dir"), filepath.Join(root, "out"), &entries); err == nil {
		t.Error("copying a directory without -r should fail like cp")
	}
	opts.Recursive = true
	if err := c.walk(filepath.Join(root, "dir"), filepath.Join(root, "out"), &entries); err != nil {
		t.Errorf("recursive walk failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "out")); err != nil {
		t.Errorf("recursive walk did not create the destination: %v", err)
	}
}
