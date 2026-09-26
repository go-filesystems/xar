// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package xar

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// This file holds the only archives in the suite that /usr/bin/xar did not
// write, and they exist for one purpose: to be REFUSED.
//
// The reference cannot produce a truncated table of contents, a mode that is
// not octal, or two members with one name, so the refusals cannot be tested
// against it. That is the whole risk of a hand-built fixture, and the reason
// every positive claim in this package is made against testdata/*.xar instead:
// an archive built by the code under test's own author, judged by that code,
// agrees with it by construction.
//
// What keeps these honest is TestCraftBaselineOpens. The builder's unmutated
// output must open and read correctly, and every case below is that baseline
// plus exactly ONE change. A crafted archive that was refused for some second,
// accidental reason would show up as a baseline that does not open.

// crafted assembles an archive. Each pointer field, when set, overrides what
// the builder would otherwise compute, so a case can make one field wrong
// without making anything else wrong.
type crafted struct {
	toc  string
	heap []byte

	magic           *uint32
	headerSize      *uint16
	version         *uint16
	tocCompressed   *uint64
	tocUncompressed *uint64
	// tocPayload replaces the compressed table of contents bytes.
	tocPayload []byte
	// truncate, when positive, cuts the finished archive to that many bytes.
	truncate int
}

func zlibBytes(p []byte) []byte {
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	if _, err := w.Write(p); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return b.Bytes()
}

// bytes renders the archive.
func (c *crafted) bytes() []byte {
	payload := c.tocPayload
	if payload == nil {
		payload = zlibBytes([]byte(c.toc))
	}
	hdr := make([]byte, minHeaderSize)
	put := func(i int, v uint32) { binary.BigEndian.PutUint32(hdr[i:], v) }
	put(0, Magic)
	binary.BigEndian.PutUint16(hdr[4:], minHeaderSize)
	binary.BigEndian.PutUint16(hdr[6:], version1)
	binary.BigEndian.PutUint64(hdr[8:], uint64(len(payload)))
	binary.BigEndian.PutUint64(hdr[16:], uint64(len(c.toc)))
	put(24, 1)

	if c.magic != nil {
		put(0, *c.magic)
	}
	if c.headerSize != nil {
		binary.BigEndian.PutUint16(hdr[4:], *c.headerSize)
	}
	if c.version != nil {
		binary.BigEndian.PutUint16(hdr[6:], *c.version)
	}
	if c.tocCompressed != nil {
		binary.BigEndian.PutUint64(hdr[8:], *c.tocCompressed)
	}
	if c.tocUncompressed != nil {
		binary.BigEndian.PutUint64(hdr[16:], *c.tocUncompressed)
	}

	out := append(append(append([]byte{}, hdr...), payload...), c.heap...)
	if c.truncate > 0 {
		out = out[:c.truncate]
	}
	return out
}

func (c *crafted) open() (*FS, error) {
	raw := c.bytes()
	f, err := OpenReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	return f.(*FS), nil
}

