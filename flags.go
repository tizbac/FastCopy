package main

import (
	"fmt"
	"io"
	"strings"
)

// Options mirrors the flags that GNU cp understands. A few extra flags
// (--progress, --fast, --jobs, --buffer) are specific to fastcopy.
type Options struct {
	Recursive      bool
	Interactive    bool
	Force          bool
	NoClobber      bool
	Update         bool
	RemoveDest     bool
	Backup         bool
	Suffix         string
	Dereference    bool
	PreserveMode   bool
	PreserveOwner  bool
	PreserveTime   bool
	PreserveLinks  bool
	Link           bool
	SymbolicLink   bool
	Verbose        bool
	Parents        bool
	OneFileSystem  bool
	TargetDir      string
	NoTargetDir    bool
	CopyContents   bool
	AttributesOnly bool
	StripSlashes   bool
	Sparse         string // "never", "always"

	// fastcopy specific
	Progress bool
	Fast     bool
	Debug    bool
	Jobs     int
	Buffer   int
	Help     bool
	Version  bool
}

func defaultOptions() Options {
	return Options{
		Suffix: "~",
		Sparse: "always",
		Jobs:   1,
		Buffer: 1 << 20, // 1 MiB
	}
}

func parseArgs(argv []string) (Options, []string, error) {
	opts := defaultOptions()
	var positional []string

	for i := 0; i < len(argv); i++ {
		arg := argv[i]

		if arg == "--" {
			positional = append(positional, argv[i+1:]...)
			break
		}
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}
		if strings.HasPrefix(arg, "--") {
			name := arg[2:]
			value := ""
			hasValue := false
			if idx := strings.Index(name, "="); idx >= 0 {
				value = name[idx+1:]
				name = name[:idx]
				hasValue = true
			}

			switch name {
			case "recursive":
				opts.Recursive = true
			case "interactive":
				opts.Interactive = true
			case "force":
				opts.Force = true
			case "no-clobber":
				opts.NoClobber = true
			case "update":
				opts.Update = true
			case "remove-destination":
				opts.RemoveDest = true
			case "backup":
				opts.Backup = true
			case "suffix":
				if !hasValue {
					return opts, nil, fmt.Errorf("--suffix requires an argument")
				}
				opts.Suffix = value
			case "dereference", "L":
				opts.Dereference = true
			case "no-dereference", "P":
				opts.Dereference = false
			case "archive":
				opts.Archive()
			case "preserve":
				list := "all"
				if hasValue {
					list = value
				}
				if err := opts.applyPreserve(list); err != nil {
					return opts, nil, err
				}
			case "no-preserve":
				list := "all"
				if hasValue {
					list = value
				}
				if err := opts.applyNoPreserve(list); err != nil {
					return opts, nil, err
				}
			case "link":
				opts.Link = true
			case "symbolic-link":
				opts.SymbolicLink = true
			case "verbose":
				opts.Verbose = true
			case "parents":
				opts.Parents = true
			case "one-file-system":
				opts.OneFileSystem = true
			case "target-directory":
				if !hasValue {
					return opts, nil, fmt.Errorf("--target-directory requires an argument")
				}
				opts.TargetDir = value
			case "no-target-directory":
				opts.NoTargetDir = true
			case "copy-contents":
				opts.CopyContents = true
			case "attributes-only":
				opts.AttributesOnly = true
			case "strip-trailing-slashes":
				opts.StripSlashes = true
			case "sparse":
				if !hasValue {
					return opts, nil, fmt.Errorf("--sparse requires an argument")
				}
				if value != "never" && value != "always" {
					return opts, nil, fmt.Errorf("--sparse must be 'never' or 'always'")
				}
				opts.Sparse = value
			case "progress":
				opts.Progress = true
			case "no-progress":
				opts.Progress = false
			case "fast":
				opts.Fast = true
			case "debug":
				opts.Debug = true
			case "jobs":
				if !hasValue {
					return opts, nil, fmt.Errorf("--jobs requires an argument")
				}
				n, err := atoi(value)
				if err != nil || n < 1 {
					return opts, nil, fmt.Errorf("--jobs must be a positive integer")
				}
				opts.Jobs = n
			case "buffer":
				if !hasValue {
					return opts, nil, fmt.Errorf("--buffer requires an argument")
				}
				n, err := parseSize(value)
				if err != nil || n < 1024 {
					return opts, nil, fmt.Errorf("--buffer must be at least 1K")
				}
				opts.Buffer = n
			case "help":
				opts.Help = true
			case "version":
				opts.Version = true
			default:
				return opts, nil, fmt.Errorf("unknown option: %s", arg)
			}
			continue
		}

		// Short flags, possibly clustered like -rpv
		for _, c := range arg[1:] {
			switch c {
			case 'a':
				opts.Archive()
			case 'r', 'R':
				opts.Recursive = true
			case 'i':
				opts.Interactive = true
			case 'f':
				opts.Force = true
			case 'n':
				opts.NoClobber = true
			case 'u':
				opts.Update = true
			case 'p':
				opts.PreserveMode = true
				opts.PreserveOwner = true
				opts.PreserveTime = true
			case 'd':
				opts.Dereference = false
				opts.PreserveLinks = true
			case 'l':
				opts.Link = true
			case 's':
				opts.SymbolicLink = true
			case 'v':
				opts.Verbose = true
			case 'P':
				opts.Dereference = false
			case 'L':
				opts.Dereference = true
			case 'b':
				opts.Backup = true
			case 'x':
				opts.OneFileSystem = true
			case 'T':
				opts.NoTargetDir = true
			case 't':
				if i+1 >= len(argv) {
					return opts, nil, fmt.Errorf("-t requires an argument")
				}
				opts.TargetDir = argv[i+1]
				i++
			case 'S':
				if i+1 >= len(argv) {
					return opts, nil, fmt.Errorf("-S requires an argument")
				}
				opts.Suffix = argv[i+1]
				i++
			case 'h':
				opts.Help = true
			default:
				return opts, nil, fmt.Errorf("unknown option: -%c", c)
			}
		}
	}

	if opts.NoClobber && opts.Interactive {
		opts.NoClobber = false // -i overrides -n, like GNU cp
	}

	if opts.StripSlashes {
		for i, a := range positional {
			if len(a) > 1 {
				positional[i] = strings.TrimRight(a, "/")
			}
		}
	}

	return opts, positional, nil
}

