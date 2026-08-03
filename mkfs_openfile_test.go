package erofs

import (
	"bytes"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

// writeFile creates, fills and closes one file.
func writeFile(t *testing.T, w *Writer, name string, data []byte) {
	t.Helper()

	f, err := w.Create(name)
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("Write(%q): %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close(%q): %v", name, err)
	}
}

// TestCreateRejectsSecondOpenFile covers two files open at once. Both
// recorded the same start offset while their bytes were appended to one
// shared stream, so the second file's region pointed at the first file's
// data and its own content was lost entirely.
func TestCreateRejectsSecondOpenFile(t *testing.T) {
	w := Create(&seekBuf{}, WithBuildTime(1000, 0))

	f1, err := w.Create("/one")
	if err != nil {
		t.Fatal(err)
	}

	f2, err := w.Create("/two")
	if err == nil {
		_ = f2.Close()
		t.Fatal("Create succeeded while another file was open for writing")
	}
	if !strings.Contains(err.Error(), "/one") {
		t.Errorf("error should name the file still open, got: %v", err)
	}
	t.Logf("rejected with: %v", err)

	// Closing the first file releases the slot.
	if err := f1.Close(); err != nil {
		t.Fatal(err)
	}
	f2, err = w.Create("/two")
	if err != nil {
		t.Fatalf("Create after closing the first file: %v", err)
	}
	if err := f2.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSequentialFilesKeepTheirData is the positive case: written one at a
// time, each file's content must round-trip.
func TestSequentialFilesKeepTheirData(t *testing.T) {
	for _, useDataFile := range []bool{false, true} {
		name := "spool"
		if useDataFile {
			name = "datafile"
		}
		t.Run(name, func(t *testing.T) {
			out := &seekBuf{}
			opts := []CreateOpt{WithBuildTime(1000, 0)}

			var blobPath string
			if useDataFile {
				df, err := os.CreateTemp(t.TempDir(), "blob-*")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = df.Close() }()
				blobPath = df.Name()
				opts = append(opts, WithDataFile(df))
			}

			w := Create(out, opts...)
			want := map[string][]byte{
				"one":   bytes.Repeat([]byte("1"), 32),
				"two":   bytes.Repeat([]byte("2"), 32),
				"three": bytes.Repeat([]byte("3"), 9000),
			}
			for _, n := range []string{"one", "two", "three"} {
				writeFile(t, w, "/"+n, want[n])
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			var readOpts []OpenOpt
			if useDataFile {
				blob, err := os.ReadFile(blobPath)
				if err != nil {
					t.Fatal(err)
				}
				readOpts = append(readOpts, WithExtraDevices(bytes.NewReader(blob)))
			}
			img, err := Open(bytes.NewReader(out.buf), readOpts...)
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range []string{"one", "two", "three"} {
				got, err := fs.ReadFile(img, n)
				if err != nil {
					t.Fatalf("read %q: %v", n, err)
				}
				if !bytes.Equal(got, want[n]) {
					t.Errorf("%q: got %d bytes starting %q, want %d bytes starting %q",
						n, len(got), firstByte(got), len(want[n]), firstByte(want[n]))
				}
			}
		})
	}
}

func firstByte(b []byte) string {
	if len(b) == 0 {
		return ""
	}

	return string(b[:1])
}

// TestCopyFromRejectsOpenFile covers CopyFrom appending to the same stream as
// an open writer, which would interleave the copied bytes into its region.
func TestCopyFromRejectsOpenFile(t *testing.T) {
	w := Create(&seekBuf{}, WithBuildTime(1000, 0))

	f, err := w.Create("/one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}

	src := fstestMapFS()
	if err := w.CopyFrom(src); err == nil {
		t.Fatal("CopyFrom succeeded while a file was open for writing")
	} else {
		t.Logf("rejected with: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.CopyFrom(src); err != nil {
		t.Fatalf("CopyFrom after closing the file: %v", err)
	}
}

// TestWriterCloseRejectsOpenFile covers serializing while a file is open. The
// entry's size is recorded by File.Close, so the image would have listed it
// as empty and dropped everything already written to it.
func TestWriterCloseRejectsOpenFile(t *testing.T) {
	w := Create(&seekBuf{}, WithBuildTime(1000, 0))

	f, err := w.Create("/one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte("x"), 100)); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err == nil {
		t.Fatal("Close succeeded while a file was open for writing")
	} else {
		t.Logf("rejected with: %v", err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close after closing the file: %v", err)
	}
}

// fstestMapFS returns a tiny in-memory source for CopyFrom.
func fstestMapFS() fs.FS {
	return fstest.MapFS{
		"hello.txt": &fstest.MapFile{Data: []byte("hi"), Mode: 0o644},
	}
}
