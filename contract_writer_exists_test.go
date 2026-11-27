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
	"strings"
	"testing"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/erofsspec"
)

// TestContractWriterAnExistingNameIsErrExist: "A method that creates a name
// refuses one that exists, whatever it names, as fs.ErrExist, the root
// included but for Mkdir, which sets the root's permissions", and "a
// creating method that refuses leaves the tree as it was, the directories
// on the way to the name unmade": after every creating method was given
// the root and the name of a file, of a directory and of a symlink, the
// image holds the three as they were made; and after each was refused a
// name of 256 bytes two new directories down, or a file to link that is
// not there, the image holds neither directory.
func TestContractWriterAnExistingNameIsErrExist(t *testing.T) {
	t.Parallel()

	calls := map[string]func(w *erofs.Writer, name string) error{
		"Create":  func(w *erofs.Writer, name string) error { _, err := w.Create(name); return err },
		"Mkdir":   func(w *erofs.Writer, name string) error { return w.Mkdir(name, 0o700) },
		"Symlink": func(w *erofs.Writer, name string) error { return w.Symlink("elsewhere", name) },
		"Mknod":   func(w *erofs.Writer, name string) error { return w.Mknod(name, fs.ModeNamedPipe|0o600, 0) },
		"Link":    func(w *erofs.Writer, name string) error { return w.Link("f", name) },
	}

	b := wBuild(t, func(w *erofs.Writer) {
		mustCreate(t, w, "f", []byte("f"))

		if err := errors.Join(w.Mkdir("d", 0o755), w.Symlink("t", "l")); err != nil {
			t.Fatal(err)
		}

		for _, existing := range []string{"/", "f", "d", "l"} {
			for call, do := range calls {
				if call == "Mkdir" && existing == "/" {
					continue
				}

				if err := do(w, existing); !errors.Is(err, fs.ErrExist) {
					t.Errorf("%s(%q): %v, want ErrExist", call, existing, err)
				}
			}
		}
	})

	for name, want := range map[string]uint8{
		"f": erofsspec.TypeRegular, "d": erofsspec.TypeDir, "l": erofsspec.TypeSymlink,
	} {
		de, err := b.ref.Lookup(b.inodeOf(t, "."), []byte(name))
		if err != nil || de.Type != want {
			t.Errorf("%s: the reference looks up %+v, %v; want type %d", name, de, err, want)
		}
	}

	if got, err := fs.ReadFile(b.fsys, "f"); err != nil || string(got) != "f" {
		t.Errorf("f reads %q, %v", got, err)
	}

	if got, err := fs.ReadLink(b.fsys, "l"); err != nil || got != "t" {
		t.Errorf("l reads %q, %v", got, err)
	}

	if _, st := b.statOf(t, "d"); st.Mode.Perm() != 0o755 {
		t.Errorf("d has mode %v, want the 0755 it was made with", st.Mode)
	}

	if entries, err := b.ref.Dirents(b.inodeOf(t, ".")); err != nil || len(entries) != 5 {
		t.Errorf("the root holds %d entries, %v; want . and .. and the three names", len(entries), err)
	}

	long := "p/q/" + strings.Repeat("n", 256)

	unmade := wBuild(t, func(w *erofs.Writer) {
		mustCreate(t, w, "f", []byte("f"))

		for call, do := range calls {
			if err := do(w, long); !errors.Is(err, fs.ErrInvalid) {
				t.Errorf("%s of a 256-byte name: %v, want ErrInvalid", call, err)
			}
		}

		if err := w.Link("missing", "p/q/h"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Link of a missing file: %v, want ErrNotExist", err)
		}
	})

	if entries, err := unmade.ref.Dirents(unmade.inodeOf(t, ".")); err != nil || len(entries) != 3 {
		t.Errorf("the root holds %d entries, %v, after the refusals; want . and .. and f", len(entries), err)
	}

	// The one existing name a creating method takes: the root, by Mkdir.
	root := wBuild(t, func(w *erofs.Writer) {
		if err := w.Mkdir("/", 0o700); err != nil {
			t.Errorf("Mkdir of the root: %v", err)
		}
	})

	if _, st := root.statOf(t, "."); st.Mode.Perm() != 0o700 {
		t.Errorf("the root has mode %v after Mkdir(\"/\", 0700)", st.Mode)
	}
}
