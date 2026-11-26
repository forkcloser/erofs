package erofs_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
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

// TestContractAFileWhoseCopyFailedIsNotInTheImage: with a data file a
// file's data is read during CopyFrom, and a file whose read fails or ends
// part of the way fails the copy. Its entry was in the tree by then, with
// the size it declared and the data file holding less: a Writer closed
// after that wrote an image where the file read another file's bytes, or
// one its own reader refuses. The entry now goes with the failure: the
// Writer does not find it, and the image a later Close writes holds what
// was copied whole, each file with its own bytes.
func TestContractAFileWhoseCopyFailedIsNotInTheImage(t *testing.T) {
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
					after := numbered(101, 9000)

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

					later := fstest.MapFS{"z": &fstest.MapFile{Data: after, Mode: 0o644}}
					if cerr := errors.Join(w.CopyFrom(later), w.Close()); cerr != nil {
						t.Fatalf("a later copy and Close: %v", cerr)
					}

					fsys, err := erofs.Open(bytes.NewReader(out.Bytes()), erofs.WithExtraDevices(df))
					if err != nil {
						t.Fatalf("the image: %v", err)
					}

					var names []string

					err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
						if err != nil || p == "." {
							return err
						}

						names = append(names, p)

						_, err = d.Info()

						return err
					})
					if err != nil || fmt.Sprint(names) != "[a z]" {
						t.Errorf("the image holds %v, %v; want a and z", names, err)
					}

					for file, content := range map[string][]byte{"a": before, "z": after} {
						if got, rerr := fs.ReadFile(fsys, file); rerr != nil || !bytes.Equal(got, content) {
							t.Errorf("%s reads %d bytes, %v; want its own %d", file, len(got), rerr, len(content))
						}
					}
				})
			}
		}
	}
}
