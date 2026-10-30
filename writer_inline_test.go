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
	"io/fs"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/disk"
)

// TestWriterNoInlineData: by default a file that fits beside its inode is
// tail-packed there (the flat-inline layout, its range inside the metadata),
// and a file of a block or more is flat-plain; with WithNoInlineData every
// regular file is flat-plain with its range on a block boundary, while
// directories and symlinks keep their inline data, and every file reads back
// the same either way.
func TestWriterNoInlineData(t *testing.T) {
	t.Parallel()

	files := map[string][]byte{
		"/small":   []byte("eleven byte"),
		"/exact":   bytes.Repeat([]byte("b"), 4096),
		"/overone": bytes.Repeat([]byte("c"), 4097),
		"/empty":   {},
	}

	for _, inline := range []bool{true, false} {
		var opts []erofs.CreateOpt
		if !inline {
			opts = append(opts, erofs.WithNoInlineData())
		}

		var buf testBuffer

		w := erofs.Create(&buf, opts...)

		for name, content := range files {
			writeFile(t, w, name, content)
		}

		if err := w.Mkdir("/d", 0o755); err != nil {
			t.Fatal(err)
		}

		if err := w.Symlink("small", "/l"); err != nil {
			t.Fatal(err)
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		img := buf.Bytes()

		fsys, err := erofs.Open(bytes.NewReader(img))
		if err != nil {
			t.Fatal(err)
		}

		for name, content := range files {
			got, err := fs.ReadFile(fsys, name[1:])
			if err != nil || !bytes.Equal(got, content) {
				t.Fatalf("inline %v: %s: %d bytes, %v; want %d", inline, name, len(got), err, len(content))
			}

			if len(content) == 0 {
				continue
			}

			layout := layoutOf(t, img, name[1:])

			st, err := fs.Lstat(fsys, name[1:])
			if err != nil {
				t.Fatal(err)
			}

			ranges := st.(interface{ DataRange() []erofs.DataRange }).DataRange()

			if inline && name == "/small" {
				// Beside the inode: the range is not on a block boundary.
				if layout != disk.LayoutFlatInline || ranges[0].Offset%4096 == 0 {
					t.Fatalf("default: %s layout %d, ranges %+v; want inline beside the inode", name, layout, ranges)
				}

				continue
			}

			if layout != disk.LayoutFlatPlain || len(ranges) != 1 || ranges[0].Offset%4096 != 0 {
				t.Fatalf(
					"inline %v: %s layout %d, ranges %+v; want one block-aligned flat-plain range",
					inline, name, layout, ranges,
				)
			}
		}

		// Directories and symlinks keep their inline data either way.
		for _, name := range []string{"d", "l"} {
			if l := layoutOf(t, img, name); l != disk.LayoutFlatInline {
				t.Fatalf("inline %v: %s's layout is %d; want inline", inline, name, l)
			}
		}

		if target, err := fs.ReadLink(fsys, "l"); err != nil || target != "small" {
			t.Fatalf("inline %v: ReadLink %q, %v", inline, target, err)
		}
	}
}

// layoutOf is the data layout the inode of name declares in its i_format.
func layoutOf(t *testing.T, img []byte, name string) uint8 {
	t.Helper()

	at := inodeOffset(t, img, name)

	return uint8((binary.LittleEndian.Uint16(img[at:]) & disk.InodeFormatLayoutMask) >> 1)
}
