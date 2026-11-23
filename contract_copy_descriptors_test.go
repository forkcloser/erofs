package erofs_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
)

// openCounter is a source that counts the regular files it has open: how
// many now, and the most at one time.
type openCounter struct {
	fs.FS

	mu        sync.Mutex
	open, max int
}

func (c *openCounter) Open(name string) (fs.File, error) {
	f, err := c.FS.Open(name)
	if err != nil {
		return nil, err
	}

	if info, serr := f.Stat(); serr != nil || !info.Mode().IsRegular() {
		return f, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.open++
	c.max = max(c.max, c.open)

	return &countedFile{File: f, counter: c}, nil
}

type countedFile struct {
	fs.File

	counter *openCounter
	closed  bool
}

func (f *countedFile) Close() error {
	f.counter.mu.Lock()
	if !f.closed {
		f.closed = true
		f.counter.open--
	}
	f.counter.mu.Unlock()

	return f.File.Close()
}

// TestContractCopyHoldsNoFilePerFile: a copy opens a source's file to read
// it and closes it when it has, one at a time. It used to hold every file
// open from CopyFrom to Close, a descriptor each (300 files under a limit
// of 128 open files failed with "too many open files"), and with a data
// file it never closed them at all.
func TestContractCopyHoldsNoFilePerFile(t *testing.T) {
	t.Parallel()

	const files = 300

	tree := fstest.MapFS{}
	for i := range files {
		// Sizes on both sides of a block: inline and in blocks.
		tree[fmt.Sprintf("d%d/f%03d", i%7, i)] = &fstest.MapFile{Data: make([]byte, 1+i*37%6000)}
	}

	for _, tc := range []struct {
		name string
		opts func(*testing.T) []erofs.CreateOpt
	}{
		{"into the spool", func(*testing.T) []erofs.CreateOpt { return nil }},
		{"into a data file", func(t *testing.T) []erofs.CreateOpt {
			t.Helper()

			df, err := os.Create(filepath.Join(t.TempDir(), "data.bin"))
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() { _ = df.Close() })

			return []erofs.CreateOpt{erofs.WithDataFile(df)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			src := &openCounter{FS: tree}
			out := &testBuffer{}
			w := erofs.Create(out, append(tc.opts(t), erofs.WithBuildTime(corpus.BuildTime, 0))...)

			if err := w.CopyFrom(src); err != nil {
				t.Fatal(err)
			}

			if src.open != 0 {
				t.Errorf("%d of the source's files are open between CopyFrom and Close; want none", src.open)
			}

			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			if src.open != 0 || src.max > 1 {
				t.Errorf(
					"%d of the source's files are still open after Close, and up to %d were at once; want none, and one",
					src.open,
					src.max,
				)
			}
		})
	}
}

// TestContractCopiedFileGoneBeforeCloseIsAnError: a source's files are read
// when the image is written. One that can no longer be opened then fails
// Close, by name: no image with a file cut short or left empty.
func TestContractCopiedFileGoneBeforeCloseIsAnError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kept"), make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "gone"), make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}

	w := erofs.Create(&testBuffer{}, erofs.WithBuildTime(corpus.BuildTime, 0))
	if err := w.CopyFrom(os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(dir, "gone")); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Close = %v; want the source's ErrNotExist for the file removed", err)
	}
}

// copiedThenChanged copies a host directory holding f, 5000 bytes of 'a',
// lets change act on the directory, and closes the Writer.
func copiedThenChanged(t *testing.T, change func(t *testing.T, dir, f string)) ([]byte, error) {
	t.Helper()

	dir := t.TempDir()
	f := filepath.Join(dir, "f")

	if err := os.WriteFile(f, bytes.Repeat([]byte("a"), 5000), 0o600); err != nil {
		t.Fatal(err)
	}

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	if err := w.CopyFrom(os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}

	change(t, dir, f)

	err := w.Close()

	return out.Bytes(), err
}

// TestContractCopiedFileThatGrewIsCopiedAsFound: a file appended to between
// CopyFrom and Close, a log being written, is copied as the copy found it,
// its first bytes: what mkfs.erofs does with a file its scan measured.
func TestContractCopiedFileThatGrewIsCopiedAsFound(t *testing.T) {
	t.Parallel()

	img, err := copiedThenChanged(t, func(t *testing.T, _, f string) {
		t.Helper()

		a, err := os.OpenFile(f, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}

		if _, err = a.Write(bytes.Repeat([]byte("b"), 4096)); err != nil {
			t.Fatal(err)
		}

		if err = a.Close(); err != nil {
			t.Fatal(err)
		}
	})
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := fs.ReadFile(openWithDevices(t, img), "f")
	if err != nil || !bytes.Equal(got, bytes.Repeat([]byte("a"), 5000)) {
		t.Errorf("f reads %d bytes, %v; want the 5000 the copy found", len(got), err)
	}
}

// TestContractCopiedFileThatShrankFailsClose: a file cut shorter between
// CopyFrom and Close cannot fill the inode made for it: Close fails, and no
// image is written with the file padded or cut.
func TestContractCopiedFileThatShrankFailsClose(t *testing.T) {
	t.Parallel()

	_, err := copiedThenChanged(t, func(t *testing.T, _, f string) {
		t.Helper()

		if err := os.Truncate(f, 100); err != nil {
			t.Fatal(err)
		}
	})
	if err == nil {
		t.Error("Close = nil; want an error")
	}
}

// readFromRecorder is an output that sees what it is asked to read from.
type readFromRecorder struct {
	*testBuffer

	files, others int
}

func (r *readFromRecorder) ReadFrom(src io.Reader) (int64, error) {
	if limited, ok := src.(*io.LimitedReader); ok {
		if _, ok := limited.R.(*os.File); ok {
			r.files++
		} else {
			r.others++
		}
	} else {
		r.others++
	}

	return io.Copy(struct{ io.Writer }{r.testBuffer}, src)
}

// TestContractHostFileReachesTheOutputAsAFile: an output that can read from
// a file is handed the host's file itself, behind its limit, for each file
// that goes into blocks: what lets an *os.File output copy in the kernel.
func TestContractHostFileReachesTheOutputAsAFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for i := range 3 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), make([]byte, 3*4096), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out := &readFromRecorder{testBuffer: &testBuffer{}}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	if err := errors.Join(w.CopyFrom(os.DirFS(dir)), w.Close()); err != nil {
		t.Fatal(err)
	}

	if out.files != 3 || out.others != 0 {
		t.Errorf("the output read from %d files and %d other readers; want 3 and 0", out.files, out.others)
	}
}
