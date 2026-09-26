// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package xar

import (
	"errors"
	"fmt"
	iofs "io/fs"
)

// Sentinel errors. Compare with errors.Is so wrapped errors keep matching.
var (
	// ErrReadOnly is returned by every mutating method (WriteFile, MkDir,
	// DeleteFile, DeleteDir, Rename). XAR is an archive format: an existing
	// archive cannot be edited in place.
	//
	// It wraps iofs.ErrPermission so a generic caller — a WebDAV, NFS or SFTP
	// server over this filesystem — can classify it without matching on the
	// message, which is what the interface's error contract asks for.
	ErrReadOnly = fmt.Errorf("xar: archive is read-only: %w", iofs.ErrPermission)

	// ErrBadMagic is returned when the first four bytes are not 'xar!'.
	ErrBadMagic = errors.New("xar: not a xar archive")

	// ErrBadHeader is returned when the 28-byte header is present but says
	// something impossible: a header shorter than itself, a table of contents
	// that extends past the end of the archive, and so on.
	ErrBadHeader = errors.New("xar: malformed header")

	// ErrUnsupportedVersion is returned for a header version this package does
	// not know. XAR has only ever had version 1.
	ErrUnsupportedVersion = errors.New("xar: unsupported archive version")

	// ErrCorrupt is returned when the table of contents parses but describes
	// something the archive cannot contain — an offset past the end of the
	// heap, a negative length, two entries with one name in one directory, a
	// member whose data stops before its declared size.
	ErrCorrupt = errors.New("xar: corrupt archive")

	// ErrUnsupportedEncoding is returned when a member's encoding style is one
	// this package cannot decode. It is raised when the member's data is read,
	// not when the archive is opened, so one undecodable member does not make
	// the rest of the archive unreachable.
	ErrUnsupportedEncoding = errors.New("xar: unsupported entry encoding")

	// ErrNotFound is returned when a path is not in the archive. It wraps
	// iofs.ErrNotExist, which the interface module requires of every driver.
	ErrNotFound = fmt.Errorf("xar: path not found: %w", iofs.ErrNotExist)

	// ErrNotDirectory is returned when ListDir names something that is not a
	// directory.
	ErrNotDirectory = errors.New("xar: not a directory")

	// ErrNotRegular is returned when ReadFile or OpenFile names a directory, a
	// symlink, or one of the special types XAR can record (fifo, socket,
	// device node). None of them has readable contents.
	ErrNotRegular = errors.New("xar: not a regular file")

	// ErrNotSymlink is returned by ReadLink for an entry XAR did not record a
	// link target for.
	ErrNotSymlink = errors.New("xar: not a symbolic link")
)
