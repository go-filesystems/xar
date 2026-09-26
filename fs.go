// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package xar

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	filesystem "github.com/go-filesystems/interface"
)

// FS is an opened, read-only XAR archive.
//
// It satisfies filesystem.Filesystem and the optional filesystem.Opener. A
// filesystem.File obtained from OpenFile is valid only while the FS is open.
type FS struct {
	r    io.ReaderAt
	size int64
	// heap is the file offset every entry's <offset> is relative to.
	heap  int64
	root  *entry
	index map[string]*entry
}

// Compile-time proof that the capabilities this package claims are the ones it
// has. A missing method here is a build failure rather than a caller's runtime
// type assertion quietly taking the slow path.
var (
	_ filesystem.Filesystem = (*FS)(nil)
	_ filesystem.Opener     = (*FS)(nil)
	_ filesystem.File       = (*handle)(nil)
)

// OpenReader reads the header and the whole table of contents of the archive in
// r, which must be size bytes long, and returns a read-only filesystem over it.
//
// Member data is NOT read: only the header and the table of contents are, and
// each member is decoded when it is read. r must stay usable for as long as the
// returned filesystem is.
func OpenReader(r io.ReaderAt, size int64) (filesystem.Filesystem, error) {
	hdr, err := parseHeader(r, size)
	if err != nil {
		return nil, err
	}
	arc, err := readTOC(r, hdr)
	if err != nil {
		return nil, err
	}
	heap := hdr.heapStart()
	root, index, err := buildTree(arc, heap, size)
	if err != nil {
		return nil, err
	}
	return &FS{r: r, size: size, heap: heap, root: root, index: index}, nil
}

// Close releases nothing: FS never opened the io.ReaderAt it was handed, so it
// does not close it either. It is here because filesystem.Filesystem has it.
func (f *FS) Close() error { return nil }

// splitPath reduces an absolute or relative path to its meaningful components.
func splitPath(p string) []string {
	out := make([]string, 0, 8)
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." {
			continue
		}
		out = append(out, s)
	}
	return out
}

// lookup resolves a path to its entry. Symlinks are NOT followed: an archive's
// link target is a string the archive supplied, which may name anything at all
// including a path outside the archive, so resolving it here would answer for
// something this package cannot see. ReadLink hands the target back and the
// caller decides.
func (f *FS) lookup(p string) (*entry, error) {
	cur := f.root
	for _, part := range splitPath(p) {
		if cur.kind != kindDir {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, p)
		}
		next := cur.child(part)
		if next == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, p)
		}
		cur = next
	}
	return cur, nil
}

// child finds a named child. The children of a directory are kept sorted by
// name, so this is a small linear scan over one level.
func (e *entry) child(name string) *entry {
	for _, c := range e.children {
		if c.name == name {
			return c
		}
	}
	return nil
}

// Stat reports what the table of contents recorded for p.
func (f *FS) Stat(p string) (filesystem.Stat, error) {
	e, err := f.lookup(p)
	if err != nil {
		return nil, err
	}
	return filesystem.NewStat(e.mode(), uint64(e.size), e.id), nil
}

// ListDir returns the entries of the directory at p, sorted by name.
func (f *FS) ListDir(p string) ([]filesystem.DirEntry, error) {
	e, err := f.lookup(p)
	if err != nil {
		return nil, err
	}
	if e.kind != kindDir {
		return nil, fmt.Errorf("%w: %s", ErrNotDirectory, p)
	}
	out := make([]filesystem.DirEntry, 0, len(e.children))
	for _, c := range e.children {
		out = append(out, filesystem.NewDirEntry(c.id, c.name, c.fileType()))
	}
	return out, nil
}

// ReadLink returns the target of the symlink at p.
func (f *FS) ReadLink(p string) (string, error) {
	e, err := f.lookup(p)
	if err != nil {
		return "", err
	}
	if e.kind != kindSymlink {
		return "", fmt.Errorf("%w: %s", ErrNotSymlink, p)
	}
	return e.link, nil
}

// ReadFile returns the whole decoded contents of the member at p.
//
// The buffer is GROWN from what is actually read rather than allocated from the
// declared <size>. A table of contents is untrusted input and its size field is
// not bounded by the archive's own length, so trusting it to size an allocation
// lets a few hundred bytes of XML ask for as much memory as it likes. Use
// OpenFile for anything whose size was not chosen here.
func (f *FS) ReadFile(p string) ([]byte, error) {
	h, err := f.OpenFile(p)
	if err != nil {
		return nil, err
	}
	defer h.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, io.NewSectionReader(h, 0, h.Size())); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// OpenFile opens the member at p for random access, implementing
// filesystem.Opener.
//
// Nothing is read here: the entry's heap extent and encoding were already
// decoded from the table of contents, and a decoder is started on the first
// ReadAt. See handle for what "random access" costs on a compressed member.
func (f *FS) OpenFile(p string) (filesystem.File, error) {
	e, err := f.lookup(p)
	if err != nil {
		return nil, err
	}
	if e.kind != kindFile {
		return nil, fmt.Errorf("%w: %s", ErrNotRegular, p)
	}
	h := &handle{fs: f, e: e}
	if stored(e.style) {
		h.sec = io.NewSectionReader(f.r, f.heap+e.offset, e.size)
	}
	return h, nil
}

