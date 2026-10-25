#!/bin/bash
# End-to-end comparison between fasttar and GNU tar: archives written by one
# are read by the other, and a handful of diagnostics must agree too.
# Run with: make test   (or: ./tests/tar_parity.sh)
HERE=$(cd "$(dirname "$0")" && pwd)
FT=${FT:-$HERE/../fasttar}
GNU=${GNU:-tar}
T=$HERE/out-tar
pass=0
fail=0
skip=0

chk() { # name expected actual
  if [ "$2" == "$3" ]; then
    echo "PASS $1"
    pass=$((pass + 1))
  else
    echo "FAIL $1: expected [$2] got [$3]"
    fail=$((fail + 1))
  fi
}
chk_has() { # name haystack needle
  case "$2" in
  *"$3"*) echo "PASS $1"; pass=$((pass + 1)) ;;
  *) echo "FAIL $1: [$2] does not contain [$3]"; fail=$((fail + 1)) ;;
  esac
}
chk_skip() { echo "SKIP $1 ($2)"; skip=$((skip + 1)); }
run_exit() {
  timeout 30 "$@" >/dev/null 2>&1
  echo $?
}

if ! command -v "$GNU" >/dev/null 2>&1; then
  chk_skip "tar parity" "GNU tar not installed"
  echo "pass=$pass fail=$fail skip=$skip"
  exit 0
fi
if [ ! -x "$FT" ]; then
  echo "FAIL fasttar binary not found at $FT (run make)"
  exit 1
fi

export LC_ALL=C
rm -rf "$T"
mkdir -p "$T/src/dir/sub"
printf 'hello tar\n' >"$T/src/a.txt"
: >"$T/src/empty"
i=1
while [ $i -le 200 ]; do
  printf 'line %04d\n' $i
  i=$((i + 1))
done >"$T/src/b.bin"
printf 'c\n' >"$T/src/dir/c.txt"
printf 'd\n' >"$T/src/dir/sub/d.txt"
printf 'u\n' >"$T/src/héllo.txt"
longname=$(printf 'l%.0s' $(seq 1 120))
printf 'long\n' >"$T/src/$longname"
ln -s a.txt "$T/src/link"
ln "$T/src/a.txt" "$T/src/hard"
find "$T/src" -exec touch -h -d '2020-01-02 03:04:05' {} +
find "$T/src" -depth -type d -exec touch -d '2020-01-02 03:04:05' {} +

# --- fasttar writes, GNU reads -------------------------------------------------
(cd "$T/src" && timeout 30 "$FT" -cf "$T/ft.tar" .)
chk "fasttar create" 0 "$?"
chk "GNU lists fasttar archive" \
  "$(cd "$T/src" && "$GNU" -tf "$T/ft.tar" | sort)" \
  "$(cd "$T/src" && "$FT" -tf "$T/ft.tar" | sort)"
mkdir -p "$T/x1"
(cd "$T/x1" && timeout 30 "$GNU" -xf "$T/ft.tar")
chk "GNU extracts fasttar archive" 0 "$?"
if diff -r --no-dereference "$T/src" "$T/x1" >/dev/null 2>&1; then
  echo "PASS extracted tree matches (GNU <- fasttar)"
  pass=$((pass + 1))
else
  echo "FAIL extracted tree matches (GNU <- fasttar)"
  fail=$((fail + 1))
fi
chk "hard link preserved (GNU <- fasttar)" \
  "$(stat -c %i "$T/x1/hard")" "$(stat -c %i "$T/x1/a.txt")"
chk "verbose listing identical" \
  "$("$FT" -tvf "$T/ft.tar")" "$("$GNU" -tvf "$T/ft.tar")"
chk "block numbers identical" \
  "$("$FT" --block-number -tf "$T/ft.tar")" "$("$GNU" --block-number -tf "$T/ft.tar")"

# --- GNU writes, fasttar reads -------------------------------------------------
(cd "$T/src" && timeout 30 "$GNU" -cf "$T/gnu.tar" .)
chk "GNU create" 0 "$?"
mkdir -p "$T/x2"
(cd "$T/x2" && timeout 30 "$FT" -xf "$T/gnu.tar")
chk "fasttar extracts GNU archive" 0 "$?"
if diff -r --no-dereference "$T/src" "$T/x2" >/dev/null 2>&1; then
  echo "PASS extracted tree matches (fasttar <- GNU)"
  pass=$((pass + 1))
else
  echo "FAIL extracted tree matches (fasttar <- GNU)"
  fail=$((fail + 1))
fi
chk "fasttar reads GNU verbose listing" \
  "$("$FT" -tf "$T/gnu.tar" | sort)" "$("$GNU" -tf "$T/gnu.tar" | sort)"

