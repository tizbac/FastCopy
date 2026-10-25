package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// opMode is the single main operation GNU tar allows per invocation.
type opMode int

const (
	modeNone opMode = iota
	modeCatenate
	modeCreate
	modeDelete
	modeDiff
	modeAppend
	modeTestLabel
	modeList
	modeUpdate
	modeExtract
)

func (m opMode) String() string {
	switch m {
	case modeCatenate:
		return "-A"
	case modeCreate:
		return "-c"
	case modeDelete:
		return "--delete"
	case modeDiff:
		return "-d"
	case modeAppend:
		return "-r"
	case modeTestLabel:
		return "--test-label"
	case modeList:
		return "-t"
	case modeUpdate:
		return "-u"
	case modeExtract:
		return "-x"
	}
	return "(none)"
}

// reading reports whether the mode consumes an existing archive.
func (m opMode) reading() bool {
	switch m {
	case modeList, modeExtract, modeDiff, modeTestLabel, modeDelete:
		return true
	}
	return false
}

// cmdKind is one entry of the ordered file-name list that GNU tar builds from
// the command line: plain names, -C, -T and --add-file are interleaved, so
// each name remembers the directory that was in effect when it was seen.
type cmdKind int

const (
	cmdName cmdKind = iota
	cmdDir
	cmdFilesFrom
	cmdAddFile
)

type cmd struct {
	kind  cmdKind
	value string
	base  string // directory context at this point (-C accumulates)
}

// Options holds every switch GNU tar 1.35 accepts. The parser never rejects an
// option that GNU tar documents; switches fasttar cannot honour either take a
// best-effort default (see README) or are reported through notImplemented().
type Options struct {
	Mode      opMode
	ModeLabel string // which switch selected Mode, for error messages

	Archive string // -f, "-" means stdin/stdout

	// ordered file list
	Cmds []cmd
	base string // accumulated -C directory

	// Local file name selection
	Excludes             []string
	ExcludeFrom          []string
	ExcludeBackups       bool
	ExcludeCaches        bool
	ExcludeCachesAll     bool
	ExcludeCachesUnder   bool
	ExcludeIgnore        []string
	ExcludeIgnoreRecursi []string
	ExcludeTag           []string
	ExcludeTagAll        []string
	ExcludeTagUnder      []string
	ExcludeVCS           bool
	ExcludeVCSIgnores    bool
	Null                 bool
	Recursion            bool // default true
	Unquote              bool // default true
	VerbatimFilesFrom    bool

	// File name matching (defaults differ between excludes and member args)
	Anchored            bool
	IgnoreCase          bool
	Wildcards           bool // --wildcards: member arguments become patterns
	NoWildcards         bool // --no-wildcards: exclusion patterns become literal
	WildcardsMatchSlash bool

	// Operation modifiers
	CheckDevice    bool
	ListedIncrFile string
	Incremental    bool
	HoleDetection  string
	IgnoreFailRead bool
	Level          int
	NoSeek         bool
	Occurrence     int // 0 = every occurrence
	SparseVersion  string
	Sparse         bool

	// Overwrite control
	KeepDirSymlink  bool
	KeepNewerFiles  bool
	KeepOldFiles    bool
	OneTopLevel     string
	OneTopLevelSet  bool
	Overwrite       bool
	OverwriteDir    bool // default true
	RecursiveUnlink bool
	RemoveFiles     bool
	SkipOldFiles    bool
	UnlinkFirst     bool
	Verify          bool

	// Select output stream
	IgnoreCmdError bool
	ToStdout       bool
	ToCommand      string

	// Handling of file attributes
	AtimePreserve   string // "", "replace", "system"
	ClampMtime      bool
	DelayDirRestore bool
	Group           string
	GroupMap        string
	ModeChange      string
	Mtime           string
	Touch           bool
	SameOwner       bool // default true
	SamePermissions bool // default: root only
	NumericOwner    bool
	Owner           string
	OwnerMap        string
	Sort            string
	SameOrder       bool
	OldFiles        string // last --overwrite/--keep-old-files/... given

	// Extended attributes
	ACLs          bool
	Selinux       bool
	Xattrs        bool
	XattrsExclude []string
	XattrsInclude []string
	XattrsChanged bool

	// Device selection
	ForceLocal   bool
	InfoScript   string
	TapeLength   int
	MultiVolume  bool
	RmtCommand   string
	RshCommand   string
	VolnoFile    string
	ArchiveNames int

	// Device blocking
	BlockingFactor  int // default 20
	RecordSize      int
	IgnoreZeros     bool
	ReadFullRecords bool

	// Archive format
	Format         string
	FormatExplicit bool // --format/-H given: GNU tar's DEFAULT_FORMAT sentinel
	PaxOptions     string
	Label          string

	// Compression (accepted, reported: fasttar never compresses itself)
	AutoCompress     bool
	CompressProgram  string
	CompressSuffix   string // gzip/bzip2/xz/... requested via a flag
	CompressionAsked bool

	// Local file selection
	Backup          string // "" = no backups, otherwise control name
	HardDereference bool
	Dereference     bool
	StartingFile    string
	Newer           string
	NewerMtime      string
	ThresholdDate   bool // GNU tar allows only one threshold date
	ThresholdTime   time.Time
	ThresholdOK     bool
	ThresholdSub    bool // the threshold came from --newer-mtime
	MtimeTime       time.Time
	MtimeOK         bool
	TextualDates    []textualDate
	OneFileSystem   bool
	AbsoluteNames   bool
	Suffix          string

	// File name transformations
	StripComponents int
	Transform       []string

	// Informative output
	Checkpoint       int
	CheckpointAction []string
	FullTime         bool
	IndexFile        string
	CheckLinks       bool
	NoQuoteChars     string
	QuoteChars       string
	QuotingStyle     string
	BlockNumber      bool
	ShowDefaults     bool
	ShowOmittedDirs  bool
	ShowSnapRanges   bool
	ShowTransformed  bool
	Totals           bool
	TotalsSignal     string
	UTC              bool
	Verbose          int
	Warnings         []string
	Interactive      bool

	// Compatibility / other
	Restrict bool
	Help     bool
	Usage    bool
	Version  bool

	// options fasttar accepts but cannot honour, reported once at startup
	Warned []string

	// fasttar extras
	Progress bool
	Fast     bool
	Debug    bool
	Buffer   int
}

