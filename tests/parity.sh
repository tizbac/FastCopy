#!/bin/bash
# Behaviour comparison against GNU cp for the tricky flags.
# Run with: make test
HERE=$(cd "$(dirname "$0")" && pwd)
FC=$HERE/../fastcopy
T=$HERE/out
ORIG=$PWD
rm -rf $T; mkdir -p $T
pass=0; fail=0
chk() {
  if [ "$2" == "$3" ]; then echo "PASS $1"; pass=$((pass+1));
  else echo "FAIL $1: expected [$2] got [$3]"; fail=$((fail+1)); fi
}

printf 'hello world\n' > $T/src.txt
mkdir -p $T/a/b && printf 'leaf\n' > $T/a/b/c.txt
printf 'x\n' > $T/a/d.txt

# -i
printf 'old\n' > $T/d1; printf 'old\n' > $T/d2
echo y | timeout 10 $FC -i $T/src.txt $T/d1
echo n | timeout 10 $FC -i $T/src.txt $T/d2
chk "-i yes" "hello world" "$(cat $T/d1)"
chk "-i no"  "old"        "$(cat $T/d2)"

# -n
printf 'old\n' > $T/d3
timeout 10 $FC -n $T/src.txt $T/d3
chk "-n" "old" "$(cat $T/d3)"

# -u : dest newer than source -> skip
printf 'old\n' > $T/d4; touch -d '2030-01-01' $T/d4
timeout 10 $FC -u $T/src.txt $T/d4
chk "-u skip newer dest" "old" "$(cat $T/d4)"
printf 'old2\n' > $T/d5; touch -d '2000-01-01' $T/d5
timeout 10 $FC -u $T/src.txt $T/d5
chk "-u copy older dest" "hello world" "$(cat $T/d5)"

# -l hard link
timeout 10 $FC -l $T/src.txt $T/d6
chk "-l inode" "$(stat -c %i $T/src.txt)" "$(stat -c %i $T/d6)"

# -s symbolic link (target is the source path as written)
cd $T
timeout 10 $FC -s a/b/c.txt s_rel
timeout 10 $FC -s $T/src.txt s_abs
chk "-s rel" "a/b/c.txt" "$(readlink s_rel)"
chk "-s abs" "$T/src.txt" "$(readlink s_abs)"
cd /tmp

# -t target dir
mkdir -p $T/tdir
timeout 10 $FC -t $T/tdir $T/src.txt $T/a/d.txt
chk "-t" "d.txt src.txt" "$(ls $T/tdir | sort | tr '\n' ' ' | sed 's/ $//')"

# -T
timeout 10 $FC -T $T/src.txt $T/plain_out
chk "-T" "hello world" "$(cat $T/plain_out)"

# --parents (absolute and relative), compare with cp
mkdir -p $T/p_cp $T/p_fc
cd $T
cp --parents a/b/c.txt p_cp
timeout 10 $FC --parents a/b/c.txt p_fc
chk "--parents tree" "$(find p_cp | sed 's/p_cp/X/' | sort)" "$(find p_fc | sed 's/p_fc/X/' | sort)"
chk "--parents modes" "$(find p_cp -type d -printf '%p %m\n' | sed 's/p_cp/X/' | sort)" "$(find p_fc -type d -printf '%p %m\n' | sed 's/p_fc/X/' | sort)"
cd /tmp

# --backup / --suffix
printf 'previous\n' > $T/d7
timeout 10 $FC --backup $T/src.txt $T/d7
chk "--backup content" "hello world" "$(cat $T/d7)"
chk "--backup tilde" "previous" "$(cat $T/d7~)"
printf 'previous2\n' > $T/d8
timeout 10 $FC -b -S '.bak' $T/src.txt $T/d8
chk "-b -S suffix" "previous2" "$(cat $T/d8.bak)"

# --remove-destination over a fifo
mkfifo $T/fifo
timeout 10 $FC --remove-destination $T/src.txt $T/fifo
chk "--remove-destination" "hello world" "$(timeout 5 cat $T/fifo)"

# --attributes-only
printf 'target-data\n' > $T/d9
timeout 10 $FC --attributes-only $T/src.txt $T/d9
chk "--attributes-only keeps data" "target-data" "$(cat $T/d9)"
rm -f $T/d10
timeout 10 $FC --attributes-only $T/src.txt $T/d10
chk "--attributes-only new size" "0" "$(stat -c %s $T/d10)"

# --strip-trailing-slashes
rm -rf $T/sl; mkdir $T/sl
timeout 10 $FC -r --strip-trailing-slashes $T/a/ $T/sl
chk "strip slashes" "a" "$(ls $T/sl | sort | tr '\n' ' ' | sed 's/ $//')"

# -a parity with cp, multi-job, custom buffer
cp -a $T/a $T/a_cp
timeout 10 $FC -a --jobs=4 --buffer=64K $T/a $T/a_fc
chk "-a --jobs=4 parity" "ok" "$(diff -r $T/a_cp $T/a_fc >/dev/null && echo ok)"
# hard links survive parallel jobs
rm -rf $T/hlc $T/hlf
mkdir -p $T/hl && ln $T/src.txt $T/hl/dup1 && ln $T/src.txt $T/hl/dup2
cp -a $T/hl $T/hlc
$FC -a --jobs=8 $T/hl $T/hlf
chk "hard links with --jobs" "ok" "$(diff -r $T/hlc $T/hlf >/dev/null && echo ok)"
chk "hard link inodes" "$(find $T/hlc -type f -printf '%i\n' | sort -u | wc -l)" "$(find $T/hlf -type f -printf '%i\n' | sort -u | wc -l)"

chk "-a hardlink/mode/time" "$(stat -c '%a %u %g' $T/a_cp/b/c.txt)" "$(stat -c '%a %u %g' $T/a_fc/b/c.txt)"

# sparse
python3 -c "
import os
f=open('$T/sparse.bin','wb'); f.seek(20*1024*1024); f.write(b'tail'); f.close()"
timeout 10 $FC --sparse=always $T/sparse.bin $T/sp_always
timeout 10 $FC --sparse=never  $T/sparse.bin $T/sp_never
chk "sparse size" "$(stat -c %s $T/sparse.bin)" "$(stat -c %s $T/sp_always)"
chk "sparse blocks" "yes" "$([ $(stat -c %B $T/sp_never) -ge $(stat -c %B $T/sp_always) ] && echo yes || echo no)"
cp --sparse=always $T/sparse.bin $T/sp_cp
chk "sparse blocks vs cp" "$(stat -c %B $T/sp_cp)" "$(stat -c %B $T/sp_always)"

# non-tty progress: one summary line
rm -rf $T/pout
out=$(timeout 20 $FC --progress $T/a $T/pout)
chk "non-tty progress line count" "1" "$(printf '%s\n' "$out" | wc -l)"
echo "  progress line: $out"

# -d and -p parity
cp -dr $T/a $T/d_cp; timeout 10 $FC -dr $T/a $T/d_fc
chk "-d parity" "ok" "$(diff -r $T/d_cp $T/d_fc >/dev/null && echo ok)"
cp -pr $T/a $T/p_cp; timeout 10 $FC -pr $T/a $T/p_fc
chk "-p mtime" "$(stat -c %Y $T/p_cp/a/b/c.txt)" "$(stat -c %Y $T/p_fc/a/b/c.txt)"

echo "passed=$pass failed=$fail"