# --- archive formats ----------------------------------------------------------
for fmt in gnu oldgnu pax posix ustar v7; do
  (cd "$T/src" && timeout 30 "$FT" -cf "$T/ft-$fmt.tar" --format=$fmt a.txt dir)
  chk "create --format=$fmt" 0 "$?"
  chk "GNU lists --format=$fmt" \
    "$(cd "$T/src" && "$GNU" -tf "$T/ft-$fmt.tar" | sort)" \
    "$(cd "$T/src" && "$FT" -tf "$T/ft-$fmt.tar" | sort)"
  rm -rf "$T/xf-$fmt"
  mkdir -p "$T/xf-$fmt"
  (cd "$T/xf-$fmt" && timeout 30 "$GNU" -xf "$T/ft-$fmt.tar")
  chk "GNU extracts --format=$fmt" "$(cat "$T/xf-$fmt/a.txt")" "hello tar"

  (cd "$T/src" && timeout 30 "$GNU" -cf "$T/gnu-$fmt.tar" --format=$fmt a.txt dir)
  chk "fasttar lists GNU --format=$fmt" \
    "$(cd "$T/src" && "$FT" -tf "$T/gnu-$fmt.tar" | sort)" \
    "$(cd "$T/src" && "$GNU" -tf "$T/gnu-$fmt.tar" | sort)"
  rm -rf "$T/xg-$fmt"
  mkdir -p "$T/xg-$fmt"
  (cd "$T/xg-$fmt" && timeout 30 "$FT" -xf "$T/gnu-$fmt.tar")
  chk "fasttar extracts GNU --format=$fmt" "$(cat "$T/xg-$fmt/a.txt")" "hello tar"
done

# --- stdin / stdout -----------------------------------------------------------
(cd "$T/src" && timeout 30 "$FT" -cf - a.txt dir) >"$T/pipe.tar"
chk "GNU reads a fasttar stdout stream" \
  "$("$GNU" -tf "$T/pipe.tar" | sort)" "$(printf 'a.txt\ndir/\ndir/c.txt\ndir/sub/\ndir/sub/d.txt\n')"
(cd "$T/src" && timeout 30 "$GNU" -cf - a.txt dir) >"$T/pipe2.tar"
chk "fasttar reads a GNU stdout stream" \
  "$("$FT" -tf "$T/pipe2.tar" | sort)" "$(printf 'a.txt\ndir/\ndir/c.txt\ndir/sub/\ndir/sub/d.txt\n')"

# A pipe is not seekable: writing an archive to stdout must not truncate it.
chk "create to a pipe" "yes" \
  "$((cd "$T/src" && timeout 30 "$FT" -cf - a.txt dir) | "$GNU" -tf - | grep -qx a.txt && echo yes)"
for op in -rf -uf -Af; do
  out=$(timeout 30 "$FT" "$op" - -C "$T/src" a.txt 2>&1)
  chk "$op with -f - exit" 2 "$?"
  chk_has "$op with -f - message" "$out" "incompatible with '-f -'"
done

# --- append, update, delete ---------------------------------------------------
cp "$T/ft.tar" "$T/app.tar"
printf 'new\n' >"$T/src/newfile.txt"
(cd "$T/src" && timeout 30 "$FT" -rf "$T/app.tar" newfile.txt)
chk "fasttar append" 0 "$?"
chk "GNU lists appendant" "yes" "$("$GNU" -tf "$T/app.tar" | grep -qx newfile.txt && echo yes)"
cp "$T/gnu.tar" "$T/app2.tar"
(cd "$T/src" && timeout 30 "$GNU" -rf "$T/app2.tar" newfile.txt)
chk "GNU append" 0 "$?"
chk "fasttar lists appendant" "yes" "$("$FT" -tf "$T/app2.tar" | grep -qx newfile.txt && echo yes)"

cp "$T/ft.tar" "$T/del.tar"
timeout 30 "$FT" --delete -f "$T/del.tar" ./a.txt
chk "fasttar --delete" 0 "$?"
chk "GNU sees deletion" "yes" "$("$GNU" -tf "$T/del.tar" | grep -qx ./a.txt || echo yes)"
cp "$T/gnu.tar" "$T/del2.tar"
timeout 30 "$GNU" --delete -f "$T/del2.tar" ./a.txt
chk "GNU --delete" 0 "$?"
chk "fasttar sees deletion" "yes" "$("$FT" -tf "$T/del2.tar" | grep -qx ./a.txt || echo yes)"

touch -d '2030-01-01' "$T/src/a.txt"
cp "$T/ft.tar" "$T/upd.tar"
(cd "$T/src" && timeout 30 "$FT" -uf "$T/upd.tar" .)
chk "fasttar update" 0 "$?"
touch -d '2020-01-02 03:04:05' "$T/src/a.txt"

# --- selection, exclusion, transform -----------------------------------------
chk "exclude directory" \
  "$("$FT" -tf "$T/ft.tar" --exclude=dir | sort)" \
  "$("$GNU" -tf "$T/ft.tar" --exclude=dir | sort)"
chk "literal member" \
  "$("$FT" -tf "$T/ft.tar" ./a.txt)" "./a.txt"
chk "wildcard member" \
  "$("$FT" --wildcards -tf "$T/ft.tar" './*.txt' | sort)" \
  "$("$GNU" --wildcards -tf "$T/ft.tar" './*.txt' | sort)"