// defaults fills in the GNU tar defaults (--show-defaults documents most).
func defaults() *Options {
	return &Options{
		Archive:             "-",
		Recursion:           true,
		Unquote:             true,
		CheckDevice:         true,
		OverwriteDir:        true,
		SameOwner:           os.Geteuid() == 0,
		SamePermissions:     os.Geteuid() == 0,
		WildcardsMatchSlash: true,
		Format:              "gnu",
		BlockingFactor:      20,
		QuotingStyle:        "escape",
		HoleDetection:       "raw",
		Backup:              "",
	}
}

// note records a switch that is accepted but does not influence fasttar's
// output, so the user is never left believing it had an effect.
func (o *Options) note(msg string) {
	for _, w := range o.Warned {
		if w == msg {
			return
		}
	}
	o.Warned = append(o.Warned, msg)
}

// setMode enforces "exactly one operation mode". Repeating the same mode is
// harmless, exactly as in GNU tar.
func (o *Options) setMode(m opMode, sw string) error {
	if o.Mode != modeNone && o.Mode != m {
		return &usageError{msg: "You may not specify more than one '-Acdtrux', '--delete' or  '--test-label' option"}
	}
	o.Mode = m
	o.ModeLabel = sw
	return nil
}

// setOldFiles records one of GNU tar's mutually exclusive "what to do with
// existing files" switches and reports the conflict GNU reports for a second,
// different one.
func (o *Options) setOldFiles(name string) error {
	if o.OldFiles != "" && o.OldFiles != name {
		return &usageError{msg: fmt.Sprintf("'%s' cannot be used with '%s'", name, o.OldFiles)}
	}
	o.OldFiles = name
	return nil
}

type parser struct {
	o   *Options
	ind int // index in argv
	av  []string
}

// nextArg returns the mandatory argument of the current option.
func (p *parser) nextArg(name string) (string, error) {
	if p.ind+1 >= len(p.av) {
		if strings.HasPrefix(name, "--") {
			return "", fmt.Errorf("option '%s' requires an argument", name)
		}
		return "", fmt.Errorf("option requires an argument -- '%s'", strings.TrimLeft(name, "-"))
	}
	p.ind++
	return p.av[p.ind], nil
}

// intArg parses a numeric option value and reports the failure with the exact
// wording GNU tar uses for that particular switch.
func (p *parser) intArg(name, s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		switch name {
		case "-b", "--blocking-factor":
			return 0, &usageError{msg: fmt.Sprintf("%s: Invalid blocking factor", s)}
		case "--record-size":
			return 0, &usageError{msg: fmt.Sprintf("%s: Invalid record size", s)}
		case "-L", "--tape-length":
			return 0, &usageError{msg: fmt.Sprintf("%s: Invalid tape length", s)}
		case "--checkpoint":
			return 0, &fatalError{msg: fmt.Sprintf("%s value is not an integer", name)}
		case "--occurrence":
			return 0, &fatalError{msg: fmt.Sprintf("%s: Invalid number", s)}
		default:
			return 0, &usageError{msg: fmt.Sprintf("%s: Invalid number of elements", s)}
		}
	}
	switch name {
	case "-b", "--blocking-factor":
		if n == 0 {
			return 0, &usageError{msg: fmt.Sprintf("%s: Invalid blocking factor", s)}
		}
	case "--record-size":
		if n == 0 {
			return 0, &fatalError{msg: "Invalid value for record_size"}
		}
		if n%512 != 0 {
			return 0, &usageError{msg: "Record size must be a multiple of 512."}
		}
	}
	return n, nil
}

// parse walks argv exactly like GNU tar: options are recognised anywhere,
// operands collect in order, "--" ends option processing.
func parseArgs(argv []string) (*Options, error) {
	o := defaults()
	p := &parser{o: o, av: argv}
	endOfOpts := false

	for p.ind = 0; p.ind < len(p.av); p.ind++ {
		arg := p.av[p.ind]

		if endOfOpts || arg == "-" || !strings.HasPrefix(arg, "-") {
			o.Cmds = append(o.Cmds, cmd{kind: cmdName, value: arg, base: o.base})
			continue
		}
		if arg == "--" {
			endOfOpts = true
			continue
		}

		var err error
		if strings.HasPrefix(arg, "--") {
			err = p.long(arg)
		} else {
			err = p.shorts(arg)
		}
		if err != nil {
			var ue *usageError
			var fe *fatalError
			var un *unsupportedError
			var ar *argRangeError
			if errors.As(err, &ue) || errors.As(err, &fe) || errors.As(err, &un) || errors.As(err, &ar) {
				return nil, err
			}
			// Anything else is an argp-style rejection: GNU tar exits 64.
			return nil, &usageError{msg: err.Error(), code: 64}
		}
	}

	if o.Help || o.Usage || o.Version || o.ShowDefaults {
		return o, nil
	}
	if o.Mode == modeNone {
		return nil, &usageError{msg: "You must specify one of the '-Acdtrux', '--delete' or '--test-label' options"}
	}
	if err := o.checkConstraints(); err != nil {
		return nil, err
	}
	return o, nil
}

