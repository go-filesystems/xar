// Copyright (c) 2026, go-filesystems
// SPDX-License-Identifier: BSD-3-Clause

package xar

import (
	"errors"
	iofs "io/fs"
	"testing"
)

// TestMutatorsAreRefused covers every mutating method of
// filesystem.Filesystem. XAR is an archive: there is no in-place edit of one.
//
// The refusal is asserted to satisfy iofs.ErrPermission as well as ErrReadOnly,
// because the interface module's error contract is about CLASSIFICATION. A
// server over this filesystem has to turn the refusal into a status code, and it
// cannot do that from a message -- go-filesystems/webdav and .../nfs each carry
// a copied table of twelve message fragments for drivers that made them guess.
func TestMutatorsAreRefused(t *testing.T) {
	f := openFixture(t, mixedXar)
	for name, err := range map[string]error{
		"WriteFile":  f.WriteFile("/x", []byte("x"), 0o644),
		"MkDir":      f.MkDir("/x", 0o755),
		"DeleteFile": f.DeleteFile("/plain.txt"),
		"DeleteDir":  f.DeleteDir("/nested"),
		"Rename":     f.Rename("/plain.txt", "/y"),
	} {
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s = %v, want ErrReadOnly", name, err)
		}
		if !errors.Is(err, iofs.ErrPermission) {
			t.Errorf("%s = %v, which errors.Is cannot classify as a permission error", name, err)
		}
	}
}

// TestErrNotExistContract is this organisation's cross-driver contract: a path
// that is not there must satisfy errors.Is(err, iofs.ErrNotExist). Measured
// across the family once and found to hold in one driver of fourteen, so each
// driver now asserts it on its own operations, in its own repository.
func TestErrNotExistContract(t *testing.T) {
	f := openFixture(t, mixedXar)
	const missing = "/there-is-no-such-member"
	for name, err := range map[string]error{
		"Stat":     second(f.Stat(missing)),
		"ReadFile": second(f.ReadFile(missing)),
		"ListDir":  second(f.ListDir(missing)),
		"ReadLink": second(f.ReadLink(missing)),
		"OpenFile": second(f.OpenFile(missing)),
	} {
		if !errors.Is(err, iofs.ErrNotExist) {
			t.Errorf("%s(%q) = %v, want an error satisfying fs.ErrNotExist", name, missing, err)
		}
	}
}

// TestStructuralFailureIsNotNotExist is the other half of that contract, and the
// half that is easy to get wrong. A corrupt archive must NOT read as "not
// found": a 404 for a damaged archive reaches a user as a routine miss and
// nothing anywhere reports the damage.
func TestStructuralFailureIsNotNotExist(t *testing.T) {
	c := baseline()
	c.tocPayload = []byte("not a zlib stream")
	_, err := c.open()
	if err == nil {
		t.Fatal("a corrupt archive opened")
	}
	if errors.Is(err, iofs.ErrNotExist) {
		t.Errorf("a corrupt archive reports %v, which a caller will treat as a missing file", err)
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("a corrupt archive reports %v, want ErrCorrupt", err)
	}
}

// second discards a two-result call's value and keeps its error.
func second[T any](_ T, err error) error { return err }
