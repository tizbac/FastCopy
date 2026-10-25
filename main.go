package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tiziano/fastcopy/internal/progress"
)

const version = "1.0.0"

func main() {
	opts, args, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "fastcopy: %v\n", err)
		usage(os.Stderr)
		os.Exit(2)
	}
	if opts.Help {
		usage(os.Stdout)
		return
	}
	if opts.Version {
		fmt.Printf("fastcopy %s (Go)\n", version)
		return
	}
	if len(args) == 0 {
		usage(os.Stderr)
		os.Exit(2)
	}

	var sources []string
	var target string
	if opts.TargetDir != "" {
		sources = args
		target = opts.TargetDir
	} else {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "fastcopy: you must specify at least two paths")
			os.Exit(2)
		}
		sources = args[:len(args)-1]
		target = args[len(args)-1]
	}

	targetIsDir := false
	if opts.NoTargetDir {
		targetIsDir = false
	} else if opts.TargetDir != "" {
		if ti, err := os.Stat(target); err == nil && ti.IsDir() {
			targetIsDir = true
		} else {
			fmt.Fprintf(os.Stderr, "fastcopy: target %q is not a directory\n", target)
			os.Exit(1)
		}
	} else if ti, err := os.Stat(target); err == nil {
		targetIsDir = ti.IsDir()
	} else if len(sources) > 1 || strings.HasSuffix(target, "/") {
		fmt.Fprintf(os.Stderr, "fastcopy: target %q is not a directory\n", target)
		os.Exit(1)
	}

	if opts.Parents && !targetIsDir {
		fmt.Fprintf(os.Stderr, "fastcopy: with --parents, the destination must be a directory\n")
		os.Exit(1)
	}

	prog := progress.New(os.Stdout, opts.Progress)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		prog.Finish()
		fmt.Fprintln(os.Stderr, "fastcopy: interrupted")
		os.Exit(130)
	}()

	c := &copier{
		opts:      &opts,
		prog:      prog,
		hardlinks: make(map[uint64]string),
	}

	var entries []entry
	var walkErr error
	for _, src := range sources {
		if src == "-" {
			fmt.Fprintln(os.Stderr, "fastcopy: reading from standard input is not supported")
			os.Exit(1)
		}

		dst := target
		if opts.Parents {
			dst = filepath.Join(target, strings.TrimPrefix(src, "/"))
			if err := c.makeParents(src, target); err != nil {
				c.warn(err)
				continue
			}
		} else if targetIsDir {
			dst = filepath.Join(target, filepath.Base(src))
		}

		if sameFile(src, dst) {
			fmt.Fprintf(os.Stderr, "fastcopy: cannot copy %q to itself\n", dst)
			os.Exit(1)
		}
		if isDir(src) && inside(src, dst) {
			fmt.Fprintf(os.Stderr, "fastcopy: cannot copy directory %q into itself %q\n", src, dst)
			os.Exit(1)
		}

		if st, err := statT(src); err == nil {
			c.rootDev = st.Dev
		}
		if err := c.walk(src, dst, &entries); err != nil {
			c.warn(err)
			walkErr = err
		}
	}

	start := time.Now()
	err = c.run(entries)
	prog.Finish()

	if err == nil {
		err = walkErr
	}

	if err != nil {
		// Every error that reached this point was already reported by warn().
		os.Exit(1)
	}
	if opts.Verbose {
		fmt.Fprintf(os.Stderr, "fastcopy: %d files, %s in %s\n",
			len(entries), progress.FormatBytes(totalBytes(entries)), progress.FormatDuration(time.Since(start)))
	}
}

func totalBytes(entries []entry) int64 {
	var t int64
	for _, e := range entries {
		t += e.size
	}
	return t
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// inside reports whether dst lies within src.
func inside(src, dst string) bool {
	a, err := filepath.Abs(src)
	b, err2 := filepath.Abs(dst)
	if err != nil || err2 != nil {
		return false
	}
	return b == a || strings.HasPrefix(b, a+"/")
}