// hasNameArgs reports whether the command line supplied at least one member
// name (or -T/-I list) to work on.
func (o *Options) hasNameArgs() bool {
	for _, c := range o.Cmds {
		if c.kind != cmdDir {
			return true
		}
	}
	return false
}

// subcommandLabel returns GNU tar's canonical name for the operation mode, as
// used in its "'--x' cannot be used with '-c'" conflict messages.
func subcommandLabel(m opMode) string {
	switch m {
	case modeAppend:
		return "-r"
	case modeCatenate:
		return "-A"
	case modeCreate:
		return "-c"
	case modeDelete:
		return "-D"
	case modeDiff:
		return "-d"
	case modeExtract:
		return "-x"
	case modeList:
		return "-t"
	case modeUpdate:
		return "-u"
	case modeTestLabel:
		return "--test-label"
	}
	return ""
}

// readsArchive reports the modes that read from an archive (-d, -x, -t).
func (o *Options) readsArchive() bool {
	return o.Mode == modeDiff || o.Mode == modeExtract || o.Mode == modeList
}

// writesArchive reports the modes that write to an archive.
func (o *Options) writesArchive() bool {
	return o.Mode == modeAppend || o.Mode == modeCatenate || o.Mode == modeCreate ||
		o.Mode == modeDelete || o.Mode == modeUpdate
}

// checkConstraints reports combinations GNU tar refuses up front. Every
// message and exit status below follows GNU tar 1.35.
func (o *Options) checkConstraints() error {
	sub := subcommandLabel(o.Mode)

	if o.Occurrence > 0 {
		if !o.hasNameArgs() {
			return &usageError{msg: "--occurrence is meaningless without a file list"}
		}
		if o.Mode != modeDelete && !o.readsArchive() {
			return &usageError{msg: fmt.Sprintf("'--occurrence' cannot be used with '%s'", sub)}
		}
	}

	if o.ClampMtime && o.Mtime == "" {
		return &usageError{msg: "--clamp-mtime needs a date specified using --mtime"}
	}

	if o.ArchiveNames > 1 && !o.MultiVolume {
		return &usageError{msg: "Multiple archive files require '-M' option"}
	}

	if o.StartingFile != "" && !o.readsArchive() {
		return &usageError{msg: fmt.Sprintf("'--starting-file' cannot be used with '%s'", sub)}
	}
	if o.SameOrder && !o.readsArchive() {
		return &usageError{msg: fmt.Sprintf("'--same-order' cannot be used with '%s'", sub)}
	}
	if o.Verify && !o.writesArchive() {
		return &usageError{msg: fmt.Sprintf("'--verify' cannot be used with '%s'", sub)}
	}
	if o.Verify && o.MultiVolume {
		return &usageError{msg: "Cannot verify multi-volume archives"}
	}
	if o.Verify && o.CompressionAsked {
		return &usageError{msg: "Cannot verify compressed archives"}
	}
	if o.CompressionAsked && o.MultiVolume {
		return &usageError{msg: "Cannot use multi-volume compressed archives"}
	}
	if o.CompressionAsked && o.Mode == modeUpdate {
		return &usageError{msg: "Cannot update compressed archives"}
	}
	if o.CompressionAsked && o.Mode == modeCatenate {
		return &usageError{msg: "Cannot concatenate compressed archives"}
	}
	if o.OneTopLevelSet && o.AbsoluteNames {
		return &usageError{msg: "'--one-top-level' cannot be used with '--absolute-names'"}
	}

	// GNU-only features need a format that can carry them.
	if (o.Label != "" && o.Mode == modeCreate) || o.Incremental || o.MultiVolume || o.Sparse {
		if o.Format == "v7" || o.Format == "ustar" {
			return &usageError{msg: "GNU features wanted on incompatible archive format"}
		}
	}
	if o.PaxOptions != "" && o.Format != "pax" && !o.readsArchive() {
		return &usageError{msg: "--pax-option can be used only on POSIX archives"}
	}
	if o.ACLs && o.Format != "pax" && !o.readsArchive() {
		return &usageError{msg: "--acls can be used only on POSIX archives"}
	}
	if o.Selinux && o.Format != "pax" && !o.readsArchive() {
		return &usageError{msg: "--selinux can be used only on POSIX archives"}
	}
	if o.Xattrs && o.Format != "pax" && !o.readsArchive() {
		return &usageError{msg: "--xattrs can be used only on POSIX archives"}
	}
	return nil
}

// splitLong breaks "--name[=value]".
func splitLong(arg string) (name, value string, hasValue bool) {
	body := arg[2:]
	if i := strings.IndexByte(body, '='); i >= 0 {
		return body[:i], body[i+1:], true
	}
	return body, "", false
}

