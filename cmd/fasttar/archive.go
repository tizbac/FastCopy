package main

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// recordSize is the byte length of one archive record (blocking factor x 512).
func recordSize(o *Options) int {
	if o.RecordSize > 0 {
		return o.RecordSize
	}
	if o.BlockingFactor > 0 {
		return o.BlockingFactor * 512
	}
	return 20 * 512
}

// goFormat maps --format onto the formats Go's archive/tar can write.
func goFormat(o *Options) tar.Format {
	switch o.Format {
	case "pax", "posix":
		return tar.FormatPAX
	case "ustar", "v7":
		// Go cannot emit a pre-POSIX V7 header; ustar is the closest match and
		// every tar reads it.
		return tar.FormatUSTAR
	default: // gnu, oldgnu
		return tar.FormatGNU
	}
}

// compression signatures fasttar recognises when reading. fasttar never
// compresses or decompresses: it tells the user how to pipe instead.
type compSig struct {
	name   string
	magic  []byte
	progr  string // program that produces it
	offset int
}

var compressionSigs = []compSig{
	{"gzip", []byte{0x1f, 0x8b}, "gzip", 0},
	{"bzip2", []byte("BZh"), "bzip2", 0},
	{"xz", []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}, "xz", 0},
	{"lzma", []byte{0x5d, 0x00, 0x00}, "xz", 0},
	{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd}, "zstd", 0},
	{"lzip", []byte("LZIP"), "lzip", 0},
	{"compress", []byte{0x1f, 0x9d}, "compress", 0},
	{"lz4", []byte{0x04, 0x22, 0x4d, 0x18}, "lz4", 0},
	{"lzo", []byte{0x89, 'L', 'Z', 'O', 0x00, 0x0d, 0x0a, 0x1a, 0x0a}, "lzop", 0},
}

func sniff(head []byte) *compSig {
	for i := range compressionSigs {
		s := &compressionSigs[i]
		if len(head) >= s.offset+len(s.magic) && bytes.Equal(head[s.offset:s.offset+len(s.magic)], s.magic) {
			return s
		}
	}
	return nil
}

// compressOptionError is what the user gets when they ask fasttar to
// compress: we do not implement it, and say exactly how to pipe instead.
func compressOptionError(o *Options) error {
	progr := compressorFor(o)
	name := o.Archive
	if name == "-" {
		name = "archive.tar." + suffixFor(progr)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "fasttar never compresses archives: %s is not built in\n", compressionAskedBy(o))
	fmt.Fprintf(&b, "  pipe the stream through %s yourself:\n", progr)
	fmt.Fprintf(&b, "      fasttar -cf - FILES | %s > %s\n", progr, name)
	fmt.Fprintf(&b, "      fasttar -xf - < %s", name)
	return &comprError{msg: b.String()}
}

