package erofs_test

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/forkcloser/erofs"
)

// sparseFS is a metadata-only source describing one file whose content lives
// in an external blob as data / hole / data.
type sparseFS struct {
	blockSize uint32
	blocks    uint64
	size      int64
	ranges    []erofs.DataRange
	// noRanges makes DataRange report nothing, the shape a metadata-only
	// source takes when it cannot describe where its data lives. The entry
	// still becomes chunk-based, just with no mappings.
	noRanges bool
}

func (s *sparseFS) BlockSize() uint32    { return s.blockSize }
func (s *sparseFS) DeviceBlocks() uint64 { return s.blocks }
func (*sparseFS) BuildTime() uint64      { return 1000 }

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
func (*sparseDir) Read([]byte) (int, error)     { return 0, io.EOF }
func (*sparseDir) Close() error                 { return nil }
func (d *sparseDir) ReadDir(int) ([]fs.DirEntry, error) {
	return []fs.DirEntry{&sparseEnt{s: d.s}}, nil
}

type sparseEnt struct{ s *sparseFS }

func (*sparseEnt) Name() string                 { return "f" }
func (*sparseEnt) IsDir() bool                  { return false }
func (*sparseEnt) Type() fs.FileMode            { return 0 }
func (e *sparseEnt) Info() (fs.FileInfo, error) { return &sparseInfo{s: e.s}, nil }

type sparseFile struct{ s *sparseFS }

func (f *sparseFile) Stat() (fs.FileInfo, error) { return &sparseInfo{s: f.s}, nil }
func (*sparseFile) Read([]byte) (int, error)     { return 0, io.EOF }
func (*sparseFile) Close() error                 { return nil }

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

func (*sparseInfo) ModTime() time.Time { return time.Unix(1000, 0) }
func (i *sparseInfo) IsDir() bool      { return i.dir }
func (*sparseInfo) Sys() any           { return nil }

func (i *sparseInfo) DataRange() []erofs.DataRange {
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
		ranges: []erofs.DataRange{
			{Device: 0, Offset: 0, Size: int64(bs)},
			{Offset: -1, Size: int64(bs)}, // a hole
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

	checkSparseBytes(t, got, bs, label)
}

// checkSparseBytes is checkSparseContent on content already read.
func checkSparseBytes(t *testing.T, got []byte, bs int, label string) {
	t.Helper()

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
	t.Parallel()

	for _, bs := range []int{512, 1024, 2048, 4096} {
		t.Run(blockSizeName(bs), func(t *testing.T) {
			t.Parallel()

			blob := sparseBlob(bs)

			out := &testBuffer{}

			w := erofs.Create(out, erofs.WithBlockSize(bs), erofs.WithBuildTime(1000, 0))
			if err := w.CopyFrom(newSparseFS(bs, blob), erofs.MetadataOnly()); err != nil {
				t.Fatal(err)
			}

			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			img, err := erofs.Open(bytes.NewReader(out.Bytes()), erofs.WithExtraDevices(bytes.NewReader(blob)))
			if err != nil {
				t.Fatal(err)
			}

			checkSparseContent(t, img, bs, "image")
			fsckWithDevice(t, out.Bytes(), blob)
		})
	}
}

// TestChunkMapSurvivesReindex covers copying a metadata-only image into
// another metadata-only image, the container layer re-index case. Holes were
// dropped while parsing the source chunk map, sliding every later extent into
// the wrong logical position, and every chunked file was marked contiguous so
// planLayout collapsed its extents into a single mapping.
func TestChunkMapSurvivesReindex(t *testing.T) {
	t.Parallel()

	const bs = 4096

	blob := sparseBlob(bs)

	out := &testBuffer{}

	w := erofs.Create(out, erofs.WithBlockSize(bs), erofs.WithBuildTime(1000, 0))
	if err := w.CopyFrom(newSparseFS(bs, blob), erofs.MetadataOnly()); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	img1, err := erofs.Open(bytes.NewReader(out.Bytes()), erofs.WithExtraDevices(bytes.NewReader(blob)))
	if err != nil {
		t.Fatal(err)
	}

	checkSparseContent(t, img1, bs, "first image")

	// Re-index the image into a fresh metadata-only image.
	out2 := &testBuffer{}

	w2 := erofs.Create(out2, erofs.WithBuildTime(1000, 0))
	if err = w2.CopyFrom(img1, erofs.MetadataOnly()); err != nil {
		t.Fatal(err)
	}

	if err = w2.Close(); err != nil {
		t.Fatal(err)
	}

	img2, err := erofs.Open(bytes.NewReader(out2.Bytes()), erofs.WithExtraDevices(bytes.NewReader(blob)))
	if err != nil {
		t.Fatal(err)
	}

	checkSparseContent(t, img2, bs, "re-indexed image")
	fsckWithDevice(t, out2.Bytes(), blob)
}

// TestDirectReadSparseFile covers Read and WriteTo on a chunk-based file with
// a hole. Both serve a contiguous file straight from the device; taking that
// path here would read the hole as the device bytes behind it.
func TestDirectReadSparseFile(t *testing.T) {
	t.Parallel()

	const bs = 4096

	blob := sparseBlob(bs)

	out := &testBuffer{}

	w := erofs.Create(out, erofs.WithBlockSize(bs), erofs.WithBuildTime(1000, 0))
	if err := w.CopyFrom(newSparseFS(bs, blob), erofs.MetadataOnly()); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	img, err := erofs.Open(bytes.NewReader(out.Bytes()), erofs.WithExtraDevices(bytes.NewReader(blob)))
	if err != nil {
		t.Fatal(err)
	}

	// fs.ReadFile goes through Read.
	checkSparseContent(t, img, bs, "sparse via Read")

	fh, err := img.Open("f")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = fh.Close() }()

	// io.Copy goes through WriteTo.
	var got bytes.Buffer
	if _, err = io.Copy(&got, fh); err != nil {
		t.Fatal(err)
	}

	checkSparseBytes(t, got.Bytes(), bs, "sparse via WriteTo")
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

	out, err := exec.CommandContext(t.Context(), "fsck.erofs", "--device="+blobPath, imgPath).CombinedOutput()
	if err != nil {
		t.Errorf("fsck.erofs rejected the image: %v\n%s", err, out)
	}
}