// --- The mutating half of filesystem.Filesystem. An archive is read-only. ---

func (f *FS) WriteFile(string, []byte, os.FileMode) error { return ErrReadOnly }
func (f *FS) MkDir(string, os.FileMode) error             { return ErrReadOnly }
func (f *FS) DeleteFile(string) error                     { return ErrReadOnly }
func (f *FS) DeleteDir(string) error                      { return ErrReadOnly }
func (f *FS) Rename(string, string) error                 { return ErrReadOnly }

// handle is one open member.
//
// XAR compresses PER MEMBER, so there is no archive-wide stream and no offset
// index into decoded bytes. That splits this type in two:
//
//   - A stored member (application/octet-stream, or no <data> at all) is served
//     straight from the backing io.ReaderAt through sec. That is real random
//     access: nothing is buffered, and concurrent ReadAt calls need no lock
//     because io.SectionReader holds no mutable state.
//
//   - A compressed member has no random access to give. A zlib or bzip2 stream
//     can only be read forwards from its start, so this handle keeps a decoder
//     and how far into the member it has got: a read ahead of that position
//     discards forward to it, and a read BEHIND it starts the stream again.
//     Memory stays O(1) — the whole member is never held — and a caller reading
//     sequentially, which is what an extractor does, pays one pass in total. A
//     caller seeking backwards repeatedly pays a pass each time, which is the
//     honest cost of the format rather than something hidden behind a cache.
//
// Concurrency follows io.ReaderAt: concurrent ReadAt calls are safe. On the
// compressed path they are safe because mu serialises them, not because they
// run in parallel — the single decoder is the shared state and there is one
// stream to advance.
type handle struct {
	fs *FS
	e  *entry

	// sec serves a stored member. Non-nil exactly when stored(e.style).
	sec *io.SectionReader

	mu  sync.Mutex
	dec io.ReadCloser
	pos int64 // how far into the decoded member dec has been read
}

// Size is the member's decoded length, from the table of contents.
func (h *handle) Size() int64 { return h.e.size }

// Close releases the decoder, if one was ever started.
func (h *handle) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dec == nil {
		return nil
	}
	err := h.dec.Close()
	h.dec = nil
	return err
}

// ReadAt fills p from off, to io.ReaderAt's contract: a short read is always
// accompanied by a non-nil error, and a read that runs out at the end of the
// member returns io.EOF.
func (h *handle) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("xar: negative offset %d", off)
	}
	if off >= h.e.size {
		return 0, io.EOF
	}
	if h.sec != nil {
		// io.SectionReader already implements the contract exactly, including
		// the io.EOF on a read that reaches the end.
		return h.sec.ReadAt(p, off)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err := h.seek(off); err != nil {
		return 0, err
	}
	// Clamp to the member's declared size: a decoder handed more bytes than the
	// member has — a stream whose encoder padded, or a <size> smaller than the
	// data — must not spill them into the caller's buffer.
	want := int64(len(p))
	if rem := h.e.size - off; want > rem {
		want = rem
	}
	n, err := io.ReadFull(h.dec, p[:want])
	h.pos += int64(n)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			// The member's data ran out BEFORE its declared size. That is the
			// archive disagreeing with itself, not the end of a file, and
			// reporting it as io.EOF would let a caller treat a truncated
			// member as a whole one.
			return n, fmt.Errorf("%w: %q ends after %d of %d bytes",
				ErrCorrupt, h.e.path, off+int64(n), h.e.size)
		}
		return n, err
	}
	if want < int64(len(p)) {
		// Fewer bytes than asked for, because the member ended. The contract
		// forbids reporting that as a success.
		return n, io.EOF
	}
	return n, nil
}

// seek positions the decoder at off in the decoded member, restarting the
// stream when it has to go backwards. Callers hold h.mu.
func (h *handle) seek(off int64) error {
	if h.dec == nil || off < h.pos {
		if err := h.restart(); err != nil {
			return err
		}
	}
	if off == h.pos {
		return nil
	}
	if _, err := io.CopyN(io.Discard, h.dec, off-h.pos); err != nil {
		return fmt.Errorf("%w: %q could not be decoded as far as byte %d: %w",
			ErrCorrupt, h.e.path, off, err)
	}
	h.pos = off
	return nil
}

// restart discards any decoder and opens a new one at the start of the member's
// heap extent. Callers hold h.mu.
func (h *handle) restart() error {
	if h.dec != nil {
		_ = h.dec.Close()
		h.dec = nil
	}
	raw := io.NewSectionReader(h.fs.r, h.fs.heap+h.e.offset, h.e.length)
	dec, err := decoder(h.e.style, raw)
	if err != nil {
		return err
	}
	h.dec, h.pos = dec, 0
	return nil
}
