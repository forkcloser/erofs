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
	"io"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
)

// errBrokenReader is what a source that fails mid-way fails with.
var errBrokenReader = errors.New("the reader broke")

// TestContractWriterFileReadFrom: "ReadFrom implements io.ReaderFrom": it
// writes what the reader yields until its end, however the reader cuts it,
// and returns the count; the file then holds those bytes, after what Write
// put there before. A reader that fails gives its error back with the
// count of what was written until then.
func TestContractWriterFileReadFrom(t *testing.T) {
	t.Parallel()

	const bs = 4096

	for _, n := range []int{0, 1, bs - 1, bs, bs + 1, 3*bs + 7, 100_000} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()

			data := corpus.Pattern(n)

			b := wBuild(t, func(w *erofs.Writer) {
				f, err := w.Create("f")
				if err != nil {
					t.Fatal(err)
				}

				if _, err = f.Write([]byte("head")); err != nil {
					t.Fatal(err)
				}

				// A byte at a time, then the rest at once: ReadFrom is
				// held to the bytes, not to the reads.
				half := n / 2

				if got, rerr := f.ReadFrom(iotest.OneByteReader(bytes.NewReader(data[:half]))); rerr != nil ||
					got != int64(half) {
					t.Fatalf("ReadFrom of %d bytes, one at a time: %d, %v", half, got, rerr)
				}

				if got, rerr := f.ReadFrom(bytes.NewReader(data[half:])); rerr != nil || got != int64(n-half) {
					t.Fatalf("ReadFrom of %d bytes: %d, %v", n-half, got, rerr)
				}

				if err = f.Close(); err != nil {
					t.Fatal(err)
				}
			})

			want := append([]byte("head"), data...)

			if got, err := fs.ReadFile(b.fsys, "f"); err != nil || !bytes.Equal(got, want) {
				t.Errorf("f reads %d bytes, %v; want the %d written", len(got), err, len(want))
			}

			if got, err := b.ref.Read(b.inodeOf(t, "f")); err != nil || !bytes.Equal(got, want) {
				t.Errorf("the reference reads %d bytes, %v; want the %d written", len(got), err, len(want))
			}
		})
	}

	t.Run("a reader that fails", func(t *testing.T) {
		t.Parallel()

		w := erofs.Create(&testBuffer{})

		f, err := w.Create("f")
		if err != nil {
			t.Fatal(err)
		}

		src := io.MultiReader(bytes.NewReader(corpus.Pattern(5000)), iotest.ErrReader(errBrokenReader))

		if got, rerr := f.ReadFrom(src); !errors.Is(rerr, errBrokenReader) || got != 5000 {
			t.Errorf("ReadFrom: %d, %v; want 5000 and the reader's error", got, rerr)
		}
	})
}

// TestContractWriterPathLength: a path is at most 4096 bytes with its
// leading slash, PATH_MAX, whichever method names it and however it is
// spelled; a longer one is ErrInvalid, a bad argument and not a bad image,
// and the path at the bound is in the image, where the reference finds it.
func TestContractWriterPathLength(t *testing.T) {
	t.Parallel()

	// Fifteen directories of 255 bytes are 3840 bytes with their slashes.
	deep := strings.Repeat("/"+strings.Repeat("d", 255), 15)
	atBound := deep + "/" + strings.Repeat("l", 255)
	over := deep + "/x/" + strings.Repeat("l", 254)

	if len(atBound) != 4096 || len(over) != 4097 {
		t.Fatalf("test premise broken: paths of %d and %d bytes", len(atBound), len(over))
	}

	calls := map[string]func(w *erofs.Writer, name string) error{
		"Create":  func(w *erofs.Writer, name string) error { return writeRegular(w, corpus.Node{Path: name, Mode: 0o644}) },
		"Mkdir":   func(w *erofs.Writer, name string) error { return w.Mkdir(name, 0o755) },
		"Symlink": func(w *erofs.Writer, name string) error { return w.Symlink("t", name) },
		"Mknod":   func(w *erofs.Writer, name string) error { return w.Mknod(name, fs.ModeNamedPipe|0o600, 0) },
	}

	for call, do := range calls {
		for _, spelled := range []string{over, over[1:], "." + over} {
			w := erofs.Create(&testBuffer{})

			if err := do(w, spelled); !errors.Is(err, fs.ErrInvalid) || errors.Is(err, erofs.ErrCorrupt) {
				t.Errorf("%s of a path of 4097 bytes: %v, want ErrInvalid and not ErrCorrupt", call, err)
			}
		}

		// Not through fsck.erofs: it walks a path in a buffer of its host's
		// PATH_MAX, 1024 bytes on macOS, and says of a longer one that it
		// cannot check it.
		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

		if err := errors.Join(do(w, atBound[1:]), w.Close()); err != nil {
			t.Errorf("%s of a path of 4096 bytes: %v", call, err)

			continue
		}

		im := corpus.Image{Name: t.Name() + "/" + call, Img: out.Bytes()}

		if in, err := lookupNoFollow(parseImage(t, im), atBound[1:]); err != nil || in == nil {
			t.Errorf("%s: the reference does not find the path at the bound: %v", call, err)
		}

		if _, err := fs.Lstat(openImage(t, im), atBound[1:]); err != nil {
			t.Errorf("%s: Lstat of the path at the bound: %v", call, err)
		}
	}
}
