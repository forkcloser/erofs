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
	"github.com/forkcloser/go-erofs/internal/corpus"
)

// errReadFails is the failure of a failingRead's file.
var errReadFails = errors.New("failingRead: this file fails where it is read")

// failingRead is a source whose file "f" reads as reads bytes and then
// fails, with errReadFails or, when ends is set, by ending there.
type failingRead struct {
	fstest.MapFS

	reads int
	ends  bool
}

type failingFile struct {
	fs.File

	left int
	end  error
}

func (s failingRead) Open(name string) (fs.File, error) {
	f, err := s.MapFS.Open(name)
	if err != nil || name != "f" {
		return f, err
	}

	end := errReadFails
	if s.ends {
		end = io.EOF
	}

	return &failingFile{File: f, left: s.reads, end: end}, nil
}

func (f *failingFile) Read(p []byte) (int, error) {
	if f.left == 0 {
		return 0, f.end
	}

	n := min(len(p), f.left)
	for i := range p[:n] {
		p[i] = 0xEE
	}

	f.left -= n

	return n, nil
}

// numbered is n bytes that tell where in which file they are.
func numbered(file byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = file + byte(i%251)
	}

	return out
}

// TestContractAFileWhoseCopyFailedIsNotInTheTree: with a data file a
// file's data is read during CopyFrom, and a file whose read fails or ends
// part of the way fails the copy. Its entry was in the tree by then, with
// the size it declared and the data file holding less. The entry goes with
// the failure, so that what the Writer reads back is what it holds; and the
// Writer has failed: a later copy and Close return the copy's error, and
// Close writes no image.
func TestContractAFileWhoseCopyFailedIsNotInTheTree(t *testing.T) {
	t.Parallel()

	for _, declared := range []int{10, 4097, 10000} {
		for _, reads := range []int{0, declared / 2} {
			for _, ends := range []bool{false, true} {
				name := fmt.Sprintf("%d bytes declared, %d read, ends %v", declared, reads, ends)

				t.Run(name, func(t *testing.T) {
					t.Parallel()

					df, err := os.Create(filepath.Join(t.TempDir(), "data"))
					if err != nil {
						t.Fatal(err)
					}

					t.Cleanup(func() { _ = df.Close() })

					out := &testBuffer{}
					w := erofs.Create(out, erofs.WithDataFile(df), erofs.WithBuildTime(corpus.BuildTime, 0))

					before := numbered(1, 5000)

					failing := failingRead{
						MapFS: fstest.MapFS{
							"a": &fstest.MapFile{Data: before, Mode: 0o644},
							"f": &fstest.MapFile{Data: make([]byte, declared), Mode: 0o644},
						},
						reads: reads,
						ends:  ends,
					}

					want := errReadFails
					if ends {
						want = io.ErrUnexpectedEOF
					}

					if cerr := w.CopyFrom(failing); !errors.Is(cerr, want) {
						t.Fatalf("CopyFrom of a file that fails part of the way: %v; want %v", cerr, want)
					}

					if _, serr := w.Stat("/f"); !errors.Is(serr, fs.ErrNotExist) {
						t.Errorf("Stat of the file whose copy failed: %v; want ErrNotExist", serr)
					}

					if info, serr := w.Stat("/a"); serr != nil || info.Size() != int64(len(before)) {
						t.Errorf(
							"Stat of the file copied whole before it: %v, %v; want its %d bytes",
							info,
							serr,
							len(before),
						)
					}

					later := fstest.MapFS{"z": &fstest.MapFile{Data: numbered(101, 9000), Mode: 0o644}}
					if cerr := w.CopyFrom(later); !errors.Is(cerr, want) {
						t.Errorf("a later copy: %v; want the failed copy's error", cerr)
					}

					if cerr := w.Close(); !errors.Is(cerr, want) {
						t.Errorf("Close: %v; want the failed copy's error", cerr)
					}

					if len(out.Bytes()) != 0 {
						t.Errorf("Close wrote an image of %d bytes; want none", len(out.Bytes()))
					}
				})
			}
		}
	}
}
