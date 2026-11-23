package erofs_test

import (
	"bytes"
	"io/fs"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
)

// linkedThenRemoved is an image of one file, /d/f, linked under the names
// given in that order, with the names in removed then removed in turn.
func linkedThenRemoved(t *testing.T, links, removed []string) []byte {
	t.Helper()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
	writeFile(t, w, "/d/f", []byte("content"))

	for _, name := range links {
		if err := w.Link("/d/f", name); err != nil {
			t.Fatalf("Link(%s): %v", name, err)
		}
	}

	for _, name := range removed {
		if err := w.Remove(name); err != nil {
			t.Fatalf("Remove(%s): %v", name, err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	return out.Bytes()
}

// TestContractRemovingALinkedNameLeavesTheOthers: removing one name of a
// file with several, the first it had included, leaves the file under the
// others, as unlink(2) does: the same inode, its content, a link count of
// the names left, and no name that was removed. Whatever the order the
// names were made and removed in, the image is the same bytes.
func TestContractRemovingALinkedNameLeavesTheOthers(t *testing.T) {
	t.Parallel()

	all := []string{"/d/f", "/a/l1", "/e/l2", "/b/l3", "/z"}

	for name, tc := range map[string]struct {
		removed []string
		left    []string
	}{
		"the first name": {[]string{"/d/f"}, []string{"a/l1", "b/l3", "e/l2", "z"}},
		"the first name, then the one that took over": {[]string{"/d/f", "/a/l1"}, []string{"b/l3", "e/l2", "z"}},
		"a later name, then the first":                {[]string{"/b/l3", "/d/f"}, []string{"a/l1", "e/l2", "z"}},
		"every later name, then the first":            {[]string{"/a/l1", "/e/l2", "/b/l3", "/z", "/d/f"}, nil},
		"the first, then each that takes over":        {[]string{"/d/f", "/a/l1", "/b/l3", "/e/l2"}, []string{"z"}},
		"a later name alone":                          {[]string{"/e/l2"}, []string{"a/l1", "b/l3", "d/f", "z"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// The links made in two orders: the image must not depend on it.
			forward := linkedThenRemoved(t, all[1:], tc.removed)
			backward := linkedThenRemoved(t, []string{"/z", "/b/l3", "/e/l2", "/a/l1"}, tc.removed)

			if !bytes.Equal(forward, backward) {
				t.Error("the image depends on the order the links were made in")
			}

			fsys := openWithDevices(t, forward)

			var (
				found []string
				inode uint64
			)

			err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
				if err != nil || !d.Type().IsRegular() {
					return err
				}

				info, err := d.Info()
				if err != nil {
					return err
				}

				st, ok := info.Sys().(*erofs.Stat)
				if !ok {
					t.Fatalf("%s: Sys is %T, want *erofs.Stat", p, info.Sys())
				}

				if len(found) > 0 && st.Ino != inode {
					t.Errorf("%s is inode %d, %s is %d: not one file", p, st.Ino, found[0], inode)
				}

				if int(st.Nlink) != len(tc.left) {
					t.Errorf("%s has a link count of %d; want %d", p, st.Nlink, len(tc.left))
				}

				if got, rerr := fs.ReadFile(fsys, p); rerr != nil || string(got) != "content" {
					t.Errorf("%s reads %q, %v", p, got, rerr)
				}

				found, inode = append(found, p), st.Ino

				return nil
			})
			if err != nil {
				t.Fatal(err)
			}

			if !slices.Equal(found, tc.left) {
				t.Errorf("the file is under %v; want %v", found, tc.left)
			}
		})
	}
}

// TestContractRemovingAFilesFirstNameLeavesItUnderItsLowestName: the name
// that takes the file over is the lowest path among those left, whatever
// order they were made in: the image is the one of the same file made under
// that name and linked to the others, and of no other name.
func TestContractRemovingAFilesFirstNameLeavesItUnderItsLowestName(t *testing.T) {
	t.Parallel()

	names := []string{"/e/l2", "/z", "/a/l1", "/b/l3"}
	removed := linkedThenRemoved(t, names, []string{"/d/f"})

	for _, first := range names {
		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

		// The directory the first name was in is left, empty.
		if err := w.Mkdir("/d", 0o755); err != nil {
			t.Fatal(err)
		}

		writeFile(t, w, first, []byte("content"))

		for _, name := range names {
			if name == first {
				continue
			}

			if err := w.Link(first, name); err != nil {
				t.Fatalf("Link(%s, %s): %v", first, name, err)
			}
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		if same := bytes.Equal(removed, out.Bytes()); same != (first == "/a/l1") {
			t.Errorf("the image is that of the file made under %s: %v; want it to be /a/l1's alone", first, same)
		}
	}
}

// TestContractRemovingManyNamesOfOneFileGrowsWithTheirNumber: the names of
// one file removed with the directory that holds them, one at a time, or
// together with the file's first name, in a time that grows with their
// number and not with its square. With the square, the quickest of the
// three shapes took 28 seconds for this many names on the machine that
// measured it, and each takes some tens of milliseconds without: the
// deadline is far from both.
func TestContractRemovingManyNamesOfOneFileGrowsWithTheirNumber(t *testing.T) {
	t.Parallel()

	const (
		names    = 200_000
		deadline = 5 * time.Second
	)

	for _, shape := range []struct {
		name   string
		first  string
		remove func(w *erofs.Writer) error
		left   []string
	}{
		{
			"the later names with their directory", "/first",
			func(w *erofs.Writer) error { return w.RemoveAll("/d") },
			[]string{"first"},
		},
		{
			"the later names in turn", "/first",
			func(w *erofs.Writer) error {
				for i := range names {
					if err := w.Remove("/d/l" + strconv.Itoa(i)); err != nil {
						return err
					}
				}

				return nil
			},
			[]string{"d", "first"},
		},
		{
			"every name with its directory", "/d/first",
			func(w *erofs.Writer) error { return w.RemoveAll("/d") },
			nil,
		},
	} {
		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
		writeFile(t, w, shape.first, []byte("content"))

		for i := range names {
			if err := w.Link(shape.first, "/d/l"+strconv.Itoa(i)); err != nil {
				t.Fatalf("%s: Link: %v", shape.name, err)
			}
		}

		done := make(chan error, 1)

		go func() { done <- shape.remove(w) }()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", shape.name, err)
			}
		case <-time.After(deadline):
			t.Fatalf("%s: %d names not removed in %v", shape.name, names, deadline)
		}

		if err := w.Close(); err != nil {
			t.Fatalf("%s: Close: %v", shape.name, err)
		}

		fsys := openWithDevices(t, out.Bytes())

		var left []string

		err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == "." {
				return err
			}

			left = append(left, p)

			if !d.IsDir() {
				info, ierr := d.Info()
				if ierr != nil {
					return ierr
				}

				if st, ok := info.Sys().(*erofs.Stat); !ok || st.Nlink != 1 {
					t.Errorf("%s: %s has %+v; want a link count of 1", shape.name, p, info.Sys())
				}
			}

			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v", shape.name, err)
		}

		if !slices.Equal(left, shape.left) {
			t.Errorf("%s: the image holds %v; want %v", shape.name, left, shape.left)
		}
	}
}