func (p *parser) long(arg string) error {
	o := p.o
	name, val, hasVal := splitLong(arg)

	// Options taking a value may also receive it as the next argument.
	need := func() (string, error) {
		if hasVal {
			return val, nil
		}
		return p.nextArg("--" + name)
	}
	needInt := func() (int, error) {
		v, err := need()
		if err != nil {
			return 0, err
		}
		return p.intArg("--"+name, v)
	}
	optInt := func(def int) (int, error) {
		if !hasVal {
			return def, nil
		}
		return p.intArg("--"+name, val)
	}

	switch name {
	// ---- main operation modes ----
	case "catenate", "concatenate":
		return o.setMode(modeCatenate, "--"+name)
	case "create":
		return o.setMode(modeCreate, "--create")
	case "delete":
		return o.setMode(modeDelete, "--delete")
	case "diff", "compare":
		return o.setMode(modeDiff, "--"+name)
	case "append":
		return o.setMode(modeAppend, "--append")
	case "test-label":
		return o.setMode(modeTestLabel, "--test-label")
	case "list":
		return o.setMode(modeList, "--list")
	case "update":
		return o.setMode(modeUpdate, "--update")
	case "extract", "get":
		return o.setMode(modeExtract, "--"+name)

	// ---- operation modifiers ----
	case "check-device":
		o.CheckDevice = true
	case "no-check-device":
		o.CheckDevice = false
	case "listed-incremental":
		v, err := need()
		if err != nil {
			return err
		}
		o.ListedIncrFile = v
		o.Incremental = true
		return notImplemented("--listed-incremental (incremental backup)", "--full archive; use GNU tar for dump-list backups")
	case "incremental":
		// GNU tar's -G only means something together with --listed-incremental;
		// without a snapshot file every member is "new", which is what fasttar
		// writes anyway.
		o.Incremental = true
	case "hole-detection":
		v, err := need()
		if err != nil {
			return err
		}
		if err := restrictedArg("--hole-detection", v, []string{"raw", "seek"}); err != nil {
			return err
		}
		o.HoleDetection = v
	case "ignore-failed-read":
		o.IgnoreFailRead = true
	case "level":
		if !hasVal {
			return &usageError{msg: "Invalid incremental level value"}
		}
		n, err := strconv.Atoi(val)
		if err != nil {
			return &usageError{msg: "Invalid incremental level value"}
		}
		o.Level = n
		// GNU tar starts the level at -1 and warns only when a different
		// value was actually given.
		if n != -1 {
			warnf("--level is meaningless without --listed-incremental")
		}
	case "no-seek":
		o.NoSeek = true
	case "seek":
		if hasVal {
			return fmt.Errorf("option '--seek' doesn't allow an argument")
		}
		o.NoSeek = false
	case "occurrence":
		n, err := optInt(1)
		if err != nil {
			return err
		}
		o.Occurrence = n
	case "sparse-version":
		v, err := need()
		if err != nil {
			return err
		}
		if !isSparseVersion(v) {
			return &usageError{msg: "Invalid sparse version value"}
		}
		o.SparseVersion = v
		o.Sparse = true
	case "sparse":
		if hasVal {
			return fmt.Errorf("option '--sparse' doesn't allow an argument")
		}
		o.Sparse = true

	// ---- local file name selection ----
	case "add-file":
		v, err := need()
		if err != nil {
			return err
		}
		o.Cmds = append(o.Cmds, cmd{kind: cmdAddFile, value: v, base: o.base})
	case "directory":
		v, err := need()
		if err != nil {
			return err
		}
		o.Cmds = append(o.Cmds, cmd{kind: cmdDir, value: v, base: o.base})
		o.base = joinBase(o.base, v)
	case "exclude":
		v, err := need()
		if err != nil {
			return err
		}
		o.Excludes = append(o.Excludes, v)
	case "exclude-backups":
		o.ExcludeBackups = true
	case "exclude-caches":
		o.ExcludeCaches = true
	case "exclude-caches-all":
		o.ExcludeCachesAll = true
	case "exclude-caches-under":
		o.ExcludeCachesUnder = true
	case "exclude-ignore":
		v, err := need()
		if err != nil {
			return err
		}
		o.ExcludeIgnore = append(o.ExcludeIgnore, v)
	case "exclude-ignore-recursive":
		v, err := need()
		if err != nil {
			return err
		}
		o.ExcludeIgnoreRecursi = append(o.ExcludeIgnoreRecursi, v)
	case "exclude-tag":
		v, err := need()
		if err != nil {
			return err
		}
		o.ExcludeTag = append(o.ExcludeTag, v)
	case "exclude-tag-all":
		v, err := need()
		if err != nil {
			return err
		}
		o.ExcludeTagAll = append(o.ExcludeTagAll, v)
	case "exclude-tag-under":
		v, err := need()
		if err != nil {
			return err
		}
		o.ExcludeTagUnder = append(o.ExcludeTagUnder, v)
	case "exclude-vcs":
		o.ExcludeVCS = true
	case "exclude-vcs-ignores":
		o.ExcludeVCSIgnores = true
	case "null":
		o.Null = true
		o.VerbatimFilesFrom = true
	case "no-null":
		o.Null = false
		o.VerbatimFilesFrom = false
	case "recursion":
		o.Recursion = true
	case "no-recursion":
		o.Recursion = false
	case "unquote":
		o.Unquote = true
	case "no-unquote":
		o.Unquote = false
	case "verbatim-files-from":
		o.VerbatimFilesFrom = true
	case "no-verbatim-files-from":
		o.VerbatimFilesFrom = false
	case "files-from":
		v, err := need()
		if err != nil {
			return err
		}
		o.Cmds = append(o.Cmds, cmd{kind: cmdFilesFrom, value: v, base: o.base})
	case "exclude-from":
		v, err := need()
		if err != nil {
			return err
		}
		if err := readable(v); err != nil {
			return err
		}
		o.ExcludeFrom = append(o.ExcludeFrom, v)

	// ---- file name matching ----
	case "anchored":
		o.Anchored = true
	case "no-anchored":
		o.Anchored = false
	case "ignore-case":
		o.IgnoreCase = true
	case "no-ignore-case":
		o.IgnoreCase = false
	case "wildcards":
		o.Wildcards = true
		o.NoWildcards = false
	case "no-wildcards":
		o.Wildcards = false
		o.NoWildcards = true
	case "wildcards-match-slash":
		o.WildcardsMatchSlash = true
	case "no-wildcards-match-slash":
		o.WildcardsMatchSlash = false

	// ---- overwrite control ----
	// GNU tar keeps one "old files" behaviour: any two different ones conflict.
	case "keep-directory-symlink":
		o.KeepDirSymlink = true
	case "keep-newer-files":
		if err := o.setOldFiles("--keep-newer-files"); err != nil {
			return err
		}
		o.KeepNewerFiles = true
	case "keep-old-files":
		if err := o.setOldFiles("--keep-old-files"); err != nil {
			return err
		}
		o.KeepOldFiles = true
	case "one-top-level":
		o.OneTopLevelSet = true
		if hasVal {
			o.OneTopLevel = val
		}
	case "overwrite":
		if err := o.setOldFiles("--overwrite"); err != nil {
			return err
		}
		o.Overwrite = true
	case "overwrite-dir":
		if err := o.setOldFiles("--overwrite-dir"); err != nil {
			return err
		}
		o.OverwriteDir = true
	case "no-overwrite-dir":
		if err := o.setOldFiles("--no-overwrite-dir"); err != nil {
			return err
		}
		o.OverwriteDir = false
	case "recursive-unlink":
		o.RecursiveUnlink = true
	case "remove-files":
		o.RemoveFiles = true
	case "skip-old-files":
		if err := o.setOldFiles("--skip-old-files"); err != nil {
			return err
		}
		o.SkipOldFiles = true
	case "unlink-first":
		if err := o.setOldFiles("--unlink-first"); err != nil {
			return err
		}
		o.UnlinkFirst = true
	case "verify":
		o.Verify = true

	// ---- select output stream ----
	case "ignore-command-error":
		o.IgnoreCmdError = true
	case "no-ignore-command-error":
		o.IgnoreCmdError = false
	case "to-stdout":
		o.ToStdout = true
	case "to-command":
		v, err := need()
		if err != nil {
			return err
		}
		if o.ToCommand != "" {
			return &usageError{msg: "Only one --to-command option allowed"}
		}
		o.ToCommand = v

	// ---- file attributes ----
	case "atime-preserve":
		m := "replace"
		if hasVal {
			m = val
		}
		if m != "replace" && m != "system" {
			return restrictedArg("--atime-preserve", val, []string{"replace", "system"})
		}
		o.AtimePreserve = m
	case "clamp-mtime":
		o.ClampMtime = true
	case "delay-directory-restore":
		o.DelayDirRestore = true
	case "no-delay-directory-restore":
		o.DelayDirRestore = false
	case "group":
		v, err := need()
		if err != nil {
			return err
		}
		o.Group = v
	case "group-map":
		v, err := need()
		if err != nil {
			return err
		}
		o.GroupMap = v
		o.note("--group-map is ignored: fasttar has no incremental snapshot to map against")
	case "mode":
		v, err := need()
		if err != nil {
			return err
		}
		if _, err := applySymbolicMode(0777, v, false); err != nil {
			return &fatalError{msg: "Invalid mode given on option"}
		}
		o.ModeChange = v
	case "mtime":
		v, err := need()
		if err != nil {
			return err
		}
		o.Mtime = v
		o.MtimeTime, o.MtimeOK = p.dateOption("--mtime", v)
	case "touch":
		o.Touch = true
	case "same-owner":
		o.SameOwner = true
	case "no-same-owner":
		o.SameOwner = false
	case "same-permissions", "preserve-permissions":
		o.SamePermissions = true
	case "no-same-permissions":
		o.SamePermissions = false
	case "numeric-owner":
		o.NumericOwner = true
	case "owner":
		v, err := need()
		if err != nil {
			return err
		}
		o.Owner = v
	case "owner-map":
		v, err := need()
		if err != nil {
			return err
		}
		o.OwnerMap = v
		o.note("--owner-map is ignored: fasttar has no incremental snapshot to map against")
	case "sort":
		v, err := need()
		if err != nil {
			return err
		}
		if err := restrictedArg("--sort", v, []string{"none", "name", "inode"}); err != nil {
			return err
		}
		o.Sort = v
	case "preserve-order", "same-order":
		o.SameOrder = true

	// ---- extended attributes ----
	// GNU tar forces a POSIX (pax) archive as soon as these are requested.
	case "acls":
		o.ACLs = true
		o.Xattrs = true
		o.XattrsChanged = true
		o.Format = "pax"
		o.note("--acls is accepted but extended attributes are not archived")
	case "no-acls":
		o.ACLs = false
		o.XattrsChanged = true
	case "selinux":
		o.Selinux = true
		o.Xattrs = true
		o.XattrsChanged = true
		o.Format = "pax"
		o.note("--selinux is accepted but extended attributes are not archived")
	case "no-selinux":
		o.Selinux = false
		o.XattrsChanged = true
	case "xattrs":
		o.Xattrs = true
		o.XattrsChanged = true
		o.Format = "pax"
		o.note("--xattrs is accepted but extended attributes are not archived")
	case "no-xattrs":
		o.Xattrs = false
		o.XattrsChanged = true
	case "xattrs-exclude":
		v, err := need()
		if err != nil {
			return err
		}
		o.XattrsExclude = append(o.XattrsExclude, v)
	case "xattrs-include":
		v, err := need()
		if err != nil {
			return err
		}
		o.XattrsInclude = append(o.XattrsInclude, v)

	// ---- device selection ----
	case "force-local":
		o.ForceLocal = true
	case "file":
		v, err := need()
		if err != nil {
			return err
		}
		o.Archive = v
		o.ArchiveNames++
	case "info-script", "new-volume-script":
		v, err := need()
		if err != nil {
			return err
		}
		o.InfoScript = v
		o.MultiVolume = true
		o.note("--" + name + " is ignored: fasttar always writes a single volume")
	case "tape-length":
		n, err := needInt()
		if err != nil {
			return err
		}
		o.TapeLength = n
		o.note("--tape-length is ignored: fasttar always writes a single volume")
	case "multi-volume":
		o.MultiVolume = true
		o.note("--multi-volume is ignored: fasttar always writes a single volume")
	case "rmt-command":
		v, err := need()
		if err != nil {
			return err
		}
		o.RmtCommand = v
		o.note("--rmt-command is ignored: remote devices are not supported")
	case "rsh-command":
		v, err := need()
		if err != nil {
			return err
		}
		o.RshCommand = v
		o.note("--rsh-command is ignored: remote archives are not supported")
	case "volno-file":
		v, err := need()
		if err != nil {
			return err
		}
		o.VolnoFile = v
		o.note("--volno-file is ignored: fasttar writes a single volume")

	// ---- device blocking ----
	case "blocking-factor":
		n, err := needInt()
		if err != nil {
			return err
		}
		o.BlockingFactor = n
	case "read-full-records":
		o.ReadFullRecords = true
	case "ignore-zeros":
		o.IgnoreZeros = true
	case "record-size":
		n, err := needInt()
		if err != nil {
			return err
		}
		o.RecordSize = n
		o.BlockingFactor = n / 512

	// ---- archive format ----
	case "format":
		v, err := need()
		if err != nil {
			return err
		}
		switch v {
		case "gnu", "oldgnu", "pax", "posix", "ustar", "v7":
			o.Format = v
			o.FormatExplicit = true
		default:
			return &usageError{msg: fmt.Sprintf("%s: Invalid archive format", v)}
		}
	case "old-archive", "portability":
		o.Format = "v7"
	case "posix":
		o.Format = "pax"
	case "pax-option":
		v, err := need()
		if err != nil {
			return err
		}
		if err := checkPaxOptions(v); err != nil {
			return err
		}
		if !o.FormatExplicit {
			o.Format = "pax"
		}
		o.PaxOptions = v
	case "label":
		v, err := need()
		if err != nil {
			return err
		}
		o.Label = v

	// ---- compression: accepted, never performed ----
	case "auto-compress":
		o.AutoCompress = true
	case "no-auto-compress":
		o.AutoCompress = false
	case "use-compress-program":
		v, err := need()
		if err != nil {
			return err
		}
		o.CompressProgram = v
		o.CompressionAsked = true
	case "bzip2":
		o.CompressSuffix = "bz2"
		o.CompressionAsked = true
	case "xz":
		o.CompressSuffix = "xz"
		o.CompressionAsked = true
	case "lzip":
		o.CompressSuffix = "lz"
		o.CompressionAsked = true
	case "lzma":
		o.CompressSuffix = "xz"
		o.CompressionAsked = true
	case "lzop":
		o.CompressSuffix = "lzo"
		o.CompressionAsked = true
	case "zstd":
		o.CompressSuffix = "zst"
		o.CompressionAsked = true
	case "gzip", "gunzip", "ungzip":
		o.CompressSuffix = "gz"
		o.CompressionAsked = true
	case "compress", "uncompress":
		o.CompressSuffix = "Z"
		o.CompressionAsked = true

	// ---- local file selection ----
	case "backup":
		ctrl := ""
		if hasVal {
			ctrl = val
		}
		c, err := backupControl(ctrl)
		if err != nil {
			return err
		}
		o.Backup = c
	case "hard-dereference":
		o.HardDereference = true
	case "dereference":
		o.Dereference = true
	case "starting-file":
		v, err := need()
		if err != nil {
			return err
		}
		o.StartingFile = v
	case "newer-mtime":
		v, err := need()
		if err != nil {
			return err
		}
		if o.ThresholdDate {
			return &usageError{msg: "More than one threshold date"}
		}
		o.ThresholdDate = true
		o.NewerMtime = v
		o.ThresholdTime, o.ThresholdOK = p.dateOption("--newer-mtime", v)
		o.ThresholdSub = true
	case "newer", "after-date":
		v, err := need()
		if err != nil {
			return err
		}
		if o.ThresholdDate {
			return &usageError{msg: "More than one threshold date"}
		}
		o.ThresholdDate = true
		o.Newer = v
		o.ThresholdTime, o.ThresholdOK = p.dateOption("--after-date", v)
	case "one-file-system":
		o.OneFileSystem = true
	case "absolute-names":
		o.AbsoluteNames = true
	case "suffix":
		v, err := need()
		if err != nil {
			return err
		}
		o.Suffix = v

	// ---- transformations ----
	case "strip-components":
		n, err := needInt()
		if err != nil {
			return err
		}
		o.StripComponents = n
	case "transform", "xform":
		v, err := need()
		if err != nil {
			return err
		}
		if _, err := parseTransform(v); err != nil {
			return err
		}
		o.Transform = append(o.Transform, v)

	// ---- informative output ----
	case "checkpoint":
		n, err := optInt(10)
		if err != nil {
			return err
		}
		o.Checkpoint = n
	case "checkpoint-action":
		v, err := need()
		if err != nil {
			return err
		}
		if err := checkCheckpointAction(v); err != nil {
			return err
		}
		o.CheckpointAction = append(o.CheckpointAction, v)
	case "full-time":
		o.FullTime = true
	case "index-file":
		v, err := need()
		if err != nil {
			return err
		}
		o.IndexFile = v
	case "check-links":
		o.CheckLinks = true
	case "no-quote-chars":
		v, err := need()
		if err != nil {
			return err
		}
		o.NoQuoteChars = v
	case "quote-chars":
		v, err := need()
		if err != nil {
			return err
		}
		o.QuoteChars = v
	case "quoting-style":
		v, err := need()
		if err != nil {
			return err
		}
		switch v {
		case "literal", "shell", "shell-always", "shell-escape", "shell-escape-always",
			"c", "c-maybe", "escape", "locale", "clocale":
		default:
			return &fatalError{msg: fmt.Sprintf("Unknown quoting style '%s'. Try 'fasttar --quoting-style=help' to get a list.", v)}
		}
		o.QuotingStyle = v
	case "block-number":
		o.BlockNumber = true
	case "show-defaults":
		o.ShowDefaults = true
	case "show-omitted-dirs":
		o.ShowOmittedDirs = true
	case "show-snapshot-field-ranges":
		o.ShowSnapRanges = true
	case "show-transformed-names", "show-stored-names":
		o.ShowTransformed = true
	case "totals":
		o.Totals = true
		if hasVal {
			o.TotalsSignal = val
		}
	case "utc":
		o.UTC = true
	case "verbose":
		o.Verbose++
	case "warning":
		v, err := need()
		if err != nil {
			return err
		}
		if v != "none" {
			if err := restrictedArg("--warning", v, warningNames); err != nil {
				return err
			}
		}
		o.Warnings = append(o.Warnings, v)
	case "interactive", "confirmation":
		o.Interactive = true

	// ---- compatibility / other ----
	case "restrict":
		o.Restrict = true
	case "help":
		o.Help = true
	case "usage":
		o.Usage = true
	case "version":
		o.Version = true

	// ---- fasttar extras ----
	case "progress":
		o.Progress = true
	case "fast":
		o.Fast = true
	case "debug":
		o.Debug = true
	case "buffer":
		v, err := need()
		if err != nil {
			return err
		}
		n, err := parseSize(v)
		if err != nil {
			return fmt.Errorf("invalid buffer size '%s'", v)
		}
		o.Buffer = n

	default:
		return fmt.Errorf("unrecognized option '%s'", "--"+name)
	}
	return nil
}

