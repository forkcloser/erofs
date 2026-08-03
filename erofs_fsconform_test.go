package erofs

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

// conformImage builds a small image with a nested directory, a symlink and a
// file whose name is not valid UTF-8.
func conformImage(t *testing.T) fs.FS {
	t.Helper()

	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0))
	if err := w.Mkdir("/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"/a.txt", "/dir/b.txt", "/dir/c.txt"} {
		writeFile(t, w, n, []byte("content of "+n))
	}
	if err := w.Symlink("a.txt", "/link"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	img, err := Open(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatal(err)
	}

	return img
}

// TestFSConformance runs the standard fs.FS conformance suite. Before paths
// were validated this reported 41 violations: absolute paths, "." elements,
// doubled slashes, trailing "/." and ".." traversal were all accepted.
func TestFSConformance(t *testing.T) {
	img := conformImage(t)
	if err := fstest.TestFS(img, "a.txt", "dir/b.txt", "dir/c.txt", "link"); err != nil {
		t.Errorf("fstest.TestFS: %v", err)
	}
}

// TestInvalidPathsRejected pins the specific shapes that used to resolve.
func TestInvalidPathsRejected(t *testing.T) {
	img := conformImage(t)

	for _, name := range []string{
		"/a.txt",             // rooted
		"/",                  // rooted root
		"./a.txt",            // "." element
		"a.txt/.",            // trailing "."
		"dir//b.txt",         // empty element
		"dir/./b.txt",        // interior "." element
		"dir/../dir/b.txt",   // ".." traversal
		"dir/b.txt/../b.txt", // ".." traversal through a file
		"../a.txt",           // escaping ".."
		"dir/",               // trailing slash
		"",                   // empty
	} {
		t.Run(name, func(t *testing.T) {
			if f, err := img.Open(name); err == nil {
				_ = f.Close()
				t.Errorf("Open(%q) succeeded; want a PathError wrapping fs.ErrInvalid", name)
			} else if !errors.Is(err, fs.ErrInvalid) {
				t.Errorf("Open(%q) err = %v; want it to wrap fs.ErrInvalid", name, err)
			}

			// Every entry point resolves through the same check.
			if _, err := fs.Stat(img, name); !errors.Is(err, fs.ErrInvalid) {
				t.Errorf("Stat(%q) err = %v; want fs.ErrInvalid", name, err)
			}
			if _, err := fs.ReadFile(img, name); !errors.Is(err, fs.ErrInvalid) {
				t.Errorf("ReadFile(%q) err = %v; want fs.ErrInvalid", name, err)
			}
			if _, err := fs.ReadDir(img, name); !errors.Is(err, fs.ErrInvalid) {
				t.Errorf("ReadDir(%q) err = %v; want fs.ErrInvalid", name, err)
			}
		})
	}
}

// TestValidPathsAccepted guards against over-rejecting.
func TestValidPathsAccepted(t *testing.T) {
	img := conformImage(t)

	for _, name := range []string{".", "a.txt", "dir", "dir/b.txt", "link"} {
		f, err := img.Open(name)
		if err != nil {
			t.Errorf("Open(%q): %v", name, err)

			continue
		}
		_ = f.Close()
	}
}

// TestNonUTF8NamesRemainReachable covers the one place this FS is deliberately
// more permissive than fs.ValidPath. EROFS names are byte strings, and the
// writer can create names that are not valid UTF-8; rejecting them on the read
// side would hide entries that are really present in the image.
func TestNonUTF8NamesRemainReachable(t *testing.T) {
	const raw = "\xff\xfe-name"

	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0))
	writeFile(t, w, "/"+raw, []byte("payload"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	img, err := Open(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatal(err)
	}

	if fs.ValidPath(raw) {
		t.Fatal("test premise broken: the name is supposed to be invalid UTF-8")
	}

	got, err := fs.ReadFile(img, raw)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", raw, err)
	}
	if string(got) != "payload" {
		t.Errorf("got %q, want %q", got, "payload")
	}

	// It must also be listed.
	ents, err := fs.ReadDir(img, ".")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range ents {
		if e.Name() == raw {
			found = true
		}
	}
	if !found {
		t.Errorf("%q missing from ReadDir(\".\")", raw)
	}
}

// TestWalkDirFromRoot covers the traversal the CLI performs.
func TestWalkDirFromRoot(t *testing.T) {
	img := conformImage(t)

	var seen []string
	err := fs.WalkDir(img, ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		seen = append(seen, p)

		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}

	want := []string{".", "a.txt", "dir", "dir/b.txt", "dir/c.txt", "link"}
	if len(seen) != len(want) {
		t.Fatalf("walked %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("walk[%d] = %q, want %q", i, seen[i], want[i])
		}
	}
}
