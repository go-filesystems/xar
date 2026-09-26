// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package xar

import (
	"compress/bzip2"
	"compress/zlib"
	"fmt"
	"io"
)

// The encoding style strings XAR puts in <encoding style="…"/>.
const (
	// encodingStored is data written to the heap unchanged.
	encodingStored = "application/octet-stream"

	// encodingZlib is a RAW ZLIB STREAM (RFC 1950) — NOT gzip, whatever the
	// name says. An archive written by /usr/bin/xar has 0x78 0xda here, a zlib
	// header with no gzip member header and no gzip trailer, so compress/gzip
	// rejects it. The media type is simply the wrong name for what xar writes,
	// and it is the name every XAR archive uses for its default compression.
	encodingZlib = "application/x-gzip"

	// encodingBzip2 is a bzip2 stream, and is named after what it is.
	encodingBzip2 = "application/x-bzip2"
)

// stored reports whether a style means the bytes in the heap are the bytes of
// the member. Those entries get true random access, straight through to the
// backing io.ReaderAt, with nothing held in memory and no serialisation between
// concurrent readers.
//
// An absent <encoding> element leaves the style empty and is treated as stored,
// which is also what an entry with no <data> at all gets.
func stored(style string) bool {
	return style == "" || style == encodingStored
}

// decoder wraps r in the decompressor for style. The returned ReadCloser must
// be closed by the caller.
func decoder(style string, r io.Reader) (io.ReadCloser, error) {
	switch {
	case stored(style):
		return io.NopCloser(r), nil
	case style == encodingZlib:
		zr, err := zlib.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("%w: zlib stream: %w", ErrCorrupt, err)
		}
		return zr, nil
	case style == encodingBzip2:
		// bzip2.NewReader has nothing to fail at construction: it reports a bad
		// stream from Read. There is no Close to call either, hence NopCloser.
		return io.NopCloser(bzip2.NewReader(r)), nil
	default:
		// "application/x-lzma" lands here, and is meant to. macOS's xar 1.8dev
		// reports "lzma support not compiled in" and cannot write one, so this
		// package has never been shown a real archive containing one; a decoder
		// judged only against a fixture written to agree with it would measure
		// nothing. The refusal names the style, which is all a caller needs.
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedEncoding, style)
	}
}
