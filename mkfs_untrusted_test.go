package erofs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"runtime"
	"testing"
	"time"

	"github.com/forkcloser/erofs/internal/disk"
)

// seekBuf is a minimal in-memory io.WriteSeeker + io.ReaderAt.
type seekBuf struct {
	buf []byte
	off int64
}

func (m *seekBuf) Write(p []byte) (int, error) {
	if need := int(m.off) + len(p); need > len(m.buf) {
		m.buf = append(m.buf, make([]byte, need-len(m.buf))...)
	}
	copy(m.buf[m.off:], p)
	m.off += int64(len(p))

	return len(p), nil
}

func (m *seekBuf) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.off = off
	case io.SeekCurrent:
		m.off += off
	case io.SeekEnd:
		m.off = int64(len(m.buf)) + off
	}

	return m.off, nil
}

// buildTamperableImage writes a small image holding one regular file and
// returns its bytes plus the nid of that file. The file's mtime differs from
// the build time so it gets a 64-bit extended inode, whose i_size field is
// wide enough to express the sizes these tests need.
func buildTamperableImage(t *testing.T) ([]byte, uint64) {
	t.Helper()

	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0))
	f, err := w.Create("/f")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Chtimes("/f", time.Unix(2000, 0), time.Unix(2000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	img, err := Open(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatal(err)
	}
	fi, err := fs.Stat(img, "f")
	if err != nil {
		t.Fatal(err)
	}
	nid := uint64(fi.Sys().(*Stat).Ino)

	inodeOff := img.(*image).metaStartPos() + int64(nid)*disk.SizeInodeCompact
	if format := binary.LittleEndian.Uint16(out.buf[inodeOff:]); format&0x01 == 0 {
		t.Fatalf("expected an extended inode for /f, got format %#x", format)
	}

	return out.buf, nid
}

// TestUntrustedChunkSizeIsBounded covers the case where an inode claims a
// petabyte-scale chunk-based file. The declared size feeds calcTrailingSize,
// which sizes the chunk-index map written into the in-memory metadata buffer;
// without a bound, CopyFrom grows that buffer until the process dies.
func TestUntrustedChunkSizeIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name  string
		idata uint32
	}{
		{"unindexed", 0},
		{"indexed", disk.LayoutChunkFormatIndexes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, nid := buildTamperableImage(t)

			img0, err := Open(bytes.NewReader(buf))
			if err != nil {
				t.Fatal(err)
			}
			inodeOff := img0.(*image).metaStartPos() + int64(nid)*disk.SizeInodeCompact

			// Extended inode, chunk-based layout, claiming a 1 PiB file.
			binary.LittleEndian.PutUint16(buf[inodeOff:], uint16(disk.LayoutChunkBased)<<1|1)
			binary.LittleEndian.PutUint64(buf[inodeOff+8:], 1<<50)
			binary.LittleEndian.PutUint32(buf[inodeOff+16:], tc.idata)

			img, err := Open(bytes.NewReader(buf))
			if err != nil {
				t.Fatalf("tampered image failed to open: %v", err)
			}

			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)

			dst := Create(&seekBuf{})
			err = dst.CopyFrom(img, MetadataOnly())
			if err == nil {
				err = dst.Close()
			}

			runtime.ReadMemStats(&after)
			grew := after.TotalAlloc - before.TotalAlloc

			if err == nil {
				t.Fatalf("CopyFrom accepted an inode claiming a 1 PiB chunk-based file")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("err = %v, want it to wrap ErrInvalid", err)
			}
			t.Logf("rejected with %v (allocated %d bytes)", err, grew)

			// The whole point is that nothing proportional to the declared
			// size is ever materialized.
			if grew > 64<<20 {
				t.Errorf("allocated %d bytes handling a bogus 1 PiB inode; want under 64 MiB", grew)
			}
		})
	}
}