// textualDate remembers a date given as a string so that, in verbose mode,
// GNU tar's "Option X: Treating date 'Y' as Z" note can be reproduced.
type textualDate struct {
	option string
	raw    string
	t      time.Time
}

// dateOption parses a date the way GNU tar does: an unrecognised string is
// replaced by INT64_MIN (which filters nothing) and reported as a warning,
// while a recognised one is remembered for the verbose report.
func (p *parser) dateOption(label, raw string) (time.Time, bool) {
	t, err := parseDate(raw)
	if err != nil {
		warnf("Substituting -9223372036854775807 for unknown date format '%s'", raw)
		return time.Time{}, false
	}
	p.o.TextualDates = append(p.o.TextualDates, textualDate{option: label, raw: raw, t: t})
	return t, true
}

// usageError marks a failure that GNU tar reports together with the
// "Try 'tar --help'" hint: bad options and refused invocations. Options that
// argp itself rejects (unknown switches, missing arguments) exit 64; the
// semantic ones exit 2.
type usageError struct {
	msg  string
	code int // 0 means GNU's default 2
}

func (e *usageError) Error() string { return e.msg }

func (e *usageError) ExitCode() int {
	if e.code == 0 {
		return 2
	}
	return e.code
}

// argRangeError is GNU tar's argp "restricted argument" diagnostic: the value
// is rejected with the list of accepted ones and no "Try --help" hint.
type argRangeError struct {
	msg  string
	vals []string
}

