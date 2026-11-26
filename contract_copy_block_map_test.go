package erofs_test

import (
	"bytes"
	"io/fs"
	"testing"

	"github.com/forkcloser/go-erofs"
)

// metadataOnlyCopy is the image of a metadata-only copy of source.
func metadataOnlyCopy(t *testing.T, source fs.FS) []byte {
	t.Helper()

	out := &testBuffer{}
	w := erofs.Create(out)

	if err := w.CopyFrom(source, erofs.MetadataOnly()); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return out.Bytes()
}

// TestContractMetadataOnlyCopyOfABlockMapWithHoles: a file with a block
// map, its chunks out of order and two of them holes, its last one cut
// short, reads from its metadata-only copy what it reads from its image:
// the holes as zeros, each chunk from where the map places it.
func TestContractMetadataOnlyCopyOfABlockMapWithHoles(t *testing.T) {
	t.Parallel()

	img, content := laidChunks(0, 100, 2, -1, 0, 1, -1, 3)
	source := openWithDevices(t, img)

	if got, err := fs.ReadFile(source, "f"); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("test premise broken: the image's file reads %d bytes, %v", len(got), err)
	}

	copied := openWithDevices(t, metadataOnlyCopy(t, source), img)

	got, err := fs.ReadFile(copied, "f")
	if err != nil || !bytes.Equal(got, content) {
		t.Errorf("the copy's file reads %d bytes, %v; want the image's %d", len(got), err, len(content))
	}

	sameTree(t, source, copied)
}

// TestContractMetadataOnlyCopyOfABlockMapOverAMappedRange: a block map's
// entry is an address of the primary device, and one inside an extra
// device's mapped range is that device's block, as erofs_map_dev places
// it. The copy names for each chunk the device its source reads it from:
// the image's blocks 2 and 7 around the device's block 1.
func TestContractMetadataOnlyCopyOfABlockMapOverAMappedRange(t *testing.T) {
	t.Parallel()

	img, device := laidOverMappedRange(4, 0, 2, 4, 7)
	source := openWithDevices(t, img, device)

	want, err := fs.ReadFile(source, "f")
	if err != nil || blocksOf(want) != "12 D1 17" {
		t.Fatalf("test premise broken: the image's file reads blocks %s, %v", blocksOf(want), err)
	}

	copied := openWithDevices(t, metadataOnlyCopy(t, source), img, device)

	got, err := fs.ReadFile(copied, "f")
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("the copy's file reads blocks %s, %v; want %s", blocksOf(got), err, blocksOf(want))
	}

	sameTree(t, source, copied)
}
