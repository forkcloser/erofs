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
	"io/fs"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
)

// TestCopyFromShortDirectoryBlock: a directory whose size leaves a block
// shorter than one dirent is refused by the reader's ReadDir, as the
// kernel's readdir refuses it, and the metadata-only copy refuses it in the
// same class rather than copying the directory short of its entries. The
// first input is the fuzzer's: b512 with the size of the directory at nid
// 55 set to 6; the second a Writer directory given a size of 6.
func TestCopyFromShortDirectoryBlock(t *testing.T) {
	t.Parallel()

	for _, im := range corpus.Committed(t) {
		if im.Name != "b512" {
			continue
		}

		edited := bytes.Clone(im.Img)
		edited[1768] = 0x06

		shortDirRefusedByBoth(t, edited)
	}

	img := buildImage(t, func(w *erofs.Writer) {
		if err := w.Mkdir("/d", 0o755); err != nil {
			t.Fatal(err)
		}

		writeFile(t, w, "/d/x", []byte("x"))
	})
	// i_size is the eight bytes at offset 8 of the extended inode.
	binary.LittleEndian.PutUint64(img[inodeOffset(t, img, "d")+8:], 6)

	shortDirRefusedByBoth(t, img)
}

// shortDirRefusedByBoth: walking img meets ErrCorrupt, and so does a
// metadata-only copy of it.
func shortDirRefusedByBoth(t *testing.T, img []byte) {
	t.Helper()

	source := openWithDevices(t, img)
	refused := false

	err := fs.WalkDir(source, ".", func(_ string, _ fs.DirEntry, err error) error {
		if errors.Is(err, erofs.ErrCorrupt) {
			refused = true

			return fs.SkipDir
		}

		return err
	})
	if err != nil || !refused {
		t.Fatalf("test premise broken: walking the image: %v, refused=%v; want one ErrCorrupt", err, refused)
	}

	var buf testBuffer

	w := erofs.Create(&buf)
	if err := w.CopyFrom(source, erofs.MetadataOnly()); !errors.Is(err, erofs.ErrCorrupt) {
		t.Fatalf("CopyFrom(MetadataOnly) of a directory block shorter than a dirent: %v; want ErrCorrupt", err)
	}
}