func (e *argRangeError) Error() string { return e.msg }

func (e *argRangeError) ExitCode() int { return 2 }

// fatalError is GNU tar's "Error is not recoverable: exiting now" path: the
// message is printed as an error and the run stops without the summary line.
type fatalError struct{ msg string }

func (e *fatalError) Error() string { return e.msg }

// notImplemented rejects options whose semantics fasttar cannot reproduce
// safely, explaining what to do instead. Returns a non-nil error so callers
// can simply `return notImplemented(...)`.
func notImplemented(option, instead string) error {
	return &unsupportedError{option: option, instead: instead}
}

type unsupportedError struct {
	option  string
	instead string
}

func (e *unsupportedError) Error() string {
	return fmt.Sprintf("%s is not supported by fasttar (%s)", e.option, e.instead)
}

// shorts handles a cluster such as -xvzf file.tar or -C/tmp.
func (p *parser) shorts(arg string) error {
	o := p.o
	body := arg[1:]

	for i := 0; i < len(body); i++ {
		c := body[i]
		rest := body[i+1:]
		// An argument may be glued to the option or come next.
		glued := func() (string, bool) {
			if rest != "" {
				return rest, true
			}
			return "", false
		}
		need := func(name string) (string, error) {
			if v, ok := glued(); ok {
				return v, nil
			}
			return p.nextArg(name)
		}

		var err error
		switch c {
		case 'A':
			err = o.setMode(modeCatenate, "-A")
		case 'c':
			err = o.setMode(modeCreate, "-c")
		case 'd':
			err = o.setMode(modeDiff, "-d")
		case 'r':
			err = o.setMode(modeAppend, "-r")
		case 't':
			err = o.setMode(modeList, "-t")
		case 'u':
			err = o.setMode(modeUpdate, "-u")
		case 'x':
			err = o.setMode(modeExtract, "-x")
		case 'f':
			var v string
			if v, err = need("-f"); err == nil {
				o.Archive = v
				o.ArchiveNames++
			}
		case 'C':
			var v string
			if v, err = need("-C"); err == nil {
				o.Cmds = append(o.Cmds, cmd{kind: cmdDir, value: v, base: o.base})
				o.base = joinBase(o.base, v)
			}
		case 'T':
			var v string
			if v, err = need("-T"); err == nil {
				o.Cmds = append(o.Cmds, cmd{kind: cmdFilesFrom, value: v, base: o.base})
			}
		case 'X':
			var v string
			if v, err = need("-X"); err == nil {
				if err := readable(v); err != nil {
					return err
				}
				o.ExcludeFrom = append(o.ExcludeFrom, v)
			}
		case 'b':
			var v string
			if v, err = need("-b"); err == nil {
				n, e := p.intArg("-b", v)
				if e != nil {
					err = e
				} else {
					o.BlockingFactor = n
				}
			}
		case 'B':
			o.ReadFullRecords = true
		case 'i':
			o.IgnoreZeros = true
		case 'H':
			var v string
			if v, err = need("-H"); err == nil {
				switch v {
				case "gnu", "oldgnu", "pax", "posix", "ustar", "v7":
					o.Format = v
					o.FormatExplicit = true
				default:
					err = &usageError{msg: fmt.Sprintf("%s: Invalid archive format", v)}
				}
			}
		case 'I':
			var v string
			if v, err = need("-I"); err == nil {
				o.CompressProgram = v
				o.CompressionAsked = true
			}
		case 'K':
			var v string
			if v, err = need("-K"); err == nil {
				o.StartingFile = v
			}
		case 'L':
			var v string
			if v, err = need("-L"); err == nil {
				n, e := p.intArg("-L", v)
				if e != nil {
					err = e
				} else {
					o.TapeLength = n
					o.note("-L/--tape-length is ignored: fasttar writes a single volume")
				}
			}
		case 'M':
			o.MultiVolume = true
			o.note("-M/--multi-volume is ignored: fasttar always writes a single volume")
		case 'F':
			var v string
			if v, err = need("-F"); err == nil {
				o.InfoScript = v
				o.MultiVolume = true
				o.note("-F/--info-script is ignored: fasttar always writes a single volume")
			}
		case 'N':
			var v string
			if v, err = need("-N"); err == nil {
				if o.ThresholdDate {
					return &usageError{msg: "More than one threshold date"}
				}
				o.ThresholdDate = true
				o.Newer = v
				o.ThresholdTime, o.ThresholdOK = p.dateOption("--after-date", v)
			}
		case 'V':
			var v string
			if v, err = need("-V"); err == nil {
				o.Label = v
			}
		case 'g':
			var v string
			if v, err = need("-g"); err == nil {
				o.ListedIncrFile = v
				o.Incremental = true
				err = notImplemented("-g/--listed-incremental", "incremental backups")
			}
		case 'G':
			// Without --listed-incremental GNU tar has no snapshot to compare
			// against, so -G simply stores every file - what fasttar does.
			o.Incremental = true
		case 'O':
			o.ToStdout = true
		case 'W':
			o.Verify = true
		case 'U':
			o.UnlinkFirst = true
		case 'S':
			o.Sparse = true
		case 'k':
			o.KeepOldFiles = true
		case 'm':
			o.Touch = true
		case 'n':
			o.NoSeek = false
		case 'o':
			// compat: create -> v7 format, extract -> no same owner
			if o.Mode == modeExtract || o.Mode == modeList {
				o.SameOwner = false
			} else {
				o.Format = "v7"
			}
		case 'p':
			o.SamePermissions = true
		case 's':
			o.SameOrder = true
		case 'v':
			o.Verbose++
		case 'w':
			o.Interactive = true
		case 'z':
			o.CompressSuffix = "gz"
			o.CompressionAsked = true
		case 'Z':
			o.CompressSuffix = "Z"
			o.CompressionAsked = true
		case 'j':
			o.CompressSuffix = "bz2"
			o.CompressionAsked = true
		case 'J':
			o.CompressSuffix = "xz"
			o.CompressionAsked = true
		case 'a':
			o.AutoCompress = true
		case 'h':
			o.Dereference = true
		case 'l':
			o.CheckLinks = true
		case 'R':
			o.BlockNumber = true
		case '?':
			o.Help = true
		default:
			return fmt.Errorf("invalid option -- '%c'", c)
		}
		if err != nil {
			return err
		}
		// Short options that consume an argument end the cluster (the rest of
		// the token, or the following argument, is their value).
		if strings.ContainsRune("fCTXbHIKLFNVg", rune(c)) {
			break
		}
	}
	return nil
}