// TestUntrustedInodeCountIsBounded covers sb.Inos, which sized the BFS
// queue's capacity directly.
func TestUntrustedInodeCountIsBounded(t *testing.T) {
	buf, _ := buildTamperableImage(t)

	// sb.Inos sits at offset 16 within the superblock.
	binary.LittleEndian.PutUint64(buf[disk.SuperBlockOffset+16:], 1<<62)

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("tampered image failed to open: %v", err)
	}
	if got := img.(*image).sb.Inos; got != 1<<62 {
		t.Fatalf("patched the wrong superblock offset: sb.Inos = %d", got)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	dst := Create(&seekBuf{})
	if err := dst.CopyFrom(img, MetadataOnly()); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}

	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 64<<20 {
		t.Errorf("allocated %d bytes for an image claiming 2^62 inodes; want under 64 MiB", grew)
	}
}

// TestUntrustedBlockCountIsRejected covers sb.Blocks, which sized the eager
// metadata read.
func TestUntrustedBlockCountIsRejected(t *testing.T) {
	buf, _ := buildTamperableImage(t)

	// sb.Blocks sits at offset 36 within the superblock.
	binary.LittleEndian.PutUint32(buf[disk.SuperBlockOffset+36:], 0xFFFFFFFF)

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("tampered image failed to open: %v", err)
	}
	if got := img.(*image).sb.Blocks; got != 0xFFFFFFFF {
		t.Fatalf("patched the wrong superblock offset: sb.Blocks = %d", got)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	dst := Create(&seekBuf{})
	err = dst.CopyFrom(img, MetadataOnly())

	runtime.ReadMemStats(&after)
	grew := after.TotalAlloc - before.TotalAlloc

	if err == nil {
		t.Fatal("CopyFrom accepted a superblock declaring far more blocks than the image holds")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want it to wrap ErrInvalid", err)
	}
	t.Logf("rejected with %v (allocated %d bytes)", err, grew)

	if grew > 64<<20 {
		t.Errorf("allocated %d bytes; want under 64 MiB", grew)
	}
}

// patchDirentNid rewrites every 8-byte dirent nid slot equal to from so it
// points at to, turning a valid tree into a cyclic graph.
func patchDirentNid(buf []byte, from, to uint64) int {
	var want, repl [8]byte
	binary.LittleEndian.PutUint64(want[:], from)
	binary.LittleEndian.PutUint64(repl[:], to)
	n := 0
	for off := 0; off+8 <= len(buf); off += 4 {
		if bytes.Equal(buf[off:off+8], want[:]) {
			copy(buf[off:off+8], repl[:])
			n++
		}
	}

	return n
}

// buildCyclicImage returns an image whose directory /d contains a dirent
// named "e" that points back at /d itself.
func buildCyclicImage(t *testing.T) []byte {
	t.Helper()

	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0))
	if err := w.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.Mkdir("/d/e", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	buf := out.buf

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	i := img.(*image)
	dNid, _, _, err := i.resolve("x", "d", false)
	if err != nil {
		t.Fatal(err)
	}
	eNid, _, _, err := i.resolve("x", "d/e", false)
	if err != nil {
		t.Fatal(err)
	}
	if patchDirentNid(buf, eNid, dNid) == 0 {
		t.Fatal("could not construct a directory cycle")
	}

	return buf
}

