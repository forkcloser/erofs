package erofs_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	erofs "github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/builder"
)

// TestChunkBasedXattrAlignment guards against the chunk-index map being written
// unaligned after the xattr area on a chunk-based (external-device) inode.
//
// The kernel and this package's reader locate the chunk-index map at
// ALIGN(inode_isize + xattr_isize, sizeof(chunk_index)). inode_isize is a
// multiple of 8, so a xattr area sized 4 (mod 8) pushes the map off alignment.
// The "user.t" = "abc" attribute makes xattr_isize == 20, so inode_isize +
// xattr_isize is 4 (mod 8) for both compact (32) and extended (64) inodes;
// before the fix the chunk index was written 4 bytes early and the file
// resolved to the wrong device block.
func TestChunkBasedXattrAlignment(t *testing.T) {
	dataPath := filepath.Join(t.TempDir(), "data.bin")
	df, err := os.Create(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = df.Close() }()

	var metaBuf testBuffer
	fsys := erofs.Create(&metaBuf, erofs.WithDataFile(df))

	f, err := fsys.Create("/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0xAB}, 4096) // exactly one chunk
	if _, err := f.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Setxattr("/file.bin", "user.t", "abc"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	dfRead, err := os.Open(dataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dfRead.Close() }()

	efs, err := erofs.Open(bytes.NewReader(metaBuf.Bytes()), erofs.WithExtraDevices(dfRead))
	if err != nil {
		t.Fatal("Open:", err)
	}

	got, err := fs.ReadFile(efs, "file.bin")
	if err != nil {
		t.Fatal("ReadFile:", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("file data mismatch: chunk-index map misaligned after xattr area (got %d bytes, first=0x%02x)", len(got), firstByte(got))
	}

	// The xattr itself should round-trip.
	fi, err := fs.Stat(efs, "file.bin")
	if err != nil {
		t.Fatal("Stat:", err)
	}
	xg, ok := fi.(interface {
		GetXattr(string) (string, bool)
	})
	if !ok {
		t.Fatal("FileInfo does not expose GetXattr")
	}
	if v, ok := xg.GetXattr("user.t"); !ok || v != "abc" {
		t.Fatalf("user.t = %q (ok=%v), want %q", v, ok, "abc")
	}
}

func firstByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}

// chunkSourceFS is a minimal source fs.FS holding one 4096-byte regular file
// whose chunk map is supplied verbatim, so a test can express a chunk mapping
// the public API would otherwise refuse to build.
type chunkSourceFS struct {
	name   string
	chunks []builder.Chunk
}

type chunkSourceInfo struct {
	name   string
	dir    bool
	chunks []builder.Chunk
}

func (i chunkSourceInfo) Name() string { return i.name }
func (i chunkSourceInfo) Size() int64  { return 4096 }
func (i chunkSourceInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}

	return 0o644
}
func (i chunkSourceInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (i chunkSourceInfo) IsDir() bool        { return i.dir }
func (i chunkSourceInfo) Sys() any {
	if i.dir {
		return &builder.Entry{Nlink: 2}
	}

	return &builder.Entry{Chunks: i.chunks}
}

type chunkSourceDirent struct{ info chunkSourceInfo }

func (d chunkSourceDirent) Name() string               { return d.info.name }
func (d chunkSourceDirent) IsDir() bool                { return d.info.dir }
func (d chunkSourceDirent) Type() fs.FileMode          { return d.info.Mode().Type() }
func (d chunkSourceDirent) Info() (fs.FileInfo, error) { return d.info, nil }

type chunkSourceDir struct{ fsys chunkSourceFS }

func (chunkSourceDir) Close() error             { return nil }
func (chunkSourceDir) Read([]byte) (int, error) { return 0, fs.ErrInvalid }
func (chunkSourceDir) Stat() (fs.FileInfo, error) {
	return chunkSourceInfo{name: ".", dir: true}, nil
}
func (d chunkSourceDir) ReadDir(int) ([]fs.DirEntry, error) {
	return []fs.DirEntry{chunkSourceDirent{chunkSourceInfo{
		name:   d.fsys.name,
		chunks: d.fsys.chunks,
	}}}, nil
}

func (fsys chunkSourceFS) Open(name string) (fs.File, error) {
	if name == "." {
		return chunkSourceDir{fsys}, nil
	}

	return nil, fs.ErrNotExist
}

// TestMetadataOnlyChunkDeviceZero checks that a metadata-only copy rejects a
// source chunk naming DeviceID 0.
//
// DeviceID 0 is the source image's own primary device. CopyFrom registers only
// the source's extra devices in the destination, so there is nothing for that
// chunk to point at. The remap used to add copyDeviceID-1 to it like any other
// id, aliasing it onto the destination's own data file: the copied file then
// read back an unrelated file's bytes with no error anywhere along the way.
func TestMetadataOnlyChunkDeviceZero(t *testing.T) {
	dir := t.TempDir()

	// Source layer: a data file, so the source image declares a device
	// table, plus one file whose chunk index names device 0.
	srcDataPath := filepath.Join(dir, "srcdata.bin")
	if err := os.WriteFile(srcDataPath, bytes.Repeat([]byte{0x11}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	srcDF, err := os.OpenFile(srcDataPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	var srcBuf testBuffer
	sw := erofs.Create(&srcBuf, erofs.WithDataFile(srcDF))
	src := chunkSourceFS{
		name:   "target",
		chunks: []builder.Chunk{{PhysicalBlock: 0, Count: 1, DeviceID: 0}},
	}
	if err := sw.CopyFrom(src, erofs.MetadataOnly()); err != nil {
		t.Fatal(err)
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = srcDF.Close()

	srcDFR, err := os.Open(srcDataPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srcDFR.Close() }()
	srcImg, err := erofs.Open(bytes.NewReader(srcBuf.Bytes()), erofs.WithExtraDevices(srcDFR))
	if err != nil {
		t.Fatal(err)
	}

	// Destination: its own data file, holding another file's bytes at the
	// block the source chunk would alias onto.
	dstDF, err := os.Create(filepath.Join(dir, "dstdata.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dstDF.Close() }()
	var dstBuf testBuffer
	dw := erofs.Create(&dstBuf, erofs.WithDataFile(dstDF))
	other, err := dw.Create("/other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Write(bytes.Repeat([]byte{0xEE}, 4096)); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}

	err = dw.CopyFrom(srcImg, erofs.MetadataOnly())
	if err == nil {
		err = dw.Close()
	}
	if err == nil {
		t.Fatal("metadata-only copy accepted a chunk naming the source's own device")
	}
	if !errors.Is(err, erofs.ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid", err)
	}
	t.Logf("rejected: %v", err)
}