// decompressError is what the user gets when they hand fasttar a compressed
// archive: GNU tar auto-detects, we point at the pipe instead.
func decompressError(name, progr string) error {
	src := name
	if src == "-" {
		src = "archive"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s is %s-compressed and fasttar does not decompress\n", src, progr)
	fmt.Fprintf(&b, "  pipe it through %s yourself:\n", progr)
	fmt.Fprintf(&b, "      %s -dc %s | fasttar -tf -", progr, name)
	return &comprError{msg: b.String()}
}

// compressorFor picks the program a compression switch implies; -a uses the
// archive suffix and -I names the program itself.
func compressorFor(o *Options) string {
	if o.CompressProgram != "" {
		return o.CompressProgram
	}
	switch o.CompressSuffix {
	case "bz2":
		return "bzip2"
	case "xz":
		return "xz"
	case "zst":
		return "zstd"
	case "lz":
		return "lzip"
	case "Z":
		return "compress"
	}
	if o.AutoCompress {
		switch strings.TrimSuffix(filepath.Base(o.Archive), ".tar") {
		case ".gz", ".tgz":
			return "gzip"
		case ".bz2", ".tbz2":
			return "bzip2"
		case ".xz", ".txz":
			return "xz"
		case ".zst", ".tzst":
			return "zstd"
		}
	}
	return "gzip"
}

// compressionAskedBy names the switches that requested compression.
func compressionAskedBy(o *Options) string {
	var parts []string
	if o.CompressProgram != "" {
		parts = append(parts, "-I/--use-compress-program")
	}
	if o.AutoCompress {
		parts = append(parts, "-a/--auto-compress")
	}
	if o.CompressSuffix != "" && o.CompressProgram == "" && !o.AutoCompress {
		parts = append(parts, "-z/-j/-J/-Z/--lzma/--lzip/--lzop/--zstd")
	}
	if len(parts) == 0 {
		return "a compression option"
	}
	return strings.Join(parts, ", ")
}

func suffixFor(prog string) string {
	switch prog {
	case "bzip2":
		return "bz2"
	case "xz":
		return "xz"
	case "zstd":
		return "zst"
	case "lzip":
		return "lz"
	case "compress":
		return "Z"
	}
	return "gz"
}

// comprError is returned when the user asked for compression (or handed us a
// compressed archive): we refuse instead of writing a mislabelled file.
type comprError struct {
	msg string
}

func (e *comprError) Error() string { return e.msg }

// openRead opens the archive for a reading mode and rejects compressed input
// with instructions on how to pipe it.
func openRead(o *Options) (io.Reader, func(), int64, error) {
	name := o.Archive
	if name == "-" {
		// Sniff the first bytes of stdin so we can recognise a compressed
		// stream and tell the user how to decompress it.
		br := newPeekReader(os.Stdin)
		head, _ := br.peek(8)
		if s := sniff(head); s != nil {
			return nil, func() {}, 0, decompressError("-", s.progr)
		}
		return br, func() {}, 0, nil
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, func() {}, 0, fmt.Errorf("cannot open %s: %w", name, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, func() {}, 0, err
	}
	head := make([]byte, 8)
	n, _ := f.ReadAt(head, 0)
	if s := sniff(head[:n]); s != nil {
		f.Close()
		return nil, func() {}, 0, decompressError(name, s.progr)
	}
	size := int64(0)
	if st.Mode().IsRegular() {
		size = st.Size()
	}
	return f, func() { f.Close() }, size, nil
}

// openWrite opens the archive for a writing mode. Compression options were
// already rejected in main; what we produce is always a plain tar stream.
func openWrite(o *Options) (*os.File, error) {
	if o.Archive == "-" {
		return os.Stdout, nil
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if o.Mode == modeAppend || o.Mode == modeUpdate || o.Mode == modeCatenate {
		flags = os.O_CREATE | os.O_RDWR
	}
	f, err := os.OpenFile(o.Archive, flags, 0666)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s: %w", o.Archive, err)
	}
	return f, nil
}

// padToRecord writes the terminating zero blocks and pads the archive to a
// whole number of records, which is what GNU tar leaves behind.
func padToRecord(w io.Writer, rec int) error {
	zeros := make([]byte, rec)
	_, err := w.Write(zeros)
	return err
}

// writeEOF appends the end-of-archive marker and record padding.
func writeEOF(w io.Writer, rec int) error {
	if _, err := w.Write(make([]byte, 1024)); err != nil { // two zero blocks
		return err
	}
	pad := rec - 1024%rec
	if pad == rec {
		pad = 0
	}
	if pad > 0 {
		if _, err := w.Write(make([]byte, pad)); err != nil {
			return err
		}
	}
	return nil
}

// archiveEnd scans a raw tar stream and returns the offset just past the last
// member (i.e. where the zero blocks start). Used by -A, -r, -u and --delete.
func archiveEnd(f *os.File, limit int64) (int64, error) {
	if limit < 0 {
		st, err := f.Stat()
		if err != nil {
			return 0, err
		}
		limit = st.Size()
	}
	off := int64(0)
	blk := make([]byte, 512)
	for off+512 <= limit {
		if _, err := f.ReadAt(blk, off); err != nil {
			return 0, err
		}
		if isZeroBlock(blk) {
			return off, nil
		}
		size, err := headerSize(blk)
		if err != nil {
			return 0, fmt.Errorf("invalid tar header at offset %d: %w", off, err)
		}
		off += 512 + (size+511)/512*512
	}
	return off, nil
}

func isZeroBlock(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// headerSize parses the octal size field of a raw header block.
func headerSize(blk []byte) (int64, error) {
	// GNU sparse and pax members store their real size in the header; for
	// skipping we only need the stored size, which is what the field holds.
	fld := bytes.TrimRight(bytes.TrimLeft(blk[124:136], " \x00"), " \x00")
	if len(fld) == 0 {
		return 0, nil
	}
	var n int64
	for _, c := range fld {
		if c < '0' || c > '7' {
			return 0, fmt.Errorf("bad size field %q", string(fld))
		}
		n = n*8 + int64(c-'0')
	}
	return n, nil
}

// rewriteArchive rewrites an existing archive without the members for which
// drop returns true. With a seekable archive it rewrites in place; with -f -
// (as GNU tar's --delete on a stream does) it streams the result to stdout.
func rewriteArchive(o *Options, drop func(name string) (bool, error)) error {
	if o.Archive == "-" {
		r := tar.NewReader(os.Stdin)
		w := tar.NewWriter(os.Stdout)
		if err := copyFiltered(r, w, o, drop); err != nil {
			return err
		}
		return w.Close()
	}

	src, err := os.Open(o.Archive)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", o.Archive, err)
	}
	defer src.Close()

	tmp := o.Archive + ".fasttar.tmp"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			dst.Close()
			os.Remove(tmp)
		}
	}()

	w := tar.NewWriter(dst)
	r := tar.NewReader(src)
	if err := copyFiltered(r, w, o, drop); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, o.Archive); err != nil {
		return err
	}
	ok = true
	return nil
}

