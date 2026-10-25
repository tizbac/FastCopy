package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tiziano/fastcopy/internal/progress"
)

const version = "1.0.0"

func main() {
	os.Exit(run(os.Args[1:]))
}

// exitCoder lets a condition that is not a failure (differences found by -d,
// a label mismatch) report GNU tar's exit status 1.
type exitCoder interface {
	error
	ExitCode() int
}

type differenceError struct{ n int }

func (e *differenceError) Error() string { return fmt.Sprintf("%d difference(s) found", e.n) }
func (e *differenceError) ExitCode() int { return 1 }

// statusError reports GNU tar's exit status for conditions that were already
// diagnostic-printed (or are silent, like a --test-label mismatch).
type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *statusError) ExitCode() int { return e.code }

func run(argv []string) int {
	o, err := parseArgs(argv)
	if err != nil {
		var ue *unsupportedError
		if errors.As(err, &ue) {
			fmt.Fprintf(os.Stderr, "fasttar: %v\n", ue.option)
			fmt.Fprintf(os.Stderr, "  %s\n", ue.instead)
			fmt.Fprintln(os.Stderr, "Try 'fasttar --help' or 'fasttar --usage' for more information.")
			return 2
		}
		var fe *fatalError
		if errors.As(err, &fe) {
			errf("%s", fe)
			infof("Error is not recoverable: exiting now")
			return 2
		}
		var ue2 *usageError
		if errors.As(err, &ue2) {
			fmt.Fprintf(os.Stderr, "fasttar: %v\n", ue2)
			fmt.Fprintln(os.Stderr, "Try 'fasttar --help' or 'fasttar --usage' for more information.")
			return ue2.ExitCode()
		}
		var ar *argRangeError
		if errors.As(err, &ar) {
			// GNU's argp restricted-argument path: no "Try --help" hint.
			fmt.Fprintf(os.Stderr, "fasttar: %v\n", ar)
			fmt.Fprintln(os.Stderr, "Valid arguments are:")
			for _, v := range ar.vals {
				fmt.Fprintf(os.Stderr, "  - '%s'\n", v)
			}
			return 2
		}
		errf("%v", err)
		return 2
	}

	switch {
	case o.Help:
		printHelp()
		return 0
	case o.Usage:
		printUsage()
		return 0
	case o.Version:
		printVersion()
		return 0
	case o.ShowDefaults:
		showDefaults(os.Stdout)
		return 0
	}

	prog := progress.New(os.Stdout, o.Progress)

	// Refuse compression up front: fasttar writes plain tar streams and says
	// exactly how to pipe the archive through a compressor instead.
	if o.CompressionAsked {
		fmt.Fprintf(os.Stderr, "%v\n", compressOptionError(o))
		return 2
	}

	warnIfUnsupported(o, prog)
	reportTextualDates(o)
	if o.Fast && !o.Mode.creates() {
		warnf("warning: --fast applies to archive creation, it has no effect on %s", o.Mode)
	}

	// GNU tar treats -V LABEL as a test of the archive's volume label in every
	// mode except -c, where it writes the label instead.
	if o.Label != "" && o.Mode != modeCreate {
		if err := checkVolumeLabel(o); err != nil {
			prog.Finish()
			return 2
		}
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		prog.Finish()
		fmt.Fprintln(os.Stderr, "fasttar: interrupted")
		os.Exit(130)
	}()

	start := time.Now()
	var opErr error
	switch o.Mode {
	case modeCreate:
		opErr = runCreate(o, prog)
	case modeAppend:
		opErr = runAppend(o, prog)
	case modeUpdate:
		opErr = runUpdate(o, prog)
	case modeCatenate:
		opErr = runCatenate(o, prog)
	case modeDelete:
		opErr = runDelete(o, prog)
	case modeExtract:
		opErr = runExtract(o, prog)
	case modeList:
		opErr = runList(o, prog)
	case modeDiff:
		opErr = runDiff(o, prog)
	case modeTestLabel:
		opErr = runTestLabel(o, prog)
	default:
		opErr = fmt.Errorf("you must specify one of the operation modes (-Acdrtux)")
	}
	prog.Finish()

	if o.Totals && o.Mode.creates() {
		elapsed := time.Since(start).Seconds()
		fmt.Fprintf(os.Stderr, "Total bytes written: %d (%s, %s/s)\n",
			totalsWritten, formatBytes(totalsWritten),
			formatBytes(int64(float64(totalsWritten)/elapsed)))
	} else if o.Totals && totalsRead > 0 {
		elapsed := time.Since(start).Seconds()
		fmt.Fprintf(os.Stderr, "Total bytes read: %d (%s, %s/s)\n",
			totalsRead, formatBytes(totalsRead),
			formatBytes(int64(float64(totalsRead)/elapsed)))
	}

	if opErr != nil {
		var ue *usageError
		if errors.As(opErr, &ue) {
			fmt.Fprintf(os.Stderr, "fasttar: %v\n", opErr)
			fmt.Fprintln(os.Stderr, "Try 'fasttar --help' or 'fasttar --usage' for more information.")
			return ue.ExitCode()
		}
		var fe *fatalError
		if errors.As(opErr, &fe) {
			errf("%s", fe)
			infof("Error is not recoverable: exiting now")
			return 2
		}
		var ec exitCoder
		if errors.As(opErr, &ec) {
			// GNU exits 1 for differences and label mismatches unless an
			// outright error also happened; a fatal label test exits 2 at
			// once, without the "due to previous errors" summary.
			if diagErrors == 0 || ec.ExitCode() == 2 {
				return ec.ExitCode()
			}
		} else {
			errf("%v", opErr)
		}
	}
	if diagErrors > 0 {
		infof("Exiting with failure status due to previous errors")
		return 2
	}
	return 0
}
