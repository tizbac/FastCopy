// Package progress draws the ANSI progress box shared by fastcopy and
// fasttar: a bordered frame with a bar, counters, throughput and ETA that
// redraws in place on a terminal and collapses to one summary line elsewhere.
package progress

import (
	"fmt"
	"io"
	"os"
	"strconv"
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
	pulse      int
}

func New(out io.Writer, enabled bool) *Progress {
	p := &Progress{
		out:     out,
		enabled: enabled,
		width:   terminalWidth(1),
		start:   time.Now(),
	}
	if !IsTTY(out) {
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

// IsTTY reports whether w is a character device (a real terminal).
func IsTTY(w io.Writer) bool {
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

// FormatBytes renders a byte count in base 1024 units.
func FormatBytes(n int64) string {
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

// FormatDuration renders a duration as 37s / 4m12s / 1h02m.
func FormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
	if d < time.Hour {
		return strconv.Itoa(int(d.Minutes())) + "m" + strconv.Itoa(int(d.Seconds())%60) + "s"
	}
	return strconv.Itoa(int(d.Hours())) + "h" +
		strconv.Itoa(int(d.Minutes())%60) + "m"
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

// SetBytesDone records an absolute byte counter. Reading modes use it to
// show how much of the archive has been consumed, where AddBytes would
// count the same member twice.
func (p *Progress) SetBytesDone(n int64) {
	p.mu.Lock()
	p.doneBytes = n
	if p.enabled && !p.plain && time.Since(p.last) > 100*time.Millisecond {
		p.last = time.Now()
		p.draw()
	}
	p.mu.Unlock()
}

// Done reports how many members have been processed (used for checkpoints).
func (p *Progress) Done() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.doneFiles
}

// Current records the member being worked on so the box can show it.
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

// unknownTotals reports whether the totals were never set, in which case the
// bar cannot show a percentage and slides a pulse instead.
func (p *Progress) unknownTotals() bool {
	return p.totalFiles <= 0 && p.totalBytes <= 0
}

func (p *Progress) boxLines() []string {
	inner := p.width - 4 // content between the two vertical bars

	unknown := p.unknownTotals()
	frac := 0.0
	if !unknown {
		if p.totalBytes > 0 {
			frac = float64(p.doneBytes) / float64(p.totalBytes)
		} else {
			frac = float64(p.doneFiles) / float64(p.totalFiles)
		}
		if frac > 1 {
			frac = 1
		}
	}

	pct := fmt.Sprintf("%5.1f%%", frac*100)
	if unknown {
		pct = "  ... "
	}
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
	if unknown {
		// No totals: sweep a short glow across the bar so it is still alive.
		p.pulse++
		glow := 6
		pos := (p.pulse / 4) % (barW + glow)
		filled = 0
		empty = 0
		head = ""
		for i := 0; i < barW; i++ {
			if i >= pos-glow && i < pos {
				head += "▒"
			} else {
				empty++
			}
		}
		if head == "" {
			head = "░"
			if empty > 0 {
				empty--
			}
		}
		filled = 0
	} else if rem := x - float64(filled); filled < barW && rem > 0.1 {
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
		colYellow + FormatBytes(p.doneBytes) + colDim + " / " + colReset + colWhite + FormatBytes(p.totalBytes) + colReset +
		colDim + " · " + colReset +
		colMagenta + fmt.Sprintf("%.1f MB/s", p.speedMB()) + colReset +
		colDim + " · " + colReset +
		colBlue + "ETA " + FormatDuration(p.eta()) + colReset
	if unknown || p.totalFiles <= 0 {
		// Extraction only knows the archive size, not the member count.
		bytes := colYellow + FormatBytes(p.doneBytes) + colReset
		eta := "ETA ?"
		if p.totalBytes > 0 {
			bytes += colDim + " / " + colReset + colWhite + FormatBytes(p.totalBytes) + colReset
			eta = "ETA " + FormatDuration(p.eta())
		}
		stats = colCyan + fmt.Sprintf("%d files", p.doneFiles) + colReset +
			colDim + " · " + colReset +
			bytes +
			colDim + " · " + colReset +
			colMagenta + fmt.Sprintf("%.1f MB/s", p.speedMB()) + colReset +
			colDim + " · " + colReset +
			colBlue + eta + colReset
	}

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
// It returns the summary line it printed (empty when there was no bar).
func (p *Progress) Finish() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled {
		return ""
	}
	if p.plain {
		files := fmt.Sprintf("%d/%d files", p.doneFiles, p.totalFiles)
		if p.totalFiles <= 0 {
			files = fmt.Sprintf("%d files", p.doneFiles)
		}
		line := fmt.Sprintf("%s, %s in %s (%.1f MB/s)",
			files, FormatBytes(p.doneBytes),
			FormatDuration(time.Since(p.start)), p.speedMB())
		fmt.Fprintln(p.out, line)
		return line
	}
	p.draw()
	fmt.Fprintln(p.out)
	return ""
}
