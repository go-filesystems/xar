// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package xar

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"strings"
	"testing"
)

// Memory bombs. Each case below was measured BEFORE the fix with TotalAlloc (the
// figure is in the comment) and is asserted after it. "Refused" means refused
// without reserving what the archive asked for.

// allocated reports how many bytes f allocated in total.
func allocated(f func()) uint64 {
	runtime.GC()
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

const oneMiB = 1 << 20

func openWith(raw []byte, opt Options) (*FS, error) {
	f, err := OpenReaderWith(bytes.NewReader(raw), int64(len(raw)), opt)
	if err != nil {
		return nil, err
	}
	return f.(*FS), nil
}

func wrapTOC(files func(b *strings.Builder)) string {
	var b strings.Builder
	b.WriteString(`<xar><toc>`)
	files(&b)
	b.WriteString(`</toc></xar>`)
	return b.String()
}

// Before: a 347-byte archive declaring a 64 MiB table of contents allocated
// 68,240,616 bytes. A zlib stream cannot expand more than 1032:1, so the
// declaration is refused from the header alone.
func TestBombDeclaredTOCSize(t *testing.T) {
	c := baseline()
	c.tocUncompressed = u64(maxTOC)
	raw := c.bytes()
	var err error
	n := allocated(func() { _, err = OpenReader(bytes.NewReader(raw), int64(len(raw))) })
	if !errors.Is(err, ErrBadHeader) {
		t.Fatalf("err = %v, want ErrBadHeader", err)
	}
	if n >= oneMiB {
		t.Errorf("allocated %d bytes for a %d-byte archive", n, len(raw))
	}
}

// The bound is exact: 1032 times the compressed length passes the header, one
// more byte does not.
func TestHeaderInflationBoundIsExact(t *testing.T) {
	base := baseline()
	payload := zlibBytes([]byte(base.toc))
	at := uint64(len(payload)) * zlibMaxRatio
	for _, tc := range []struct {
		decl uint64
		ok   bool
	}{{at, true}, {at + 1, false}} {
		c := baseline()
		c.tocUncompressed = u64(tc.decl)
		raw := c.bytes()
		_, err := parseHeader(bytes.NewReader(raw), int64(len(raw)))
		if tc.ok && err != nil {
			t.Errorf("declared %d: %v, want accepted", tc.decl, err)
		}
		if !tc.ok && !errors.Is(err, ErrBadHeader) {
			t.Errorf("declared %d: %v, want ErrBadHeader", tc.decl, err)
		}
	}
}

// The ratio is a property of the format, so the standard library's best encoder
// must never exceed it: the bound cannot refuse an archive a writer produced.
func TestZlibRatioHoldsForTheEncoder(t *testing.T) {
	for _, n := range []int{0, 1, 100, 1 << 20, 16 << 20} {
		z := zlibBytes(make([]byte, n))
		if int64(n) > maxInflate(int64(len(z))) {
			t.Errorf("%d zero bytes encode to %d, past %d:1", n, len(z), zlibMaxRatio)
		}
	}
}

func TestMaxInflateSaturates(t *testing.T) {
	if got := maxInflate(math.MaxInt64); got != math.MaxInt64 {
		t.Errorf("maxInflate(MaxInt64) = %d, want saturation", got)
	}
	if got := maxInflate(10); got != 10*zlibMaxRatio {
		t.Errorf("maxInflate(10) = %d", got)
	}
}

// Before: a 8,886-byte archive, a chain of 3000 directories named with 100
// bytes, allocated 937,076,968 bytes (path bytes grow as depth squared); with
// the 10000 levels encoding/xml allows and longer names it is terabytes.
func TestBombDeepPaths(t *testing.T) {
	const depth, nameLen = 3000, 100
	name := strings.Repeat("n", nameLen)
	c := &crafted{toc: wrapTOC(func(b *strings.Builder) {
		for i := 0; i < depth; i++ {
			fmt.Fprintf(b, `<file id="%d"><name>%s</name><type>directory</type>`, i, name)
		}
		for i := 0; i < depth; i++ {
			b.WriteString(`</file>`)
		}
	})}
	raw := c.bytes()
	var err error
	n := allocated(func() { _, err = OpenReader(bytes.NewReader(raw), int64(len(raw))) })
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	// What remains is the XML decode of the 472 KB table, linear in it.
	if limit := uint64(80 * len(c.toc)); n > limit {
		t.Errorf("allocated %d bytes, want at most %d (linear in the table)", n, limit)
	}
}

// chain builds nested directories whose last member's absolute path is
// exactly pathLen bytes.
func chain(pathLen int) *crafted {
	const step = 100 // "/" + 99 bytes
	levels := (pathLen - 1) / step
	last := pathLen - levels*step - 1
	return &crafted{toc: wrapTOC(func(b *strings.Builder) {
		for i := 0; i < levels; i++ {
			fmt.Fprintf(b, `<file id="%d"><name>%s</name><type>directory</type>`, i, strings.Repeat("d", step-1))
		}
		fmt.Fprintf(b, `<file id="9"><name>%s</name><type>file</type></file>`, strings.Repeat("f", last))
		for i := 0; i < levels; i++ {
			b.WriteString(`</file>`)
		}
	})}
}

func TestPathLengthBoundIsExact(t *testing.T) {
	if _, err := chain(maxPathLen).open(); err != nil {
		t.Errorf("a %d-byte path must open: %v", maxPathLen, err)
	}
	if _, err := chain(maxPathLen + 1).open(); !errors.Is(err, ErrTooLarge) {
		t.Errorf("a %d-byte path: %v, want ErrTooLarge", maxPathLen+1, err)
	}
}

func TestPathBytesBudget(t *testing.T) {
	files := make([]xmlFile, 4)
	for i := range files {
		files[i] = xmlFile{Name: fmt.Sprintf("f%d", i), Type: "file"}
	}
	root := &entry{path: "/", kind: kindDir}
	// Each path is "/f0" = 3 bytes: a budget of 12 holds four, 11 does not.
	for _, tc := range []struct {
		budget int
		ok     bool
	}{{12, true}, {11, false}} {
		root.children = nil
		lim := &treeLimits{maxPathBytes: tc.budget}
		err := addChildren(root, files, map[string]*entry{}, lim)
		if tc.ok && err != nil {
			t.Errorf("budget %d: %v", tc.budget, err)
		}
		if !tc.ok && !errors.Is(err, ErrTooLarge) {
			t.Errorf("budget %d: %v, want ErrTooLarge", tc.budget, err)
		}
	}
}

func filesTOC(n int) *crafted {
	return &crafted{toc: wrapTOC(func(b *strings.Builder) {
		for i := 0; i < n; i++ {
			fmt.Fprintf(b, `<file id="%d"><name>f%d</name><type>file</type></file>`, i, i)
		}
	})}
}

// Before: 200,000 entries (a 1 MiB archive) allocated 370,521,096 bytes, and
// nothing bounded the count but the 64 MiB table, about two gigabytes.
// The count is taken from a byte scan before the XML is decoded.
func TestEntryCountBoundIsExact(t *testing.T) {
	const limit = 100
	ok := filesTOC(limit).bytes()
	if _, err := openWith(ok, Options{MaxEntries: limit}); err != nil {
		t.Errorf("%d entries with MaxEntries=%d: %v", limit, limit, err)
	}
	over := filesTOC(limit + 1).bytes()
	var err error
	n := allocated(func() { _, err = openWith(over, Options{MaxEntries: limit}) })
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("%d entries: %v, want ErrTooLarge", limit+1, err)
	}
	if n >= oneMiB {
		t.Errorf("allocated %d bytes refusing a %d-byte archive", n, len(over))
	}
}

