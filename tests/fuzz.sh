#!/bin/bash
# Copy a mixed tree twice - once with GNU cp, once with fastcopy - and compare.
HERE=$(cd "$(dirname "$0")" && pwd)
FC=$HERE/../fastcopy
T=$HERE/fuzzout
rm -rf $T; mkdir -p $T/src

python3 - <<EOF
import os
base = '$T/src'
dirs = ['', 'deep', 'deep/sub', 'empty']
for d in dirs + ['deep/sub', 'empty']:
    os.makedirs(os.path.join(base, d), exist_ok=True)
for i in range(12):
    p = os.path.join(base, dirs[i % 4], 'f%d.bin' % i)
    with open(p, 'wb') as f:
        f.write(os.urandom(30000 * (i + 1)))
    os.chmod(p, [0o644, 0o755, 0o600, 0o444][i % 4])
    os.utime(p, (1000000000 + i, 1200000000 + i))
with open(os.path.join(base, 'deep/sparse.bin'), 'wb') as f:
    f.seek(8 * 1024 * 1024)
    f.write(b'tail')
os.symlink('f0.bin', os.path.join(base, 'link_file'))
os.symlink('deep', os.path.join(base, 'link_dir'))
os.symlink('/nope', os.path.join(base, 'dangling'))
os.link(os.path.join(base, dirs[1 % 4], 'f1.bin'), os.path.join(base, 'deep/sub/f1_dup.bin'))
os.link(os.path.join(base, dirs[1 % 4], 'f1.bin'), os.path.join(base, 'deep/f1_dup2.bin'))
EOF

sync

fail=0
report() { # report <name> <ok?>
  if [ "$2" == "ok" ]; then echo "ok   $1"; else echo "FAIL $1"; fail=1; fi
}

# Contents of every regular file, as "<relative path> <hash>".
sums() {
  find "$1" -type f -printf '%P\n' | sort | while read -r f; do
    printf '%s %s\n' "$f" "$(md5sum <"$1/$f" | cut -d' ' -f1)"
  done
}

cp -a $T/src $T/cp_out
$FC -a --jobs=8 --fast $T/src $T/fc_out

sums $T/cp_out > $T/a
sums $T/fc_out > $T/b
report "tree contents (-a --jobs=8 --fast)" "$(cmp -s $T/a $T/b && echo ok)"

find $T/cp_out -printf '%P %m %u %g %s %Y\n' | sed 's|^cp_out|X|' | sort > $T/cpmeta
find $T/fc_out -printf '%P %m %u %g %s %Y\n' | sed 's|^fc_out|X|' | sort > $T/fcmeta
report "metadata (mode/owner/size/mtime)" "$(cmp -s $T/cpmeta $T/fcmeta && echo ok)"

cp_n=$(find $T/cp_out -type f -printf '%i\n' | sort -u | wc -l)
fc_n=$(find $T/fc_out -type f -printf '%i\n' | sort -u | wc -l)
report "hard links preserved" "$([ "$cp_n" == "$fc_n" ] && echo ok)"
echo "     ($cp_n inodes for $(find $T/fc_out -type f | wc -l) files)"

cp_b=$(stat -c %B $T/cp_out/deep/sparse.bin)
fc_b=$(stat -c %B $T/fc_out/deep/sparse.bin)
report "sparse holes preserved" "$([ "$cp_b" == "$fc_b" ] && echo ok)"
echo "     (allocated blocks: cp=$cp_b fastcopy=$fc_b)"

for f in link_file link_dir dangling; do
  a=$(readlink $T/cp_out/$f); b=$(readlink $T/fc_out/$f)
  report "symlink $f" "$([ "$a" == "$b" ] && echo ok)"
done

rm -rf $T/xcp $T/xfc
cp -ax $T/src $T/xcp
$FC -ax $T/src $T/xfc
report "-x one-file-system" "$(cmp -s <(sums $T/xcp) <(sums $T/xfc) && echo ok)"

echo "fuzz: $([ $fail == 0 ] && echo PASSED || echo FAILED)"
exit $fail
