// Copyright 2026 Forkcloser. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package erofs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io/fs"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/disk"
)

// TestWriterSuperblockChecksum: every image the Writer produces carries the
// superblock checksum mkfs.erofs writes by default: the SB_CHKSUM feature
// set, the stored value the kernel's CRC-32C over the span from the byte
// after the checksum to the end of the superblock's block, so a reader
// verifying it
// opens the image and a flipped byte anywhere in that span, the padding and
// the device slots included, is corruption.
func TestWriterSuperblockChecksum(t *testing.T) {
	t.Parallel()

	for _, blockSize := range []int{512, 4096, 65536} {
		var buf testBuffer

		w := erofs.Create(&buf, erofs.WithBlockSize(blockSize))
		writeFile(t, w, "/a", bytes.Repeat([]byte("a"), 3000))

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		img := buf.Bytes()
		sb := img[disk.SuperBlockOffset:]

		if binary.LittleEndian.Uint32(sb[8:12])&disk.FeatureCompatSBChksum == 0 {
			t.Fatalf("block size %d: SB_CHKSUM not set", blockSize)
		}

		span := blockSize
		if span > disk.SuperBlockOffset {
			span -= disk.SuperBlockOffset
		}

		span -= 8

		want := ^crc32.Update(^uint32(0x5045B54A), crc32.MakeTable(crc32.Castagnoli), sb[8:8+span])
		if got := binary.LittleEndian.Uint32(sb[4:8]); got != want {
			t.Fatalf("block size %d: checksum %#08x; the kernel's formula gives %#08x", blockSize, got, want)
		}

		fsys, err := erofs.Open(bytes.NewReader(img))
		if err != nil {
			t.Fatalf("block size %d: %v", blockSize, err)
		}

		if got, err := fs.ReadFile(fsys, "a"); err != nil || len(got) != 3000 {
			t.Fatalf("block size %d: %d bytes, %v", blockSize, len(got), err)
		}

		// A byte of the padding past the superblock, inside the span.
		bad := bytes.Clone(img)
		bad[disk.SuperBlockOffset+disk.SizeSuperBlock+64] ^= 0x01

		if _, err := erofs.Open(bytes.NewReader(bad)); !errors.Is(err, erofs.ErrCorrupt) {
			t.Fatalf("block size %d: a flipped byte in the span: %v; want ErrCorrupt", blockSize, err)
		}
	}
}
