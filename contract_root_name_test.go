package erofs_test

import (
	"io/fs"
	"testing"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
)

// TestContractTheRootIsNamedDotThroughALink: a FileInfo's Name is a base
// name. The root is ".", asked for directly or reached through a symbolic
// link, as a file reached through a link bears its own name. The root
// through a link used to bear the whole path that was asked, slashes
// included.
func TestContractTheRootIsNamedDotThroughALink(t *testing.T) {
	t.Parallel()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
	writeFile(t, w, "/d/file", []byte("x"))

	for target, link := range map[string]string{"..": "/d/up", "/": "/d/abs", "file": "/d/tofile", ".": "/d/self"} {
		if err := w.Symlink(target, link); err != nil {
			t.Fatalf("Symlink(%q, %q): %v", target, link, err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fsys := openWithDevices(t, out.Bytes())

	for name, want := range map[string]string{
		".":        ".",
		"d/up":     ".",
		"d/abs":    ".",
		"d/up/d":   "d",
		"d/self":   "d",
		"d/tofile": "file",
	} {
		info, err := fs.Stat(fsys, name)
		if err != nil {
			t.Errorf("Stat(%s): %v", name, err)

			continue
		}

		if info.Name() != want {
			t.Errorf("Stat(%s).Name() = %q; want %q", name, info.Name(), want)
		}

		f, err := fsys.Open(name)
		if err != nil {
			t.Errorf("Open(%s): %v", name, err)

			continue
		}

		if opened, err := f.Stat(); err != nil || opened.Name() != want {
			t.Errorf("Open(%s).Stat().Name() = %v, %v; want %q", name, opened, err, want)
		}

		_ = f.Close()
	}

	// The root through a link is still the root to list.
	entries, err := fs.ReadDir(fsys, "d/up")
	if err != nil || len(entries) != 1 || entries[0].Name() != "d" {
		t.Errorf("ReadDir through the link: %v, %v; want the root's one entry, d", entries, err)
	}
}
