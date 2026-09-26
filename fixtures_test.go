// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package xar

import (
	"bytes"
	_ "embed"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

// The fixtures are EMBEDDED, not read from testdata at run time.
//
// The CI matrix builds the test binary with `go test -c` and runs it inside a
// container for four emulated architectures, and testdata/ is not mounted
// there. A fixture read with os.ReadFile passes on the four native lanes and
// fails on the other four.
//
// Both archives were written by /usr/bin/xar 1.8dev and, before either was
// committed, the reference listed and extracted each one and every member was
// compared byte for byte against the source tree it came from — see
// testdata/gen.sh, which refuses to leave a fixture behind that its own
// reference cannot read back.

//go:embed testdata/mixed.xar
var mixedXar []byte

//go:embed testdata/bzip2.xar
var bzip2Xar []byte

// storeBin is the member of mixed.xar that xar was told not to compress. It is
// embedded separately so the test asserts the member's BYTES against the file
// that went in, rather than against anything this package computed.
//
//go:embed testdata/store.bin
var storeBin []byte

// bigTxt is the >64 KiB member. It is generated rather than embedded because it
// is 80,000 bytes of a repeating pattern, and a pattern stated here is an
// expectation independent of the archive.
func bigTxt() []byte { return bytes.Repeat([]byte("abcdefgh"), 10000) }

// openFixture opens an embedded archive, failing the test if it will not open.
func openFixture(t *testing.T, raw []byte) *FS {
	t.Helper()
	f, err := OpenReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	return f.(*FS)
}

// want describes one member as the reference recorded it.
type want struct {
	path string
	// mode is the full POSIX st_mode: type bits from <type>, permissions from
	// <mode>.
	mode uint16
	// style is the <encoding style> the archive carries. Asserted so that the
	// corpus is PROVEN to reach each decoder rather than assumed to.
	style string
	body  []byte
	link  string
	dir   bool
}

// mixedWant is every member of mixed.xar.
func mixedWant() []want {
	return []want{
		{path: "/plain.txt", mode: modeRegular | 0o755, style: encodingZlib, body: []byte("hello xar\n")},
		// An empty file. xar emits NO <data> element for one, so the style is
		// empty and the stored path serves it.
		{path: "/empty.txt", mode: modeRegular | 0o644, style: "", body: []byte{}},
		{path: "/nested", mode: modeDir | 0o755, dir: true},
		{path: "/nested/deep", mode: modeDir | 0o755, dir: true},
		{path: "/nested/deep/file.txt", mode: modeRegular | 0o600, style: encodingZlib, body: []byte("nested content here\n")},
		{path: "/big.txt", mode: modeRegular | 0o644, style: encodingZlib, body: bigTxt()},
		{path: "/alias.lnk", mode: modeSymlink | 0o755, link: "plain.txt"},
		// Setuid, which is the mode xar writes WITHOUT a leading zero.
		{path: "/setuid.sh", mode: modeRegular | 0o4755, style: encodingZlib, body: []byte("setuid\n")},
		{path: "/store.bin", mode: modeRegular | 0o644, style: encodingStored, body: storeBin},
	}
}

// bzip2Want is every member of bzip2.xar.
func bzip2Want() []want {
	return []want{
		{path: "/plain.txt", mode: modeRegular | 0o755, style: encodingBzip2, body: []byte("hello xar\n")},
		{path: "/nested", mode: modeDir | 0o755, dir: true},
		{path: "/nested/deep", mode: modeDir | 0o755, dir: true},
		{path: "/nested/deep/file.txt", mode: modeRegular | 0o600, style: encodingBzip2, body: []byte("nested content here\n")},
	}
}

// statOf is Stat, failing the test rather than returning an error.
func statOf(t *testing.T, f filesystem.Filesystem, p string) filesystem.Stat {
	t.Helper()
	st, err := f.Stat(p)
	if err != nil {
		t.Fatalf("Stat(%q): %v", p, err)
	}
	return st
}
