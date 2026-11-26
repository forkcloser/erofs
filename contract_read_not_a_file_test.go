package erofs_test

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"testing"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
)

// TestContractReadOfADirectoryIsAnError: a directory opened and read is
// ErrIsDirectory, by Read and by WriteTo, as fs.ReadFile already said. Its
// blocks, the raw entries, used to come back as content with a nil error.
func TestContractReadOfADirectoryIsAnError(t *testing.T) {
	t.Parallel()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
	writeFile(t, w, "/d/file", []byte("content"))

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fsys := openWithDevices(t, out.Bytes())

	for _, name := range []string{"d", "."} {
		f, err := fsys.Open(name)
		if err != nil {
			t.Fatal(err)
		}

		if got, err := io.ReadAll(f); !errors.Is(err, erofs.ErrIsDirectory) || len(got) != 0 {
			t.Errorf("io.ReadAll of %s: %d bytes, %v; want none and ErrIsDirectory", name, len(got), err)
		}

		var copied bytes.Buffer
		if n, err := io.Copy(&copied, f); !errors.Is(err, erofs.ErrIsDirectory) || n != 0 {
			t.Errorf("io.Copy of %s: %d bytes, %v; want none and ErrIsDirectory", name, n, err)
		}

		// It is still a directory to list.
		if entries, err := f.(fs.ReadDirFile).ReadDir(-1); err != nil || len(entries) != 1 {
			t.Errorf("ReadDir of %s after the reads: %d entries, %v; want 1", name, len(entries), err)
		}

		_ = f.Close()
	}
}

// TestContractAFifoHasNoContent: a FIFO, a socket or a device has no content
// in the image, whatever size and start block its inode carries: it reads
// as empty. A FIFO with a size used to read as that many bytes of whatever
// block of the image its inode named.
func TestContractAFifoHasNoContent(t *testing.T) {
	t.Parallel()

	img := corpus.Assemble(corpus.Layout{
		BlockBits:  12,
		NoChecksum: true,
		Entries: []corpus.AEntry{
			{
				Name: "pipe", Mode: 0o010644, Nlink: 1, Mtime: corpus.AssembledBuildTime,
				Data: []byte("sixteen bytes..."), Plain: true,
			},
		},
	})

	fsys, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	fi, err := fs.Stat(fsys, "pipe")
	if err != nil {
		t.Fatal(err)
	}

	if fi.Mode().Type() != fs.ModeNamedPipe || fi.Size() != 16 {
		t.Fatalf("the image holds %v of %d bytes; want a FIFO whose inode says 16: the test tests nothing",
			fi.Mode(), fi.Size())
	}

	if got := readBoth(t, fsys, "pipe"); got != [2]int{} {
		t.Errorf("a FIFO whose inode says 16 bytes reads %d by io.ReadAll and %d by io.Copy; want none", got[0], got[1])
	}

	if got, err := fs.ReadFile(fsys, "pipe"); err != nil || len(got) != 0 {
		t.Errorf("fs.ReadFile: %d bytes, %v; want none and no error", len(got), err)
	}
}

// readBoth is how many bytes io.ReadAll then io.Copy get from the file.
func readBoth(t *testing.T, fsys fs.FS, name string) [2]int {
	t.Helper()

	var got [2]int

	f, err := fsys.Open(name)
	if err != nil {
		t.Fatal(err)
	}

	read, err := io.ReadAll(f)
	if err != nil {
		t.Errorf("io.ReadAll of %s: %v", name, err)
	}

	got[0] = len(read)
	_ = f.Close()

	if f, err = fsys.Open(name); err != nil {
		t.Fatal(err)
	}

	n, err := io.Copy(io.Discard, f)
	if err != nil {
		t.Errorf("io.Copy of %s: %v", name, err)
	}

	got[1] = int(n)
	_ = f.Close()

	return got
}
