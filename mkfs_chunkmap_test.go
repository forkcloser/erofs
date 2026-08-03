package erofs

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"testing"
	"time"
)

// sparseFS is a metadata-only source describing one file whose content lives
// in an external blob as data / hole / data.
type sparseFS struct {
	blockSize uint32
	blocks    uint64
	size      int64
	ranges    []DataRange
	// noRanges makes DataRange report nothing, the shape a metadata-only
	// source takes when it cannot describe where its data lives. The entry
	// still becomes chunk-based, just with no mappings.
	noRanges bool
}

func (s *sparseFS) BlockSize() uint32    { return s.blockSize }
func (s *sparseFS) DeviceBlocks() uint64 { return s.blocks }
func (s *sparseFS) BuildTime() uint64    { return 1000 }

func (s *sparseFS) Open(name string) (fs.File, error) {
	switch name {
	case ".":
		return &sparseDir{s: s}, nil
	case "f":
		return &sparseFile{s: s}, nil
	}

	return nil, fs.ErrNotExist
}

type sparseDir struct{ s *sparseFS }

func (d *sparseDir) Stat() (fs.FileInfo, error) { return &sparseInfo{s: d.s, dir: true}, nil }
func (d *sparseDir) Read([]byte) (int, error)   { return 0, io.EOF }
func (d *sparseDir) Close() error               { return nil }
func (d *sparseDir) ReadDir(int) ([]fs.DirEntry, error) {
	return []fs.DirEntry{&sparseEnt{s: d.s}}, nil
}

type sparseEnt struct{ s *sparseFS }

func (e *sparseEnt) Name() string               { return "f" }
func (e *sparseEnt) IsDir() bool                { return false }
func (e *sparseEnt) Type() fs.FileMode          { return 0 }
func (e *sparseEnt) Info() (fs.FileInfo, error) { return &sparseInfo{s: e.s}, nil }

type sparseFile struct{ s *sparseFS }

func (f *sparseFile) Stat() (fs.FileInfo, error) { return &sparseInfo{s: f.s}, nil }
func (f *sparseFile) Read([]byte) (int, error)   { return 0, io.EOF }
func (f *sparseFile) Close() error               { return nil }

type sparseInfo struct {
	s   *sparseFS
	dir bool
}

func (i *sparseInfo) Name() string {
	if i.dir {
		return "."
	}

	return "f"
}

func (i *sparseInfo) Size() int64 {
	if i.dir {
		return 0
	}

	return i.s.size
}

func (i *sparseInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}

	return 0o644
}

func (i *sparseInfo) ModTime() time.Time { return time.Unix(1000, 0) }
func (i *sparseInfo) IsDir() bool        { return i.dir }
func (i *sparseInfo) Sys() any           { return nil }

func (i *sparseInfo) DataRange() []DataRange {
	if i.dir || i.s.noRanges {
		return nil
	}

	return i.s.ranges
}

// sparseBlob lays out four blocks: the marker in block 1 must never surface,
// because logical block 1 of the file is a hole.
func sparseBlob(bs int) []byte {
	blob := make([]byte, 8*bs)
	copy(blob[0*bs:], bytes.Repeat([]byte("A"), bs))
	copy(blob[1*bs:], bytes.Repeat([]byte("X"), bs))
	copy(blob[2*bs:], bytes.Repeat([]byte("B"), bs))
	copy(blob[3*bs:], bytes.Repeat([]byte("C"), bs))

	return blob
}

func newSparseFS(bs int, blob []byte) *sparseFS {
	return &sparseFS{
		blockSize: uint32(bs),
		blocks:    uint64(len(blob) / bs),
		size:      int64(4 * bs),
		ranges: []DataRange{
			{Device: 0, Offset: 0, Size: int64(bs)},
			{Offset: holeOffset, Size: int64(bs)},
			{Device: 0, Offset: int64(2 * bs), Size: int64(2 * bs)},
		},
	}
}

