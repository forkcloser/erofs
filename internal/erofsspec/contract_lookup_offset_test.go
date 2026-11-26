package erofsspec_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/forkcloser/go-erofs/internal/erofsspec"
)

// TestLookupTakesANameOffsetBelowTheFirstForNoName: a directory block whose
// second name offset is below its first gives the block's first name no
// byte. namei.c compares with that and goes on (erofs_dirnamecmp: "it will
// return 1 to just skip the invalid name"), so the lookup of a name the
// directory does not have ends as not found; it is no fault of the search.
func TestLookupTakesANameOffsetBelowTheFirstForNoName(t *testing.T) {
	t.Parallel()

	img := bytes.Clone(committedImage(t, "small-b4096-extended"))

	ref, err := erofsspec.Parse(img)
	if err != nil {
		t.Fatal(err)
	}

	root, err := ref.Inode(ref.Super.RootNid)
	if err != nil {
		t.Fatal(err)
	}

	extents, err := ref.Extents(root)
	if err != nil || len(extents) == 0 {
		t.Fatalf("the root's extents: %v, %v", extents, err)
	}

	second := extents[0].Physical + erofsspec.DirentSize + 8
	if first := binary.LittleEndian.Uint16(img[extents[0].Physical+8:]); first == 0 {
		t.Fatal("the root's first name offset is zero already")
	}

	binary.LittleEndian.PutUint16(img[second:], 0)

	ref, err = erofsspec.Parse(withChecksum(img))
	if err != nil {
		t.Fatal(err)
	}

	root, err = ref.Inode(ref.Super.RootNid)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ref.Lookup(root, []byte("zzz")); !errors.Is(err, erofsspec.ErrNotExist) {
		t.Errorf("Lookup: %v, want ErrNotExist", err)
	}

	if _, err := ref.Resolve("zzz", 0); !errors.Is(err, erofsspec.ErrNotExist) {
		t.Errorf("Resolve: %v, want ErrNotExist", err)
	}
}
