package erofsspec_test

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/forkcloser/go-erofs/internal/erofsspec"
)

// TestPrefixTableIsThePackedInodesUnderFragmentsAlone: the mount takes a
// packed inode when the image has the FRAGMENTS feature (super.c), and the
// long xattr prefix table is read from it then (xattr.c). A packed_nid
// without the feature names nothing: the table is the image's own, at
// xattr_prefix_start, whatever PLAIN_XATTR_PFX says.
func TestPrefixTableIsThePackedInodesUnderFragmentsAlone(t *testing.T) {
	t.Parallel()

	img := bytes.Clone(committedImage(t, "xattr-prefix-512"))

	ref, err := erofsspec.Parse(img)
	if err != nil {
		t.Fatal(err)
	}

	if ref.Super.FeatureIncompat&erofsspec.FeatureIncompatFragments != 0 || len(ref.Prefixes) == 0 {
		t.Fatalf(
			"the image has FRAGMENTS or no prefix: incompat %#x, %d prefixes",
			ref.Super.FeatureIncompat,
			len(ref.Prefixes),
		)
	}

	file, err := ref.Resolve("epoch", 0)
	if err != nil {
		t.Fatal(err)
	}

	// PLAIN_XATTR_PFX cleared, and a regular file named as the packed inode.
	compat := binary.LittleEndian.Uint32(img[erofsspec.SuperOffset+8:]) &^ erofsspec.FeatureCompatPlainXattrPfx
	binary.LittleEndian.PutUint32(img[erofsspec.SuperOffset+8:], compat)
	binary.LittleEndian.PutUint64(img[erofsspec.SuperOffset+96:], file.Nid)

	edited, err := erofsspec.Parse(withChecksum(img))
	if err != nil {
		t.Fatalf("Parse: %v; the kernel mounts this image", err)
	}

	if !slices.Equal(edited.Prefixes, ref.Prefixes) {
		t.Errorf("prefixes %v, want the image's own %v", edited.Prefixes, ref.Prefixes)
	}
}
