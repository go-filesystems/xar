// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

// Package xar is a pure-Go, read-only driver for the XAR (eXtensible ARchive)
// format — the container a macOS `.pkg` installer is, and the format
// /usr/bin/xar writes.
//
// OpenReader returns a filesystem.Filesystem over an archive held in any
// io.ReaderAt, so a `.pkg` can be walked and read without being unpacked. The
// optional filesystem.Opener capability is implemented, so a caller can stream
// one member instead of materialising it.
//
// # The layout
//
// A 28-byte big-endian header, a zlib-compressed XML table of contents, then a
// heap of member data:
//
//	offset  size  field
//	0       4     magic, 'xar!' (0x78617221)
//	4       2     HeaderSize — the size of THIS header
//	6       2     Version
//	8       8     TOCLengthCompressed
//	16      8     TOCLengthUncompressed
//	24      4     ChecksumAlgorithm
//
// The heap begins at HeaderSize + TOCLengthCompressed. HeaderSize, not the
// constant 28, is the base: the field exists so the header can grow, and a
// reader that assumes 28 is reading a different file than the one it was given.
// Every member's <offset> is relative to that heap start, never to the file.
//
// The table of contents is <xar><toc>, with <file> elements nested inside one
// another to express directories. A member's path is therefore built by walking
// the nesting — there is no path string anywhere in the format, only a <name>
// per level.
//
// # Four things the format does that a reader will get wrong
//
// Each of these was observed in an archive written by /usr/bin/xar 1.8dev, not
// inferred from a specification.
//
//  1. encoding style="application/x-gzip" IS NOT GZIP. It is a raw zlib
//     stream (RFC 1950): 0x78 0xda, no gzip member header, no gzip trailer.
//     compress/gzip refuses it outright, which at least fails loudly; the
//     naming is the trap. The stored encoding is "application/octet-stream"
//     and bzip2 is "application/x-bzip2".
//
//  2. AN EMPTY FILE HAS NO <data> ELEMENT AT ALL. It is not <data> with
//     <size>0</size>; the element is simply absent. A reader that requires
//     <data> loses every empty member.
//
//  3. <mode> IS OCTAL, AND SOMETIMES HAS NO LEADING ZERO. xar writes
//     <mode>0644</mode> for an ordinary file but <mode>4755</mode> for a
//     setuid one. Both are octal. strconv.ParseUint with base 0 is therefore
//     wrong in a way no leading-zero mode can reveal: it reads "0644" as
//     octal, because of the zero, and "4755" as decimal. Only an explicit
//     base 8 is right.
//
//  4. <ea> ELEMENTS LOOK EXACTLY LIKE <data>. An extended attribute is stored
//     in the heap with its own <offset>, <length>, <size> and <encoding>, and
//     its own <name>. A reader that searches a <file> for any descendant
//     offset/length hands back an xattr's bytes as the file's contents — most
//     visibly on an empty file, which has an <ea> and no <data>. A reader that
//     takes any descendant <name> names the file after the xattr.
//
// Members are compressed individually, each with its own encoding, so there is
// no single archive-wide stream and no flat offset index into decoded bytes.
//
// # What this package does not do
//
// The header's ChecksumAlgorithm and the TOC's own <checksum> element are
// parsed past, not verified, and neither are the per-member
// <extracted-checksum>/<archived-checksum> digests. Nothing here authenticates
// an archive; a `.pkg` signature in particular is not checked, and a caller
// that needs provenance must establish it by other means.
//
// LZMA-encoded members ("application/x-lzma") are recognised and refused with
// ErrUnsupportedEncoding rather than decoded. The refusal is per member and
// happens when the data is read, so an archive with one LZMA member still
// opens and lists. It is untested against a real archive on purpose: macOS's
// xar 1.8dev reports "lzma support not compiled in" and cannot write one, and a
// fixture hand-built to agree with this reader would prove nothing about it.
//
// Extended attributes, uid/gid, the timestamps and the Finder metadata are
// read past. An archive is read-only: every mutating method of
// filesystem.Filesystem returns ErrReadOnly.
package xar
