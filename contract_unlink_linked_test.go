package erofs_test

import (
	"bytes"
	"io/fs"
	"math/rand/v2"
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

// TestContractAFileHandedOnAgainIsUnderItsLowestName: the lowest path left
// takes the file each time its first name goes, through names linked
// between two such removals, linked lower than those already there, and
// linked and removed by the dozen: the image is the one of the file made
// under that name and linked to the others, and of no other name. Each
// name is in a directory of its own: among the names of one directory the
// image does not tell which one the file is under.
func TestContractAFileHandedOnAgainIsUnderItsLowestName(t *testing.T) {
	t.Parallel()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
	writeFile(t, w, "/m/first", []byte("content"))

	call := func(err error) {
		t.Helper()

		if err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range []string{"/q/n", "/c/n", "/x/n", "/k/n"} {
		call(w.Link("/m/first", name))
	}

	// The file goes on under /c/n, then, with lower names linked since,
	// under /a/n.
	call(w.Remove("/m/first"))

	for _, name := range []string{"/b/n", "/a/n", "/y/n"} {
		call(w.Link("/q/n", name))
	}

	call(w.Remove("/c/n"))
	call(w.Remove("/a/n"))

	// Names that come and go, more of them than stay, lower than any.
	for i := range 40 {
		name := "/0/n" + strconv.Itoa(i)
		call(w.Link("/x/n", name))
		call(w.Remove(name))
	}

	// Under /b/n, and then under /j/n, and then under /k/n.
	call(w.Link("/y/n", "/j/n"))
	call(w.Remove("/b/n"))
	call(w.Remove("/j/n"))
	call(w.Close())

	left := []string{"/k/n", "/q/n", "/x/n", "/y/n"}

	for _, first := range left {
		direct := &testBuffer{}
		d := erofs.Create(direct, erofs.WithBuildTime(corpus.BuildTime, 0))

		// The directories the names that went were in are left, empty.
		for _, dir := range []string{"/0", "/a", "/b", "/c", "/j", "/m"} {
			call(d.Mkdir(dir, 0o755))
		}

		writeFile(t, d, first, []byte("content"))

		for _, name := range left {
			if name != first {
				call(d.Link(first, name))
			}
		}

		call(d.Close())

		if same := bytes.Equal(out.Bytes(), direct.Bytes()); same != (first == "/k/n") {
			t.Errorf("the image is that of the file made under %s: %v; want it to be /k/n's alone", first, same)
		}
	}
}

// TestContractTheLowestNameTakesAFileAfterOthersWent: the names of a file
// are put in order when its first name goes, and stay so through the names
// removed since, the lowest among them, and through the removed being
// dropped once they outnumber the live: the next first name to go leaves
// the file under the lowest still there, /c/n, and not under /e/n, which a
// list out of order after the drop would give.
func TestContractTheLowestNameTakesAFileAfterOthersWent(t *testing.T) {
	t.Parallel()

	call := func(err error) {
		t.Helper()

		if err != nil {
			t.Fatal(err)
		}
	}

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
	writeFile(t, w, "/0/n", []byte("content"))

	for _, name := range []string{"/a/n", "/b/n", "/c/n", "/e/n"} {
		call(w.Link("/0/n", name))
	}

	// Under /a/n; the lowest of the others goes; then two names come and
	// go, and a third comes, at which the removed are dropped.
	call(w.Remove("/0/n"))
	call(w.Remove("/b/n"))

	for _, name := range []string{"/f/n", "/g/n"} {
		call(w.Link("/a/n", name))
		call(w.Remove(name))
	}

	call(w.Link("/a/n", "/h/n"))
	call(w.Remove("/a/n"))
	call(w.Close())

	left := []string{"/c/n", "/e/n", "/h/n"}

	for _, first := range left {
		direct := &testBuffer{}
		d := erofs.Create(direct, erofs.WithBuildTime(corpus.BuildTime, 0))

		for _, dir := range []string{"/0", "/a", "/b", "/f", "/g"} {
			call(d.Mkdir(dir, 0o755))
		}

		writeFile(t, d, first, []byte("content"))

		for _, name := range left {
			if name != first {
				call(d.Link(first, name))
			}
		}

		call(d.Close())

		if same := bytes.Equal(out.Bytes(), direct.Bytes()); same != (first == "/c/n") {
			t.Errorf("the image is that of the file made under %s: %v; want it to be /c/n's alone", first, same)
		}
	}
}

// TestContractWhatACallerHoldsKeepsItsName: an entry of a listing, an info
// and an open file answer the name they were listed, asked or opened under,
// whatever becomes of the file since. A file whose first name goes is found
// under its next name; what the caller already holds is not renamed for
// it: a listing walked to remove each entry by its name removes each, and
// the standard library's walk meets every name once.
func TestContractWhatACallerHoldsKeepsItsName(t *testing.T) {
	t.Parallel()

	// One file under a, b and c of /d, a its first name, and under /e/o;
	// another file, /d/z.
	build := func(t *testing.T) (*erofs.Writer, *testBuffer) {
		t.Helper()

		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
		writeFile(t, w, "/d/a", []byte("content"))
		writeFile(t, w, "/d/z", []byte("another"))

		for _, name := range []string{"/d/b", "/d/c", "/e/o"} {
			if err := w.Link("/d/a", name); err != nil {
				t.Fatal(err)
			}
		}

		return w, out
	}

	list := func(t *testing.T, w *erofs.Writer, dir string) []fs.DirEntry {
		t.Helper()

		f, err := w.Open(dir)
		if err != nil {
			t.Fatal(err)
		}

		entries, err := f.(fs.ReadDirFile).ReadDir(-1)
		if err != nil {
			t.Fatal(err)
		}

		return entries
	}

	// names is what each entry says: its name, its info's, the file's size.
	names := func(entries []fs.DirEntry) []string {
		var out []string

		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				return append(out, err.Error())
			}

			out = append(out, e.Name()+"/"+info.Name()+":"+strconv.FormatInt(info.Size(), 10))
		}

		return out
	}

	t.Run("each entry of a listing removed by its name", func(t *testing.T) {
		t.Parallel()

		w, out := build(t)
		entries := list(t, w, "/d")
		want := []string{"a/a:7", "b/b:7", "c/c:7", "z/z:7"}

		for _, e := range entries {
			if err := w.Remove("/d/" + e.Name()); err != nil {
				t.Errorf("Remove(/d/%s): %v", e.Name(), err)
			}
		}

		if got := names(entries); !slices.Equal(got, want) {
			t.Errorf("the entries held, each with its info's name, say %v; want %v", got, want)
		}

		if got := namesOf(t, w, out); !slices.Equal(got, []string{"d/", "e/", "e/o"}) {
			t.Errorf("the image holds %v; want d/, e/ and e/o", got)
		}
	})

	t.Run("each file a walk meets removed", func(t *testing.T) {
		t.Parallel()

		w, out := build(t)

		var met []string

		err := fs.WalkDir(w, "/d", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}

			met = append(met, p)

			return w.Remove(p)
		})
		if err != nil {
			t.Errorf("the walk: %v", err)
		}

		if want := []string{"/d/a", "/d/b", "/d/c", "/d/z"}; !slices.Equal(met, want) {
			t.Errorf("the walk met %v; want %v", met, want)
		}

		if got := namesOf(t, w, out); !slices.Equal(got, []string{"d/", "e/", "e/o"}) {
			t.Errorf("the image holds %v; want d/, e/ and e/o", got)
		}
	})

	t.Run("a listing, an info and an open file held while the first name goes", func(t *testing.T) {
		t.Parallel()

		w, _ := build(t)
		inD, inE := list(t, w, "/d"), list(t, w, "/e")

		info, err := w.Stat("/d/a")
		if err != nil {
			t.Fatal(err)
		}

		f, err := w.Open("/d/a")
		if err != nil {
			t.Fatal(err)
		}

		defer func() {
			if err := f.Close(); err != nil {
				t.Error(err)
			}
		}()

		said := func() []string {
			opened, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}

			return slices.Concat(names(inD), names(inE), []string{info.Name(), opened.Name()})
		}

		before := said()

		// The file goes on under /d/b, and then under /d/c.
		for _, name := range []string{"/d/a", "/d/b"} {
			if err := w.Remove(name); err != nil {
				t.Fatal(err)
			}
		}

		if after := said(); !slices.Equal(after, before) {
			t.Errorf("what was held said %v, and says %v once the file's first names are gone", before, after)
		}
	})
}

