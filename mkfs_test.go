package erofs_test

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"testing"
	"time"

	erofs "github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/erofstest"
)

// --- Merge tests ---

// --- DataRange-driven CopyFrom tests ---

// dataRangerFileInfo is a test fs.FileInfo whose DataRange() returns
// caller-supplied ranges, simulating a source (e.g. ext4 or OCI layer
// reader) that knows where its file bytes live without holding a data reader.
// content is the data returned by Read(); if nil, Read returns zeros.
type dataRangerFileInfo struct {
	name    string
	size    int64
	ranges  []erofs.DataRange
	content []byte // data served by Read(); nil means return zeros
}

func (fi *dataRangerFileInfo) Name() string                 { return fi.name }
func (fi *dataRangerFileInfo) Size() int64                  { return fi.size }
func (*dataRangerFileInfo) Mode() fs.FileMode               { return 0o644 }
func (*dataRangerFileInfo) ModTime() time.Time              { return time.Time{} }
func (*dataRangerFileInfo) IsDir() bool                     { return false }
func (*dataRangerFileInfo) Sys() any                        { return nil }
func (fi *dataRangerFileInfo) DataRange() []erofs.DataRange { return fi.ranges }

// dataRangerFS is a minimal fs.FS that exposes one regular file whose
// FileInfo implements DataRange().  It also implements DeviceBlocks() so
// CopyFrom records the backing-device size.
type dataRangerFS struct {
	file *dataRangerFileInfo
	// deviceBlocks is the declared size (in 4096-byte blocks) of the backing device.
	deviceBlocks uint64
}

func (f *dataRangerFS) DeviceBlocks() uint64 { return f.deviceBlocks }

