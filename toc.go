// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package xar

import (
	"compress/zlib"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

// The <type> values XAR records. Anything else — fifo, socket, a device node —
// is kept in the tree as kindOther so that it is visible to ListDir and refused
// by ReadFile, rather than silently becoming an empty regular file.
const (
	typeFile      = "file"
	typeDirectory = "directory"
	typeSymlink   = "symlink"
)

// kind is what an entry is.
type kind uint8

const (
	kindFile kind = iota
	kindDir
	kindSymlink
	kindOther
)

// POSIX st_mode type bits, ORed into the permission bits <mode> carries.
//
// os.FileMode is the wrong shape for filesystem.Stat, which carries a uint16:
// os.ModeDir is 1<<31, so narrowing an os.FileMode to uint16 throws every type
// bit away and leaves a directory unable to say it is one.
const (
	modeDir     = 0o040000
	modeRegular = 0o100000
	modeSymlink = 0o120000
	modeFIFO    = 0o010000
	modePerm    = 0o007777
)

// --- The XML shape of the table of contents. ---
//
// Every field below binds to a DIRECT CHILD of its element, which is what
// keeps <ea> out of the way. An extended attribute is a sibling of <data>
// carrying its own <offset>, <length>, <size>, <encoding> and <name>, so a
// reader that searches a <file> subtree for those names picks up an xattr's
// heap location as the file's data and an xattr's name as the file's name.
// Binding to direct children makes that structurally impossible rather than
// merely unlikely.

type xmlArchive struct {
	XMLName xml.Name `xml:"xar"`
	TOC     xmlTOC   `xml:"toc"`
}

type xmlTOC struct {
	Files []xmlFile `xml:"file"`
}

type xmlFile struct {
	ID   uint64   `xml:"id,attr"`
	Name string   `xml:"name"`
	Type string   `xml:"type"`
	Mode string   `xml:"mode"`
	Link string   `xml:"link"`
	Data *xmlData `xml:"data"`
	// Directories nest their children as further <file> elements. There is no
	// path string anywhere in a XAR table of contents, so this nesting IS the
	// directory tree and a path exists only once it has been walked.
	Files []xmlFile `xml:"file"`
}

type xmlData struct {
	// Offset is relative to the START OF THE HEAP, not to the archive.
	Offset int64 `xml:"offset"`
	// Length is how many bytes the member occupies in the heap, encoded.
	Length int64 `xml:"length"`
	// Size is the member's length once decoded. For a stored member the two
	// are equal; for a compressed one Size is the larger.
	Size     int64       `xml:"size"`
	Encoding xmlEncoding `xml:"encoding"`
}

type xmlEncoding struct {
	Style string `xml:"style,attr"`
}

// entry is one decoded member of the archive.
type entry struct {
	name string
	path string
	id   uint64
	kind kind
	perm uint16
	link string

	// style is the encoding style string; stored(style) decides whether the
	// heap bytes can be served directly.
	style string
	// offset is relative to the heap start; length is the encoded extent in
	// the heap; size is the decoded length.
	offset, length, size int64

	children []*entry
}

// readTOC decompresses and parses the table of contents.
func readTOC(r io.ReaderAt, h *header) (*xmlArchive, error) {
	raw := io.NewSectionReader(r, int64(h.headerSize), int64(h.tocCompressed))
	zr, err := zlib.NewReader(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: table of contents is not a zlib stream: %w", ErrCorrupt, err)
	}
	defer zr.Close()

	// Read exactly the declared uncompressed length, then require the stream to
	// be finished. The declaration is used as a HARD BOUND, not as a hint: it
	// caps the allocation before any of the archive's own data is trusted, and
	// checking it also catches a header whose two TOC lengths disagree with the
	// bytes between them.
	buf := make([]byte, h.tocUncompressed)
	if _, err := io.ReadFull(zr, buf); err != nil {
		return nil, fmt.Errorf("%w: table of contents is shorter than the declared %d bytes: %w",
			ErrCorrupt, h.tocUncompressed, err)
	}
	if n, err := zr.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		return nil, fmt.Errorf("%w: table of contents is longer than the declared %d bytes",
			ErrCorrupt, h.tocUncompressed)
	}

	var arc xmlArchive
	if err := xml.Unmarshal(buf, &arc); err != nil {
		return nil, fmt.Errorf("%w: table of contents XML: %w", ErrCorrupt, err)
	}
	return &arc, nil
}

// buildTree turns the nested <file> elements into an entry tree rooted at a
// synthetic directory, and indexes every entry by its absolute path.
func buildTree(arc *xmlArchive, heap, size int64) (*entry, map[string]*entry, error) {
	root := &entry{name: "", path: "/", kind: kindDir, perm: 0o755}
	index := map[string]*entry{"/": root}
	if err := addChildren(root, arc.TOC.Files, index, heap, size); err != nil {
		return nil, nil, err
	}
	return root, index, nil
}