// joinBase implements the accumulating -C behaviour.
func joinBase(base, dir string) string {
	if strings.HasPrefix(dir, "/") {
		return dir
	}
	if base == "" {
		return dir
	}
	return strings.TrimSuffix(base, "/") + "/" + dir
}

// backupControl maps --backup argument names, mirroring GNU tar.
func backupControl(s string) (string, error) {
	if s == "" {
		if e := os.Getenv("VERSION_CONTROL"); e != "" {
			s = e
		} else {
			s = "existing"
		}
	}
	switch s {
	case "none", "off", "t", "numbered", "nil", "existing", "never", "simple", "nil2":
		return s, nil
	}
	return "", fmt.Errorf("invalid backup type '%s'", s)
}

// parseSize accepts 512, 4K, 1M, 2GiB.
func parseSize(s string) (int, error) {
	s = strings.TrimSpace(s)
	mult := 1
	for _, u := range []struct {
		suf string
		m   int
	}{
		{"KiB", 1024}, {"K", 1024}, {"k", 1024},
		{"MiB", 1024 * 1024}, {"M", 1024 * 1024}, {"m", 1024 * 1024},
		{"GiB", 1024 * 1024 * 1024}, {"G", 1024 * 1024 * 1024}, {"g", 1024 * 1024 * 1024},
	} {
		if strings.HasSuffix(s, u.suf) {
			mult = u.m
			s = strings.TrimSuffix(s, u.suf)
			break
		}
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid size '%s'", s)
	}
	return n * mult, nil
}

// showDefaults prints the effective configuration, like GNU tar --show-defaults.
func showDefaults(w io.Writer) {
	d := defaults()
	fmt.Fprintf(w, "--format=%s -f%s -b%d --quoting-style=%s\n",
		d.Format, d.Archive, d.BlockingFactor, d.QuotingStyle)
}

// creates reports whether the mode writes to an archive (as opposed to
// reading one).
func (m opMode) creates() bool {
	switch m {
	case modeCreate, modeAppend, modeUpdate, modeCatenate:
		return true
	}
	return false
}