# GNU prints a bare "/" for the over-stripped "./" root member; ignore it.
chk "strip-components listing" \
  "$("$FT" -tf "$T/ft.tar" --strip-components=1 --show-transformed-names | grep -v '^/$' | sort)" \
  "$("$GNU" -tf "$T/ft.tar" --strip-components=1 --show-transformed-names | grep -v '^/$' | sort)"
(cd "$T/src" && timeout 30 "$FT" -cf "$T/tf-ft.tar" --transform='s/^dir/DIR/' dir)
(cd "$T/src" && timeout 30 "$GNU" -cf "$T/tf-gnu.tar" --transform='s/^dir/DIR/' dir)
chk "--transform create" \
  "$("$GNU" -tf "$T/tf-ft.tar" | sort)" "$("$GNU" -tf "$T/tf-gnu.tar" | sort)"

# --- compression is refused ---------------------------------------------------
out=$(timeout 30 "$FT" -czf "$T/z.tar" -C "$T/src" a.txt 2>&1)
chk "compressed create exit" 2 "$?"
chk_has "compressed create hint" "$out" "pipe"
gzip -c "$T/ft.tar" >"$T/gz.tar.gz"
out=$(timeout 30 "$FT" -tf "$T/gz.tar.gz" 2>&1)
chk "compressed read exit" 2 "$?"
chk_has "compressed read hint" "$out" "gzip -dc"

# --- diagnostics and exit codes ----------------------------------------------
chk "no mode" 2 "$(run_exit "$FT" -f "$T/ft.tar")"
chk "two modes" 2 "$(run_exit "$FT" -c -x -f "$T/x.tar" .)"
chk "unknown long option" 64 "$(run_exit "$FT" --frobnicate -tf "$T/ft.tar")"
chk "bad blocking factor" 2 "$(run_exit "$FT" -b 0 -cf "$T/x.tar" .)"
chk "bad sort value" 2 "$(run_exit "$FT" --sort=bogus -tf "$T/ft.tar")"
chk "missing member" 2 "$(run_exit "$FT" -tf "$T/ft.tar" nosuch)"
chk "missing member message" \
  "fasttar: nosuch: Not found in archive" \
  "$(timeout 30 "$FT" -tf "$T/ft.tar" nosuch 2>&1 >/dev/null | head -1)"

# --- diff mode ----------------------------------------------------------------
rm -rf "$T/x3"
mkdir -p "$T/x3"
(cd "$T/x3" && timeout 30 "$FT" -xf "$T/ft.tar")
chk "diff identical tree" 0 "$(cd "$T/x3" && run_exit "$FT" -df "$T/ft.tar")"
printf 'changed\n' >"$T/x3/a.txt"
chk "diff changed file" 1 "$(cd "$T/x3" && run_exit "$FT" -df "$T/ft.tar")"

# --- volume labels ------------------------------------------------------------
(cd "$T/src" && timeout 30 "$FT" -cf "$T/lbl.tar" -V MYLABEL a.txt)
chk "test-label" "MYLABEL" "$("$FT" --test-label -f "$T/lbl.tar")"
chk "GNU reads labeled archive" "yes" "$("$GNU" -tf "$T/lbl.tar" >/dev/null && echo yes)"

# --- --fast and --totals ------------------------------------------------------
rm -rf "$T/fastx"
mkdir -p "$T/fastx"
(cd "$T/src" && timeout 60 "$FT" --fast -cf "$T/fast.tar" .)
chk "create --fast" 0 "$?"
(cd "$T/fastx" && timeout 60 "$FT" --fast -xf "$T/fast.tar")
chk "extract --fast" 0 "$?"
chk_has "--totals line" \
  "$(timeout 30 "$FT" --totals -cf "$T/tot.tar" -C "$T/src" a.txt 2>&1)" "Total bytes written:"

# --- progress bar on a pseudo-terminal ---------------------------------------
if command -v script >/dev/null 2>&1; then
  out=$(script -qec "LC_ALL=C $FT --progress -cf $T/prog.tar -C $T/src a.txt" /dev/null)
  case "$out" in
  *$'\x1b['*) echo "PASS progress box on a tty"; pass=$((pass + 1)) ;;
  *) echo "FAIL progress box on a tty: no ANSI output"; fail=$((fail + 1)) ;;
  esac
else
  chk_skip "progress box" "script(1) not installed"
fi
out=$("$FT" --progress -cf "$T/prog2.tar" -C "$T/src" a.txt)
case "$out" in
*$'\x1b['*) echo "FAIL progress escapes when stdout is redirected"; fail=$((fail + 1)) ;;
*files*) echo "PASS progress summary line when redirected"; pass=$((pass + 1)) ;;
*) echo "FAIL progress summary line when redirected: [$out]"; fail=$((fail + 1)) ;;
esac

echo "pass=$pass fail=$fail skip=$skip"
[ "$fail" -eq 0 ]
