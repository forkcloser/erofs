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
	"github.com/forkcloser/erofs/internal/builder"
	"github.com/forkcloser/erofs/internal/disk"
	"github.com/forkcloser/erofs/internal/erofstest"
)

// TestCopyFromPreservesModTime covers a source with no platform stat: the
// modification time then has to come from fs.FileInfo.ModTime, which is the
// only place a generic fs.FS — embed.FS, fstest.MapFS, os.DirFS on a platform
// without a recognised Stat_t — can carry it. Every entry used to read back
// as the epoch.
func TestCopyFromPreservesModTime(t *testing.T) {
	t.Parallel()

	when := time.Date(2024, time.March, 4, 5, 6, 7, 890, time.UTC)
	src := fstest.MapFS{
		"dir":   &fstest.MapFile{Mode: fs.ModeDir | 0o755, ModTime: when},
		"dir/f": &fstest.MapFile{Data: []byte("x"), Mode: 0o644, ModTime: when.Add(time.Hour)},
		"link": &fstest.MapFile{
			Data:    []byte("dir/f"),
			Mode:    fs.ModeSymlink | 0o777,
			ModTime: when.Add(2 * time.Hour),
		},
		"untimed": &fstest.MapFile{Data: []byte("y"), Mode: 0o644},
		"withsys": &fstest.MapFile{Data: []byte("z"), Mode: 0o644, ModTime: when, Sys: &builder.Entry{UID: 7}},
		"stampedsys": &fstest.MapFile{
			Data:    []byte("w"),
			Mode:    0o644,
			ModTime: when,
			Sys:     &builder.Entry{Mtime: 1234, MtimeNs: 5},
		},
	}

	var buf testBuffer

	w := erofs.Create(&buf, erofs.WithBuildTime(99, 0))
	if err := w.CopyFrom(src); err != nil {
		t.Fatal("CopyFrom:", err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	lstat := func(name string) time.Time {
		t.Helper()

		fi, err := efs.(interface {
			Lstat(name string) (fs.FileInfo, error)
		}).Lstat(name)
		if err != nil {
			t.Fatalf("lstat %s: %v", name, err)
		}

		return fi.ModTime()
	}
	for name, want := range map[string]time.Time{
		"dir":        when,
		"dir/f":      when.Add(time.Hour),
		"link":       when.Add(2 * time.Hour),
		"withsys":    when, // Sys() carried ownership but no time
		"stampedsys": time.Unix(1234, 5),
	} {
		if got := lstat(name); !got.Equal(want) {
			t.Errorf("%s: mtime %v, want %v", name, got, want)
		}
	}
	// A zero ModTime is not a time; it must not be forced onto the entry.
	if got := lstat("untimed"); !got.Equal(time.Unix(0, 0)) {
		t.Errorf("untimed: mtime %v, want the epoch", got)
	}
}

// TestMknodMode covers the fs.FileMode contract of Mknod: the four special
// types are accepted, everything else is refused.
func TestMknodMode(t *testing.T) {
	t.Parallel()

	var buf testBuffer

	w := erofs.Create(&buf)
	for name, mode := range map[string]fs.FileMode{
		"/chr":  fs.ModeDevice | fs.ModeCharDevice | 0o666,
		"/blk":  fs.ModeDevice | 0o660,
		"/fifo": fs.ModeNamedPipe | 0o644,
		"/sock": fs.ModeSocket | 0o755,
	} {
		if err := w.Mknod(name, mode, 1<<8|3); err != nil {
			t.Fatalf("Mknod(%s, %v): %v", name, mode, err)
		}
	}

	for name, mode := range map[string]fs.FileMode{
		"/reg": 0o644,
		"/dir": fs.ModeDir | 0o755,
		"/sym": fs.ModeSymlink | 0o777,
	} {
		if err := w.Mknod(name, mode, 0); !errors.Is(err, erofs.ErrInvalid) {
			t.Errorf("Mknod(%s, %v) = %v, want ErrInvalid", name, mode, err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())

	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	erofstest.CheckDevice(t, efs, "chr", fs.ModeDevice|fs.ModeCharDevice, 1<<8|3)
	erofstest.CheckDevice(t, efs, "blk", fs.ModeDevice, 1<<8|3)
	erofstest.CheckMode(t, efs, "fifo", fs.ModeNamedPipe|0o644)
	erofstest.CheckMode(t, efs, "sock", fs.ModeSocket|0o755)

	if st := erofstest.Stat(t, efs, "fifo"); st.Rdev != 0 {
		t.Errorf("fifo rdev = %d, want 0", st.Rdev)
	}

	erofstest.CheckNotExists(t, efs, "reg")
}

// TestChownRange covers the uid/gid bounds on both Chown methods.
func TestChownRange(t *testing.T) {
	t.Parallel()

	var buf testBuffer

	w := erofs.Create(&buf)

	f, err := w.Create("/f")
	if err != nil {
		t.Fatal(err)
	}

	if err = f.Chown(-1, 0); !errors.Is(err, erofs.ErrInvalid) {
		t.Errorf("File.Chown(-1, 0) = %v, want ErrInvalid", err)
	}

	if err = f.Chown(1, 2); err != nil {
		t.Errorf("File.Chown(1, 2) = %v", err)
	}

	if err = f.Close(); err != nil {
		t.Fatal(err)
	}

	if err = w.Chown("/f", 0, -1); !errors.Is(err, erofs.ErrInvalid) {
		t.Errorf("Writer.Chown(0, -1) = %v, want ErrInvalid", err)
	}

	if err = w.Chown("/f", 1<<32, 0); !errors.Is(err, erofs.ErrInvalid) {
		t.Errorf("Writer.Chown(1<<32, 0) = %v, want ErrInvalid", err)
	}

	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	if st := erofstest.Stat(t, efs, "f"); st.UID != 1 || st.GID != 2 {
		t.Errorf("uid:gid after rejected chowns = %d:%d, want 1:2", st.UID, st.GID)
	}
}

// TestCopyFromRejectsWideRdev covers a source handing over a *erofs.Stat
// whose device number does not fit the 32-bit i_rdev. It used to be
// truncated, so the copy named some other device.
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
