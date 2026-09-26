// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package xar

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Magic is the four-byte signature at offset 0 of every XAR archive: 'xar!'.
const Magic uint32 = 0x78617221

const (
	// minHeaderSize is the size of the header this package knows. The header's
	// own HeaderSize field may be larger — the field exists so the structure
	// can grow — and the heap is based on that field, not on this constant.
	minHeaderSize = 28

	// version1 is the only XAR version there has ever been.
	version1 = 1

	// maxTOC bounds the decompressed table of contents. The header declares
	// the uncompressed length, and this is the largest declaration accepted;
	// without a bound, a hundred-byte zlib stream in an untrusted archive can
	// ask for gigabytes before anything has been parsed.
	maxTOC = 64 << 20
)

// header is the decoded 28-byte big-endian archive header.
type header struct {
	headerSize      uint16
	version         uint16
	tocCompressed   uint64
	tocUncompressed uint64
	checksumAlg     uint32
}

// parseHeader reads and validates the header of an archive of the given size.
func parseHeader(r io.ReaderAt, size int64) (*header, error) {
	if size < minHeaderSize {
		return nil, fmt.Errorf("%w: archive is %d bytes, shorter than the %d-byte header",
			ErrBadHeader, size, minHeaderSize)
	}
	var buf [minHeaderSize]byte
	if _, err := r.ReadAt(buf[:], 0); err != nil {
		return nil, fmt.Errorf("xar: read header: %w", err)
	}
	if got := binary.BigEndian.Uint32(buf[0:4]); got != Magic {
		return nil, fmt.Errorf("%w: magic is %#08x, want %#08x", ErrBadMagic, got, Magic)
	}
	h := &header{
		headerSize:      binary.BigEndian.Uint16(buf[4:6]),
		version:         binary.BigEndian.Uint16(buf[6:8]),
		tocCompressed:   binary.BigEndian.Uint64(buf[8:16]),
		tocUncompressed: binary.BigEndian.Uint64(buf[16:24]),
		checksumAlg:     binary.BigEndian.Uint32(buf[24:28]),
	}
	if h.headerSize < minHeaderSize {
		return nil, fmt.Errorf("%w: HeaderSize is %d, below the %d bytes the header occupies",
			ErrBadHeader, h.headerSize, minHeaderSize)
	}
	if h.version != version1 {
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedVersion, h.version)
	}
	if h.tocCompressed == 0 {
		return nil, fmt.Errorf("%w: TOCLengthCompressed is zero", ErrBadHeader)
	}
	if h.tocUncompressed == 0 || h.tocUncompressed > maxTOC {
		return nil, fmt.Errorf("%w: TOCLengthUncompressed is %d, outside 1..%d",
			ErrBadHeader, h.tocUncompressed, maxTOC)
	}
	// The heap starts after the header and the compressed table of contents,
	// so both must fit inside the archive.
	//
	// HeaderSize is bounded FIRST and separately. The obvious single check,
	// `h.tocCompressed > uint64(size)-uint64(h.headerSize)`, is wrong for a
	// header that declares itself larger than the whole archive: the unsigned
	// subtraction wraps to roughly 2^64 and the comparison then accepts every
	// length there is.
	if int64(h.headerSize) > size {
		return nil, fmt.Errorf("%w: HeaderSize is %d, past the end of a %d-byte archive",
			ErrBadHeader, h.headerSize, size)
	}
	if h.tocCompressed > uint64(size-int64(h.headerSize)) {
		return nil, fmt.Errorf("%w: table of contents is %d bytes at %d, past the end of a %d-byte archive",
			ErrBadHeader, h.tocCompressed, h.headerSize, size)
	}
	return h, nil
}

// heapStart is the file offset the heap begins at, and the base every entry's
// <offset> is relative to.
//
// It is HeaderSize + TOCLengthCompressed. Both halves matter: using the
// constant 28 rather than the declared HeaderSize misreads any archive whose
// header grew, and using TOCLengthUncompressed rather than the compressed
// length lands in the middle of the heap, because the compressed table of
// contents is what physically occupies the file.
func (h *header) heapStart() int64 { return int64(h.headerSize) + int64(h.tocCompressed) }
