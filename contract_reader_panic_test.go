package erofs_test

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"testing"

	"github.com/forkcloser/go-erofs"
)

// errReaderBug is what buggyReader panics with.
var errReaderBug = errors.New("buggyReader: a bug of the reader's own")

// buggyReader is a source with a bug of its own: once armed, its read of an
// inode, the 64 bytes the package reads for one, panics.
type buggyReader struct {
	io.ReaderAt

	armed bool
}

func (r *buggyReader) ReadAt(p []byte, off int64) (int, error) {
	if r.armed && len(p) == 64 {
		panic(errReaderBug)
	}

	return r.ReaderAt.ReadAt(p, off)
}

// TestContractReaderDoesNotCallAPanicACorruptImage: a panic under the read
// of an inode is the panic it is, its value intact, for the caller and for
// a fuzzer to see. It is not answered as an error that says the image is
// corrupt: the image is not what failed.
func TestContractReaderDoesNotCallAPanicACorruptImage(t *testing.T) {
	t.Parallel()

	out := &testBuffer{}
	w := erofs.Create(out)
	writeFile(t, w, "f", []byte("data"))

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	src := &buggyReader{ReaderAt: bytes.NewReader(out.Bytes())}

	fsys, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	src.armed = true

	defer func() {
		v := recover()

		if got, ok := v.(error); !ok || !errors.Is(got, errReaderBug) {
			t.Errorf("Stat recovered %v; want the reader's own panic", v)
		}
	}()

	info, err := fs.Stat(fsys, "f")

	t.Errorf("Stat returned %v, %v; want the reader's panic", info, err)
}
