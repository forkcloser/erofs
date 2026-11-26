package erofs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io/fs"
	"testing"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
	"github.com/forkcloser/go-erofs/internal/disk"
)

// rootInodeAt is where the root's inode starts in img: the inode table's
// first byte, and 32 bytes a slot to the root's nid.
func rootInodeAt(img []byte) int {
	sb := img[disk.SuperBlockOffset:]

	return int(binary.LittleEndian.Uint32(sb[40:]))<<sb[12] + int(binary.LittleEndian.Uint16(sb[14:]))*32
}

// soundImage is an image of one file that opens, without the checksum an
// edit would have to redo.
func soundImage(t *testing.T) []byte {
	t.Helper()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
	writeFile(t, w, "/a", []byte("a"))

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	img := out.Bytes()
	img[disk.SuperBlockOffset+8] &^= disk.FeatureCompatSBChksum

	if _, err := erofs.Open(bytes.NewReader(img)); err != nil {
		t.Fatalf("the image before any edit: %v", err)
	}

	return img
}

// TestContractOpenWithFragmentsAndNoPackedInode: a packed nid of zero is no
// packed inode, whatever the feature says, and the mount reads none
// (super.c: "erofs_sb_has_fragments(sbi) && sbi->packed_nid"). Nid 0 is the
// inode that starts the table, here the root, and no file to read as one.
func TestContractOpenWithFragmentsAndNoPackedInode(t *testing.T) {
	t.Parallel()

	img := soundImage(t)
	sb := img[disk.SuperBlockOffset:]

	if nid := binary.LittleEndian.Uint16(sb[14:]); nid != 0 {
		t.Fatalf("the root is nid %d; the test wants it at 0, where a packed nid of zero points", nid)
	}

	binary.LittleEndian.PutUint32(sb[80:], binary.LittleEndian.Uint32(sb[80:])|disk.FeatureIncompatFragments)
	binary.LittleEndian.PutUint64(sb[96:], 0)

	fsys, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("Open = %v; want the image opened", err)
	}

	if got, err := fs.ReadFile(fsys, "a"); err != nil || string(got) != "a" {
		t.Fatalf("ReadFile(a) = %q, %v", got, err)
	}
}

// TestContractOpenRefusesWhatTheKernelDoesNotMount: the kernel reads the
// root at mount and mounts nothing when it cannot be read or is no
// directory (super.c: "rootino(nid %llu) is not a directory"), and the
// packed inode likewise when the image has one. Open used to take such an
// image and fail at every call made on it afterwards.
func TestContractOpenRefusesWhatTheKernelDoesNotMount(t *testing.T) {
	t.Parallel()

	for name, edit := range map[string]func(img []byte){
		// i_mode is the 16 bits at byte 4 of either inode form.
		"a root that is a regular file": func(img []byte) {
			binary.LittleEndian.PutUint16(img[rootInodeAt(img)+4:], disk.StatTypeReg|0o644)
		},
		"a root that is a symbolic link": func(img []byte) {
			binary.LittleEndian.PutUint16(img[rootInodeAt(img)+4:], disk.StatTypeSymlink|0o777)
		},
		"a root whose mode names no type": func(img []byte) {
			binary.LittleEndian.PutUint16(img[rootInodeAt(img)+4:], 0o644)
		},
		// The kernel's erofs_read_inode refuses an i_format with a bit it
		// does not know.
		"a root whose format has an undefined bit": func(img []byte) {
			at := rootInodeAt(img)
			binary.LittleEndian.PutUint16(img[at:], binary.LittleEndian.Uint16(img[at:])|0x8000)
		},
		// FRAGMENTS and a packed inode far past the image: the mount reads
		// it before the root.
		"a packed inode that cannot be read": func(img []byte) {
			sb := img[disk.SuperBlockOffset:]
			binary.LittleEndian.PutUint32(sb[80:], binary.LittleEndian.Uint32(sb[80:])|disk.FeatureIncompatFragments)
			binary.LittleEndian.PutUint64(sb[96:], 1<<40)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			img := soundImage(t)
			edit(img)

			_, err := erofs.Open(bytes.NewReader(img))
			if !errors.Is(err, erofs.ErrCorrupt) && !errors.Is(err, erofs.ErrNotImplemented) {
				t.Errorf("Open = %v; want the image refused, ErrCorrupt or ErrNotImplemented", err)
			}
		})
	}
}
