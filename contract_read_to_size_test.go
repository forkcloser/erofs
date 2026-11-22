package erofs_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
)

// bothWays is what reading one file gave by Open and io.ReadAll, then by
// Open and io.Copy: the two ways a caller reads a file that do not go
// through fs.ReadFile, the first by Read and the second by WriteTo.
type bothWays struct {
	read    int
	readErr error
	copied  int64
	copyErr error
}

// corrupt reports whether both ways ended in ErrCorrupt.
func (b bothWays) corrupt() bool {
	return errors.Is(b.readErr, erofs.ErrCorrupt) && errors.Is(b.copyErr, erofs.ErrCorrupt)
}

func readBothWays(t *testing.T, fsys fs.FS, name string) bothWays {
	t.Helper()

	var got bothWays

	f, err := fsys.Open(name)
	if err != nil {
		t.Fatalf("Open %s: %v", name, err)
	}

	read, err := io.ReadAll(f)
	got.read, got.readErr = len(read), err
	_ = f.Close()

	if f, err = fsys.Open(name); err != nil {
		t.Fatalf("Open %s: %v", name, err)
	}

	got.copied, got.copyErr = io.Copy(io.Discard, f)
	_ = f.Close()

	return got
}

// TestContractReadEndsAtTheFilesSizeOrFails: no Read or WriteTo ends before
// the size the inode declares without an error. Data that stops first is
// ErrCorrupt, whichever way the file is read: an image opened without the
// device its files live on, an image shorter than its superblock says behind
// a reader with no length, and a chunk map running past the image's end. Each
// read these files short, or empty, with a nil error.
func TestContractReadEndsAtTheFilesSizeOrFails(t *testing.T) {
	t.Parallel()

	t.Run("an image without its device", func(t *testing.T) {
		t.Parallel()

		var im corpus.Image

		for _, c := range corpus.Committed(t) {
			if c.Name == "small-blobdev-512" {
				im = c
			}
		}

		if im.Img == nil {
			t.Fatal("the committed small-blobdev-512 image is missing")
		}

		whole, err := erofs.Open(bytes.NewReader(im.Img), erofs.WithExtraDevices(bytes.NewReader(im.Blob)))
		if err != nil {
			t.Fatal(err)
		}

		flat, err := erofs.Open(bytes.NewReader(im.Img))
		if err != nil {
			t.Fatal(err)
		}

		files := 0

		err = fs.WalkDir(whole, ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}

			want, err := fs.ReadFile(whole, name)
			if err != nil || len(want) == 0 {
				return err
			}

			files++

			if got := readBothWays(t, flat, name); !got.corrupt() {
				t.Errorf("%s (%d bytes): io.ReadAll %d bytes, %v; io.Copy %d bytes, %v; want ErrCorrupt from both",
					name, len(want), got.read, got.readErr, got.copied, got.copyErr)
			}

			return nil
		})
		if err != nil {
			t.Fatal(err)
		}

		if files == 0 {
			t.Fatal("the image holds no file with data")
		}
	})

	t.Run("an image cut inside a file, behind a reader with no length", func(t *testing.T) {
		t.Parallel()

		content := bytes.Repeat([]byte("0123456789abcdef"), 4096)

		img := metadataFirst(t, writeImage(t, []corpus.Node{
			{Path: "big", Type: tar.TypeReg, Mode: 0o644, Data: content},
		}, erofs.WithNoInlineData()))

		full, err := erofs.Open(bytes.NewReader(img))
		if err != nil {
			t.Fatal(err)
		}

		info, err := fs.Stat(full, "big")
		if err != nil {
			t.Fatal(err)
		}

		ranges := info.(interface{ DataRange() []erofs.DataRange }).DataRange()
		if len(ranges) != 1 {
			t.Fatalf("the file has %d ranges, want one", len(ranges))
		}

		cut, err := erofs.Open(readAtOnly{bytes.NewReader(img[:ranges[0].Offset+ranges[0].Size/2])})
		if err != nil {
			t.Fatalf("Open of the cut image: %v", err)
		}

		if got := readBothWays(t, cut, "big"); !got.corrupt() {
			t.Errorf("io.ReadAll %d of %d bytes, %v; io.Copy %d bytes, %v; want ErrCorrupt from both",
				got.read, len(content), got.readErr, got.copied, got.copyErr)
		}
	})

	t.Run("a chunk map past the image's end", func(t *testing.T) {
		t.Parallel()

		// A block map of 8000 bytes from byte 4256, in an image of two blocks.
		src := laidChunkedFile(2000, 0)
		src.size = 2 * 4096
		laidSuper(src.head, 12, 2)

		fsys, err := erofs.Open(bytes.NewReader(src.whole()))
		if err != nil {
			t.Fatal(err)
		}

		if got := readBothWays(t, fsys, "f"); !got.corrupt() {
			t.Errorf("io.ReadAll %d bytes, %v; io.Copy %d bytes, %v; want ErrCorrupt from both",
				got.read, got.readErr, got.copied, got.copyErr)
		}
	})
}

// TestContractDirectoryBlockThatCannotBeReadIsAnError: a directory block the
// directory's size covers and the image does not hold is ErrCorrupt from
// ReadDir and from a lookup in it. A chunk-based root whose one map entry lay
// at the image's end listed no name with a nil error, and every name in it
// was ErrNotExist.
func TestContractDirectoryBlockThatCannotBeReadIsAnError(t *testing.T) {
	t.Parallel()

	// The root's map starts right after its inode, at byte 4160: the image
	// is cut there, and its superblock says one block.
	cut := bytes.Clone(laidChunkedRoot(1).whole()[:4096+64])
	laidSuper(cut, 12, 1)

	fsys, err := erofs.Open(bytes.NewReader(cut))
	if err != nil {
		t.Fatalf("Open of the image cut at the map: %v", err)
	}

	if entries, err := fs.ReadDir(fsys, "."); !errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf("ReadDir: %d entries, %v; want ErrCorrupt", len(entries), err)
	}

	if _, err := fs.Stat(fsys, "00"); !errors.Is(err, erofs.ErrCorrupt) || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat of a name in it: %v; want ErrCorrupt, not ErrNotExist", err)
	}
}