// baseline is a small but complete archive: a zlib member, a stored member, a
// nested directory, and inside it a member with NO <data>, which is how XAR
// spells an empty file.
func baseline() *crafted {
	z := zlibBytes([]byte("hello\n"))
	raw := []byte("0123456789")
	toc := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<xar>
 <toc>
  <checksum style="sha1"><offset>0</offset><size>0</size></checksum>
  <file id="1">
   <data><length>%d</length><offset>0</offset><size>6</size><encoding style="%s"/></data>
   <mode>0644</mode><type>file</type><name>hello.txt</name>
  </file>
  <file id="2">
   <data><length>%d</length><offset>%d</offset><size>%d</size><encoding style="%s"/></data>
   <mode>0600</mode><type>file</type><name>raw.bin</name>
  </file>
  <file id="3">
   <mode>0755</mode><type>directory</type><name>d</name>
   <file id="4"><mode>0644</mode><type>file</type><name>inner.txt</name></file>
  </file>
 </toc>
</xar>`, len(z), encodingZlib, len(raw), len(z), len(raw), encodingStored)
	return &crafted{toc: toc, heap: append(append([]byte{}, z...), raw...)}
}

// TestCraftBaselineOpens is the control for every case in this file. If it ever
// fails, the refusals below stop meaning what they claim, because they would no
// longer differ from a working archive by only the thing under test.
func TestCraftBaselineOpens(t *testing.T) {
	f, err := baseline().open()
	if err != nil {
		t.Fatalf("the crafted baseline must open: %v", err)
	}
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/hello.txt", "hello\n"},
		{"/raw.bin", "0123456789"},
		{"/d/inner.txt", ""},
	} {
		got, err := f.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", tc.path, err)
		}
		if string(got) != tc.body {
			t.Errorf("ReadFile(%q) = %q, want %q", tc.path, got, tc.body)
		}
	}
}

func u16(v uint16) *uint16 { return &v }
func u32(v uint32) *uint32 { return &v }
func u64(v uint64) *uint64 { return &v }

// TestHeaderRefusals covers every way the 28-byte header can be wrong.
func TestHeaderRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*crafted)
		want error
	}{
		{"magic is not xar!", func(c *crafted) { c.magic = u32(0x21726178) }, ErrBadMagic},
		{"HeaderSize below the header it describes", func(c *crafted) { c.headerSize = u16(27) }, ErrBadHeader},
		// The unsigned-subtraction case: a HeaderSize past the end of the
		// archive. Checked on its own precisely because folding it into the
		// TOC-length comparison wraps 2^64 and accepts everything.
		{"HeaderSize past the end of the archive", func(c *crafted) { c.headerSize = u16(60000) }, ErrBadHeader},
		{"version is not 1", func(c *crafted) { c.version = u16(2) }, ErrUnsupportedVersion},
		{"TOCLengthCompressed is zero", func(c *crafted) { c.tocCompressed = u64(0) }, ErrBadHeader},
		{"TOCLengthUncompressed is zero", func(c *crafted) { c.tocUncompressed = u64(0) }, ErrBadHeader},
		{"TOCLengthUncompressed beyond the bound", func(c *crafted) { c.tocUncompressed = u64(maxTOC + 1) }, ErrBadHeader},
		{"TOCLengthCompressed past the end of the archive", func(c *crafted) { c.tocCompressed = u64(1 << 40) }, ErrBadHeader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := baseline()
			tc.mut(c)
			if _, err := c.open(); !errors.Is(err, tc.want) {
				t.Errorf("open = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestHeaderTooShort covers an archive shorter than the header itself.
func TestHeaderTooShort(t *testing.T) {
	for _, n := range []int{0, 4, 27} {
		raw := baseline().bytes()[:n]
		if _, err := OpenReader(bytes.NewReader(raw), int64(len(raw))); !errors.Is(err, ErrBadHeader) {
			t.Errorf("%d-byte archive = %v, want ErrBadHeader", n, err)
		}
	}
}

// errReaderAt fails every read, which is how the header's own I/O error is
// reached: a size that promises 28 bytes and a reader that will not give them.
type errReaderAt struct{ err error }

func (e errReaderAt) ReadAt([]byte, int64) (int, error) { return 0, e.err }

func TestHeaderReadError(t *testing.T) {
	sentinel := errors.New("device went away")
	_, err := OpenReader(errReaderAt{sentinel}, 4096)
	if !errors.Is(err, sentinel) {
		t.Errorf("open = %v, want the reader's own error", err)
	}
}

// TestTOCRefusals covers the table of contents: the zlib layer, the declared
// length, and the XML.
func TestTOCRefusals(t *testing.T) {
	base := baseline()
	for _, tc := range []struct {
		name string
		mut  func(*crafted)
		want error
	}{
		{
			"table of contents is not a zlib stream",
			func(c *crafted) { c.tocPayload = []byte("this is not zlib at all") },
			ErrCorrupt,
		},
		{
			// TOCLengthUncompressed larger than the bytes behind it.
			"table of contents shorter than declared",
			func(c *crafted) { c.tocUncompressed = u64(uint64(len(base.toc)) + 1) },
			ErrCorrupt,
		},
		{
			// TOCLengthUncompressed smaller than the bytes behind it. This is
			// the case a reader that merely ALLOCATED the declared length would
			// pass, having silently parsed a truncated document.
			"table of contents longer than declared",
			func(c *crafted) { c.tocUncompressed = u64(uint64(len(base.toc)) - 1) },
			ErrCorrupt,
		},
		{
			"table of contents is not XML",
			func(c *crafted) { c.toc = "<xar><toc><file id=\"1\"></toc></xar>" },
			ErrCorrupt,
		},
		{
			"root element is not <xar>",
			func(c *crafted) { c.toc = "<archive><toc></toc></archive>" },
			ErrCorrupt,
		},
		{
			"<file id> is not a number",
			func(c *crafted) { c.toc = strings.Replace(c.toc, `id="1"`, `id="one"`, 1) },
			ErrCorrupt,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := baseline()
			tc.mut(c)
			if _, err := c.open(); !errors.Is(err, tc.want) {
				t.Errorf("open = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestTOCTreeRefusals covers what the table of contents can say that the archive
// cannot contain.
func TestTOCTreeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*crafted)
	}{
		{"empty name", func(c *crafted) {
			c.toc = strings.Replace(c.toc, "<name>hello.txt</name>", "<name></name>", 1)
		}},
		{"name is a dot", func(c *crafted) {
			c.toc = strings.Replace(c.toc, "<name>hello.txt</name>", "<name>.</name>", 1)
		}},
		{"name climbs out of the directory", func(c *crafted) {
			c.toc = strings.Replace(c.toc, "<name>hello.txt</name>", "<name>..</name>", 1)
		}},
		{"name carries a separator", func(c *crafted) {
			c.toc = strings.Replace(c.toc, "<name>hello.txt</name>", "<name>a/b</name>", 1)
		}},
		{"two members with one name", func(c *crafted) {
			c.toc = strings.Replace(c.toc, "<name>raw.bin</name>", "<name>hello.txt</name>", 1)
		}},
		// The failure is on a member nested inside a directory, so this is also
		// the case that shows the refusal propagating back out of the recursion
		// rather than being lost a level down.
		{"a nested member's name is unusable", func(c *crafted) {
			c.toc = strings.Replace(c.toc, "<name>inner.txt</name>", "<name>..</name>", 1)
		}},
		{"mode is not octal", func(c *crafted) {
			c.toc = strings.Replace(c.toc, "<mode>0644</mode>", "<mode>rwxr-xr-x</mode>", 1)
		}},
		{"mode is out of range", func(c *crafted) {
			c.toc = strings.Replace(c.toc, "<mode>0644</mode>", "<mode>7777777777777</mode>", 1)
		}},
		{"negative offset", func(c *crafted) {
			c.toc = strings.Replace(c.toc, "<offset>0</offset><size>6</size>", "<offset>-1</offset><size>6</size>", 1)
		}},
		{"negative size", func(c *crafted) {
			c.toc = strings.Replace(c.toc, "<offset>0</offset><size>6</size>", "<offset>0</offset><size>-6</size>", 1)
		}},
		{"offset past the end of the archive", func(c *crafted) {
			c.toc = strings.Replace(c.toc, "<offset>0</offset><size>6</size>", "<offset>999999</offset><size>6</size>", 1)
		}},
		{"length runs past the end of the archive", func(c *crafted) {
			c.toc = strings.Replace(c.toc, "<offset>0</offset><size>6</size>", "<offset>1</offset><size>6</size>", 1)
			c.truncate = minHeaderSize + len(zlibBytes([]byte(c.toc))) + 2
		}},
		{"a stored member's length and size disagree", func(c *crafted) {
			c.toc = strings.Replace(c.toc, "<length>10</length>", "<length>9</length>", 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := baseline()
			tc.mut(c)
			if _, err := c.open(); !errors.Is(err, ErrCorrupt) {
				t.Errorf("open = %v, want ErrCorrupt", err)
			}
		})
	}
}

// withMember returns a baseline whose hello.txt has been given a different
// <data> block and heap content.
func withMember(t *testing.T, style string, size int, heap []byte) *FS {
	t.Helper()
	c := baseline()
	c.toc = strings.Replace(c.toc,
		fmt.Sprintf(`<data><length>%d</length><offset>0</offset><size>6</size><encoding style="%s"/></data>`,
			len(zlibBytes([]byte("hello\n"))), encodingZlib),
		fmt.Sprintf(`<data><length>%d</length><offset>0</offset><size>%d</size><encoding style="%s"/></data>`,
			len(heap), size, style), 1)
	// raw.bin sits after hello.txt in the heap, so move it and keep it valid.
	c.toc = strings.Replace(c.toc,
		fmt.Sprintf(`<offset>%d</offset>`, len(zlibBytes([]byte("hello\n")))),
		fmt.Sprintf(`<offset>%d</offset>`, len(heap)), 1)
	c.heap = append(append([]byte{}, heap...), []byte("0123456789")...)
	f, err := c.open()
	if err != nil {
		t.Fatalf("crafting a member with style %q: %v", style, err)
	}
	return f
}

// TestUnsupportedEncoding covers a style this package will not decode. The
// archive OPENS and the member is visible: the refusal is per member, so one
// member nobody can decode does not hide the rest of the archive.
func TestUnsupportedEncoding(t *testing.T) {
	f := withMember(t, "application/x-lzma", 6, []byte("\x5d\x00\x00\x80\x00"))

	if st := statOf(t, f, "/hello.txt"); st.Size() != 6 {
		t.Errorf("the member should still be visible; Size() = %d", st.Size())
	}
	if _, err := f.ListDir("/"); err != nil {
		t.Errorf("the archive should still list: %v", err)
	}
	if _, err := f.ReadFile("/hello.txt"); !errors.Is(err, ErrUnsupportedEncoding) {
		t.Errorf("ReadFile = %v, want ErrUnsupportedEncoding", err)
	}
	// OpenFile itself succeeds -- nothing is decoded there -- and the refusal
	// arrives on the first ReadAt, which is where the decoder is started.
	h, err := f.OpenFile("/hello.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer h.Close()
	if _, err := h.ReadAt(make([]byte, 4), 0); !errors.Is(err, ErrUnsupportedEncoding) {
		t.Errorf("ReadAt = %v, want ErrUnsupportedEncoding", err)
	}
	// And on a seek, which reaches the decoder through a different statement.
	if _, err := h.ReadAt(make([]byte, 1), 3); !errors.Is(err, ErrUnsupportedEncoding) {
		t.Errorf("ReadAt(off=3) = %v, want ErrUnsupportedEncoding", err)
	}
}

// TestDataRefusals covers a member whose heap bytes do not match what the table
// of contents said about them.
func TestDataRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		style string
		size  int
		heap  []byte
	}{
		// zlib.NewReader reads and validates the two-byte header, so a stream
		// that is not zlib fails at construction.
		{"not a zlib stream", encodingZlib, 6, []byte("not zlib")},
		// A valid zlib stream that decodes to fewer bytes than <size> claims.
		{"zlib member ends before its declared size", encodingZlib, 4096, zlibBytes([]byte("short"))},
		// bzip2.NewReader cannot fail; the stream is rejected from Read, which
		// is a DIFFERENT branch from the zlib case above -- a non-EOF error out
		// of the decoder rather than an early end.
		{"not a bzip2 stream", encodingBzip2, 64, []byte("BZh9 and then nonsense that is not bzip2")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := withMember(t, tc.style, tc.size, tc.heap)
			if _, err := f.ReadFile("/hello.txt"); err == nil {
				t.Fatal("ReadFile succeeded on a member the archive cannot supply")
			} else if !errors.Is(err, ErrCorrupt) && !isBzip2Error(err) {
				t.Errorf("ReadFile = %v, want a refusal", err)
			}
		})
	}
}

// isBzip2Error reports whether err came out of compress/bzip2 rather than from
// this package. compress/bzip2's StructuralError is passed through unwrapped:
// it is the decoder's own account of the stream and says more than ErrCorrupt
// would.
func isBzip2Error(err error) bool {
	return strings.Contains(err.Error(), "bzip2")
}

// TestSeekPastTheEndOfAShortStream reaches the discard inside seek with a
// decoder that runs out before the offset asked for. The member claims 4096
// bytes and its stream holds five, so a read at 1000 cannot get there.
func TestSeekPastTheEndOfAShortStream(t *testing.T) {
	f := withMember(t, encodingZlib, 4096, zlibBytes([]byte("short")))
	h, err := f.OpenFile("/hello.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer h.Close()
	if _, err := h.ReadAt(make([]byte, 8), 1000); !errors.Is(err, ErrCorrupt) {
		t.Errorf("ReadAt(1000) = %v, want ErrCorrupt", err)
	}
}

// TestSpecialTypes covers the <type> values XAR can record that are neither a
// file, a directory nor a symlink. They stay visible, and they are refused as
// contents rather than becoming empty regular files.
func TestSpecialTypes(t *testing.T) {
	c := baseline()
	// Also drops <mode> from this member, which is the absent-mode case.
	c.toc = strings.Replace(c.toc,
		"<mode>0755</mode><type>directory</type><name>d</name>",
		"<type>fifo</type><name>d</name>", 1)
	f, err := c.open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if st := statOf(t, f, "/d"); st.Mode() != modeFIFO {
		t.Errorf("Stat(/d).Mode() = %#o, want %#o (fifo, no permissions recorded)", st.Mode(), uint16(modeFIFO))
	}
	if _, err := f.ReadFile("/d"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("ReadFile(/d) = %v, want ErrNotRegular", err)
	}
	// A fifo is not a directory, so its nested <file> children are unreachable
	// through it -- the walk stops at a non-directory rather than descending.
	if _, err := f.Stat("/d/inner.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat(/d/inner.txt) = %v, want ErrNotFound", err)
	}
	entries, err := f.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "d" && e.FileType() != 0 {
			t.Errorf("a fifo's FileType() = %d, want 0", e.FileType())
		}
	}
}

// TestReadFileGrowsRatherThanTrusting is the allocation guard. The member says
// it is 2 GiB and its stream holds five bytes; ReadFile must fail on the data
// without having tried to reserve the declared size.
func TestReadFileGrowsRatherThanTrusting(t *testing.T) {
	f := withMember(t, encodingZlib, 2<<30, zlibBytes([]byte("short")))
	got, err := f.ReadFile("/hello.txt")
	if err == nil {
		t.Fatalf("ReadFile returned %d bytes for a member that holds five", len(got))
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("ReadFile = %v, want ErrCorrupt", err)
	}
}

// TestDecoderWrapsStoredForAbsentEncoding covers the encoding-less <data>
// element: no <encoding> at all means the heap bytes are the member's bytes.
func TestDecoderWrapsStoredForAbsentEncoding(t *testing.T) {
	c := baseline()
	c.toc = strings.Replace(c.toc,
		fmt.Sprintf(`<encoding style="%s"/>`, encodingStored), "", 1)
	f, err := c.open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, err := f.ReadFile("/raw.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "0123456789" {
		t.Errorf("ReadFile = %q, want %q", got, "0123456789")
	}
}

// TestDecoderReturnsAReader checks decoder directly for the shapes the fixtures
// cover implicitly, so a change to the mapping fails here by name.
func TestDecoderReturnsAReader(t *testing.T) {
	for _, style := range []string{"", encodingStored, encodingZlib, encodingBzip2} {
		var src io.Reader
		switch style {
		case encodingZlib:
			src = bytes.NewReader(zlibBytes([]byte("x")))
		case encodingBzip2:
			src = bytes.NewReader([]byte("BZh9"))
		default:
			src = bytes.NewReader([]byte("x"))
		}
		dec, err := decoder(style, src)
		if err != nil {
			t.Errorf("decoder(%q) = %v", style, err)
			continue
		}
		if err := dec.Close(); err != nil {
			t.Errorf("decoder(%q).Close = %v", style, err)
		}
	}
	if _, err := decoder("application/x-made-up", bytes.NewReader(nil)); !errors.Is(err, ErrUnsupportedEncoding) {
		t.Errorf("decoder of an unknown style = %v, want ErrUnsupportedEncoding", err)
	}
}
