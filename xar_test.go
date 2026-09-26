// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package xar

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestMemberBytes asserts every member's CONTENTS, byte for byte, against the
// bytes that went into the archive. Not its length, not a checksum of it, and
// not how many members there are: a size that agrees and bytes that do not is
// exactly what a wrong offset base or a wrong decompressor produces.
func TestMemberBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
		want []want
	}{
		{"mixed", mixedXar, mixedWant()},
		{"bzip2", bzip2Xar, bzip2Want()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := openFixture(t, tc.raw)
			for _, w := range tc.want {
				if w.dir || w.link != "" {
					continue
				}
				got, err := f.ReadFile(w.path)
				if err != nil {
					t.Fatalf("ReadFile(%q): %v", w.path, err)
				}
				if !bytes.Equal(got, w.body) {
					t.Errorf("ReadFile(%q): %d bytes, want %d; first difference at %d",
						w.path, len(got), len(w.body), firstDiff(got, w.body))
				}
			}
		})
	}
}

// firstDiff reports the index of the first differing byte, or -1.
func firstDiff(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

// TestCorpusReachesEveryEncoding asserts that the fixtures actually exercise the
// stored, zlib and bzip2 paths. A test suite that merely HOPES its corpus covers
// a decoder proves nothing about the decoder it never called, so the encoding
// each member carries is asserted as a fact about the archive.
func TestCorpusReachesEveryEncoding(t *testing.T) {
	reached := map[string]bool{}
	for _, tc := range []struct {
		raw  []byte
		want []want
	}{{mixedXar, mixedWant()}, {bzip2Xar, bzip2Want()}} {
		f := openFixture(t, tc.raw)
		for _, w := range tc.want {
			e := f.index[w.path]
			if e == nil {
				t.Fatalf("%q is not in the archive", w.path)
			}
			if e.style != w.style {
				t.Errorf("%q: encoding style %q, want %q", w.path, e.style, w.style)
			}
			reached[e.style] = true
		}
	}
	for _, style := range []string{"", encodingStored, encodingZlib, encodingBzip2} {
		if !reached[style] {
			t.Errorf("no fixture member uses encoding %q, so that path is untested", style)
		}
	}
}

// TestCorpusReachesEveryShape asserts the rest of the corpus's reach: an empty
// member, one over 64 KiB, a directory two levels deep, a symlink, and a mode
// that is neither 0644 nor written with a leading zero.
func TestCorpusReachesEveryShape(t *testing.T) {
	f := openFixture(t, mixedXar)

	if e := f.index["/empty.txt"]; e.size != 0 {
		t.Errorf("/empty.txt size %d, want 0", e.size)
	} else if f.index["/empty.txt"].style != "" {
		t.Errorf("/empty.txt has an encoding style, so it is not the no-<data> shape")
	}
	if e := f.index["/big.txt"]; e.size <= 64<<10 {
		t.Errorf("/big.txt is %d bytes, which does not exceed 64 KiB", e.size)
	}
	if e := f.index["/nested/deep/file.txt"]; e == nil {
		t.Error("the corpus has no member two directories deep")
	}
	if e := f.index["/alias.lnk"]; e.kind != kindSymlink {
		t.Error("the corpus has no symlink")
	}
	// The mode xar writes without a leading zero. This is the member that tells
	// strconv base 8 from base 0.
	if e := f.index["/setuid.sh"]; e.perm != 0o4755 {
		t.Errorf("/setuid.sh perm %o, want 4755", e.perm)
	}
	for _, p := range []string{"/plain.txt", "/nested/deep/file.txt", "/setuid.sh"} {
		if e := f.index[p]; e.perm == 0o644 {
			t.Errorf("%q has perm 0644, so it cannot witness a non-default mode", p)
		}
	}
}

// TestStatMode asserts the full st_mode of every member: the type bits with the
// permission bits the archive recorded.
func TestStatMode(t *testing.T) {
	f := openFixture(t, mixedXar)
	for _, w := range mixedWant() {
		st := statOf(t, f, w.path)
		if st.Mode() != w.mode {
			t.Errorf("Stat(%q).Mode() = %#o, want %#o", w.path, st.Mode(), w.mode)
		}
	}
}

// TestStatSizeAndInode checks the other two things a Stat carries. The inode is
// the <file id> attribute, which is the only stable identity XAR gives a member.
func TestStatSizeAndInode(t *testing.T) {
	f := openFixture(t, mixedXar)
	st := statOf(t, f, "/big.txt")
	if st.Size() != 80000 {
		t.Errorf("/big.txt Size() = %d, want 80000", st.Size())
	}
	if st.Inode() != 6 {
		t.Errorf("/big.txt Inode() = %d, want 6 (the <file id>)", st.Inode())
	}
	if st := statOf(t, f, "/"); st.Mode() != modeDir|0o755 {
		t.Errorf("root Mode() = %#o, want %#o", st.Mode(), modeDir|0o755)
	}
}

// TestListDirNamesExactly asserts the whole listing of each directory. An
// assertion that a name is PRESENT cannot see an extra entry, and an extra entry
// is what an <ea> read as a member looks like.
func TestListDirNamesExactly(t *testing.T) {
	f := openFixture(t, mixedXar)
	for _, tc := range []struct {
		dir   string
		names []string
	}{
		// Sorted by name, and note that this is NOT the order the table of
		// contents lists them in: xar wrote the members in reverse, so a
		// listing that matched document order would be wrong here.
		{"/", []string{"alias.lnk", "big.txt", "empty.txt", "nested", "plain.txt", "setuid.sh", "store.bin"}},
		{"/nested", []string{"deep"}},
		{"/nested/deep", []string{"file.txt"}},
	} {
		got, err := f.ListDir(tc.dir)
		if err != nil {
			t.Fatalf("ListDir(%q): %v", tc.dir, err)
		}
		names := make([]string, len(got))
		for i, e := range got {
			names[i] = e.Name()
		}
		if len(names) != len(tc.names) {
			t.Fatalf("ListDir(%q) = %v, want %v", tc.dir, names, tc.names)
		}
		for i := range names {
			if names[i] != tc.names[i] {
				t.Errorf("ListDir(%q)[%d] = %q, want %q", tc.dir, i, names[i], tc.names[i])
			}
		}
	}
}

// TestXattrIsNotAMember is the <ea> trap, asserted from both sides.
//
// mixed.xar carries an extended attribute on empty.txt and on plain.txt. An
// <ea> is a sibling of <data> with its own <offset>, <length>, <size>,
// <encoding> AND <name>. empty.txt has an <ea> and no <data> at all, so a reader
// that searches a <file> subtree for an offset serves the attribute's bytes as
// the file's contents, and one that searches for a name calls the file
// "com.example.marker".
func TestXattrIsNotAMember(t *testing.T) {
	f := openFixture(t, mixedXar)

	body, err := f.ReadFile("/empty.txt")
	if err != nil {
		t.Fatalf("ReadFile(/empty.txt): %v", err)
	}
	if len(body) != 0 {
		t.Errorf("/empty.txt is %d bytes (%q); the xattr's bytes have been served as the file",
			len(body), body)
	}
	if _, err := f.Stat("/com.example.marker"); !errors.Is(err, ErrNotFound) {
		t.Error("an extended attribute's <name> has become a member of the archive")
	}
	// And the file that HAS both a <data> and an <ea> still reads as its data.
	if b, err := f.ReadFile("/plain.txt"); err != nil || !bytes.Equal(b, []byte("hello xar\n")) {
		t.Errorf("ReadFile(/plain.txt) = %q, %v; want the file's data, not its xattr", b, err)
	}
}

// TestReadLink asserts the symlink target, and that ReadLink refuses everything
// else.
func TestReadLink(t *testing.T) {
	f := openFixture(t, mixedXar)
	got, err := f.ReadLink("/alias.lnk")
	if err != nil {
		t.Fatalf("ReadLink(/alias.lnk): %v", err)
	}
	if got != "plain.txt" {
		t.Errorf("ReadLink(/alias.lnk) = %q, want %q", got, "plain.txt")
	}
	for _, p := range []string{"/plain.txt", "/nested"} {
		if _, err := f.ReadLink(p); !errors.Is(err, ErrNotSymlink) {
			t.Errorf("ReadLink(%q) = %v, want ErrNotSymlink", p, err)
		}
	}
}

// TestSymlinkIsNotFollowed pins the decision that a link target is handed back
// rather than resolved. The target could name anything, including a path this
// package cannot see, so resolving it would answer for something else.
func TestSymlinkIsNotFollowed(t *testing.T) {
	f := openFixture(t, mixedXar)
	if _, err := f.ReadFile("/alias.lnk"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("ReadFile(/alias.lnk) = %v, want ErrNotRegular: the link must not resolve to its target", err)
	}
	if st := statOf(t, f, "/alias.lnk"); st.Mode() != modeSymlink|0o755 {
		t.Errorf("Stat(/alias.lnk).Mode() = %#o, want a symlink", st.Mode())
	}
}

// TestDirEntryFileType checks the type byte this organisation's consumers
// switch on: 2 is the directory marker go-filesystems/unarchive compares
// against, 10 is DT_LNK, and 0 is everything else.
func TestDirEntryFileType(t *testing.T) {
	f := openFixture(t, mixedXar)
	got, err := f.ListDir("/")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	wantType := map[string]uint8{
		"alias.lnk": 10, "big.txt": 0, "empty.txt": 0, "nested": 2,
		"plain.txt": 0, "setuid.sh": 0, "store.bin": 0,
	}
	for _, e := range got {
		if e.FileType() != wantType[e.Name()] {
			t.Errorf("%q FileType() = %d, want %d", e.Name(), e.FileType(), wantType[e.Name()])
		}
	}
}

// TestOpenerStreamsEveryMember reads each member through filesystem.Opener in
// small chunks and asserts the reassembled bytes. This is the path an extractor
// takes, and it is a different path from ReadFile for a compressed member: the
// decoder is advanced across many calls instead of one.
func TestOpenerStreamsEveryMember(t *testing.T) {
	f := openFixture(t, mixedXar)
	for _, w := range mixedWant() {
		if w.dir || w.link != "" {
			continue
		}
		h, err := f.OpenFile(w.path)
		if err != nil {
			t.Fatalf("OpenFile(%q): %v", w.path, err)
		}
		if h.Size() != int64(len(w.body)) {
			t.Errorf("%q Size() = %d, want %d", w.path, h.Size(), len(w.body))
		}
		var got bytes.Buffer
		if _, err := io.CopyBuffer(&got, io.NewSectionReader(h, 0, h.Size()), make([]byte, 7)); err != nil {
			t.Fatalf("streaming %q: %v", w.path, err)
		}
		if err := h.Close(); err != nil {
			t.Errorf("Close(%q): %v", w.path, err)
		}
		if !bytes.Equal(got.Bytes(), w.body) {
			t.Errorf("streamed %q in 7-byte chunks: first difference at %d",
				w.path, firstDiff(got.Bytes(), w.body))
		}
	}
}

// TestReadAtBackwards makes a compressed member's handle seek backwards, which
// it can only serve by restarting the stream. Then forwards again, to show the
// restart left the handle usable.
func TestReadAtBackwards(t *testing.T) {
	f := openFixture(t, mixedXar)
	h, err := f.OpenFile("/big.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer h.Close()
	body := bigTxt()

	for _, off := range []int64{70000, 8, 65536, 3, 79992} {
		buf := make([]byte, 8)
		n, err := h.ReadAt(buf, off)
		if err != nil {
			t.Fatalf("ReadAt(%d): %v", off, err)
		}
		if n != 8 || !bytes.Equal(buf, body[off:off+8]) {
			t.Errorf("ReadAt(%d) = %q, want %q", off, buf[:n], body[off:off+8])
		}
	}
}

// TestReadAtStoredIsRandomAccess checks the other branch of ReadAt: a stored
// member goes straight to the backing io.ReaderAt, in any order, with nothing
// buffered.
func TestReadAtStoredIsRandomAccess(t *testing.T) {
	f := openFixture(t, mixedXar)
	h, err := f.OpenFile("/store.bin")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer h.Close()
	for _, off := range []int64{4000, 0, 2048, 1} {
		buf := make([]byte, 16)
		if _, err := h.ReadAt(buf, off); err != nil {
			t.Fatalf("ReadAt(%d): %v", off, err)
		}
		if !bytes.Equal(buf, storeBin[off:off+16]) {
			t.Errorf("ReadAt(%d) = %x, want %x", off, buf, storeBin[off:off+16])
		}
	}
}

// TestReadAtContract exercises the parts of io.ReaderAt's contract a caller
// wrapping this in an io.SectionReader depends on.
func TestReadAtContract(t *testing.T) {
	f := openFixture(t, mixedXar)
	for _, p := range []string{"/plain.txt", "/store.bin"} {
		h, err := f.OpenFile(p)
		if err != nil {
			t.Fatalf("OpenFile(%q): %v", p, err)
		}
		size := h.Size()

		// At or past the end: 0, io.EOF.
		if n, err := h.ReadAt(make([]byte, 4), size); n != 0 || !errors.Is(err, io.EOF) {
			t.Errorf("%s: ReadAt at size = %d, %v; want 0, io.EOF", p, n, err)
		}
		if n, err := h.ReadAt(make([]byte, 4), size+100); n != 0 || !errors.Is(err, io.EOF) {
			t.Errorf("%s: ReadAt past size = %d, %v; want 0, io.EOF", p, n, err)
		}
		// A short read must carry io.EOF, never a nil error.
		buf := make([]byte, size+10)
		n, err := h.ReadAt(buf, 0)
		if int64(n) != size || !errors.Is(err, io.EOF) {
			t.Errorf("%s: ReadAt oversized = %d, %v; want %d, io.EOF", p, n, err, size)
		}
		// A negative offset is a caller error, not an EOF.
		if _, err := h.ReadAt(make([]byte, 1), -1); err == nil {
			t.Errorf("%s: ReadAt(-1) succeeded", p)
		}
		// An exact read carries no error at all.
		if n, err := h.ReadAt(make([]byte, size), 0); int64(n) != size || err != nil {
			t.Errorf("%s: exact ReadAt = %d, %v; want %d, nil", p, n, err, size)
		}
		if err := h.Close(); err != nil {
			t.Errorf("%s: Close: %v", p, err)
		}
		// Close is idempotent: the decoder is dropped, not closed twice.
		if err := h.Close(); err != nil {
			t.Errorf("%s: second Close: %v", p, err)
		}
	}
}

// TestReadAtEmptyBuffer covers a zero-length read, which io.Copy issues at the
// tail of a section and which must not be mistaken for the end of the member.
func TestReadAtEmptyBuffer(t *testing.T) {
	f := openFixture(t, mixedXar)
	h, err := f.OpenFile("/plain.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer h.Close()
	if n, err := h.ReadAt(nil, 0); n != 0 || err != nil {
		t.Errorf("ReadAt(nil, 0) = %d, %v; want 0, nil", n, err)
	}
}

// TestConcurrentReadAt runs ReadAt on one handle from several goroutines, which
// io.ReaderAt's contract permits and a mount serving parallel requests relies
// on. The compressed path has one decoder and one position, so this is the test
// that would catch the two racing if mu were dropped -- under -race, which the
// native lanes run.
func TestConcurrentReadAt(t *testing.T) {
	f := openFixture(t, mixedXar)
	body := bigTxt()
	h, err := f.OpenFile("/big.txt")
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer h.Close()

	const readers = 8
	errs := make(chan error, readers)
	for i := range readers {
		go func(i int) {
			off := int64(i) * 4096
			buf := make([]byte, 512)
			if _, err := h.ReadAt(buf, off); err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(buf, body[off:off+512]) {
				errs <- errors.New("concurrent ReadAt returned the wrong bytes")
				return
			}
			errs <- nil
		}(i)
	}
	for range readers {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

// TestNotFound covers the error contract the interface module requires: a path
// that is not there must satisfy errors.Is(err, fs.ErrNotExist), which
// ErrNotFound wraps.
func TestNotFound(t *testing.T) {
	f := openFixture(t, mixedXar)
	for _, p := range []string{"/nope", "/nested/nope", "/nested/deep/file.txt/under-a-file", "/plain.txt/x"} {
		if _, err := f.Stat(p); !errors.Is(err, ErrNotFound) {
			t.Errorf("Stat(%q) = %v, want ErrNotFound", p, err)
		}
		if _, err := f.ReadFile(p); !errors.Is(err, ErrNotFound) {
			t.Errorf("ReadFile(%q) = %v, want ErrNotFound", p, err)
		}
		if _, err := f.ListDir(p); !errors.Is(err, ErrNotFound) {
			t.Errorf("ListDir(%q) = %v, want ErrNotFound", p, err)
		}
		if _, err := f.ReadLink(p); !errors.Is(err, ErrNotFound) {
			t.Errorf("ReadLink(%q) = %v, want ErrNotFound", p, err)
		}
		if _, err := f.OpenFile(p); !errors.Is(err, ErrNotFound) {
			t.Errorf("OpenFile(%q) = %v, want ErrNotFound", p, err)
		}
	}
}

// TestWrongKind covers ListDir on a file and ReadFile/OpenFile on a directory.
func TestWrongKind(t *testing.T) {
	f := openFixture(t, mixedXar)
	if _, err := f.ListDir("/plain.txt"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("ListDir(/plain.txt) = %v, want ErrNotDirectory", err)
	}
	if _, err := f.ReadFile("/nested"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("ReadFile(/nested) = %v, want ErrNotRegular", err)
	}
	if _, err := f.OpenFile("/nested"); !errors.Is(err, ErrNotRegular) {
		t.Errorf("OpenFile(/nested) = %v, want ErrNotRegular", err)
	}
}

// TestPathForms checks that the several spellings of one path agree, including
// the relative and dot-laden ones a caller may hand over.
func TestPathForms(t *testing.T) {
	f := openFixture(t, mixedXar)
	body := []byte("nested content here\n")
	for _, p := range []string{
		"/nested/deep/file.txt",
		"nested/deep/file.txt",
		"/nested//deep/file.txt",
		"./nested/./deep/file.txt",
	} {
		got, err := f.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", p, err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("ReadFile(%q) = %q, want %q", p, got, body)
		}
	}
	for _, p := range []string{"/", "", ".", "//"} {
		if _, err := f.ListDir(p); err != nil {
			t.Errorf("ListDir(%q): %v", p, err)
		}
	}
}

// TestCloseIsNotTheCallersFileHandle pins that FS does not close the io.ReaderAt
// it was handed. It never opened it, so it has no business closing it.
func TestCloseIsNotTheCallersFileHandle(t *testing.T) {
	f := openFixture(t, mixedXar)
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := f.ReadFile("/plain.txt"); err != nil {
		t.Errorf("the backing reader was closed by FS.Close: %v", err)
	}
}