// copyFiltered writes every member r yields that drop() keeps.
func copyFiltered(r *tar.Reader, w *tar.Writer, o *Options, drop func(name string) (bool, error)) error {
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("cannot read %s: %w", o.Archive, err)
		}
		dropIt, err := drop(h.Name)
		if err != nil {
			return err
		}
		if dropIt {
			continue
		}
		h2 := *h
		h2.Format = goFormat(o)
		if err := w.WriteHeader(&h2); err != nil {
			return err
		}
		if !isHeaderOnly(h2.Typeflag) {
			if _, err := io.Copy(w, r); err != nil {
				return err
			}
		}
	}
	return nil
}

func isHeaderOnly(t byte) bool {
	switch t {
	case tar.TypeDir, tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return true
	}
	return false
}

// peekReader lets us sniff stdin without losing the bytes we already read.
type peekReader struct {
	r    io.Reader
	buf  []byte
	pos  int
	off  int64
	size int64
}

func newPeekReader(r io.Reader) *peekReader { return &peekReader{r: r} }

func (p *peekReader) peek(n int) ([]byte, error) {
	for len(p.buf) < n {
		chunk := make([]byte, 4096)
		k, err := p.r.Read(chunk)
		if k > 0 {
			p.buf = append(p.buf, chunk[:k]...)
		}
		if err != nil {
			if len(p.buf) >= n {
				break
			}
			return p.buf, err
		}
	}
	if len(p.buf) > n {
		return p.buf[:n], nil
	}
	return p.buf, nil
}

func (p *peekReader) Read(b []byte) (int, error) {
	if p.pos < len(p.buf) {
		n := copy(b, p.buf[p.pos:])
		p.pos += n
		return n, nil
	}
	return p.r.Read(b)
}

// absPath turns an archive name into the path where it is extracted.
func outputPath(base, name string) string {
	if strings.HasPrefix(name, "/") {
		return name
	}
	if base == "" {
		return name
	}
	return filepath.Join(base, name)
}
