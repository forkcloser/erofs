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
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/forkcloser/erofs"
)

// TestReaderSharedXattrOutsideImage: a shared xattr id that points outside
// a complete image is corruption, not the reader's end of file. The images
// are assembled from the format's definitions (the Writer stores every
// xattr inline, mkfs.erofs shares only from a directory source): a 4 KiB
// block image whose inode a is compact, and a 512 byte block image whose
// inode a is extended; in each, the last byte of a's first shared id is
// set, which sends the id gigabytes past the end.
func TestReaderSharedXattrOutsideImage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		file string
		at   int
	}{
		{"internal/corpus/testdata/assembled-shared.erofs", 4271},
		{"internal/corpus/testdata/assembled-shared-512-extended.erofs", 1775},
	} {
		img, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}

		fsys, err := erofs.Open(bytes.NewReader(img))
		if err != nil {
			t.Fatal(err)
		}

		st, err := fs.Lstat(fsys, "a")
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}

		if len(st.Sys().(*erofs.Stat).Xattrs) == 0 {
			t.Fatalf("%s: a has no xattrs; the fixture should carry shared ones", tc.file)
		}

		bad := bytes.Clone(img)
		bad[tc.at] = 0xFF

		fsys, err = erofs.Open(bytes.NewReader(bad))
		if err != nil {
			t.Fatal(err)
		}

		if _, err = fs.Lstat(fsys, "a"); !errors.Is(err, erofs.ErrCorrupt) {
			t.Fatalf("%s: a shared id past the image: %v; want ErrCorrupt", tc.file, err)
		}
	}
}
