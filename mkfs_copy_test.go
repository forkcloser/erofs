package erofs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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

// buildHardlinkSourceImage builds an image in which /original and /hardlink
// are two names for one inode, and /other is unrelated.
func buildHardlinkSourceImage(t *testing.T) []byte {
	t.Helper()

	var buf testBuffer

	w := erofs.Create(&buf)
	for name, content := range map[string]string{"/original": "shared content", "/other": "unrelated"} {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}

		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}

	if err := w.Link("/original", "/hardlink"); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return append([]byte(nil), buf.Bytes()...)
}

// checkHardlinkPair asserts that a and b name one inode with two links and
// that other is a distinct inode with one.
func checkHardlinkPair(t *testing.T, efs fs.FS, a, b, other string) {
	t.Helper()

	aSt, bSt, oSt := erofstest.Stat(t, efs, a), erofstest.Stat(t, efs, b), erofstest.Stat(t, efs, other)
	if aSt.Ino != bSt.Ino {
		t.Errorf("%s ino %d != %s ino %d: want one inode", a, aSt.Ino, b, bSt.Ino)
	}

	if aSt.Nlink != 2 || bSt.Nlink != 2 {
		t.Errorf("nlink %s=%d %s=%d, want 2 and 2", a, aSt.Nlink, b, bSt.Nlink)
	}

	if oSt.Ino == aSt.Ino {
		t.Errorf("%s shares ino %d with %s", other, oSt.Ino, a)
	}

	if oSt.Nlink != 1 {
		t.Errorf("%s nlink = %d, want 1", other, oSt.Nlink)
	}
}

