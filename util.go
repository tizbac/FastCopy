package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// parseSize accepts cp-ish sizes: 512, 4K, 1M, 8MiB, 2G
func parseSize(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	mult := 1
	switch {
	case strings.HasSuffix(s, "KiB"), strings.HasSuffix(s, "K"), strings.HasSuffix(s, "k"):
		mult = 1024
		s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "KiB"), "K"), "k")
	case strings.HasSuffix(s, "MiB"), strings.HasSuffix(s, "M"), strings.HasSuffix(s, "m"):
		mult = 1024 * 1024
		s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "MiB"), "M"), "m")
	case strings.HasSuffix(s, "GiB"), strings.HasSuffix(s, "G"), strings.HasSuffix(s, "g"):
		mult = 1024 * 1024 * 1024
		s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "GiB"), "G"), "g")
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

func atoi(s string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(s))
}

// cpErr formats an error the way GNU cp does: "cannot <verb> <path>: <reason>".
// The syscall errors already contain the path, so this keeps messages short and
// avoids printing the same path twice.
func cpErr(verb, path string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("cannot %s %s: %w", verb, path, pe.Err)
	}
	return fmt.Errorf("cannot %s %s: %w", verb, path, err)
}

// Human readable byte count, base 1024.
func fmtBytes(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	f := float64(n)
	for _, u := range units {
		f /= 1024
		if f < 1024 {
			return strconv.FormatFloat(f, 'f', 1, 64) + " " + u
		}
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + " EB"
}

func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
	return strconv.Itoa(int(d.Minutes())) + "m" + strconv.Itoa(int(d.Seconds())%60) + "s"
}

// terminalWidth asks the kernel for the terminal columns, falling back to COLUMNS/80.
// TIOCGWINSZ is 0x5413 on Linux; 0x5401 is TCGETS, which would write a struct termios
// into the winsize and smash the stack.
type winsize struct {
	Rows, Cols, X, Y uint16
}

const tiocgwinsz = 0x5413

func terminalWidth(fd int) int {
	ws := &winsize{}
	r, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(tiocgwinsz), uintptr(unsafe.Pointer(ws)))
	if errno == 0 && r == 0 && ws.Cols > 0 {
		return int(ws.Cols)
	}
	if c, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && c > 0 {
		return c
	}
	return 80
}