// TestContractAFilesNamesComeAndGo: random sequences of names of one file
// linked and removed, the name the file is under among them. Whatever came
// and went, the file ends under the name it was under until that one went,
// and then under the lowest left: the image is that of the file made under
// that name and linked to the others. Each name is in a directory of its
// own, where the image tells which one the file is under.
func TestContractAFilesNamesComeAndGo(t *testing.T) {
	t.Parallel()

	const dirs = 14

	name := func(i int) string { return "/" + string(rune('a'+i)) + "/n" }

	for seed := range uint64(400) {
		rnd := rand.New(rand.NewPCG(seed, 0x6865697273))

		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

		// Every directory is there from the start, and stays.
		for i := range dirs {
			if err := w.Mkdir("/"+string(rune('a'+i)), 0o755); err != nil {
				t.Fatal(err)
			}
		}

		under := name(rnd.IntN(dirs))
		writeFile(t, w, under, []byte("content"))

		names := []string{under}
		log := []string{"Create(" + under + ")"}

		for range 20 + rnd.IntN(100) {
			// Few names, and more are linked than removed; many, and more
			// are removed: the removed come to outnumber the live, over
			// and over.
			link := 2
			if len(names) > 5 {
				link = 1
			}

			if at := rnd.IntN(dirs); !slices.Contains(names, name(at)) && rnd.IntN(3) < link {
				from := names[rnd.IntN(len(names))]
				log = append(log, "Link("+from+", "+name(at)+")")

				if err := w.Link(from, name(at)); err != nil {
					t.Fatalf("seed %d: %v after %v", seed, err, log)
				}

				names = append(names, name(at))

				continue
			}

			if len(names) == 1 {
				continue
			}

			gone := names[rnd.IntN(len(names))]
			log = append(log, "Remove("+gone+")")

			if err := w.Remove(gone); err != nil {
				t.Fatalf("seed %d: %v after %v", seed, err, log)
			}

			names = slices.DeleteFunc(names, func(n string) bool { return n == gone })
			if gone == under {
				under = slices.Min(names)
			}
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		direct := &testBuffer{}
		d := erofs.Create(direct, erofs.WithBuildTime(corpus.BuildTime, 0))

		for i := range dirs {
			if err := d.Mkdir("/"+string(rune('a'+i)), 0o755); err != nil {
				t.Fatal(err)
			}
		}

		writeFile(t, d, under, []byte("content"))

		for _, n := range names {
			if n != under {
				if err := d.Link(under, n); err != nil {
					t.Fatal(err)
				}
			}
		}

		if err := d.Close(); err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(out.Bytes(), direct.Bytes()) {
			t.Fatalf(
				"seed %d: the image is not that of the file made under %s and linked to %v, after %v",
				seed,
				under,
				names,
				log,
			)
		}
	}
}

// TestContractRemovingManyNamesOfOneFileGrowsWithTheirNumber: the names of
// one file removed with the directory that holds them, one at a time, or
// together with the file's first name, or one at a time the name the file
// is under first, in a time that grows with their number and not with its
// square. Each shape has the number of names that puts the square it once
// cost, or would cost, tens of seconds away, and what it costs now tens of
// milliseconds away, on the machine that measured them: the deadline is far
// from both, the race detector's slowness counted.
func TestContractRemovingManyNamesOfOneFileGrowsWithTheirNumber(t *testing.T) {
	t.Parallel()

	const deadline = 5 * time.Second

	inPathOrder := func(w *erofs.Writer, names []string) error {
		slices.Sort(names)

		for _, name := range names {
			if err := w.Remove(name); err != nil {
				return err
			}
		}

		return nil
	}

	for _, shape := range []struct {
		name   string
		names  int
		first  string
		remove func(w *erofs.Writer, names int) error
		left   []string
	}{
		{
			"the later names with their directory", 200_000, "/first",
			func(w *erofs.Writer, _ int) error { return w.RemoveAll("/d") },
			[]string{"first"},
		},
		{
			"the later names in turn", 200_000, "/first",
			func(w *erofs.Writer, names int) error {
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
			"every name with its directory", 200_000, "/d/first",
			func(w *erofs.Writer, _ int) error { return w.RemoveAll("/d") },
			nil,
		},
		{
			// Each name removed is the lowest path left, the one the file
			// is under: every removal but the last hands the file on.
			"every name in path order, the one the file is under first", 50_000, "/a",
			func(w *erofs.Writer, names int) error {
				order := make([]string, 0, names+1)
				order = append(order, "/a")

				for i := range names {
					order = append(order, "/d/l"+strconv.Itoa(i))
				}

				return inPathOrder(w, order)
			},
			[]string{"d"},
		},
		{
			// As many names again linked once the first are in order, as
			// they are when a first name has gone: the names that waited
			// join the others once, and not at every name that goes.
			"names linked onto names in order, then every name in path order", 20_000, "/a",
			func(w *erofs.Writer, names int) error {
				if err := w.Remove("/a"); err != nil {
					return err
				}

				order := make([]string, 0, 2*names)

				for i := range names {
					order = append(order, "/d/l"+strconv.Itoa(i), "/d/m"+strconv.Itoa(i))

					if err := w.Link("/d/l0", "/d/m"+strconv.Itoa(i)); err != nil {
						return err
					}
				}

				return inPathOrder(w, order)
			},
			[]string{"d"},
		},
	} {
		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
		writeFile(t, w, shape.first, []byte("content"))

		for i := range shape.names {
			if err := w.Link(shape.first, "/d/l"+strconv.Itoa(i)); err != nil {
				t.Fatalf("%s: Link: %v", shape.name, err)
			}
		}

		done := make(chan error, 1)
		start := time.Now()

		go func() { done <- shape.remove(w, shape.names) }()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", shape.name, err)
			}
		case <-time.After(deadline):
			t.Fatalf("%s: %d names not removed in %v", shape.name, shape.names, deadline)
		}

		t.Logf("%s, %d names: %v", shape.name, shape.names, time.Since(start))

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
