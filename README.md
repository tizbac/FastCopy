# fastcopy

A Go rewrite of the old C++/Boost `fastcopy` tool (kept in [`legacy/`](legacy) as
the reference implementation). It copies like `cp`, prints a coloured ANSI
progress bar when you ask for one, and can schedule files in the order they
physically sit on the disk.

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

## Build

```
make            # builds ./fastcopy
make test       # compares behaviour against GNU cp
make clean
```

or simply `go build -o fastcopy .`.

Requires Go 1.24+ and Linux (FIEMAP and the `stat`/`ioctl` calls are Linux
specific).

## Layout

```
main.go     entry point, argument → copy plan
flags.go    cp-compatible flag parser
copy.go     tree walk, job queue, per-file copy
progress.go ANSI progress bar
fiemap.go   FS_IOC_FIEMAP extent lookup
util.go     terminal size, size parsing
legacy/     the original C++/Boost implementation
```
