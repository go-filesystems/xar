# xar

A pure-Go, read-only reader for the **XAR** (eXtensible ARchive) format — the
container a macOS `.pkg` installer is, and the format `/usr/bin/xar` writes.

`CGO_ENABLED=0`, no dependency outside the standard library except
[`go-filesystems/interface`](https://github.com/go-filesystems/interface).
`compress/zlib`, `compress/bzip2` and `encoding/xml` do all the decoding.

## Use

```go
import (
	"os"

	"github.com/go-filesystems/xar"
)

f, err := os.Open("Installer.pkg")
if err != nil {
	return err
}
defer f.Close()
st, err := f.Stat()
if err != nil {
	return err
}

fsys, err := xar.OpenReader(f, st.Size())
if err != nil {
	return err
}
defer fsys.Close()

entries, err := fsys.ListDir("/")     // Distribution, PackageInfo, Bom, Payload…
data, err := fsys.ReadFile("/PackageInfo")
```

`OpenReader` returns a `filesystem.Filesystem`. Only the header and the table of
contents are read; each member is decoded when it is read.

The optional `filesystem.Opener` capability is implemented, so a member can be
streamed rather than materialised:

```go
if o, ok := fsys.(filesystem.Opener); ok {
	h, err := o.OpenFile("/Payload")
	if err != nil {
		return err
	}
	defer h.Close()
	_, err = io.Copy(dst, io.NewSectionReader(h, 0, h.Size()))
}
```

A stored member is served straight from the backing `io.ReaderAt`: real random
access, nothing buffered, no lock. A compressed member has no random access to
give, because a zlib or bzip2 stream can only be read forwards from its start,
so the handle keeps a decoder and its position — a read ahead of it discards
forward, a read behind it restarts the stream. Memory is O(1) either way, and a
sequential reader pays one pass in total.

An archive is read-only. `WriteFile`, `MkDir`, `DeleteFile`, `DeleteDir` and
`Rename` return `ErrReadOnly`, which wraps `fs.ErrPermission`. A path that is
not there wraps `fs.ErrNotExist`; a corrupt archive deliberately does **not**,
so a damaged file cannot reach a user as a routine 404.

## The layout

| offset | size | field |
|---|---|---|
| 0 | 4 | magic, `xar!` (`0x78617221`) |
| 4 | 2 | `HeaderSize` — the size of this header |
| 6 | 2 | `Version` |
| 8 | 8 | `TOCLengthCompressed` |
| 16 | 8 | `TOCLengthUncompressed` |
| 24 | 4 | `ChecksumAlgorithm` |

Then `TOCLengthCompressed` bytes of zlib-compressed XML, then the heap. The heap
begins at `HeaderSize + TOCLengthCompressed`, and every member's `<offset>` is
relative to that — never to the file.

`HeaderSize`, not the constant 28, is the base. The field exists so the header
can grow, and both halves of the sum matter: using `TOCLengthUncompressed`
instead lands in the middle of the heap, because the *compressed* table of
contents is what physically occupies the file.

The table of contents is `<xar><toc>`, with `<file>` elements nested inside one
another to express directories. There is no path string anywhere in the format,
only a `<name>` per level, so a path exists only once the nesting has been
walked.

## Four things the format does that a reader will get wrong

Each was observed in an archive written by `xar 1.8dev`, not inferred from a
specification.

1. **`encoding style="application/x-gzip"` is not gzip.** It is a raw zlib
   stream (RFC 1950) — `78 da`, no gzip member header, no gzip trailer.
   `compress/gzip` refuses it outright, so at least it fails loudly; the naming
   is the trap. Stored is `application/octet-stream`, and bzip2 is
   `application/x-bzip2`.

2. **An empty file has no `<data>` element at all.** Not `<data>` with
   `<size>0</size>` — the element is simply absent. A reader that requires
   `<data>` loses every empty member.

3. **`<mode>` is octal and its leading zero is optional.** xar writes
   `<mode>0644</mode>` for an ordinary file but `<mode>4755</mode>` for a setuid
   one. Both are octal, so `strconv.ParseUint` with base 0 is wrong in a way no
   leading-zero mode can reveal: it reads `0644` as octal, *because of the
   zero*, and `4755` as decimal. Only an explicit base 8 is right.

4. **`<ea>` looks exactly like `<data>`.** An extended attribute lives in the
   heap with its own `<offset>`, `<length>`, `<size>`, `<encoding>` — and its own
   `<name>`. A reader that searches a `<file>` subtree for any descendant
   offset serves an xattr's bytes as the file's contents, most visibly on an
   empty file, which has an `<ea>` and no `<data>`. One that searches for any
   descendant name calls the file `com.apple.provenance`.

## What real `.pkg` files use, measured

The encoding names make `application/x-gzip` look like the format's default,
because it is what `xar` writes unless told otherwise. Real installers do not
agree, and **a reader that supports only zlib cannot read Apple's own packages**:

| package | member | encoding | heap → decoded |
|---|---|---|---|
| Safari27.0TahoeAuto.pkg (Apple, signed) | `Payload` | `application/octet-stream` | 255,426,183 → 255,426,183 |
| | `Scripts` | `application/octet-stream` | 18,216 → 18,216 |
| | `PackageInfo` | `application/x-bzip2` | 529 → 1,002 |
| | `Bom` | `application/x-bzip2` | 3,448 → 63,198 |
| GLPI-Agent-1.7.3_arm64.pkg (third party, signed) | `Payload` | `application/octet-stream` | 21,609,167 → 21,609,167 |
| | `Bom` | `application/x-gzip` | 264,415 → 1,169,586 |
| | `License.txt` | `application/x-gzip` | 6,819 → 17,987 |

The `Payload` — the part holding the installed files, and by far the largest
member — is **stored** in both, which is why it is served straight from the
backing `io.ReaderAt` with nothing buffered. Apple compresses its metadata with
**bzip2** and not with zlib at all, so `compress/bzip2` is required to read a
macOS system package rather than being an extra.

Every member above was read through `Opener` and compared against
`xar -xf`: all byte-identical. Streaming the 255 MB `Payload` peaked at **4.9 MB**
of resident memory, and the 1.1 MB zlib `Bom` at 5.0 MB, which is the O(1) claim
above measured rather than asserted.

Both packages are signed. The `<signature>`/`<x-signature>` and X509 elements sit
beside `<file>` under `<toc>` and are ignored, which is what lets a signed archive
be read at all — it is **not** a signature check. The GLPI package's
`Distribution` member also has no `<mode>` element at all, and an absent mode is
reported as zero permission bits, because that is what the archive recorded.

## What this package does not do

The header's `ChecksumAlgorithm`, the table of contents' own `<checksum>`, and
each member's `<extracted-checksum>`/`<archived-checksum>` are parsed past, not
verified. **Nothing here authenticates an archive**; a `.pkg` signature in
particular is not checked, and a caller that needs provenance must establish it
by other means.

LZMA members (`application/x-lzma`) are refused with `ErrUnsupportedEncoding`.
The refusal is per member and is raised when the data is read, so an archive
with one LZMA member still opens and lists. It is untested against a real
archive on purpose: macOS's `xar 1.8dev` reports *"lzma support not compiled
in"* and cannot write one, and a fixture hand-built to agree with this reader
would prove nothing about it.

Extended attributes, uid/gid, the timestamps and the Finder metadata are read
past. Symlinks are reported, with their target, but **not followed** — a link
target is a string the archive supplied and may name anything at all.

## Fixtures

`testdata/*.xar` are written by the reference implementation, and
`testdata/gen.sh` regenerates them. The script **asserts its own premise**: it
makes the reference list and extract each archive and compares every member byte
for byte against the source tree before leaving the fixture behind. Two traps it
records:

- `xar -cf out.xar -C srcdir files…` prints `Error adding file X` for every
  member **and exits 0**, leaving a 208-byte archive with an empty table of
  contents. xar has to be run with its working directory already inside the
  source tree.
- `xar --compression=lzma` reports *"lzma support not compiled in"*, so no LZMA
  fixture can be produced on macOS.

The fixtures are **embedded** with `//go:embed`, not read from `testdata` at run
time: the CI matrix builds the test binary with `go test -c` and runs it in a
container for four emulated architectures, where `testdata/` is not mounted.

## Tests

100% statement coverage, gated in CI on four native lanes (linux/amd64,
linux/arm64, darwin/arm64, windows/amd64) plus four emulated ones (riscv64,
loong64, ppc64le, s390x).

Every **positive** claim is made against an archive the reference wrote. The
hand-built archives in `craft_test.go` exist only to be **refused** — the
reference cannot produce a truncated table of contents or a mode that is not
octal — and they are kept honest by `TestCraftBaselineOpens`: the builder's
unmutated output must open and read correctly, so every refusal differs from a
working archive by exactly one thing.

Each decision was ablated one at a time and the ablation re-run to see which
test caught it. Three ablations initially **passed**, and each was a missing
assertion rather than a harmless one:

| ablation | caught by, before | fix |
|---|---|---|
| heap base uses the constant 28 instead of `HeaderSize` | **nothing** — every archive xar writes declares 28, so the field and the constant are the same number in every real fixture | `TestDeclaredHeaderSizeLocatesEverything`, a crafted archive with a 32-byte header |
| the declared `TOCLengthUncompressed` is not enforced | **nothing** — the existing case declared one byte fewer, which cut the XML mid-document, so the XML parser caught it and the length check was never reached | `TestTOCLongerThanDeclaredWithValidPrefix`, whose table of contents ends in a newline so the short prefix is still valid XML |
| `ReadFile` sizes its buffer from the untrusted `<size>` | **nothing that mattered** — the test declared 2 GiB and asserted an error, and the allocating version allocated 2 GiB and returned the same error | the declared size is now 256 TiB, which a reader that trusts it cannot survive |

## Licence

BSD-3-Clause.
