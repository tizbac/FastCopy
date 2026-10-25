# fastcopy / fasttar

A Go rewrite of the old C++/Boost `fastcopy` tool (kept in [`legacy/`](legacy) as
the reference implementation), plus **fasttar**, a GNU tar 1.35 compatible
archiver built on the same progress bar and disk-order machinery.

```
fastcopy -rv --progress /data /backup
fastcopy -a --fast --jobs=4 --progress /photos /mnt/other
```

## Why "fast"

The original tool read files in the order their data blocks live on the platter
(EXT2/EXT3 extent map via `open_extents`). This port uses the modern **FIEMAP**
ioctl (`FS_IOC_FIEMAP`) to get the physical offset of every file, then copies in
that order. On a spinning disk that turns a copy into one long sequential sweep
instead of a head-jumping scatter. On SSDs it makes little difference, so it is
opt-in:

- `--fast` — sort the copy queue by physical extent (falls back to walk order
  when the filesystem does not support FIEMAP, e.g. tmpfs or some network
  mounts).
- `--debug` — print the extent map of every file so you can see what `--fast`
  is doing.

## Progress bar

`--progress` draws a bordered bar with file count, bytes and MB/s. It redraws in
place on a terminal; when stdout is redirected it prints a single summary line
instead, so it never corrupts piped output. `-v` messages are printed above the
bar rather than over it.

```
┌──────────────────────────────────────────────────────┐
│ 40/40 files  ███████████████████████████████  100.0% │
│ 80.0 MB / 80.0 MB   1125.2 MB/s   eta 0s             │
└──────────────────────────────────────────────────────┘
```

## Options

Everything that GNU `cp` accepts is supported with the same meaning, including
clustered short flags (`-rpv`), `-t DIR`, `-T`, `--parents`,
`--preserve=`/`--no-preserve=`, `--sparse=`, `--backup`/`-S`,
`--remove-destination`, `--attributes-only`, `--copy-contents` and
`--strip-trailing-slashes`.

The port adds:

| flag | meaning |
|------|---------|
| `--progress` | show the ANSI progress bar |
| `--fast` | copy in physical disk order (FIEMAP) |
| `--debug` | dump the physical extent of every file |
| `--jobs=N` | run N copy workers in parallel (default 1) |
| `--buffer=SIZE` | copy buffer size, default 1M (`64K`, `4M` …) |

Directories are **not** recursed unless you pass `-r`/`-R`, exactly like `cp`:

```
fastcopy -r src dest      # not fastcopy src dest
```

# fasttar

`fasttar` is a drop-in GNU tar 1.35 work-alike: it takes the whole GNU tar
command line (all short and long options, including `--format`, `--transform`,
`--exclude`, `-C`, `-T`, `--delete`, `-r/-u/-A`, `--test-label`,
`--checkpoint-action`, …), reads and writes GNU, oldgnu, pax, ustar and v7
archives, and reproduces GNU's diagnostics and exit codes:

| exit | meaning |
|------|---------|
| 0 | success |
| 1 | differences found by `-d`, or a `--test-label` mismatch |
| 2 | usage or runtime error |
| 64 | command line parse error (argp) |

It shares fastcopy's tooling, so it adds four flags of its own:

| flag | meaning |
|------|---------|
| `--progress` | show the same ANSI progress box |
| `--fast` | write members in physical disk order (FIEMAP), like fastcopy |
| `--debug` | dump the physical extent of every file |
| `--buffer=SIZE` | I/O buffer size, default 1M (`64K`, `4M` …) |

```
fasttar -cvf backup.tar --progress --fast /photos
fasttar -xvf backup.tar -C /mnt/restore --progress
fasttar --transform='s|^src/|pkg/|' -cf out.tar src
```

## No compression, on purpose

fasttar writes plain tar streams and does not link a compressor. Asking for one
is refused, with instructions instead of a broken archive:

```
$ fasttar -czf backup.tar.gz /photos
fasttar: fasttar never compresses archives: -z/-j/-J/-Z/--lzma/--lzip/--lzop/--zstd is not built in
  pipe the stream through gzip yourself:
      fasttar -cf - FILES | gzip > backup.tar.gz
      fasttar -xf - < backup.tar.gz
```

On reading, a compressed file is recognised by its magic bytes and the same hint
is printed (`gzip -dc file.tar.gz | fasttar -tf -`).

## Known differences from GNU tar 1.35

Everything else aims to be byte-for-byte compatible, including listings, `-tv`
columns, `-d` messages, `--block-number` values, checkpoints and error wording.
The intentional differences are:

- no built-in compression (see above);
- sparse files are *read* and extracted with their holes, but never *written*
  as sparse members (`-S`, `--sparse-version`, `--hole-detection` are accepted);
- incremental backups (`-g`/`--listed-incremental`) are not implemented; `-G`
  is accepted;
- xattrs, ACLs and SELinux labels are not archived; `--xattrs`, `--acls` and
  `--selinux` only force the pax format, as in GNU tar;
- `--quoting-style`: `literal`, `escape`, `c` and `c-maybe` are reproduced
  exactly; the shell/locale styles fall back to the escape layout;
- `--show-defaults` and `--version` print fasttar's own text;
- multi-volume options (`-M`, `-L`, `-F`, `--volno-file`, `--rsh-command`) are
  accepted as single-volume no-ops, and `--level`, `--group-map`,
  `--owner-map` are accepted (they only affect incremental backups);
- `--mode` is applied when creating an archive, not when extracting;
- members are written in command line/readdir order (or physical order with
  `--fast`), so archives made by fasttar may list members in a different order
  than GNU tar would; the contents are identical.

`--fast` sorts hard links after the file they point at, so the archives it
writes extract cleanly with GNU tar too.

# Build

```
make            # builds ./fastcopy and ./fasttar
make test       # go tests + behaviour comparisons against GNU cp and GNU tar
make clean
```

or `go build -o fastcopy .` and `go build -o fasttar ./cmd/fasttar`.

Requires Go 1.24+ and Linux (FIEMAP and the `stat`/`ioctl` calls are Linux
specific). The tar implementation itself is portable Go; only `--fast` and the
ownership bits are Linux.

## Layout

```
main.go          entry point, argument → copy plan
flags.go         cp-compatible flag parser
copy.go          tree walk, job queue, per-file copy
util.go          terminal size, size parsing
internal/progress ANSI progress bar shared by both tools
internal/fiemap   FS_IOC_FIEMAP extent lookup, shared by both tools
cmd/fasttar/     the tar implementation
legacy/          the original C++/Boost implementation
tests/           behaviour comparisons against GNU cp and GNU tar
```