// checkSparseContent asserts the file reads back as A / zeros / B / C. A
// non-zero logical block 1 means a hole was mapped onto real device data,
// exposing blob bytes the image never referenced.
func checkSparseContent(t *testing.T, img fs.FS, bs int, label string) {
	t.Helper()

	got, err := fs.ReadFile(img, "f")
	if err != nil {
		t.Fatalf("%s: read: %v", label, err)
	}
	if len(got) != 4*bs {
		t.Fatalf("%s: read %d bytes, want %d", label, len(got), 4*bs)
	}
	for blk, want := range []byte{'A', 0, 'B', 'C'} {
		block := got[blk*bs : (blk+1)*bs]
		if !bytes.Equal(block, bytes.Repeat([]byte{want}, bs)) {
			t.Errorf("%s: logical block %d starts with %q, want all %q",
				label, blk, block[0], want)
		}
	}
}

// TestChunkMapHonoursBlockSize covers extents described at block granularity
// while the chunk size was forced up to a 4096-byte floor. Any extent or hole
// boundary inside a chunk was silently swallowed, so a hole read back as real
// blob content.
func TestChunkMapHonoursBlockSize(t *testing.T) {
	for _, bs := range []int{512, 1024, 2048, 4096} {
		t.Run(blockSizeName(bs), func(t *testing.T) {
			blob := sparseBlob(bs)

			out := &seekBuf{}
			w := Create(out, WithBlockSize(bs), WithBuildTime(1000, 0))
			if err := w.CopyFrom(newSparseFS(bs, blob), MetadataOnly()); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			img, err := Open(bytes.NewReader(out.buf), WithExtraDevices(bytes.NewReader(blob)))
			if err != nil {
				t.Fatal(err)
			}
			checkSparseContent(t, img, bs, "image")
			fsckWithDevice(t, out.buf, blob)
		})
	}
}

// TestChunkMapSurvivesReindex covers copying a metadata-only image into
// another metadata-only image, the container layer re-index case. Holes were
// dropped while parsing the source chunk map, sliding every later extent into
// the wrong logical position, and every chunked file was marked contiguous so
// planLayout collapsed its extents into a single mapping.
func TestChunkMapSurvivesReindex(t *testing.T) {
	const bs = 4096
	blob := sparseBlob(bs)

	out := &seekBuf{}
	w := Create(out, WithBlockSize(bs), WithBuildTime(1000, 0))
	if err := w.CopyFrom(newSparseFS(bs, blob), MetadataOnly()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	img1, err := Open(bytes.NewReader(out.buf), WithExtraDevices(bytes.NewReader(blob)))
	if err != nil {
		t.Fatal(err)
	}
	checkSparseContent(t, img1, bs, "first image")

	// Re-index the image into a fresh metadata-only image.
	out2 := &seekBuf{}
	w2 := Create(out2, WithBuildTime(1000, 0))
	if err := w2.CopyFrom(img1, MetadataOnly()); err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}

	img2, err := Open(bytes.NewReader(out2.buf), WithExtraDevices(bytes.NewReader(blob)))
	if err != nil {
		t.Fatal(err)
	}
	checkSparseContent(t, img2, bs, "re-indexed image")
	fsckWithDevice(t, out2.buf, blob)
}

func blockSizeName(bs int) string {
	switch bs {
	case 512:
		return "512"
	case 1024:
		return "1024"
	case 2048:
		return "2048"
	default:
		return "4096"
	}
}

// fsckWithDevice validates an image against the reference implementation.
func fsckWithDevice(t *testing.T, image, blob []byte) {
	t.Helper()

	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		return
	}
	dir := t.TempDir()
	imgPath := dir + "/img.erofs"
	blobPath := dir + "/blob.bin"
	if err := os.WriteFile(imgPath, image, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("fsck.erofs", "--device="+blobPath, imgPath).CombinedOutput()
	if err != nil {
		t.Errorf("fsck.erofs rejected the image: %v\n%s", err, out)
	}
}