func (f *dataRangerFS) Open(name string) (fs.File, error) {
	if name == "." {
		return &dataRangerDir{fs: f}, nil
	}

	if name == f.file.name {
		return &dataRangerFile{info: f.file}, nil
	}

	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

type dataRangerDir struct {
	fs      *dataRangerFS
	didRead bool
}

func (*dataRangerDir) Stat() (fs.FileInfo, error) {
	return &dataRangerDirInfo{}, nil
}

func (*dataRangerDir) Read([]byte) (int, error) {
	//nolint:err113 // a fake's failure fixture
	return 0, &fs.PathError{Op: "read", Path: ".", Err: errors.New("is a directory")}
}
func (*dataRangerDir) Close() error { return nil }
func (d *dataRangerDir) ReadDir(int) ([]fs.DirEntry, error) {
	if d.didRead {
		return nil, io.EOF
	}

	d.didRead = true

	return []fs.DirEntry{&dataRangerEntry{info: d.fs.file}}, nil
}

type dataRangerDirInfo struct{}

func (*dataRangerDirInfo) Name() string       { return "." }
func (*dataRangerDirInfo) Size() int64        { return 0 }
func (*dataRangerDirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o755 }
func (*dataRangerDirInfo) ModTime() time.Time { return time.Time{} }
func (*dataRangerDirInfo) IsDir() bool        { return true }
func (*dataRangerDirInfo) Sys() any           { return nil }

type dataRangerEntry struct{ info *dataRangerFileInfo }

func (e *dataRangerEntry) Name() string               { return e.info.name }
func (*dataRangerEntry) IsDir() bool                  { return false }
func (*dataRangerEntry) Type() fs.FileMode            { return 0 }
func (e *dataRangerEntry) Info() (fs.FileInfo, error) { return e.info, nil }

type dataRangerFile struct {
	info   *dataRangerFileInfo
	offset int64
}

func (f *dataRangerFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *dataRangerFile) Read(p []byte) (int, error) {
	if f.offset >= f.info.size {
		return 0, io.EOF
	}

	var n int
	if f.info.content != nil {
		// Serve actual content so leak-detection tests can catch accidental reads.
		n = copy(p, f.info.content[f.offset:])
	} else {
		// No content supplied: return zeros up to size.
		end := min(int64(len(p)), f.info.size-f.offset)
		for i := range p[:end] {
			p[i] = 0
		}

		n = int(end)
	}

	f.offset += int64(n)

	return n, nil
}
func (*dataRangerFile) Close() error { return nil }

// TestCopyFromDataRange verifies that CopyFrom(MetadataOnly) uses the
// DataRange() accessor from a source FileInfo to synthesise chunk indexes
// in the output EROFS image, and that the resulting image is valid.
//
// It also verifies that chunksFromRanges rejects invalid inputs (negative
// offsets, non-positive sizes, unaligned offsets).
func TestCopyFromDataRange(t *testing.T) {
	t.Parallel()

	const blockSize = 4096

	t.Run("single contiguous range", func(t *testing.T) {
		t.Parallel()

		// One file, two blocks starting at physical block 10 on device 0.
		src := &dataRangerFS{
			deviceBlocks: 1024,
			file: &dataRangerFileInfo{
				name: "data.bin",
				size: blockSize * 2,
				ranges: []erofs.DataRange{
					{Device: 0, Offset: blockSize * 10, Size: blockSize * 2},
				},
			},
		}

		var buf testBuffer

		w := erofs.Create(&buf)
		if err := w.CopyFrom(src, erofs.MetadataOnly()); err != nil {
			t.Fatal("CopyFrom:", err)
		}

		if err := w.Close(); err != nil {
			t.Fatal("Close:", err)
		}

		erofstest.FsckErofsBytes(t, buf.Bytes())

		// Open the output image and verify DataRange() on the file.
		dstFS, err := erofs.Open(bytes.NewReader(buf.Bytes()),
			erofs.WithExtraDevices(bytes.NewReader(make([]byte, 1024*blockSize))))
		if err != nil {
			t.Fatal("Open:", err)
		}

		info := statDataRange(t, dstFS, "data.bin")
		if len(info) == 0 {
			t.Fatal("DataRange() returned nil for chunk-based file")
		}
		// Device 0 on the source becomes DeviceID=1 in EROFS, which is
		// what buildChunkDataRanges stores directly into DataRange.Device.
		got := info[0]
		if got.Device != 1 {
			t.Errorf("Device = %d, want 1", got.Device)
		}

		if got.Offset != blockSize*10 {
			t.Errorf("Offset = %d, want %d", got.Offset, blockSize*10)
		}

		if got.Size != blockSize*2 {
			t.Errorf("Size = %d, want %d", got.Size, blockSize*2)
		}
	})

	t.Run("multiple ranges on one device", func(t *testing.T) {
		t.Parallel()

		// Non-contiguous ranges: block 5 (1 block) and block 20 (3 blocks).
		src := &dataRangerFS{
			deviceBlocks: 1024,
			file: &dataRangerFileInfo{
				name: "sparse.bin",
				size: blockSize * 4, // 4 logical blocks total
				ranges: []erofs.DataRange{
					{Device: 0, Offset: blockSize * 5, Size: blockSize * 1},
					{Device: 0, Offset: blockSize * 20, Size: blockSize * 3},
				},
			},
		}

		var buf testBuffer

		w := erofs.Create(&buf)
		if err := w.CopyFrom(src, erofs.MetadataOnly()); err != nil {
			t.Fatal("CopyFrom:", err)
		}

		if err := w.Close(); err != nil {
			t.Fatal("Close:", err)
		}

		erofstest.FsckErofsBytes(t, buf.Bytes())

		dstFS, err := erofs.Open(bytes.NewReader(buf.Bytes()),
			erofs.WithExtraDevices(bytes.NewReader(make([]byte, 1024*blockSize))))
		if err != nil {
			t.Fatal("Open:", err)
		}

		ranges := statDataRange(t, dstFS, "sparse.bin")
		if len(ranges) != 2 {
			t.Fatalf("DataRange() len = %d, want 2; ranges = %v", len(ranges), ranges)
		}

		if ranges[0].Device != 1 || ranges[0].Offset != blockSize*5 || ranges[0].Size != blockSize {
			t.Errorf("ranges[0] = %+v, want {Device:1 Offset:%d Size:%d}", ranges[0], blockSize*5, blockSize)
		}

		if ranges[1].Device != 1 || ranges[1].Offset != blockSize*20 || ranges[1].Size != blockSize*3 {
			t.Errorf("ranges[1] = %+v, want {Device:1 Offset:%d Size:%d}", ranges[1], blockSize*20, blockSize*3)
		}
	})

	t.Run("no data written for metadata-only", func(t *testing.T) {
		t.Parallel()

		// CopyFrom(MetadataOnly) must not copy any file data into the image.
		// Read() returns the marker so that if CopyFrom accidentally opens
		// and reads the file the marker will appear in the output image.
		marker := bytes.Repeat([]byte("DATA_LEAK_CHECK"), 100)
		src := &dataRangerFS{
			deviceBlocks: 256,
			file: &dataRangerFileInfo{
				name:    "file.bin",
				size:    int64(len(marker)),
				content: marker,
				ranges:  []erofs.DataRange{{Device: 0, Offset: 0, Size: int64(len(marker))}},
			},
		}

		var buf testBuffer

		w := erofs.Create(&buf)
		if err := w.CopyFrom(src, erofs.MetadataOnly()); err != nil {
			t.Fatal("CopyFrom:", err)
		}

		if err := w.Close(); err != nil {
			t.Fatal("Close:", err)
		}

		if bytes.Contains(buf.Bytes(), marker) {
			t.Error("metadata-only image contains file data — leak detected")
		}
	})

	t.Run("sparse_hole_round_trip", func(t *testing.T) {
		t.Parallel()

		// A full-coverage slice with a hole entry should round-trip correctly.
		// File layout: 1 data block at physical block 7, then 3-block hole.
		// After CopyFrom(MetadataOnly) the output image should be valid (fsck),
		// and reading the file back must yield:
		//   - DataRange with Device=1, Offset=blockSize*7, Size=blockSize  (data)
		//   - DataRange with Offset==-1, Size=blockSize*3                  (hole)
		const blockSize = 4096

		src := &dataRangerFS{
			deviceBlocks: 1024,
			file: &dataRangerFileInfo{
				name: "sparse.bin",
				size: blockSize * 4,
				ranges: []erofs.DataRange{
					{Device: 0, Offset: blockSize * 7, Size: blockSize},
					{Offset: -1, Size: blockSize * 3},
				},
			},
		}

		var buf testBuffer

		w := erofs.Create(&buf)
		if err := w.CopyFrom(src, erofs.MetadataOnly()); err != nil {
			t.Fatal("CopyFrom:", err)
		}

		if err := w.Close(); err != nil {
			t.Fatal("Close:", err)
		}

		erofstest.FsckErofsBytes(t, buf.Bytes())

		dstFS, err := erofs.Open(bytes.NewReader(buf.Bytes()),
			erofs.WithExtraDevices(bytes.NewReader(make([]byte, 1024*blockSize))))
		if err != nil {
			t.Fatal("Open:", err)
		}

		ranges := statDataRange(t, dstFS, "sparse.bin")

		// Verify total coverage equals file size.
		var total int64
		for _, r := range ranges {
			total += r.Size
		}

		if total != blockSize*4 {
			t.Fatalf("total DataRange size = %d, want %d; ranges = %+v", total, blockSize*4, ranges)
		}

		// Find the data range and verify it points to physical block 7 on device 1.
		var dataRanges, holeRanges []erofs.DataRange

		for _, r := range ranges {
			if r.Offset == -1 {
				holeRanges = append(holeRanges, r)
			} else {
				dataRanges = append(dataRanges, r)
			}
		}

		if len(dataRanges) != 1 {
			t.Fatalf("want 1 data range, got %d; ranges = %+v", len(dataRanges), ranges)
		}

		dr := dataRanges[0]
		if dr.Device != 1 || dr.Offset != blockSize*7 || dr.Size != blockSize {
			t.Errorf("data range = %+v, want {Device:1 Offset:%d Size:%d}", dr, blockSize*7, blockSize)
		}
		// Hole ranges should cover the remaining 3 blocks.
		var holeTotal int64
		for _, r := range holeRanges {
			holeTotal += r.Size
		}

		if holeTotal != blockSize*3 {
			t.Errorf("hole total = %d, want %d; holeRanges = %+v", holeTotal, blockSize*3, holeRanges)
		}
	})
}

// TestChunksFromRangesValidation verifies that chunksFromRanges rejects
// invalid DataRange inputs instead of silently producing corrupt chunk entries.
func TestChunksFromRangesValidation(t *testing.T) {
	t.Parallel()

	// Use a non-EROFS source that exercises the chunksFromRanges code path.
	// CopyFrom(MetadataOnly) on a DataRange-implementing source calls it.
	tryRanges := func(t *testing.T, ranges []erofs.DataRange) error {
		t.Helper()

		src := &dataRangerFS{
			deviceBlocks: 1024,
			file: &dataRangerFileInfo{
				name:   "f.bin",
				size:   4096 * 4,
				ranges: ranges,
			},
		}

		var buf testBuffer

		w := erofs.Create(&buf)

		err := w.CopyFrom(src, erofs.MetadataOnly())
		if err == nil {
			_ = w.Close()
		}

		return err
	}

	t.Run("negative offset", func(t *testing.T) {
		t.Parallel()

		err := tryRanges(t, []erofs.DataRange{{Device: 0, Offset: -4096, Size: 4096}})
		if err == nil {
			t.Fatal("expected error for negative Offset, got nil")
		}
	})

	t.Run("zero size", func(t *testing.T) {
		t.Parallel()

		err := tryRanges(t, []erofs.DataRange{{Device: 0, Offset: 0, Size: 0}})
		if err == nil {
			t.Fatal("expected error for zero Size, got nil")
		}
	})

	t.Run("negative size", func(t *testing.T) {
		t.Parallel()

		err := tryRanges(t, []erofs.DataRange{{Device: 0, Offset: 0, Size: -1}})
		if err == nil {
			t.Fatal("expected error for negative Size, got nil")
		}
	})

	t.Run("unaligned offset", func(t *testing.T) {
		t.Parallel()

		err := tryRanges(t, []erofs.DataRange{{Device: 0, Offset: 100, Size: 4096}})
		if err == nil {
			t.Fatal("expected error for unaligned Offset, got nil")
		}
	})

	t.Run("device out of range", func(t *testing.T) {
		t.Parallel()

		err := tryRanges(t, []erofs.DataRange{{Device: 1, Offset: 4096, Size: 4096}})
		if err == nil {
			t.Fatal("expected error for Device!=0, got nil")
		}
	})

	t.Run("device wrap overflow", func(t *testing.T) {
		t.Parallel()

		err := tryRanges(t, []erofs.DataRange{{Device: 0xFFFF, Offset: 4096, Size: 4096}})
		if err == nil {
			t.Fatal("expected error for Device=0xFFFF (wraps to DeviceID=0), got nil")
		}
	})

	t.Run("total_size_mismatch_under", func(t *testing.T) {
		t.Parallel()

		// Slice total (4096) < file size (16384).
		err := tryRanges(t, []erofs.DataRange{{Device: 0, Offset: 0, Size: 4096}})
		if err == nil {
			t.Fatal("expected error for under-coverage, got nil")
		}
	})

	t.Run("total_size_mismatch_over", func(t *testing.T) {
		t.Parallel()

		// Slice total (4096*5) > file size (16384).
		err := tryRanges(t, []erofs.DataRange{{Device: 0, Offset: 0, Size: 4096 * 5}})
		if err == nil {
			t.Fatal("expected error for over-coverage, got nil")
		}
	})

	t.Run("hole_entry_accepted", func(t *testing.T) {
		t.Parallel()

		// Full-coverage slice with a hole entry — holes are now supported.
		// 1 data block + 3-block hole = 4 blocks == file size (16384).
		err := tryRanges(t, []erofs.DataRange{
			{Device: 0, Offset: 0, Size: 4096},
			{Offset: -1, Size: 4096 * 3},
		})
		if err != nil {
			t.Fatalf("unexpected error for valid hole entry: %v", err)
		}
	})

	t.Run("non_final_size_unaligned", func(t *testing.T) {
		t.Parallel()

		// Middle range has non-block-aligned Size (100 bytes).
		// total = 100 + (16384-100) = 16384 so coverage is satisfied.
		err := tryRanges(t, []erofs.DataRange{
			{Device: 0, Offset: 0, Size: 100},
			{Device: 0, Offset: 4096, Size: 4096*4 - 100},
		})
		if err == nil {
			t.Fatal("expected error for non-final unaligned Size, got nil")
		}
	})

	t.Run("final_size_partial_ok", func(t *testing.T) {
		t.Parallel()

		// Final range has a partial last block (file size is 16384 = 4*4096 exactly,
		// but here we use a 3.5-block file to get a partial tail).
		// Override file size via a separate dataRangerFS with size 4096*3+512.
		partialSize := int64(4096*3 + 512)
		src := &dataRangerFS{
			deviceBlocks: 1024,
			file: &dataRangerFileInfo{
				name: "f.bin",
				size: partialSize,
				ranges: []erofs.DataRange{
					{Device: 0, Offset: 0, Size: 4096 * 3},
					{Device: 0, Offset: 4096 * 3, Size: 512},
				},
			},
		}

		var buf testBuffer

		w := erofs.Create(&buf)

		err := w.CopyFrom(src, erofs.MetadataOnly())
		if err == nil {
			_ = w.Close()
		}

		if err != nil {
			t.Fatalf("unexpected error for valid partial final range: %v", err)
		}
	})

	t.Run("valid range passes", func(t *testing.T) {
		t.Parallel()

		err := tryRanges(t, []erofs.DataRange{{Device: 0, Offset: 4096, Size: 4096 * 4}})
		if err != nil {
			t.Fatalf("unexpected error for valid range: %v", err)
		}
	})
}

// statDataRange opens the named file in fsys via Stat() and returns its
// DataRange() if the FileInfo supports it.
func statDataRange(t *testing.T, fsys fs.FS, name string) []erofs.DataRange {
	t.Helper()

	f, err := fsys.Open(name)
	if err != nil {
		t.Fatalf("Open(%s): %v", name, err)
	}

	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat(%s): %v", name, err)
	}

	type dataRanger interface {
		DataRange() []erofs.DataRange
	}

	dr, ok := info.(dataRanger)
	if !ok {
		t.Fatalf("FileInfo for %s does not implement DataRange()", name)
	}

	return dr.DataRange()
}
