package erofs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	erofs "github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/disk"
	"github.com/forkcloser/erofs/internal/erofstest"
)

func TestCopyFromRejectsWideRdev(t *testing.T) {
	t.Parallel()

	src := fstest.MapFS{
		"dev": &fstest.MapFile{Mode: fs.ModeDevice | fs.ModeCharDevice | 0o600, Sys: &erofs.Stat{Rdev: 1<<32 | 5}},
	}

	var buf testBuffer

	w := erofs.Create(&buf)

	err := w.CopyFrom(src)
	if err == nil {
		err = w.Close()
	}

	if !errors.Is(err, erofs.ErrInvalid) {
		t.Errorf("copying an rdev of 1<<32|5 = %v, want ErrInvalid", err)
	}
}

// hugeFileFS holds one regular file, "huge", declaring size bytes with nothing
// behind them: enough to reach the writer's layout without the bytes.
type hugeFileFS struct{ size int64 }

func (h hugeFileFS) Open(name string) (fs.File, error) {
	switch name {
	case ".":
		return hugeFile{hugeFileInfo{name: ".", mode: fs.ModeDir | 0o755}}, nil
	case "huge":
		return hugeFile{h.file()}, nil
	}

	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (h hugeFileFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}

	return []fs.DirEntry{fs.FileInfoToDirEntry(h.file())}, nil
}

func (h hugeFileFS) file() hugeFileInfo { return hugeFileInfo{name: "huge", size: h.size, mode: 0o644} }

type hugeFile struct{ info hugeFileInfo }

func (f hugeFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (hugeFile) Read([]byte) (int, error)     { return 0, io.EOF }
func (hugeFile) Close() error                 { return nil }

type hugeFileInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (i hugeFileInfo) Name() string      { return i.name }
func (i hugeFileInfo) Size() int64       { return i.size }
func (i hugeFileInfo) Mode() fs.FileMode { return i.mode }
func (hugeFileInfo) ModTime() time.Time  { return time.Unix(0, 0) }
func (i hugeFileInfo) IsDir() bool       { return i.mode.IsDir() }
func (hugeFileInfo) Sys() any            { return nil }

// TestCopyFromRejectsImagePast32BitBlocks covers flat file data adding up to
// more blocks than the 32-bit block addresses can name. The addresses used to
// wrap: MetaBlkAddr named a block inside the file's own data, and the image's
// block count came out small. Close now refuses before writing anything.
func TestCopyFromRejectsImagePast32BitBlocks(t *testing.T) {
	t.Parallel()

	var buf testBuffer

	w := erofs.Create(&buf, erofs.WithBlockSize(4096))

	// 2^32 blocks of 4 KiB, with the superblock area in front: the block
	// after the data, where the metadata goes, is past 2^32 - 1.
	err := w.CopyFrom(hugeFileFS{size: 1 << 44})
	if err == nil {
		err = w.Close()
	}

	if !errors.Is(err, erofs.ErrInvalid) {
		t.Errorf("packing a %d byte file at a 4 KiB block size = %v, want ErrInvalid", int64(1<<44), err)
	}

	if len(buf.Bytes()) != 0 {
		t.Errorf("%d bytes written before the refusal, want none", len(buf.Bytes()))
	}
}

// TestCopyFromImageRejectsSizePastInt64 covers a metadata-only copy of an image
// whose regular file declares an i_size past 1<<63. The reader refuses that
// size; the copy took it, and the Writer then reported a negative Size.
func TestCopyFromImageRejectsSizePastInt64(t *testing.T) {
	t.Parallel()

	var buf testBuffer

	w := erofs.Create(&buf, erofs.WithBuildTime(1000, 0))

	f, err := w.Create("/f")
	if err != nil {
		t.Fatal(err)
	}

	if _, err = f.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}

	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	// An mtime off the build time gets /f an extended inode, whose i_size is
	// 64 bits wide.
	if err = w.Chtimes("/f", time.Unix(2000, 0), time.Unix(2000, 0)); err != nil {
		t.Fatal(err)
	}

	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	image := append([]byte(nil), buf.Bytes()...)

	src, err := erofs.Open(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}

	nid := erofstest.Stat(t, src, "f").Ino
	superBlock := image[disk.SuperBlockOffset:]
	metaStart := int64(binary.LittleEndian.Uint32(superBlock[40:44])) << superBlock[12]
	inode := image[metaStart+int64(nid)*disk.SizeInodeCompact:]

	if inode[0]&1 == 0 {
		t.Fatal("/f got a compact inode; the test needs an extended one")
	}

	binary.LittleEndian.PutUint64(inode[8:16], 1<<63)

	if src, err = erofs.Open(bytes.NewReader(image)); err != nil {
		t.Fatal(err)
	}

	dst := erofs.Create(&testBuffer{})

	err = dst.CopyFrom(src, erofs.MetadataOnly())
	if err == nil {
		fi, statErr := dst.Stat("/f")
		if statErr == nil {
			t.Fatalf("CopyFrom accepted an i_size of 1<<63; the Writer reports Size %d", fi.Size())
		}

		t.Fatalf("CopyFrom accepted an i_size of 1<<63 (Stat: %v)", statErr)
	}

	if !errors.Is(err, erofs.ErrInvalid) {
		t.Errorf("CopyFrom = %v, want ErrInvalid", err)
	}
}
