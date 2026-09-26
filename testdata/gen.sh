#!/bin/sh
# Regenerates the XAR fixtures with the reference implementation, /usr/bin/xar.
#
# Run from this directory:  sh gen.sh
#
# The script ASSERTS ITS OWN PREMISE before leaving anything behind: after
# writing each archive it makes the reference list it and extract it, and
# compares the extracted tree byte for byte against the source tree. A
# generator that exits 0 without having done the work is worse than one that
# fails, so nothing here is trusted on an exit status alone.
#
# Two traps met while writing this:
#
#   - `xar -cf out.xar -C srcdir files...` prints "Error adding file X" for
#     every member and still exits 0, leaving a 208-byte archive with an empty
#     table of contents. xar must be run with its working directory already
#     inside the source tree.
#   - `xar --compression=lzma` reports "lzma support not compiled in" on
#     macOS's xar 1.8dev, so no LZMA fixture can be produced here. See
#     encodingLZMA in encoding.go for what the reader does with one anyway.
set -e

xar --version

src=$(mktemp -d)
trap 'rm -rf "$src"' EXIT

# A small file whose mode is deliberately not 0644.
printf 'hello xar\n' > "$src/plain.txt"
chmod 0755 "$src/plain.txt"

# An empty file. xar emits NO <data> element at all for one, which is the
# single most surprising thing about the format.
: > "$src/empty.txt"
chmod 0644 "$src/empty.txt"

# A nested directory, two levels deep, with a 0600 leaf.
mkdir -p "$src/nested/deep"
printf 'nested content here\n' > "$src/nested/deep/file.txt"
chmod 0600 "$src/nested/deep/file.txt"

# Over 64 KiB, and highly compressible so the zlib path is definitely taken.
i=0
while [ $i -lt 10000 ]; do printf 'abcdefgh'; i=$((i + 1)); done > "$src/big.txt"

# A symlink: <type>symlink</type> plus <link type="file">plain.txt</link>.
ln -s plain.txt "$src/alias.lnk"

# Setuid, which is how the corpus reaches the OTHER shape of <mode>. xar writes
# an ordinary mode with a leading zero -- <mode>0644</mode> -- but writes a
# setuid one WITHOUT it: <mode>4755</mode>. Both are octal. A corpus of only
# leading-zero modes cannot tell strconv base 8 from base 0, because base 0
# reads "0644" as octal by the leading zero and "4755" as DECIMAL.
printf 'setuid\n' > "$src/setuid.sh"
chmod 4755 "$src/setuid.sh"

# 4096 incompressible bytes, forced to the `none` encoding by --no-compress so
# that ONE archive reaches both the stored and the deflated path.
cp store.bin "$src/store.bin"

# An extended attribute, so the fixture carries the <ea> elements that make a
# careless reader wrong in two separate ways. An <ea> has its own <offset>,
# <length>, <size> and <encoding> -- and its own <name>. On empty.txt, which has
# NO <data> element at all, a reader that takes any descendant offset/length
# hands back these marker bytes as the file's contents, and a reader that takes
# any descendant <name> calls the file "com.example.marker".
xattr -w com.example.marker "MARKER-BYTES-NOT-FILE-CONTENT" "$src/empty.txt"
xattr -w com.example.marker "MARKER-BYTES-NOT-FILE-CONTENT" "$src/plain.txt"

( cd "$src" && xar -cf mixed.xar --no-compress '\.bin$' \
    plain.txt empty.txt nested big.txt alias.lnk setuid.sh store.bin )
( cd "$src" && xar --compression=bzip2 -cf bzip2.xar plain.txt nested )

for a in mixed.xar bzip2.xar; do
    echo "--- $a"
    ( cd "$src" && xar -tf "$a" )
    out=$(mktemp -d)
    ( cd "$out" && xar -xf "$src/$a" )
    # The reference must read back what it just wrote, byte for byte, BEFORE
    # this fixture is allowed to judge any Go code.
    ( cd "$src" && xar -tf "$a" ) | while read -r p; do
        [ -f "$src/$p" ] || continue
        cmp "$src/$p" "$out/$p"
    done
    rm -rf "$out"
    cp "$src/$a" .
    echo "--- $a round-tripped by the reference"
done