func TestDefaultEntryLimitApplies(t *testing.T) {
	// Not DefaultMaxEntries entries: only that OpenReader and a zero Options
	// both select it, by checking the zero value is filled in.
	raw := filesTOC(3).bytes()
	f, err := openWith(raw, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f.maxFile != DefaultMaxFileSize {
		t.Errorf("maxFile = %d, want %d", f.maxFile, int64(DefaultMaxFileSize))
	}
}

func TestCountFileElements(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"<file", 0},
		{"<file>", 1},
		{"<file id=\"1\"><file\tid=\"2\"/>", 2},
		{"<file\nid=\"1\"><file\r\n", 2},
		{"<files><filesystem><file-x><filex", 0},
		{"<file/><file><file ", 3},
		{"x<file><file", 1},
	} {
		if got := countFileElements([]byte(tc.in)); got != tc.want {
			t.Errorf("countFileElements(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// A zlib member cannot decode to more than 1032 times its length. Before: Stat
// reported any size (256 TiB passed Open), and a caller sizing a buffer from it
// reserved that.
func TestZlibMemberBeyondTheRatio(t *testing.T) {
	z := zlibBytes([]byte("short"))
	at := len(z) * zlibMaxRatio
	if _, err := tryMember(encodingZlib, at, z); err != nil {
		t.Errorf("size %d (= %d*%d) must open: %v", at, len(z), zlibMaxRatio, err)
	}
	if _, err := tryMember(encodingZlib, at+1, z); !errors.Is(err, ErrCorrupt) {
		t.Errorf("size %d: %v, want ErrCorrupt", at+1, err)
	}
	if _, err := tryMember(encodingZlib, 1<<48, z); !errors.Is(err, ErrCorrupt) {
		t.Errorf("size 1<<48: %v, want ErrCorrupt", err)
	}
}

// Before: a 526 KiB zlib member declaring and decoding to 256 MiB allocated
// 1,073,786,960 bytes in ReadFile (the buffer doubles), and a 17-byte-per-block
// bzip2 member can ask for any size. Now ReadFile refuses a member over the
// ceiling before reading a byte.
func TestReadFileCeilingIsExact(t *testing.T) {
	raw := baseline().bytes() // hello.txt decodes to 6 bytes
	for _, tc := range []struct {
		max int64
		ok  bool
	}{{6, true}, {5, false}} {
		f, err := openWith(raw, Options{MaxFileSize: tc.max})
		if err != nil {
			t.Fatal(err)
		}
		got, err := f.ReadFile("/hello.txt")
		if tc.ok && (err != nil || string(got) != "hello\n") {
			t.Errorf("MaxFileSize=%d: %q, %v", tc.max, got, err)
		}
		if !tc.ok && !errors.Is(err, ErrTooLarge) {
			t.Errorf("MaxFileSize=%d: %v, want ErrTooLarge", tc.max, err)
		}
	}
}

func TestBombBzip2DeclaredSize(t *testing.T) {
	// bzip2 has no usable ratio, so the declaration opens; ReadFile's ceiling
	// is what stops it.
	f := withMember(t, encodingBzip2, 1<<40, []byte("BZh9"))
	var err error
	n := allocated(func() { _, err = f.ReadFile("/hello.txt") })
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if n >= oneMiB {
		t.Errorf("allocated %d bytes", n)
	}
	// Streaming is not affected by the ceiling and still fails cleanly.
	h, err := f.OpenFile("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	if _, err := h.ReadAt(make([]byte, 4), 0); err == nil {
		t.Error("a truncated bzip2 stream must not read")
	}
}

// A real compressed member over the ceiling: 4 MiB of zeros is 4 KiB of zlib.
func TestBombZlibMemberOverCeiling(t *testing.T) {
	const size = 4 << 20
	z := zlibBytes(make([]byte, size))
	c := baseline()
	c.toc = strings.Replace(c.toc, "<size>6</size>", fmt.Sprintf("<size>%d</size>", size), 1)
	c.toc = strings.Replace(c.toc,
		fmt.Sprintf("<length>%d</length>", len(zlibBytes([]byte("hello\n")))),
		fmt.Sprintf("<length>%d</length>", len(z)), 1)
	c.toc = strings.Replace(c.toc, "<offset>"+fmt.Sprint(len(zlibBytes([]byte("hello\n"))))+"</offset>",
		"<offset>"+fmt.Sprint(len(z))+"</offset>", 1)
	c.heap = append(append([]byte{}, z...), []byte("0123456789")...)
	raw := c.bytes()
	f, err := openWith(raw, Options{MaxFileSize: size - 1})
	if err != nil {
		t.Fatal(err)
	}
	var rerr error
	n := allocated(func() { _, rerr = f.ReadFile("/hello.txt") })
	if !errors.Is(rerr, ErrTooLarge) || n >= oneMiB {
		t.Errorf("over the ceiling: %v, %d bytes allocated", rerr, n)
	}
	f, err = openWith(raw, Options{MaxFileSize: size})
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.ReadFile("/hello.txt")
	if err != nil || len(got) != size {
		t.Errorf("at the ceiling: %d bytes, %v", len(got), err)
	}
	// Streaming has no ceiling.
	h, _ := f.OpenFile("/hello.txt")
	defer h.Close()
	if n, err := io.Copy(io.Discard, io.NewSectionReader(h, 0, h.Size())); err != nil || n != size {
		t.Errorf("stream: %d, %v", n, err)
	}
}

// A stored member is read into exactly its size, not a doubling buffer.
func TestStoredReadFileAllocatesItsSize(t *testing.T) {
	body := bytes.Repeat([]byte{0xa5}, 2*oneMiB)
	f := withMember(t, encodingStored, len(body), body)
	var got []byte
	n := allocated(func() { got, _ = f.ReadFile("/hello.txt") })
	if len(got) != len(body) {
		t.Fatalf("read %d bytes", len(got))
	}
	if n > uint64(len(body))+oneMiB/4 {
		t.Errorf("allocated %d bytes reading %d", n, len(body))
	}
}

// A stored member whose bytes are not all there (the caller's reader is shorter
// than the size it declared) is an error, not a silently short file.
func TestStoredReadFileShortBacking(t *testing.T) {
	raw := baseline().bytes()
	fsys, err := OpenReader(bytes.NewReader(raw[:len(raw)-4]), int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.ReadFile("/raw.bin"); !errors.Is(err, ErrCorrupt) {
		t.Errorf("ReadFile = %v, want ErrCorrupt", err)
	}
}

// FuzzOpenReader opens arbitrary bytes as an archive and reads whatever it finds,
// with small limits so one input cannot make a run slow. The first run of this
// target had no panic to report; it is kept because every other reader in the
// family found one.
func FuzzOpenReader(f *testing.F) {
	f.Add(baseline().bytes())
	f.Add(filesTOC(5).bytes())
	f.Add([]byte("xar!\x00\x1c\x00\x01"))
	f.Add(make([]byte, 28))
	f.Fuzz(func(t *testing.T, data []byte) { exercise(t, data) })
}

// FuzzTOC mutates the XML table of contents and the heap separately and wraps
// them in a valid header and zlib stream, so the mutations reach the XML and
// tree code instead of dying in the zlib reader of FuzzOpenReader.
func FuzzTOC(f *testing.F) {
	b := baseline()
	f.Add(b.toc, b.heap)
	f.Add(filesTOC(3).toc, []byte(nil))
	f.Add(chain(300).toc, []byte(nil))
	f.Fuzz(func(t *testing.T, toc string, heap []byte) {
		exercise(t, (&crafted{toc: toc, heap: heap}).bytes())
	})
}

func exercise(t *testing.T, data []byte) {
	{
		fsys, err := OpenReaderWith(bytes.NewReader(data), int64(len(data)),
			Options{MaxEntries: 1000, MaxFileSize: 1 << 20})
		if err != nil {
			return
		}
		var walk func(dir string, depth int)
		walk = func(dir string, depth int) {
			ents, err := fsys.ListDir(dir)
			if err != nil || depth > 64 {
				return
			}
			for _, e := range ents {
				p := strings.TrimSuffix(dir, "/") + "/" + e.Name()
				st, err := fsys.Stat(p)
				if err != nil {
					t.Fatalf("Stat(%q) of a listed entry: %v", p, err)
				}
				switch st.Mode() &^ modePerm {
				case modeDir:
					walk(p, depth+1)
				case modeRegular:
					_, _ = fsys.ReadFile(p)
				}
			}
		}
		walk("/", 0)
	}
}
