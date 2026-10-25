package progress

import (
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// terminalWidth asks the kernel for the terminal columns, falling back to
// COLUMNS/80. TIOCGWINSZ is 0x5413 on Linux; 0x5401 is TCGETS, which would
// write a struct termios into the winsize and smash the stack.
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
