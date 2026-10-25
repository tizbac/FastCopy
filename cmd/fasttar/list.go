package main

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tiziano/fastcopy/internal/progress"
)

// listWriter renders -t / -tv output with GNU tar's column layout: the size
// field grows as larger members are seen, right aligned with a minimum width
// of four characters.
type listWriter struct {
	o     *Options
	prog  *progress.Progress
	out   io.Writer
	maxSz int64
	buf   []string
}

func newListWriter(o *Options, prog *progress.Progress) *listWriter {
	w := io.Writer(os.Stdout)
	if o.IndexFile != "" && o.IndexFile != "-" {
		f, err := os.Create(o.IndexFile)
		if err == nil {
			w = f
		}
	}
	return &listWriter{o: o, prog: prog, out: w}
}

func (l *listWriter) write(h *tar.Header, name string, block int64) error {
	if h.Typeflag == tar.TypeXGlobalHeader || h.Typeflag == tar.TypeXHeader {
		return nil
	}
	prefix := ""
	if l.o.BlockNumber {
		prefix = fmt.Sprintf("block %d: ", block)
	}
	display := name
	isDir := h.Typeflag == tar.TypeDir
	if isDir && !strings.HasSuffix(display, "/") {
		display += "/"
	}
	if h.Typeflag == 'V' && l.o.Verbose > 0 {
		// GNU tar tags the volume label record in the listing.
		display += "--Volume Header--"
	}

	display = l.quote(display)
	if l.o.Verbose == 0 {
		_, err := fmt.Fprintln(l.out, prefix+display)
		return err
	}

	if h.Size > l.maxSz {
		l.maxSz = h.Size
	}

	owner := ownerField(l.o, h)
	mode := modeString(h)
	date := timeStamp(l.o, h.ModTime)

	// GNU tar pads the owner/group field so that owner, the separator space and
	// the right aligned size together occupy at least 19 characters, growing the
	// size column as larger members are seen.
	width := len(strconv.FormatInt(l.maxSz, 10))
	if pad := 19 - len(owner) - 1; width < pad {
		width = pad
	}
	if width < 1 {
		width = 1
	}
	line := fmt.Sprintf("%s%s %s %*d %s %s", prefix, mode, owner, width, h.Size, date, display)
	switch h.Typeflag {
	case tar.TypeSymlink:
		line += " -> " + l.quote(h.Linkname)
	case tar.TypeLink:
		line += " link to " + l.quote(h.Linkname)
	}
	_, err := fmt.Fprintln(l.out, line)
	return err
}

func (l *listWriter) close() error {
	if f, ok := l.out.(*os.File); ok && f != os.Stdout {
		return f.Close()
	}
	return nil
}

// ownerField renders user/group, or uid/gid with --numeric-owner.
func ownerField(o *Options, h *tar.Header) string {
	if o.NumericOwner {
		return strconv.Itoa(h.Uid) + "/" + strconv.Itoa(h.Gid)
	}
	u, g := h.Uname, h.Gname
	if u == "" {
		u = strconv.Itoa(h.Uid)
	}
	if g == "" {
		g = strconv.Itoa(h.Gid)
	}
	return u + "/" + g
}

// timeStamp renders the modification time like GNU tar: local by default,
// seconds with --full-time, UTC with --utc.
func timeStamp(o *Options, t time.Time) string {
	if o.UTC {
		t = t.UTC()
	}
	if o.FullTime {
		return t.Format("2006-01-02 15:04:05")
	}
	return t.Format("2006-01-02 15:04")
}

