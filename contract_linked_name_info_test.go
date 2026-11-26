package erofs_test

import (
	"fmt"
	"io/fs"
	"testing"
	"time"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
)

// describe is what an info says: its name, and the file.
func describe(t *testing.T, info fs.FileInfo) string {
	t.Helper()

	st, ok := info.Sys().(*erofs.Stat)
	if !ok {
		t.Fatalf("Sys is %T, want *erofs.Stat", info.Sys())
	}

	return fmt.Sprintf(
		"%s: %d bytes, %v, %d, %d links, uid %d, xattrs %v",
		info.Name(), info.Size(), info.Mode(), info.ModTime().Unix(), st.Nlink, st.UID, st.Xattrs,
	)
}

// TestContractAFilesSecondNameReadsAsTheFile: before Close as after, every
// name of a file answers with the file, and with the name asked: the entry
// a listing gives, Stat, and the info of the file opened. A listing used to
// give a second name an entry of no size, with the mode the file had when
// it was linked and no time, and Stat of a second name answered the first.
func TestContractAFilesSecondNameReadsAsTheFile(t *testing.T) {
	t.Parallel()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
	writeFile(t, w, "/d/a", []byte("twelve bytes"))

	call := func(err error) {
		t.Helper()

		if err != nil {
			t.Fatal(err)
		}
	}

	// What the file is, set after it got its second name, through either.
	when := time.Unix(1_600_000_000, 0)

	call(w.Link("/d/a", "/d/b"))
	call(w.Chmod("/d/a", 0o600))
	call(w.Chown("/d/b", 7, 8))
	call(w.Chtimes("/d/a", when, when))
	call(w.Setxattr("/d/b", "user.k", "v"))

	want := map[string]string{
		"a": "a: 12 bytes, -rw-------, 1600000000, 2 links, uid 7, xattrs map[user.k:v]",
		"b": "b: 12 bytes, -rw-------, 1600000000, 2 links, uid 7, xattrs map[user.k:v]",
	}

	check := func(how, name string, info fs.FileInfo, err error) {
		t.Helper()

		if err != nil {
			t.Errorf("%s of %s: %v", how, name, err)

			return
		}

		if got := describe(t, info); got != want[name] {
			t.Errorf("%s of %s says\n %s; want\n %s", how, name, got, want[name])
		}
	}

	judge := func(when string, fsys fs.FS, dir string) {
		t.Helper()

		entries, err := fs.ReadDir(fsys, dir)
		if err != nil || len(entries) != 2 {
			t.Fatalf("%s, the listing: %d entries, %v; want two", when, len(entries), err)
		}

		for _, e := range entries {
			name := e.Name()
			if e.Type() != 0 {
				t.Errorf("%s, the listing gives %s the type %v; want a regular file's", when, name, e.Type())
			}

			info, err := e.Info()
			check(when+", the listing's entry", name, info, err)

			info, err = fs.Stat(fsys, dir+"/"+name)
			check(when+", Stat", name, info, err)

			f, err := fsys.Open(dir + "/" + name)
			if err != nil {
				t.Errorf("%s, Open(%s): %v", when, name, err)

				continue
			}

			info, err = f.Stat()
			check(when+", the open file", name, info, err)

			if err := f.Close(); err != nil {
				t.Errorf("%s, Close(%s): %v", when, name, err)
			}
		}
	}

	judge("before Close", w, "/d")
	call(w.Close())
	judge("after Close", openWithDevices(t, out.Bytes()), "d")
}
