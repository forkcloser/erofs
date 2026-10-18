// Copyright 2026 Forkcloser. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package erofs_test

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
)

// TestCopyFromChunkOnMappedDevice: a chunk addressed on the primary device
// lands where the kernel's erofs_map_dev lands it, inside an extra device's
// mapped range on that device, so the metadata-only copy names that device
// and the block on it, as it already did for flat files; and a chunk the
// reader refuses at Stat, its bytes past the device, is refused by the copy
// in the same class. The input is the fuzzer's: small-blobdev-512 with the
// device id of d/sub/e07's one chunk masked to 0 by the one-device mask, so
// its block 14 lies in the blob mapped at block 13 of a 13-block image.
func TestCopyFromChunkOnMappedDevice(t *testing.T) {
	t.Parallel()

	for _, src := range corpus.Committed(t) {
		if src.Name != "small-blobdev-512" {
			continue
		}

		edited := bytes.Clone(src.Img)
		edited[3010] = 0x5a

		source := openWithDevices(t, edited, src.Blob)

		want, err := fs.ReadFile(source, "d/sub/e07")
		if err != nil || len(want) != 1 {
			t.Fatalf("test premise broken: e07 through the blob's mapped range: %q, %v", want, err)
		}

		var buf testBuffer

		w := erofs.Create(&buf)
		if err = w.CopyFrom(source, erofs.MetadataOnly()); err != nil {
			t.Fatal(err)
		}

		if err = w.Close(); err != nil {
			t.Fatal(err)
		}

		copied := openWithDevices(t, buf.Bytes(), edited, src.Blob)

		got, err := fs.ReadFile(copied, "d/sub/e07")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("the copy reads e07 as %q, %v; want %q", got, err, want)
		}

		sameTree(t, source, copied)

		// Block 60 lies past both the image's 13 blocks and the blob's 47
		// from block 13: Stat refuses the source, and so must the copy.
		edited[3012] = 0x3c
		source = openWithDevices(t, edited, src.Blob)

		if _, err := fs.Stat(source, "d/sub/e07"); !errors.Is(err, erofs.ErrCorrupt) {
			t.Fatalf("test premise broken: Stat of a chunk past every device: %v", err)
		}

		w = erofs.Create(&buf)
		if err := w.CopyFrom(source, erofs.MetadataOnly()); !errors.Is(err, erofs.ErrCorrupt) {
			t.Fatalf("CopyFrom(MetadataOnly) of a chunk past every device: %v; want ErrCorrupt", err)
		}
	}
}
