package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// ANSI colors used by the progress bar.
const (
	colReset   = "\x1b[0m"
	colDim     = "\x1b[38;5;245m"
	colFrame   = "\x1b[38;5;240m"
	colFilled  = "\x1b[1;38;5;40m"
	colHead    = "\x1b[38;5;84m"
	colEmpty   = "\x1b[38;5;238m"
	colCyan    = "\x1b[1;38;5;39m"
	colYellow  = "\x1b[1;38;5;214m"
	colMagenta = "\x1b[1;38;5;208m"
	colBlue    = "\x1b[38;5;75m"
	colWhite   = "\x1b[38;5;250m"
)

// Progress draws a multi-line ANSI box on the terminal. It is safe to call
// from several worker goroutines.
type Progress struct {
	mu         sync.Mutex
	out        io.Writer
	enabled    bool
	plain      bool // non-tty output: emit one summary line at the end
	width      int
	totalFiles int
	totalBytes int64
	doneFiles  int
	doneBytes  int64
	current    string
	start      time.Time
	lines      int
	last       time.Time
}

func NewProgress(out io.Writer, enabled bool) *Progress {
	p := &Progress{
		out:     out,
		enabled: enabled,
		width:   terminalWidth(1),
		start:   time.Now(),
	}
	if !isTTY(out) {
		p.plain = true
	}
	if p.width > 72 {
		p.width = 72
	}
	if p.width < 44 {
		p.width = 44
	}
	return p
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func (p *Progress) SetTotal(files int, bytes int64) {
	p.mu.Lock()
	p.totalFiles = files
	p.totalBytes = bytes
	p.mu.Unlock()
}

func (p *Progress) AddBytes(n int64) {
	p.mu.Lock()
	p.doneBytes += n
	if p.enabled && !p.plain && time.Since(p.last) > 100*time.Millisecond {
		p.last = time.Now()
		p.draw()
	}
	p.mu.Unlock()
}

func (p *Progress) AddFile() {
	p.mu.Lock()
	p.doneFiles++
	if p.enabled {
		p.last = time.Now()
		p.draw()
	}
	p.mu.Unlock()
}

func (p *Progress) Current(name string) {
	p.mu.Lock()
	p.current = name
	p.mu.Unlock()
}

// Active reports whether an animated box is currently drawn on the terminal.
func (p *Progress) Active() bool {
	return p.enabled && !p.plain
}

// Line prints a message just above the box so the bar keeps its place, and is
// a plain line when there is no bar. Use it for everything the tool prints
// while the bar is running, otherwise the box drifts down one line per message.
func (p *Progress) Line(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !p.enabled || p.plain {
		fmt.Fprintln(p.out, msg)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lines > 0 {
		fmt.Fprintf(p.out, "\x1b[%dA", p.lines)
	}
	fmt.Fprintf(p.out, "%s\x1b[K\n", msg)
	lines := p.boxLines()
	p.emit(lines)
	p.lines = len(lines)
}

func (p *Progress) speedMB() float64 {
	elapsed := time.Since(p.start).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(p.doneBytes) / elapsed / 1e6
}

func (p *Progress) eta() time.Duration {
	if p.doneBytes <= 0 || p.totalBytes <= 0 || p.doneBytes >= p.totalBytes {
		return 0
	}
	rate := float64(p.doneBytes) / time.Since(p.start).Seconds()
	if rate <= 0 {
		return 0
	}
	return time.Duration(float64(p.totalBytes-p.doneBytes) / rate * float64(time.Second))
}

// draw must be called with p.mu held. It assumes the cursor sits one line
// below the box that was drawn last time.
func (p *Progress) draw() {
	if p.plain {
		return
	}
	lines := p.boxLines()
	if p.lines > 0 {
		// Move back to the first line of the box and repaint it in place.
		fmt.Fprintf(p.out, "\x1b[%dA", p.lines)
	}
	p.emit(lines)
	p.lines = len(lines)
}

func (p *Progress) emit(lines []string) {
	for _, l := range lines {
		fmt.Fprintf(p.out, "%s\x1b[K\n", l)
	}
}

func (p *Progress) boxLines() []string {
	inner := p.width - 4 // content between the two vertical bars

	frac := 0.0
	if p.totalBytes > 0 {
		frac = float64(p.doneBytes) / float64(p.totalBytes)
	} else if p.totalFiles > 0 {
		frac = float64(p.doneFiles) / float64(p.totalFiles)
	}
	if frac > 1 {
		frac = 1
	}

	pct := fmt.Sprintf("%5.1f%%", frac*100)
	barW := inner - len(pct) - 2
	if barW < 1 {
		barW = 1
	}

	x := frac * float64(barW)
	filled := int(x)
	if filled > barW {
		filled = barW
	}
	head := ""
	empty := barW - filled
	if rem := x - float64(filled); filled < barW && rem > 0.1 {
		if rem < 0.5 {
			head = "▒"
		} else {
			head = "▓"
		}
		empty--
	}

	bar := colFilled + strings.Repeat("█", filled) + colReset
	if head != "" {
		bar += colHead + head + colReset
	}
	if empty > 0 {
		bar += colEmpty + strings.Repeat("░", empty) + colReset
	}

	stats := colCyan + fmt.Sprintf("%d/%d files", p.doneFiles, p.totalFiles) + colReset +
		colDim + " · " + colReset +
		colYellow + fmtBytes(p.doneBytes) + colDim + " / " + colReset + colWhite + fmtBytes(p.totalBytes) + colReset +
		colDim + " · " + colReset +
		colMagenta + fmt.Sprintf("%.1f MB/s", p.speedMB()) + colReset +
		colDim + " · " + colReset +
		colBlue + "ETA " + fmtDuration(p.eta()) + colReset

	file := p.current
	if file == "" {
		file = "-"
	}
	if len(file) > inner-2 {
		file = "…" + file[len(file)-inner+3:]
	}
	fileLine := colDim + "→ " + colReset + file

	top := colFrame + "╭" + strings.Repeat("─", inner+2) + "╮" + colReset
	bottom := colFrame + "╰" + strings.Repeat("─", inner+2) + "╯" + colReset

	lines := []string{
		top,
		" " + bar + " " + colYellow + pct + colReset + " ",
		" " + stats + " ",
		" " + fileLine + " ",
		bottom,
	}
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return lines
}

// Finish repaints the completed bar and leaves the terminal in a clean state.
func (p *Progress) Finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled {
		return
	}
	if p.plain {
		fmt.Fprintf(p.out, "fastcopy: %d/%d files, %s in %s (%.1f MB/s)\n",
			p.doneFiles, p.totalFiles, fmtBytes(p.doneBytes), fmtDuration(time.Since(p.start)), p.speedMB())
		return
	}
	p.draw()
	fmt.Fprintln(p.out)
}