// modeString renders the ten character mode column, including the type letter
// and the hard link 'h' GNU tar uses.
func modeString(h *tar.Header) string {
	var b strings.Builder
	switch h.Typeflag {
	case tar.TypeDir:
		b.WriteByte('d')
	case tar.TypeSymlink:
		b.WriteByte('l')
	case tar.TypeLink:
		b.WriteByte('h')
	case tar.TypeChar:
		b.WriteByte('c')
	case tar.TypeBlock:
		b.WriteByte('b')
	case tar.TypeFifo:
		b.WriteByte('p')
	case 'V':
		b.WriteByte('V')
	default:
		b.WriteByte('-')
	}
	bits := os.FileMode(h.Mode).Perm()
	setuid := h.Mode&04000 != 0
	setgid := h.Mode&02000 != 0
	sticky := h.Mode&01000 != 0

	perm := func(shift uint, execSet bool) {
		if bits&(4<<shift) != 0 {
			b.WriteByte('r')
		} else {
			b.WriteByte('-')
		}
		if bits&(2<<shift) != 0 {
			b.WriteByte('w')
		} else {
			b.WriteByte('-')
		}
		if execSet {
			b.WriteByte('x')
		} else if bits&(1<<shift) != 0 {
			b.WriteByte('x')
		} else {
			b.WriteByte('-')
		}
	}
	perm(6, false)
	if setuid && bits&0100 != 0 {
		b.WriteByte('s')
	} else if setuid {
		b.WriteByte('S')
	}
	perm(3, false)
	if setgid && bits&0010 != 0 {
		b.WriteByte('s')
	} else if setgid {
		b.WriteByte('S')
	}
	perm(0, false)
	if sticky && bits&0001 != 0 {
		b.WriteByte('t')
	} else if sticky {
		b.WriteByte('T')
	}
	return b.String()
}

// quote applies the selected --quoting-style to a displayed name.
func (l *listWriter) quote(s string) string {
	return quoteName(l.o, s)
}

func quoteName(o *Options, s string) string {
	needs := func(c byte) bool {
		if strings.IndexByte(o.NoQuoteChars, c) >= 0 {
			return false
		}
		if strings.IndexByte(o.QuoteChars, c) >= 0 {
			return true
		}
		if c < 0x20 || c >= 0x7f {
			return true
		}
		return strings.ContainsRune("|&;<>()$`\\\"'*?[]#~=% ", rune(c))
	}

	switch o.QuotingStyle {
	case "literal":
		return s
	case "c", "c-maybe":
		if o.QuotingStyle == "c-maybe" {
			// GNU quotes only names with unprintable characters here.
			need := false
			for i := 0; i < len(s) && !need; i++ {
				need = s[i] == '"' || s[i] < 0x20 || s[i] >= 0x7f
			}
			if !need {
				return s
			}
		}
		var b strings.Builder
		b.WriteByte('"')
		for i := 0; i < len(s); i++ {
			c := s[i]
			switch c {
			case '"', '\\':
				b.WriteByte('\\')
				b.WriteByte(c)
			case '\n':
				b.WriteString(`\n`)
			case '\t':
				b.WriteString(`\t`)
			case '\r':
				b.WriteString(`\r`)
			default:
				if c < 0x20 || c >= 0x7f {
					fmt.Fprintf(&b, `\%03o`, c)
				} else {
					b.WriteByte(c)
				}
			}
		}
		b.WriteByte('"')
		return b.String()
	case "shell", "shell-escape":
		needsQuote := false
		for i := 0; i < len(s); i++ {
			if needs(s[i]) {
				needsQuote = true
				break
			}
		}
		if !needsQuote {
			return s
		}
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	case "shell-always", "shell-escape-always":
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	default: // escape, locale, clocale
		return escapeStyle(o, s, localeIsUTF8())
	}
}

// escapeStyle is GNU's default quoting: backslash and control characters get
// their C escapes, unprintable bytes become octal escapes, and printable
// multibyte characters are left alone in a UTF-8 locale.
func escapeStyle(o *Options, s string, utf8locale bool) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x80 && utf8locale {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r != utf8.RuneError || size > 1 {
				if unicode.IsPrint(r) {
					b.WriteString(s[i : i+size])
				} else {
					for j := 0; j < size; j++ {
						fmt.Fprintf(&b, `\%03o`, s[i+j])
					}
				}
				i += size
				continue
			}
			fmt.Fprintf(&b, `\%03o`, c)
			i++
			continue
		}
		i++
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '\a':
			b.WriteString(`\a`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\v':
			b.WriteString(`\v`)
		default:
			if c < 0x20 || c >= 0x7f {
				fmt.Fprintf(&b, `\%03o`, c)
			} else if strings.IndexByte(o.QuoteChars, c) >= 0 && strings.IndexByte(o.NoQuoteChars, c) < 0 {
				b.WriteByte('\\')
				b.WriteByte(c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}

// localeIsUTF8 reports whether the active locale uses UTF-8, which is when
// GNU tar prints multibyte names as they are.
func localeIsUTF8() bool {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(k); v != "" {
			v = strings.ToLower(v)
			return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
		}
	}
	return false
}
