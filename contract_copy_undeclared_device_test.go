package erofs_test

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
)

// twoBlocksImage is an image holding /dir/blocks, two whole blocks of fill:
// no inline tail, so every byte of the file sits at a block address.
func twoBlocksImage(t *testing.T, fill byte) []byte {
	t.Helper()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
	writeFile(t, w, "/dir/blocks", bytes.Repeat([]byte{fill}, 8192))

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return out.Bytes()
}

// subtree is dir of the image, as fs.Sub gives it: a value that is no longer
// the image, and declares no device.
func subtree(t *testing.T, img []byte, dir string) fs.FS {
	t.Helper()

	sub, err := fs.Sub(openWithDevices(t, img), dir)
	if err != nil {
		t.Fatal(err)
	}

	return sub
}

// TestContractMetadataOnlyCopyNeedsTheSourcesDevice: a metadata-only copy
// stores, in place of a file's data, addresses on the source's device. A
// source that declares no device has none in the copy for them to name:
// the copy is refused, ErrInvalid, where it used to succeed and leave the
// file reading the device an earlier layer declared, or no device at all.
func TestContractMetadataOnlyCopyNeedsTheSourcesDevice(t *testing.T) {
	t.Parallel()

	const bs = 4096

	ranged := func(ranges ...erofs.DataRange) fs.FS {
		var size int64
		for _, r := range ranges {
			size += r.Size
		}

		// Behind the struct only Open shows: the ranges without DeviceBlocks.
		return struct{ fs.FS }{
			&dataRangerFS{deviceBlocks: 64, file: &dataRangerFileInfo{name: "f", size: size, ranges: ranges}},
		}
	}

	for _, tc := range []struct {
		name string
		// lower is copied first, metadata-only, when it is not nil.
		lower func(*testing.T) fs.FS
		src   func(*testing.T) fs.FS
	}{
		{
			name: "a subtree of an image",
			src:  func(t *testing.T) fs.FS { t.Helper(); return subtree(t, twoBlocksImage(t, 'B'), "dir") },
		},
		{
			name:  "a subtree of an image, over a layer that has a device",
			lower: func(t *testing.T) fs.FS { t.Helper(); return openWithDevices(t, twoBlocksImage(t, 'A')) },
			src:   func(t *testing.T) fs.FS { t.Helper(); return subtree(t, twoBlocksImage(t, 'B'), "dir") },
		},
		{
			name: "ranges from a source with no DeviceBlocks",
			src:  func(*testing.T) fs.FS { return ranged(erofs.DataRange{Offset: 0, Size: 2 * bs}) },
		},
		{
			name: "a range after a hole",
			src: func(*testing.T) fs.FS {
				return ranged(erofs.DataRange{Offset: -1, Size: bs}, erofs.DataRange{Offset: bs, Size: bs})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			w := erofs.Create(&testBuffer{}, erofs.WithBuildTime(corpus.BuildTime, 0))

			if tc.lower != nil {
				if err := w.CopyFrom(tc.lower(t), erofs.MetadataOnly()); err != nil {
					t.Fatalf("the lower layer: %v", err)
				}
			}

			if err := w.CopyFrom(tc.src(t), erofs.MetadataOnly()); !errors.Is(err, erofs.ErrInvalid) {
				t.Errorf("CopyFrom(MetadataOnly) = %v; want ErrInvalid", err)
			}
		})
	}

	// A file that is one hole has no address to place: it needs no device.
	t.Run("a hole alone is copied", func(t *testing.T) {
		t.Parallel()

		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

		src := ranged(erofs.DataRange{Offset: -1, Size: 2 * bs})
		if err := errors.Join(w.CopyFrom(src, erofs.MetadataOnly()), w.Close()); err != nil {
			t.Fatal(err)
		}

		got, err := fs.ReadFile(openWithDevices(t, out.Bytes()), "f")
		if err != nil || !bytes.Equal(got, make([]byte, 2*bs)) {
			t.Errorf("ReadFile: %d bytes, %v; want %d zeros", len(got), err, 2*bs)
		}
	})
}