// TestUntrustedDirectoryCycleTerminates covers a dirent graph that is not a
// tree. Without a visited set the BFS revisits the same subtree forever,
// growing the queue and the path strings without bound.
func TestUntrustedDirectoryCycleTerminates(t *testing.T) {
	buf := buildCyclicImage(t)

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("cyclic image failed to open: %v", err)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	done := make(chan error, 1)
	go func() {
		dst := Create(&seekBuf{})
		done <- dst.CopyFrom(img, MetadataOnly())
	}()

	select {
	case err := <-done:
		runtime.ReadMemStats(&after)
		grew := after.TotalAlloc - before.TotalAlloc
		if err == nil {
			t.Fatal("CopyFrom accepted an image with a directory cycle")
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("err = %v, want it to wrap ErrInvalid", err)
		}
		t.Logf("rejected with %v (allocated %d bytes)", err, grew)
		if grew > 64<<20 {
			t.Errorf("allocated %d bytes on a cyclic image; want under 64 MiB", grew)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("CopyFrom did not terminate on a cyclic image")
	}
}

// TestUntrustedDirectoryCycleFullImageTerminates covers the same cyclic image
// copied in full-image mode. That path does not use copyFromImage at all: it
// walks the source through fs.WalkDir, which has no cycle detection, so the
// visited set that bounds the metadata-only path does not help. What stops it
// is the path length bound, since a cycle yields ever-deeper paths.
func TestUntrustedDirectoryCycleFullImageTerminates(t *testing.T) {
	buf := buildCyclicImage(t)

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("cyclic image failed to open: %v", err)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	done := make(chan error, 1)
	go func() {
		dst := Create(&seekBuf{})
		done <- dst.CopyFrom(img)
	}()

	select {
	case err := <-done:
		runtime.GC()
		runtime.ReadMemStats(&after)
		if err == nil {
			t.Fatal("full-image CopyFrom accepted an image with a directory cycle")
		}
		// Cumulative allocation is high because resolving each path walks it
		// from the root, so a deep chain costs O(depth^2) lookups. What has to
		// stay bounded is resident memory, which is what an OOM is made of.
		t.Logf("rejected with %v", err)
		t.Logf("cumulative alloc %d bytes, heap in use after GC %d bytes",
			after.TotalAlloc-before.TotalAlloc, after.HeapInuse)
		if after.HeapInuse > 256<<20 {
			t.Errorf("heap in use is %d bytes after a cyclic image; want well under 256 MiB", after.HeapInuse)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("full-image CopyFrom did not terminate on a cyclic image")
	}
}

// TestReaderWalkOnCyclicImage records the reader's behaviour on the same
// image. fs.WalkDir has no cycle detection of its own, so the walk is
// expected to keep descending; the callback bounds it so this test can
// assert the exposure without running away.
func TestReaderWalkOnCyclicImage(t *testing.T) {
	buf := buildCyclicImage(t)

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("cyclic image failed to open: %v", err)
	}

	const budget = 200
	errBudget := errors.New("visit budget exhausted")
	visits := 0
	deepest := ""

	err = fs.WalkDir(img, ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		visits++
		if len(p) > len(deepest) {
			deepest = p
		}
		if visits >= budget {
			return errBudget
		}

		return nil
	})

	if errors.Is(err, errBudget) {
		t.Logf("fs.WalkDir descended %d times on a 3-entry image (deepest path %d bytes); "+
			"callers walking an untrusted image must impose their own depth limit", visits, len(deepest))

		return
	}
	t.Logf("fs.WalkDir terminated after %d visits: %v", visits, err)
}

// badSizeFS is a source fs.FS holding one regular file that lies about its
// size. A hostile or simply buggy source is not obliged to report a sane
// value, and the writer must not carry it into the layout.
type badSizeFS struct{ size int64 }

type badSizeInfo struct {
	name string
	size int64
	dir  bool
}

