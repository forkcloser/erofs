package erofsspec_test

// The reference is anchored to erofs-utils before it judges anything: the
// corpus tree is written to a tar, mkfs.erofs builds an image from it under
// every option the reader claims to read, and the reference parses the
// image back to the tree, field for field, with nothing of the erofs
// package involved. The committed images run on every lane; the full tree
// is built and checked where mkfs.erofs is present. A disagreement is the
// reference's defect or mkfs.erofs's, never the package's.

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"testing"

	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/erofsspec"
)

// TestAnchoredToCommitted: every committed image parses back to its tree.
func TestAnchoredToCommitted(t *testing.T) {
	t.Parallel()

	for _, im := range corpus.Committed(t) {
		t.Run(im.Name, func(t *testing.T) {
			t.Parallel()

			ref, err := erofsspec.Parse(im.Img, im.Extra())
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			// An image with no tree is one the kernel refuses past the
			// superblock: the walk must say so, as unsupported.
			if im.Tree == nil {
				if err := ref.Walk(
					func(erofsspec.Entry) error { return nil },
				); !errors.Is(
					err,
					erofsspec.ErrUnsupported,
				) {
					t.Fatalf("Walk: %v, want ErrUnsupported", err)
				}

				return
			}

			corpus.Check(t, im, ref, im.Tree, false)
		})
	}
}

// TestAnchoredToMkfs: every image mkfs.erofs builds from the full tree
// under the whole matrix parses back to it.
func TestAnchoredToMkfs(t *testing.T) {
	t.Parallel()

	corpus.MkfsPath(t)

	nodes := corpus.Full()
	tarball := corpus.Tar(t, nodes)
	nsec := keepsNanoseconds(t)

	for name, args := range corpus.Matrix() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			im := corpus.Mkfs(t, name, args, tarball)
			corpus.Fsck(t, im)

			ref, err := erofsspec.Parse(im.Img, im.Extra())
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			corpus.Check(t, im, ref, nodes, nsec)
		})
	}
}

// keepsNanoseconds says whether this mkfs.erofs stores a tar entry's
// sub-second mtime: the erofs-utils build does so only where configure
// found the stat structure's nanosecond member (include/erofs/defs.h,
// ST_MTIM_NSEC_SET), which the macOS build of 1.9.4 does not; there every
// inode carries zero nanoseconds and the expectation follows the tool.
func keepsNanoseconds(tb testing.TB) bool {
	tb.Helper()

	probe := []corpus.Node{
		{Path: "f", Type: tar.TypeReg, Mode: 0o644, Data: []byte("ns"), Sec: 1_700_000_000, Nsec: 123_456_789},
	}
	im := corpus.Mkfs(tb, "nsec probe", []string{"-b4096"}, corpus.Tar(tb, probe))

	ref, err := erofsspec.Parse(im.Img)
	if err != nil {
		tb.Fatalf("nsec probe: %v", err)
	}

	in, err := ref.Resolve("f", 0)
	if err != nil {
		tb.Fatalf("nsec probe: %v", err)
	}

	switch in.MtimeNs {
	case 123_456_789:
		return true
	case 0:
		tb.Log("this mkfs.erofs build stores no nanoseconds: expecting zero")

		return false
	default:
		tb.Fatalf("nsec probe: %d nanoseconds, want 123456789 or 0", in.MtimeNs)

		return false
	}
}

// TestRefusals: the kernel's refusals the reference reproduces on a
// committed image edited at the documented offsets, each a rule cited in
// the reader.
func TestRefusals(t *testing.T) {
	t.Parallel()

	base := committedImage(t, "small-b4096-extended")

	edit := func(off int, b ...byte) []byte {
		img := bytes.Clone(base)
		copy(img[off:], b)

		return img
	}

	for _, tc := range []struct {
		name string
		img  []byte
		want error
	}{
		{"magic", edit(erofsspec.SuperOffset, 0), erofsspec.ErrCorrupt},
		{"checksum", edit(erofsspec.SuperOffset+4, 0, 0, 0, 0), erofsspec.ErrCorrupt},
		{"blkszbits 8", edit(erofsspec.SuperOffset+12, 8), erofsspec.ErrCorrupt},
		{"blkszbits 17", edit(erofsspec.SuperOffset+12, 17), erofsspec.ErrCorrupt},
		{"dirblkbits", edit(erofsspec.SuperOffset+90, 1), erofsspec.ErrCorrupt},
		{"unknown incompat", edit(erofsspec.SuperOffset+80, 0, 0x02), erofsspec.ErrCorrupt},
		{"metabox", edit(erofsspec.SuperOffset+80, 0x00, 0x01), erofsspec.ErrUnsupported},
		{"too short", base[:erofsspec.SuperOffset+100], erofsspec.ErrTruncated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			img := tc.img
			if tc.name != "checksum" && tc.name != "too short" {
				img = withChecksum(img)
			}

			_, err := erofsspec.Parse(img)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Parse: %v, want %v", err, tc.want)
			}
		})
	}
}

func committedImage(tb testing.TB, name string) []byte {
	tb.Helper()

	for _, im := range corpus.Committed(tb) {
		if im.Name == name {
			return im.Img
		}
	}

	tb.Fatalf("no committed image %q", name)

	return []byte{}
}

// withChecksum recomputes the superblock checksum after an edit, as
// super.c's erofs_superblock_csum_verify computes it.
func withChecksum(img []byte) []byte {
	n := 1 << img[erofsspec.SuperOffset+12]
	if n > erofsspec.SuperOffset {
		n -= erofsspec.SuperOffset
	}

	n -= 8
	if n < 0 || erofsspec.SuperOffset+8+n > len(img) {
		return img
	}

	crc := ^crc32.Update(
		^uint32(erofsspec.ChecksumSeed),
		crc32.MakeTable(crc32.Castagnoli),
		img[erofsspec.SuperOffset+8:erofsspec.SuperOffset+8+n],
	)
	binary.LittleEndian.PutUint32(img[erofsspec.SuperOffset+4:], crc)

	return img
}
