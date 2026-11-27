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
	"errors"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/builder"
	"github.com/forkcloser/go-erofs/internal/corpus"
)

// What the writer refuses of a name or a target with a NUL, and the link
// count it gives a directory.

// TestWriterNameWithANul: a name with a NUL is refused by every method that
// creates one and by a copy of a source that lists one: written, it is
// another name, "a" a second time for "a\x00b" beside "a". The image that
// follows the refusals has what was created under sound names, and no more.
func TestWriterNameWithANul(t *testing.T) {
	t.Parallel()

	var buf testBuffer

	w := erofs.Create(&buf)
	writeFile(t, w, "/a", []byte("x"))

	for what, step := range map[string]func() error{
		"Create":  func() error { _, err := w.Create("/a\x00b"); return err },
		"Mkdir":   func() error { return w.Mkdir("/d\x00", 0o755) },
		"Symlink": func() error { return w.Symlink("t", "/\x00l") },
		"Link":    func() error { return w.Link("/a", "/a\x00h") },
		"Mknod":   func() error { return w.Mknod("/n\x00x", fs.ModeNamedPipe|0o600, 0) },
		"parent":  func() error { return w.Mkdir("/p\x00/x", 0o755) },
		// Into a Writer of its own: what a copy that stops leaves behind
		// is not this test's.
		"CopyFrom": func() error {
			return erofs.Create(&testBuffer{}).CopyFrom(fstest.MapFS{"sub/c\x00d": &fstest.MapFile{Data: []byte("y")}})
		},
	} {
		if err := step(); !errors.Is(err, erofs.ErrInvalid) {
			t.Fatalf("%s with a NUL in the name: %v; want ErrInvalid", what, err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	im := corpus.Image{Name: t.Name(), Img: buf.Bytes()}
	b := wBuilt{img: im.Img, ref: parseImage(t, im), fsys: openImage(t, im)}

	checkDirectory(t, b, ".", []string{"a"})
	fsckUnlessPlatformLimit(t, im)

	var got []string

	err := fs.WalkDir(b.fsys, ".", func(p string, _ fs.DirEntry, err error) error {
		got = append(got, p)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	if want := []string{".", "a"}; !slices.Equal(got, want) {
		t.Errorf("the image holds %q; want %q", got, want)
	}
}

// TestWriterCopyFromRefusesATargetSymlinkRefuses: a source whose symlink
// has a target Symlink would refuse, one with a NUL, is refused by
// CopyFrom too: the target is no other whoever wrote it.
func TestWriterCopyFromRefusesATargetSymlinkRefuses(t *testing.T) {
	t.Parallel()

	src := fstest.MapFS{"l": &fstest.MapFile{Data: []byte("a\x00b"), Mode: fs.ModeSymlink | 0o777}}

	if err := erofs.Create(&testBuffer{}).CopyFrom(src); !errors.Is(err, erofs.ErrInvalid) {
		t.Fatalf("CopyFrom of a symlink whose target holds a NUL: %v; want ErrInvalid", err)
	}
}

// TestWriterDirectoryLinkCount: a directory's link count is two plus its
// subdirectories in the image at Close, whatever a source claimed: an
// overlay that replaces a file with a directory raises its parent's count,
// and a source whose directory count follows another convention (APFS
// counts every entry) does not reach the image. SetNlink stays the one
// override.
func TestWriterDirectoryLinkCount(t *testing.T) {
	t.Parallel()

	var buf testBuffer

	w := erofs.Create(&buf)

	lower := fstest.MapFS{
		"d":   &fstest.MapFile{Mode: fs.ModeDir | 0o755},
		"d/b": &fstest.MapFile{Data: []byte("B"), Mode: 0o644},
	}
	if err := w.CopyFrom(lower); err != nil {
		t.Fatal(err)
	}

	upper := fstest.MapFS{
		"d":   &fstest.MapFile{Mode: fs.ModeDir | 0o755, Sys: &builder.Entry{Nlink: 12}},
		"d/b": &fstest.MapFile{Mode: fs.ModeDir | 0o755, Sys: &builder.Entry{Nlink: 7}},
		"e":   &fstest.MapFile{Mode: fs.ModeDir | 0o755, Sys: &builder.Entry{Nlink: 9}},
	}
	if err := w.CopyFrom(upper, erofs.Merge()); err != nil {
		t.Fatal(err)
	}

	if err := w.SetNlink("/e", 5); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	im := corpus.Image{Name: t.Name(), Img: buf.Bytes()}
	b := wBuilt{img: im.Img, ref: parseImage(t, im), fsys: openImage(t, im)}

	for name, want := range map[string]uint64{".": 4, "d": 3, "d/b": 2, "e": 5} {
		if _, st := b.statOf(t, name); st.Nlink != want {
			t.Errorf("%s: nlink %d, want %d", name, st.Nlink, want)
		}

		if got := uint64(b.inodeOf(t, name).Nlink); got != want {
			t.Errorf("%s: the reference reads nlink %d, want %d", name, got, want)
		}
	}

	fsckUnlessPlatformLimit(t, im)
}