// Archive (-a) is GNU cp's "-dR --preserve=all".
func (o *Options) Archive() {
	o.Recursive = true
	o.Dereference = false
	o.PreserveMode = true
	o.PreserveOwner = true
	o.PreserveTime = true
	o.PreserveLinks = true
}

func (o *Options) applyPreserve(list string) error {
	for _, item := range strings.Split(list, ",") {
		switch strings.TrimSpace(item) {
		case "mode":
			o.PreserveMode = true
		case "ownership":
			o.PreserveOwner = true
		case "timestamps":
			o.PreserveTime = true
		case "links":
			o.PreserveLinks = true
		case "all":
			o.PreserveMode = true
			o.PreserveOwner = true
			o.PreserveTime = true
			o.PreserveLinks = true
		case "context", "xattr":
			// Accepted for compatibility with cp, not implemented.
		default:
			return fmt.Errorf("invalid attribute: %q", item)
		}
	}
	return nil
}

func (o *Options) applyNoPreserve(list string) error {
	for _, item := range strings.Split(list, ",") {
		switch strings.TrimSpace(item) {
		case "mode":
			o.PreserveMode = false
		case "ownership":
			o.PreserveOwner = false
		case "timestamps":
			o.PreserveTime = false
		case "links":
			o.PreserveLinks = false
		case "all":
			o.PreserveMode = false
			o.PreserveOwner = false
			o.PreserveTime = false
			o.PreserveLinks = false
		case "context", "xattr":
		default:
			return fmt.Errorf("invalid attribute: %q", item)
		}
	}
	return nil
}

func (o *Options) preserveAny() bool {
	return o.PreserveMode || o.PreserveOwner || o.PreserveTime
}

func usage(w io.Writer) {
	text := `fastcopy - copy files like cp, with an ANSI progress bar and
optional disk-order (physical extent) scheduling.

Usage:
  fastcopy [OPTION]... [-T] SOURCE DEST
  fastcopy [OPTION]... SOURCE... DIRECTORY
  fastcopy [OPTION]... -t DIRECTORY SOURCE...

cp-compatible options:
  -a, --archive                copy everything (same as -dR --preserve=all)
  -b, --backup                 back up every existing destination file
  -d, --no-dereference         copy links as links, preserve hard links
  -f, --force                  remove an unopenable destination and retry
  -h, --help                   show this help and exit
  -i, --interactive            ask before overwriting
  -l, --link                   make hard links instead of copying
  -L, --dereference            always follow links in SOURCE
  -n, --no-clobber             skip existing destination files silently
  -p, --preserve               keep mode, ownership and timestamps
  -P, --no-dereference         never follow links in SOURCE (default)
  -r, -R, --recursive          copy directories recursively
  -s, --symbolic-link          make symbolic links instead of copying
  -S, --suffix=SUFFIX          backup suffix (default '~')
  -u, --update                 copy only when the source is newer
  -v, --verbose                print every file as it is copied
  -x, --one-file-system        stay on the same filesystem
  -t, --target-directory=DIR   copy every SOURCE into DIR
  -T, --no-target-directory    treat DEST as an ordinary file
      --parents                recreate the SOURCE path under DIR
      --preserve[=LIST]        attributes to keep (mode, ownership, time, links)
      --no-preserve=LIST       attributes to drop
      --sparse=WHEN            never | always (default always)
      --remove-destination     unlink DEST before opening it
      --attributes-only        copy the attributes, not the data
      --copy-contents          copy the contents of special files
      --strip-trailing-slashes drop the trailing '/' from every SOURCE

fastcopy options:
      --progress               ANSI progress bar (files, bytes, MB/s)
      --fast                   schedule files by their physical disk position
      --debug                  print the physical extent of every file
      --jobs=N                 parallel copy workers (default 1)
      --buffer=SIZE            copy buffer size (default 1M)
      --version                print the version and exit

Examples:
  fastcopy -rv --progress /data /backup
  fastcopy -a --fast --progress /photos /mnt/other
`
	fmt.Fprint(w, text)
}
