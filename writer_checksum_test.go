// Copyright 2026 Forkcloser
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
// verifying it opens the image and a flipped byte anywhere in that span, the
// padding and the device slots included, is corruption.
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
