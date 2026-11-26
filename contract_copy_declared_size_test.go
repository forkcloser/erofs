package erofs_test

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/go-erofs"
)

// misreadFile is a one-file source whose file "f" reads as reads bytes,
// whatever size it declares: a file that grows or shrinks while it is
// packed.
type misreadFile struct {
	fstest.MapFS

	reads int
}

type misreadingFile struct {
	fs.File

	left int
}

func (m misreadFile) Open(name string) (fs.File, error) {
	f, err := m.MapFS.Open(name)
	if err != nil || name != "f" {
		return f, err
	}

	return &misreadingFile{File: f, left: m.reads}, nil
}

func (f *misreadingFile) Read(p []byte) (int, error) {
	if f.left == 0 {
		return 0, io.EOF
	}

	n := min(len(p), f.left)
	clear(p[:n])
	f.left -= n

	return n, nil
}

// TestContractCopiedFileIsTheSizeItsSourceDeclared: a regular file copied
// from a source is as long as the source declared it, with a data file as
// without one: a file that reads longer is stored to its declared size, and
// one that reads shorter fails the copy with io.ErrUnexpectedEOF, during
// CopyFrom where the data is read into a data file, at Close where it is
// read into the image.
func TestContractCopiedFileIsTheSizeItsSourceDeclared(t *testing.T) {
	t.Parallel()

	const declared = 10

	source := func(reads int) fs.FS {
		return misreadFile{
			MapFS: fstest.MapFS{"f": &fstest.MapFile{Data: make([]byte, declared), Mode: 0o644}},
			reads: reads,
		}
	}

	for _, withDataFile := range []bool{false, true} {
		t.Run(fmt.Sprintf("data file %v", withDataFile), func(t *testing.T) {
			t.Parallel()

			create := func() (*erofs.Writer, *os.File) {
				if !withDataFile {
					return erofs.Create(&testBuffer{}), nil
				}

				df, err := os.Create(filepath.Join(t.TempDir(), "data"))
				if err != nil {
					t.Fatal(err)
				}

				t.Cleanup(func() { _ = df.Close() })

				return erofs.Create(&testBuffer{}, erofs.WithDataFile(df)), df
			}

			// A megabyte where ten bytes were declared.
			w, df := create()
			if err := w.CopyFrom(source(1 << 20)); err != nil {
				t.Fatalf("CopyFrom of a file that reads longer than it declared: %v", err)
			}

			info, err := w.Stat("/f")
			if err != nil {
				t.Fatal(err)
			}

			if info.Size() != declared {
				t.Errorf("the copied file is %d bytes; the source declared %d", info.Size(), declared)
			}

			if df != nil {
				st, serr := df.Stat()
				if serr != nil {
					t.Fatal(serr)
				}

				// One block of the data file, at most.
				if st.Size() > 4096 {
					t.Errorf("the data file holds %d bytes of a file declared %d", st.Size(), declared)
				}
			}

			if cerr := w.Close(); cerr != nil {
				t.Errorf("Close: %v", cerr)
			}

			// Fewer than the ten declared: none, four, and all but one.
			for _, reads := range []int{0, 4, declared - 1} {
				w, _ = create()

				err = w.CopyFrom(source(reads))
				if err == nil {
					err = w.Close()
				} else if !withDataFile {
					t.Errorf("CopyFrom read the file's data without a data file: %v", err)
				}

				if !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Errorf(
						"a file that reads %d bytes where it declared %d: %v, want io.ErrUnexpectedEOF",
						reads,
						declared,
						err,
					)
				}
			}
		})
	}
}
