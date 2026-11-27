package erofsspec_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/forkcloser/go-erofs/internal/erofsspec"
)

// TestBlockMapEntryOfAllOnesIsAHoleUnderThe48BitFlag: the 48-bit flag of a
// chunk format widens the address of an 8-byte chunk index; a block map's
// 4-byte entry is a hole when it is all ones, with the flag as without
// (data.c, erofs_map_blocks: `startblk != (u32)EROFS_NULL_ADDR`).
func TestBlockMapEntryOfAllOnesIsAHoleUnderThe48BitFlag(t *testing.T) {
	t.Parallel()

	img := bytes.Clone(committedImage(t, "chunk-blockmap"))

	ref, err := erofsspec.Parse(img)
	if err != nil {
		t.Fatal(err)
	}

	in, err := ref.Resolve("sizes/4097", 0)
	if err != nil {
		t.Fatal(err)
	}

	if in.Layout != erofsspec.LayoutChunkBased || in.ChunkFormat&erofsspec.ChunkFormatIndexes != 0 {
		t.Fatalf("sizes/4097 is not a block map: layout %d, chunk format %#x", in.Layout, in.ChunkFormat)
	}

	// A nid counts 32-byte slots from meta_blkaddr; the map follows the
	// inode and its xattrs, on a multiple of its entry's four bytes.
	loc := int64(ref.Super.MetaBlkAddr)*int64(ref.Super.BlockSize) + int64(in.Nid)*32

	size := int64(32)
	if in.Extended {
		size = 64
	}

	first := (loc + size + int64(in.XattrSize) + 3) / 4 * 4
	binary.LittleEndian.PutUint32(img[first:], 0xFFFFFFFF)

	// The chunk format is the low sixteen bits of i_u, at byte 16.
	for _, flag := range []uint16{0, erofsspec.ChunkFormat48Bit} {
		format := binary.LittleEndian.Uint16(img[loc+16:]) | flag
		binary.LittleEndian.PutUint16(img[loc+16:], format)

		edited, err := erofsspec.Parse(withChecksum(img))
		if err != nil {
			t.Fatal(err)
		}

		file, err := edited.Inode(in.Nid)
		if err != nil {
			t.Fatalf("chunk format %#x: %v", format, err)
		}

		data, err := edited.Read(file)
		if err != nil || len(data) != 4097 || !bytes.Equal(data[:4096], make([]byte, 4096)) {
			t.Errorf("chunk format %#x: read %d bytes, %v; want 4097, the first block a hole", format, len(data), err)
		}
	}
}