func (i badSizeInfo) Name() string { return i.name }
func (i badSizeInfo) Size() int64  { return i.size }
func (i badSizeInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}

	return 0o644
}
func (i badSizeInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (i badSizeInfo) IsDir() bool        { return i.dir }
func (i badSizeInfo) Sys() any           { return nil }

type badSizeDirent struct{ info badSizeInfo }

func (d badSizeDirent) Name() string               { return d.info.name }
func (d badSizeDirent) IsDir() bool                { return d.info.dir }
func (d badSizeDirent) Type() fs.FileMode          { return d.info.Mode().Type() }
func (d badSizeDirent) Info() (fs.FileInfo, error) { return d.info, nil }

type badSizeDir struct{ fsys badSizeFS }

func (badSizeDir) Close() error             { return nil }
func (badSizeDir) Read([]byte) (int, error) { return 0, fs.ErrInvalid }
func (d badSizeDir) Stat() (fs.FileInfo, error) {
	return badSizeInfo{name: ".", size: 4096, dir: true}, nil
}
func (d badSizeDir) ReadDir(int) ([]fs.DirEntry, error) {
	return []fs.DirEntry{badSizeDirent{badSizeInfo{name: "bad", size: d.fsys.size}}}, nil
}

type badSizeFile struct {
	io.Reader
	info badSizeInfo
}

func (badSizeFile) Close() error                 { return nil }
func (f badSizeFile) Stat() (fs.FileInfo, error) { return f.info, nil }

func (fsys badSizeFS) Open(name string) (fs.File, error) {
	switch name {
	case ".":
		return badSizeDir{fsys}, nil
	case "bad":
		return badSizeFile{bytes.NewReader(nil), badSizeInfo{name: "bad", size: fsys.size}}, nil
	}

	return nil, fs.ErrNotExist
}

// TestCopyFromNegativeSize checks that a source reporting a negative size is
// rejected at ingestion. Left unchecked it became e.size = 2^64-1, which
// planLayout read back as int(-1): small enough to look inline-able, so the
// entry reserved -1 trailing bytes and every later inode landed one byte off
// its nid slot — a fully corrupt image built without an error.
func TestCopyFromNegativeSize(t *testing.T) {
	out := &seekBuf{}
	w := Create(out)
	err := w.CopyFrom(badSizeFS{size: -1})
	if err == nil {
		err = w.Close()
	}
	if err == nil {
		t.Fatal("CopyFrom/Close accepted a source with Size() == -1")
	}
	t.Logf("rejected: %v", err)
}

// TestEmptySymlinkTargetRejected covers a symlink whose i_size is 0.
//
// readLink returned "" for it, and resolve then folded the empty target away
// with path.Clean and restarted the walk at the root: the link itself resolved
// to the root directory, and — the dangerous part — a path *through* it
// discarded every component to its left, so "sub/el/secret" served "/secret".
// The writer used to produce such images itself via Symlink("", ...); this one
// has to be built by hand now that it does not.
func TestEmptySymlinkTargetRejected(t *testing.T) {
	out := &seekBuf{}
	w := Create(out)
	f, err := w.Create("/secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("TOPSECRET")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Mkdir("/sub", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.Symlink("x", "/sub/el"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	img0, err := Open(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatal(err)
	}
	fi, err := img0.(*image).Lstat("sub/el")
	if err != nil {
		t.Fatal(err)
	}
	nid := uint64(fi.Sys().(*Stat).Ino)

	// Zero the symlink's i_size in place. The field is 32 bits wide in a
	// compact inode and 64 in an extended one; both start at offset 8.
	inodeOff := img0.(*image).metaStartPos() + int64(nid)*disk.SizeInodeCompact
	if binary.LittleEndian.Uint16(out.buf[inodeOff:])&0x01 == 0 {
		binary.LittleEndian.PutUint32(out.buf[inodeOff+8:], 0)
	} else {
		binary.LittleEndian.PutUint64(out.buf[inodeOff+8:], 0)
	}

	img, err := Open(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatalf("tampered image failed to open: %v", err)
	}

	if target, err := img.(*image).ReadLink("sub/el"); err == nil {
		t.Errorf("ReadLink(sub/el) = %q, want an error", target)
	} else if !errors.Is(err, ErrInvalid) {
		t.Errorf("ReadLink err = %v, want it to wrap ErrInvalid", err)
	}
	if fi, err := fs.Stat(img, "sub/el"); err == nil {
		t.Errorf("Stat(sub/el) = mode=%v isdir=%v, want an error (it resolved to the root)",
			fi.Mode(), fi.IsDir())
	}
	if data, err := fs.ReadFile(img, "sub/el/secret"); err == nil {
		t.Errorf("ReadFile(sub/el/secret) = %q, want an error (the path was re-rooted at /)", data)
	}
}

// TestWriterRejectsEmptySymlinkTarget checks both entry points: the direct API
// and a CopyFrom source that reports an empty target.
func TestWriterRejectsEmptySymlinkTarget(t *testing.T) {
	w := Create(&seekBuf{})
	if err := w.Symlink("", "/el"); err == nil {
		t.Error("Symlink accepted an empty target")
	} else if !errors.Is(err, ErrInvalid) {
		t.Errorf("Symlink err = %v, want it to wrap ErrInvalid", err)
	}

	// checkLimits backstops the CopyFrom paths, where the target comes from
	// the source rather than the caller.
	w2 := Create(&seekBuf{})
	w2.addChild(&fsEntry{path: "/el", mode: disk.StatTypeSymlink | 0o777})
	if err := w2.Close(); err == nil {
		t.Error("Close accepted an entry with an empty symlink target")
	} else if !errors.Is(err, ErrInvalid) {
		t.Errorf("Close err = %v, want it to wrap ErrInvalid", err)
	}
}
