package erofs_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
)

var errNoWrite = errors.New("the data file takes no write")

// writeRefuser is a data file whose writes fail.
type writeRefuser struct{ *os.File }

func (writeRefuser) Write([]byte) (int, error) { return 0, errNoWrite }

// TestContractFailedCreateLeavesNothing: a Create that fails for want of a
// place to put the file's data leaves neither the file nor the directories
// it would have made: Close writes an image without them.
func TestContractFailedCreateLeavesNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		opt  func(*testing.T) erofs.CreateOpt
	}{
		{
			name: "no directory for the spool",
			opt: func(t *testing.T) erofs.CreateOpt {
				t.Helper()

				return erofs.WithTempDir(filepath.Join(t.TempDir(), "missing"))
			},
		},
		{
			// One byte in: the first Create has padding to write.
			name: "a data file that refuses the padding",
			opt: func(t *testing.T) erofs.CreateOpt {
				t.Helper()

				df, err := os.Create(filepath.Join(t.TempDir(), "data.bin"))
				if err != nil {
					t.Fatal(err)
				}

				t.Cleanup(func() { _ = df.Close() })

				if _, err := df.Write([]byte{1}); err != nil {
					t.Fatal(err)
				}

				return erofs.WithDataFile(writeRefuser{df})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out := &testBuffer{}
			w := erofs.Create(out, tc.opt(t), erofs.WithBuildTime(corpus.BuildTime, 0))

			if _, err := w.Create("/d/f"); err == nil {
				t.Fatal("Create succeeded: the case tests nothing")
			}

			for _, name := range []string{"/d/f", "/d"} {
				if _, err := w.Stat(name); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("Stat(%s) after the failed Create: %v; want ErrNotExist", name, err)
				}
			}

			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			entries, err := fs.ReadDir(openWithDevices(t, out.Bytes()), ".")
			if err != nil || len(entries) != 0 {
				t.Errorf("the image's root: %d entries, %v; want an empty directory", len(entries), err)
			}
		})
	}
}

// TestContractCreateAfterAFailedCreate: once the cause is gone the same
// name is created as if the first call had not been made.
func TestContractCreateAfterAFailedCreate(t *testing.T) {
	t.Parallel()

	spool := filepath.Join(t.TempDir(), "spool")
	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithTempDir(spool), erofs.WithBuildTime(corpus.BuildTime, 0))

	if _, err := w.Create("/d/f"); err == nil {
		t.Fatal("Create succeeded without a spool directory: the test tests nothing")
	}

	if err := os.Mkdir(spool, 0o700); err != nil {
		t.Fatal(err)
	}

	writeFile(t, w, "/d/f", []byte("written"))

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := fs.ReadFile(openWithDevices(t, out.Bytes()), "d/f")
	if err != nil || string(got) != "written" {
		t.Errorf("d/f reads %q, %v; want %q", got, err, "written")
	}
}