// TestCopyFromHostHardlinks copies a host directory holding a hard link:
// the two names must share one inode in the image, as they do on the host.
func TestCopyFromHostHardlinks(t *testing.T) {
	t.Parallel()

	switch runtime.GOOS {
	case "linux", "darwin", "freebsd":
	default:
		t.Skipf("no syscall.Stat_t support on %s", runtime.GOOS)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "original"), []byte("shared content"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Link(filepath.Join(dir, "original"), filepath.Join(dir, "hardlink")); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "other"), []byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf testBuffer

	w := erofs.Create(&buf)
	if err := w.CopyFrom(os.DirFS(dir)); err != nil {
		t.Fatal("CopyFrom:", err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())

	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	erofstest.CheckFile(t, efs, "original", "shared content")
	erofstest.CheckFile(t, efs, "hardlink", "shared content")
	erofstest.CheckFile(t, efs, "other", "unrelated")
	checkHardlinkPair(t, efs, "original", "hardlink", "other")
}

// TestCopyFromImageHardlinks copies an image with a hard link through both
// routes — the fs.WalkDir path with data, and the metadata-only fast path —
// and expects one inode with two names either way.
func TestCopyFromImageHardlinks(t *testing.T) {
	t.Parallel()

	srcFS, err := erofs.Open(bytes.NewReader(buildHardlinkSourceImage(t)))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("full", func(t *testing.T) {
		t.Parallel()

		var buf testBuffer

		w := erofs.Create(&buf)
		if err := w.CopyFrom(srcFS); err != nil {
			t.Fatal("CopyFrom:", err)
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		erofstest.FsckErofsBytes(t, buf.Bytes())

		efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}

		erofstest.CheckFile(t, efs, "original", "shared content")
		erofstest.CheckFile(t, efs, "hardlink", "shared content")
		checkHardlinkPair(t, efs, "original", "hardlink", "other")
	})

	t.Run("metadataOnly", func(t *testing.T) {
		t.Parallel()

		dataFile, err := os.Create(filepath.Join(t.TempDir(), "data.bin"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = dataFile.Close() }()

		var buf testBuffer

		w := erofs.Create(&buf, erofs.WithDataFile(dataFile))
		if err = w.CopyFrom(srcFS, erofs.MetadataOnly()); err != nil {
			t.Fatal("CopyFrom:", err)
		}

		if err = w.Close(); err != nil {
			t.Fatal(err)
		}

		efs, err := erofs.Open(bytes.NewReader(buf.Bytes()), erofs.WithExtraDevices(dataFile))
		if err != nil {
			t.Fatal(err)
		}

		checkHardlinkPair(t, efs, "original", "hardlink", "other")
	})
}

// TestCopyFromHardlinkScope checks that inode identity is scoped to one
// CopyFrom call: a second source whose nids happen to coincide must not be
// linked to the first source's files.
func TestCopyFromHardlinkScope(t *testing.T) {
	t.Parallel()

	srcA, err := erofs.Open(bytes.NewReader(buildHardlinkSourceImage(t)))
	if err != nil {
		t.Fatal(err)
	}
	// Same shape as srcA — its nids coincide — but different names.
	var (
		bbuf testBuffer
		f    *erofs.File
	)

	wb := erofs.Create(&bbuf)
	for name, content := range map[string]string{"/b-original": "distinct b content", "/b-other": "b unrelated"} {
		f, err = wb.Create(name)
		if err != nil {
			t.Fatal(err)
		}

		if _, err = f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}

		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
	}

	if err = wb.Link("/b-original", "/b-hardlink"); err != nil {
		t.Fatal(err)
	}

	if err = wb.Close(); err != nil {
		t.Fatal(err)
	}

	srcB, err := erofs.Open(bytes.NewReader(bbuf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	var buf testBuffer

	w := erofs.Create(&buf)
	if err = w.CopyFrom(srcA); err != nil {
		t.Fatal("CopyFrom A:", err)
	}

	if err = w.CopyFrom(srcB); err != nil {
		t.Fatal("CopyFrom B:", err)
	}

	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	erofstest.CheckFile(t, efs, "original", "shared content")
	erofstest.CheckFile(t, efs, "b-original", "distinct b content")
	erofstest.CheckFile(t, efs, "b-hardlink", "distinct b content")
	checkHardlinkPair(t, efs, "original", "hardlink", "other")
	checkHardlinkPair(t, efs, "b-original", "b-hardlink", "b-other")

	if a, b := erofstest.Stat(t, efs, "original"), erofstest.Stat(t, efs, "b-original"); a.Ino == b.Ino {
		t.Errorf("original and b-original share ino %d across CopyFrom calls", a.Ino)
	}
}

// TestCopyFromHardlinkIdentity pins the identity rule: (Dev, Ino) with
// Nlink > 1 links; a matching Ino on another device, an unknown Ino, or a
// link count of 1 does not — and a source's link count is never copied onto
// a file whose other names the image does not hold.
func TestCopyFromHardlinkIdentity(t *testing.T) {
	t.Parallel()

	src := fstest.MapFS{
		"a": &fstest.MapFile{Data: []byte("dev1"), Mode: 0o644, Sys: &builder.Entry{Nlink: 2, Dev: 1, Ino: 100}},
		"a2": &fstest.MapFile{
			Data: []byte("ignored: same inode as a"),
			Mode: 0o644,
			Sys:  &builder.Entry{Nlink: 2, Dev: 1, Ino: 100},
		},
		"b":       &fstest.MapFile{Data: []byte("dev2"), Mode: 0o644, Sys: &builder.Entry{Nlink: 2, Dev: 2, Ino: 100}},
		"noino":   &fstest.MapFile{Data: []byte("n1"), Mode: 0o644, Sys: &builder.Entry{Nlink: 3}},
		"noino2":  &fstest.MapFile{Data: []byte("n2"), Mode: 0o644, Sys: &builder.Entry{Nlink: 3}},
		"single":  &fstest.MapFile{Data: []byte("s1"), Mode: 0o644, Sys: &builder.Entry{Nlink: 1, Dev: 1, Ino: 200}},
		"single2": &fstest.MapFile{Data: []byte("s2"), Mode: 0o644, Sys: &builder.Entry{Nlink: 1, Dev: 1, Ino: 200}},
		"partial": &fstest.MapFile{Data: []byte("p"), Mode: 0o644, Sys: &builder.Entry{Nlink: 5, Dev: 1, Ino: 300}},
	}

	var buf testBuffer

	w := erofs.Create(&buf)
	if err := w.CopyFrom(src); err != nil {
		t.Fatal("CopyFrom:", err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())

	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	erofstest.CheckFile(t, efs, "a", "dev1")
	erofstest.CheckFile(t, efs, "a2", "dev1")
	erofstest.CheckFile(t, efs, "b", "dev2")
	checkHardlinkPair(t, efs, "a", "a2", "b")

	for _, pair := range [][2]string{{"noino", "noino2"}, {"single", "single2"}} {
		x, y := erofstest.Stat(t, efs, pair[0]), erofstest.Stat(t, efs, pair[1])
		if x.Ino == y.Ino {
			t.Errorf("%s and %s share ino %d, want distinct inodes", pair[0], pair[1], x.Ino)
		}

		if x.Nlink != 1 || y.Nlink != 1 {
			t.Errorf(
				"%s/%s nlink %d/%d, want 1: a count is computed from names, not copied",
				pair[0],
				pair[1],
				x.Nlink,
				y.Nlink,
			)
		}
	}

	if st := erofstest.Stat(t, efs, "partial"); st.Nlink != 1 {
		t.Errorf("partial nlink = %d, want 1: the source's other four names are not in the image", st.Nlink)
	}
}

// TestCopyFromHardlinkOverwrite covers a later layer replacing one name of
// a hard-linked pair with a plain file: the survivor keeps the inode with a
// link count of 1, and the new file is its own inode.
func TestCopyFromHardlinkOverwrite(t *testing.T) {
	t.Parallel()

	srcA, err := erofs.Open(bytes.NewReader(buildHardlinkSourceImage(t)))
	if err != nil {
		t.Fatal(err)
	}

	over := fstest.MapFS{
		"hardlink": &fstest.MapFile{Data: []byte("replaced"), Mode: 0o644},
	}

	var buf testBuffer

	w := erofs.Create(&buf)
	if err = w.CopyFrom(srcA); err != nil {
		t.Fatal(err)
	}

	if err = w.CopyFrom(over, erofs.Merge()); err != nil {
		t.Fatal(err)
	}

	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())

	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	erofstest.CheckFile(t, efs, "original", "shared content")
	erofstest.CheckFile(t, efs, "hardlink", "replaced")

	o, h := erofstest.Stat(t, efs, "original"), erofstest.Stat(t, efs, "hardlink")
	if o.Nlink != 1 || h.Nlink != 1 || o.Ino == h.Ino {
		t.Errorf("original ino %d nlink %d, hardlink ino %d nlink %d: want two inodes with one name each",
			o.Ino, o.Nlink, h.Ino, h.Nlink)
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