// addChildren converts one level of <file> elements into children of parent,
// recursing into the nesting to build each path.
func addChildren(parent *entry, files []xmlFile, index map[string]*entry, heap, size int64) error {
	seen := make(map[string]bool, len(files))
	for i := range files {
		f := &files[i]
		if err := checkName(f.Name); err != nil {
			return err
		}
		if seen[f.Name] {
			// Two members with one name in one directory. Extracting the
			// archive would write one over the other, so the archive is
			// refused rather than silently resolved one way.
			return fmt.Errorf("%w: %q appears twice in %q", ErrCorrupt, f.Name, parent.path)
		}
		seen[f.Name] = true

		e := &entry{
			name: f.Name,
			path: path.Join(parent.path, f.Name),
			id:   f.ID,
			kind: kindOf(f.Type),
			link: f.Link,
		}
		perm, err := parseMode(f.Mode)
		if err != nil {
			return fmt.Errorf("%w: %q: %w", ErrCorrupt, e.path, err)
		}
		e.perm = perm

		// An absent <data> is how XAR spells an empty file: not <size>0</size>
		// but no element at all. Directories and symlinks have none either, so
		// the zero values below are right for all three.
		if f.Data != nil {
			e.style = f.Data.Encoding.Style
			e.offset, e.length, e.size = f.Data.Offset, f.Data.Length, f.Data.Size
			if err := checkExtent(e, heap, size); err != nil {
				return err
			}
		}

		parent.children = append(parent.children, e)
		index[e.path] = e

		// Nesting is how XAR spells a directory, so only a directory may nest.
		// A <file> inside a symlink, a fifo or a regular file describes a path
		// that cannot exist, and the archive is refused rather than resolved:
		// dropping such children silently loses members, and keeping them
		// creates a path whose parent is not a directory -- which is exactly
		// the case that let this package's index and its tree walk give two
		// different answers for one path.
		if len(f.Files) > 0 && e.kind != kindDir {
			return fmt.Errorf("%w: %q is of type %q but nests %d member(s)",
				ErrCorrupt, e.path, f.Type, len(f.Files))
		}
		if err := addChildren(e, f.Files, index, heap, size); err != nil {
			return err
		}
	}
	sort.Slice(parent.children, func(i, j int) bool {
		return parent.children[i].name < parent.children[j].name
	})
	return nil
}

// checkName refuses a <name> that could not be one path component, because such
// a name is how an archive walks out of the directory it is extracted into.
//
// A BACKSLASH IS DELIBERATELY ALLOWED. It is an ordinary character in a POSIX
// filename, so an archive carrying one is not malformed and is not refused
// here. It is a separator on Windows, which makes it a hazard at the point
// where a name becomes a path on a real disk — and that is where
// go-filesystems/unarchive already refuses it, knowing the destination this
// package does not see. Refusing it here would instead make a lawful archive
// unopenable everywhere.
func checkName(name string) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return fmt.Errorf("%w: %q is not a usable name", ErrCorrupt, name)
	}
	return nil
}

// checkExtent bounds a member's heap extent against the archive it came from.
func checkExtent(e *entry, heap, size int64) error {
	if e.offset < 0 || e.length < 0 || e.size < 0 {
		return fmt.Errorf("%w: %q has offset %d, length %d, size %d",
			ErrCorrupt, e.path, e.offset, e.length, e.size)
	}
	if e.offset > size-heap || e.length > size-heap-e.offset {
		return fmt.Errorf("%w: %q spans heap bytes %d..%d, past the end of a %d-byte archive",
			ErrCorrupt, e.path, e.offset, e.offset+e.length, size)
	}
	// A stored member's two lengths describe the same bytes, so they must
	// agree. Without this, a stored entry declaring a size larger than its
	// heap extent reads as a file that ends early for no stated reason.
	if stored(e.style) && e.length != e.size {
		return fmt.Errorf("%w: %q is stored but declares length %d and size %d",
			ErrCorrupt, e.path, e.length, e.size)
	}
	return nil
}

// kindOf maps a <type> string onto what this package does with the entry.
func kindOf(t string) kind {
	switch t {
	case typeDirectory:
		return kindDir
	case typeSymlink:
		return kindSymlink
	case typeFile:
		return kindFile
	default:
		return kindOther
	}
}

// parseMode reads <mode>, WHICH IS OCTAL AND WHOSE LEADING ZERO IS OPTIONAL.
//
// /usr/bin/xar writes <mode>0644</mode> for an ordinary file and
// <mode>4755</mode> for a setuid one. Both are octal. strconv.ParseUint with
// base 0 gets the first right for the wrong reason — it infers octal from the
// leading zero — and the second wrong, reading 4755 as decimal, which is
// 0o11223. Base 8 is stated explicitly here, and the fixture carries a setuid
// member so that no corpus of leading-zero modes can hide the difference.
//
// An absent <mode> yields zero permissions, which is what the archive said.
func parseMode(s string) (uint16, error) {
	if s == "" {
		return 0, nil
	}
	m, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("mode %q is not octal: %w", s, err)
	}
	return uint16(m & modePerm), nil
}

// mode is the POSIX st_mode filesystem.Stat carries: the type bits from <type>
// ORed with the permission bits from <mode>.
func (e *entry) mode() uint16 {
	switch e.kind {
	case kindDir:
		return modeDir | e.perm
	case kindSymlink:
		return modeSymlink | e.perm
	case kindOther:
		// XAR's remaining types are fifo, socket and device nodes. None has
		// contents, and a caller that has to tell them apart needs more than a
		// uint16, so they share one marker rather than being guessed at.
		return modeFIFO | e.perm
	default:
		return modeRegular | e.perm
	}
}

// fileType is the DirEntry type byte this organisation's drivers agree on:
// 2 for a directory, 10 (DT_LNK) for a symlink, 0 for everything else.
func (e *entry) fileType() uint8 {
	switch e.kind {
	case kindDir:
		return 2
	case kindSymlink:
		return 10
	default:
		return 0
	}
}
