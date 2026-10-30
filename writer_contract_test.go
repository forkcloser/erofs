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
	"strings"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/builder"
)

// The writer refuses what its reader and the kernel refuse, and what it
// serves before Close is what Open serves after.

// TestWriterStatBeforeClose: Stat's FileInfo before Close carries the
// entry's Stat and the accessor interfaces, with the values Open reports
// after Close, inode number and directory size apart.
func TestWriterStatBeforeClose(t *testing.T) {
	t.Parallel()

	var buf testBuffer

	w := erofs.Create(&buf)
	writeFile(t, w, "/f", []byte("content"))

	for _, step := range []func() error{
		func() error { return w.Chown("/f", 1234, 5678) },
		func() error { return w.Setxattr("/f", "user.k", "v") },
		func() error { return w.Link("/f", "/g") },
		func() error { return w.Mkdir("/d", 0o755) },
		func() error { return w.Mkdir("/d/a", 0o755) },
		func() error { return w.Mkdir("/d/b", 0o755) },
		func() error { return w.Mknod("/c", fs.ModeCharDevice|fs.ModeDevice|0o600, 0x0105) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}

	before := map[string]erofs.Stat{}

	for _, name := range []string{"f", "g", "d", "c"} {
		st, err := w.Stat(name)
		if err != nil {
			t.Fatal(err)
		}

		sys, ok := st.Sys().(*erofs.Stat)
		if !ok {
			t.Fatalf("%s: Sys is %T before Close; want *erofs.Stat", name, st.Sys())
		}

		before[name] = *sys

		if _, ok := st.(interface {
			UID() uint32
			GID() uint32
			Ino() uint64
			Nlink() uint64
			Rdev() uint64
		}); !ok {
			t.Fatalf("%s: the FileInfo lacks the ownership, inode and device accessors before Close", name)
		}

		if _, ok := st.(interface {
			GetAllXattr() map[string]string
			GetXattr(name string) (string, bool)
		}); !ok {
			t.Fatalf("%s: the FileInfo lacks the xattr accessors before Close", name)
		}
	}

	if f := before["f"]; f.UID != 1234 || f.GID != 5678 || f.Nlink != 2 || f.Xattrs["user.k"] != "v" || f.Size != 7 {
		t.Fatalf("f before Close: %+v", f)
	}

	if d := before["d"]; d.Nlink != 4 {
		t.Fatalf("d before Close: nlink %d; want 2 plus two subdirectories", d.Nlink)
	}

	if c := before["c"]; c.Rdev != 0x0105 {
		t.Fatalf("c before Close: rdev %#x; want 0x0105", c.Rdev)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fsys, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range before {
		st, err := fs.Lstat(fsys, name)
		if err != nil {
			t.Fatal(err)
		}

		got := *st.Sys().(*erofs.Stat)
		got.Ino = 0

		if got.Mode.IsDir() {
			// Directory blocks exist only once the image is laid out.
			got.Size = 0
		}

		if got.Mode != want.Mode || got.Size != want.Size || got.Rdev != want.Rdev || got.UID != want.UID ||
			got.GID != want.GID || got.Mtime != want.Mtime || got.MtimeNs != want.MtimeNs || got.Nlink != want.Nlink ||
			len(got.Xattrs) != len(want.Xattrs) {
			t.Fatalf("%s: after Close %+v; before Close %+v", name, got, want)
		}

		for k, v := range want.Xattrs {
			if got.Xattrs[k] != v {
				t.Fatalf("%s: xattr %q is %q after Close, %q before", name, k, got.Xattrs[k], v)
			}
		}
	}
}

// TestWriterSymlinkTargets: a target the kernel's readlink cannot deliver
// whole, over 4095 bytes or holding a NUL, is refused; the longest valid one
// is written and read back.
func TestWriterSymlinkTargets(t *testing.T) {
	t.Parallel()

	var buf testBuffer

	w := erofs.Create(&buf)

	for name, target := range map[string]string{"/long": strings.Repeat("t", 4096), "/nul": "a\x00b"} {
		if err := w.Symlink(target, name); !errors.Is(err, erofs.ErrInvalid) {
			t.Fatalf("Symlink(%q): %v; want ErrInvalid", name, err)
		}
	}

	longest := strings.Repeat("t", 4095)
	if err := w.Symlink(longest, "/ok"); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fsys, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	if got, err := fs.ReadLink(fsys, "ok"); err != nil || got != longest {
		t.Fatalf("the longest target read back: %d bytes, %v", len(got), err)
	}
}

// TestWriterNameLength: a name over EROFS_NAME_LEN is refused by every
// method that creates one, since the kernel's lookup could never reach it;
// one of exactly that length is written and reachable.
func TestWriterNameLength(t *testing.T) {
	t.Parallel()

	var buf testBuffer

	w := erofs.Create(&buf)
	long := "/" + strings.Repeat("n", 256)

	for what, step := range map[string]func() error{
		"Create":  func() error { _, err := w.Create(long); return err },
		"Mkdir":   func() error { return w.Mkdir(long, 0o755) },
		"Symlink": func() error { return w.Symlink("t", long) },
		"Mknod":   func() error { return w.Mknod(long, fs.ModeNamedPipe|0o600, 0) },
		"parent":  func() error { return w.Mkdir(long+"/x", 0o755) },
	} {
		if err := step(); !errors.Is(err, erofs.ErrInvalid) {
			t.Fatalf("%s with a 256-byte name: %v; want ErrInvalid", what, err)
		}
	}

	exact := strings.Repeat("n", 255)
	writeFile(t, w, "/"+exact, []byte("x"))

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fsys, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	if got, err := fs.ReadFile(fsys, exact); err != nil || string(got) != "x" {
		t.Fatalf("a 255-byte name read back: %q, %v", got, err)
	}
}

// TestWriterXattrNames: an empty name and a bare prefix are refused, as
// Linux refuses them; a prefixed name with a suffix is stored.
func TestWriterXattrNames(t *testing.T) {
	t.Parallel()

	var buf testBuffer

	w := erofs.Create(&buf)
	writeFile(t, w, "/f", []byte("x"))

	for _, name := range []string{"", "user.", "trusted.", "security."} {
		if err := w.Setxattr("/f", name, "v"); !errors.Is(err, erofs.ErrInvalid) {
			t.Fatalf("Setxattr(%q): %v; want ErrInvalid", name, err)
		}
	}

	// The POSIX ACL names are whole names the format indexes, with no
	// suffix by design.
	for _, name := range []string{"user.k", "system.posix_acl_access", "system.posix_acl_default"} {
		if err := w.Setxattr("/f", name, "v"); err != nil {
			t.Fatalf("Setxattr(%q): %v", name, err)
		}
	}

	st, err := w.Stat("/f")
	if err != nil {
		t.Fatal(err)
	}

	if xs := st.(interface{ GetAllXattr() map[string]string }).GetAllXattr(); len(xs) != 3 || xs["user.k"] != "v" {
		t.Fatalf("xattrs: %v; want the three set", xs)
	}
}

// TestWriterCleansPaths: every spelling of the root and of an entry the
// Writer doc names resolves, "./" included.
func TestWriterCleansPaths(t *testing.T) {
	t.Parallel()

	var buf testBuffer

	w := erofs.Create(&buf)
	writeFile(t, w, "/x", []byte("x"))

	for _, name := range []string{"", ".", "/", "./", "/.", "x/..", "./x/.."} {
		st, err := w.Stat(name)
		if err != nil || !st.IsDir() {
			t.Fatalf("Stat(%q): %v, %v; want the root", name, st, err)
		}
	}

	for _, name := range []string{"x", "/x", "./x", "x/", "/x/", "./x/."} {
		if st, err := w.Stat(name); err != nil || st.Name() != "x" {
			t.Fatalf("Stat(%q): %v, %v; want x", name, st, err)
		}
	}
}

// TestWriterCopyFromRefusesWhatTheAPIRefuses: a source whose entry the API
// would refuse (a 256-byte name, a symlink target with a NUL) is refused
// by CopyFrom too, since the kernel refuses the image whoever wrote it.
func TestWriterCopyFromRefusesWhatTheAPIRefuses(t *testing.T) {
	t.Parallel()

	for what, src := range map[string]fstest.MapFS{
		"name": {strings.Repeat("n", 256): &fstest.MapFile{Data: []byte("x"), Mode: 0o644}},
		"target": {
			"l": &fstest.MapFile{Data: []byte("a\x00b"), Mode: fs.ModeSymlink | 0o777},
		},
	} {
		var buf testBuffer

		w := erofs.Create(&buf)
		if err := w.CopyFrom(src); !errors.Is(err, erofs.ErrInvalid) {
			t.Fatalf("CopyFrom with a bad %s: %v; want ErrInvalid", what, err)
		}
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

	fsys, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]uint64{".": 4, "d": 3, "d/b": 2, "e": 5} {
		st, err := fs.Stat(fsys, name)
		if err != nil {
			t.Fatal(err)
		}

		if got := st.Sys().(*erofs.Stat).Nlink; got != want {
			t.Errorf("%s: nlink %d, want %d", name, got, want)
		}
	}
}
