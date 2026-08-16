package erofs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/forkcloser/erofs/internal/builder"
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

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	// Open bounds the superblock's geometry against the image, so a block
	// count this far past the end never reaches the code that would size an
	// allocation from it. It used to open cleanly and be caught later, in
	// CopyFrom.
	_, err := Open(bytes.NewReader(buf))

	runtime.ReadMemStats(&after)
	grew := after.TotalAlloc - before.TotalAlloc

	if err == nil {
		t.Fatal("Open accepted a superblock declaring far more blocks than the image holds")
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

// buildSymlinkCycleImage returns an image whose root dirent "a" points back
// at the root, so "a/a/a/..." resolves forever, plus a symlink "l" at the root
// with the given target.
func buildSymlinkCycleImage(t *testing.T, target string) []byte {
	t.Helper()

	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0))
	if err := w.Mkdir("/a", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.Symlink(target, "/l"); err != nil {
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
	aNid, _, _, err := i.resolve("x", "a", false)
	if err != nil {
		t.Fatal(err)
	}
	if patchDirentNid(buf, aNid, uint64(i.sb.RootNid)) == 0 {
		t.Fatal("could not construct a directory cycle")
	}

	return buf
}

// TestResolveWorkIsBounded covers the total work a single resolve can be made
// to do. maxSymlinks bounds the number of hops but bounded nothing per hop:
// each hop restarts the walk from the root over curPath + "/" + target, where
// curPath is the whole prefix already walked, so a self-referential directory
// grew the path every hop and the cost was quadratic in the accumulated
// length. A 12 KiB image drove 33 million reads and 94 seconds of CPU through
// one Open.
func TestResolveWorkIsBounded(t *testing.T) {
	// maxSymlinks hops over at most maxPathLen/2 components each, at a couple
	// of reads per component. Clear of the real figure (~1M) and far below the
	// tens of millions the unbounded walk reached.
	const budget = 4 * maxSymlinks * (maxPathLen / 2)

	for _, tc := range []struct {
		name   string
		target string
	}{
		// Relative: every hop prepends the prefix already walked, so the
		// path grows and the work per hop grows with it.
		{"relative", strings.Repeat("a/", 500) + "l"},
		// Absolute: the prefix is not prepended, so the path keeps its
		// length. Just under PATH_MAX is the most expensive shape that
		// survives the per-hop cap — the steady state the budget must hold.
		{"absolute", "/" + strings.Repeat("a/", (maxPathLen-4)/2) + "l"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := buildSymlinkCycleImage(t, tc.target)

			cr := &countingReaderAt{ra: bytes.NewReader(buf)}
			img, err := Open(cr)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := img.Open("l"); err == nil {
				t.Fatal("Open resolved a cyclic symlink chain")
			}
			if cr.calls > budget {
				t.Errorf("Open made %d ReadAt calls, over the %d budget", cr.calls, budget)
			}
			t.Logf("bounded at %d ReadAt calls from a %d byte image", cr.calls, len(buf))
		})
	}
}

// TestResolveRejectsOverlongPath covers the caller-supplied half of the same
// bound. curPath is rebuilt by concatenation per component, so walking an
// arbitrarily long name costs O(len(name)^2) in copying even before any
// symlink is involved — and fs.WalkDir over a cyclic image generates exactly
// those names.
func TestResolveRejectsOverlongPath(t *testing.T) {
	buf := buildSymlinkCycleImage(t, "/a/l")

	cr := &countingReaderAt{ra: bytes.NewReader(buf)}
	img, err := Open(cr)
	if err != nil {
		t.Fatal(err)
	}
	before := cr.calls
	_, err = img.Open(strings.Repeat("a/", maxPathLen) + "l")
	if err == nil {
		t.Fatal("Open accepted a path over PATH_MAX")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want it to wrap ErrInvalid", err)
	}
	if n := cr.calls - before; n > 1 {
		t.Errorf("rejecting an over-long path took %d ReadAt calls, want it rejected before any walk", n)
	}
}

// patchDirentName replaces a marker name in an image with a same-length name
// the writer would never produce, so a test can express a hostile dirent
// without hand-assembling a directory block.
func patchDirentName(t *testing.T, buf []byte, from, to string) {
	t.Helper()

	if len(from) != len(to) {
		t.Fatalf("replacement %q is %d bytes, marker %q is %d", to, len(to), from, len(from))
	}
	i := bytes.Index(buf, []byte(from))
	if i < 0 {
		t.Fatalf("marker %q not found in the image", from)
	}
	copy(buf[i:], to)
}

// TestDirentNameIsABaseName covers a dirent whose name carries a path
// separator. fs.DirEntry.Name is contractually a base name, and the standard
// extraction pattern — fs.WalkDir plus filepath.Join — writes outside the
// destination directory the moment it is not: classic zip-slip, driven by the
// image rather than by the caller.
func TestDirentNameIsABaseName(t *testing.T) {
	out := &seekBuf{}
	w := Create(out)
	f, err := w.Create("/AAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("pwned")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	patchDirentName(t, out.buf, "AAAAAAAAAA", "../../evil")

	img, err := Open(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatalf("tampered image failed to open: %v", err)
	}

	if ents, err := fs.ReadDir(img, "."); err == nil {
		for _, e := range ents {
			t.Errorf("ReadDir returned name %q, want an error", e.Name())
		}
	} else if !errors.Is(err, ErrInvalid) {
		t.Errorf("ReadDir err = %v, want it to wrap ErrInvalid", err)
	}

	err = fs.WalkDir(img, ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != "." {
			t.Errorf("WalkDir yielded path %q, want an error", p)
		}

		return nil
	})
	if err == nil {
		t.Error("WalkDir walked an image with a separator in a dirent name")
	}
}

// TestMergeWhiteoutCannotEscapeItsDirectory covers a dirent name carrying a
// path traversal in merge mode. copyFromImage built childPath by plain
// concatenation, so a nested entry named "y/../../../.wh..wh..opq" produced a
// path whose path.Dir collapses to "/" — and the opaque-whiteout branch then
// removed every entry contributed by every prior layer.
func TestMergeWhiteoutCannotEscapeItsDirectory(t *testing.T) {
	out := &seekBuf{}
	w := Create(out)
	if err := w.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.Mkdir("/d/BBBBBBBBBBBBBBBBBBBBBBB", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	patchDirentName(t, out.buf, "BBBBBBBBBBBBBBBBBBBBBBB", "y/../../../.wh..wh..opq")

	hostile, err := Open(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatalf("tampered image failed to open: %v", err)
	}

	dst := Create(&seekBuf{})
	keep, err := dst.Create("/keep")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keep.Write([]byte("PRIOR LAYER DATA")); err != nil {
		t.Fatal(err)
	}
	if err := keep.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dst.Mkdir("/etc", 0o755); err != nil {
		t.Fatal(err)
	}

	err = dst.CopyFrom(hostile, MetadataOnly(), Merge())
	if err == nil {
		t.Fatal("merge accepted a dirent name containing a path separator")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want it to wrap ErrInvalid", err)
	}
	for _, p := range []string{"/keep", "/etc"} {
		if _, ok := dst.byPath[p]; !ok {
			t.Errorf("%s from the prior layer was removed", p)
		}
	}
}

// strictReaderAt is the shape a caller writes when the image is already in
// memory: index the backing slice directly. *bytes.Reader and *os.File answer
// a negative or past-the-end offset with an error; this one would panic, so it
// reports instead. It deliberately does not expose Size, which exercises the
// bound taken from the superblock rather than from the reader.
type strictReaderAt struct {
	t   *testing.T
	buf []byte
}

func (s *strictReaderAt) ReadAt(p []byte, off int64) (int, error) {
	s.t.Helper()

	if off < 0 || off > int64(len(s.buf)) {
		s.t.Errorf("ReadAt offset %d is outside the %d byte image", off, len(s.buf))

		return 0, io.EOF
	}
	n := copy(p, s.buf[off:])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

// exerciseUntrusted drives every read path over an image, ignoring errors:
// the point is what offsets reach the reader, not what the calls return.
func exerciseUntrusted(t *testing.T, buf []byte) {
	t.Helper()

	img, err := Open(&strictReaderAt{t: t, buf: buf})
	if err != nil {
		return
	}
	_ = fs.WalkDir(img, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			if dr, ok := fi.(interface{ DataRange() []DataRange }); ok {
				_ = dr.DataRange()
			}
		}
		if !d.IsDir() {
			_, _ = fs.ReadFile(img, p)
			if f, err := img.Open(p); err == nil {
				if wt, ok := f.(io.WriterTo); ok {
					_, _ = wt.WriteTo(io.Discard)
				}
				_ = f.Close()
			}
		}

		return nil
	})
	_, _ = fs.Stat(img, "f")
	_, _ = img.(*image).Lstat("f")
	_, _ = img.(*image).ReadLink("f")
}

// TestUntrustedNidStaysInBounds covers a dirent nid large enough that
// metaStartPos + nid*32 wraps to a negative int64. The offset was handed to
// the caller's io.ReaderAt untouched: an error for *bytes.Reader and *os.File,
// a panic for a slice-backed one. readInfo's recover() turned its own panic
// into an error but did nothing about the offset already having escaped, and
// loadBlock, openDirect and buildChunkDataRanges have no such net at all.
func TestUntrustedNidStaysInBounds(t *testing.T) {
	buf, _ := buildTamperableImage(t)

	img0, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	fNid, _, _, err := img0.(*image).resolve("x", "f", false)
	if err != nil {
		t.Fatal(err)
	}
	if patchDirentNid(buf, fNid, 1<<58) == 0 {
		t.Fatal("could not patch the dirent nid")
	}

	exerciseUntrusted(t, buf)
}

// TestUntrustedChunkAddrStaysInBounds covers a chunk index whose physical
// block address, shifted by BlkSizeBits, overflows int64. The 16-bit high half
// only reaches that at the maximum block size, which is why the default-sized
// fuzz corpus never produced it.
func TestUntrustedChunkAddrStaysInBounds(t *testing.T) {
	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0), WithBlockSize(65536))
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
	buf := out.buf

	img0, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	fi, err := fs.Stat(img0, "f")
	if err != nil {
		t.Fatal(err)
	}
	nid := uint64(fi.Sys().(*Stat).Ino)
	inodeOff := img0.(*image).metaStartPos() + int64(nid)*disk.SizeInodeCompact

	// Extended inode, chunk-based with an index map, one block-sized chunk.
	binary.LittleEndian.PutUint16(buf[inodeOff:], uint16(disk.LayoutChunkBased)<<1|1)
	binary.LittleEndian.PutUint64(buf[inodeOff+8:], 65536)
	binary.LittleEndian.PutUint32(buf[inodeOff+16:], disk.LayoutChunkFormatIndexes)

	img1, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	ino, err := (&file{img: img1.(*image), nid: nid}).readInfo()
	if err != nil {
		t.Fatal(err)
	}
	base := inodeOff + ino.flatDataOffset()
	if base%disk.SizeChunkIndex != 0 {
		base = (base + disk.SizeChunkIndex - 1) & ^int64(disk.SizeChunkIndex-1)
	}
	// The largest address the two fields can express, which is not the
	// null-chunk sentinel: (2^48-2) << 16 wraps past MaxInt64.
	binary.LittleEndian.PutUint16(buf[base:], 0xFFFF)
	binary.LittleEndian.PutUint16(buf[base+2:], 0)
	binary.LittleEndian.PutUint32(buf[base+4:], 0xFFFFFFFE)

	exerciseUntrusted(t, buf)
}

// TestUntrustedChunkIndexAllocIsBacked covers a chunk-based inode whose
// declared size implies a chunk-index map just under maxChunkIndexBytes.
// openDirect and buildChunkDataRanges sized their buffer from that declared
// size and allocated it before the read that would have shown the map is not
// there, so an 8 KiB image drove a 64 MiB allocation on every Stat or
// DataRange call.
func TestUntrustedChunkIndexAllocIsBacked(t *testing.T) {
	buf, nid := buildTamperableImage(t)

	img0, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	inodeOff := img0.(*image).metaStartPos() + int64(nid)*disk.SizeInodeCompact

	// Enough chunks for an index map just under the cap.
	const size = (maxChunkIndexBytes / disk.SizeChunkIndex) * 4096
	binary.LittleEndian.PutUint16(buf[inodeOff:], uint16(disk.LayoutChunkBased)<<1|1)
	binary.LittleEndian.PutUint64(buf[inodeOff+8:], uint64(size))
	binary.LittleEndian.PutUint32(buf[inodeOff+16:], disk.LayoutChunkFormatIndexes)

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("tampered image failed to open: %v", err)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	const calls = 4
	for range calls {
		fi, err := fs.Stat(img, "f")
		if err != nil {
			continue
		}
		if dr, ok := fi.(interface{ DataRange() []DataRange }); ok {
			_ = dr.DataRange()
		}
	}

	runtime.ReadMemStats(&after)
	grew := after.TotalAlloc - before.TotalAlloc

	// Nothing proportional to the declared size should have been allocated;
	// before the bound this was 64 MiB per call.
	if grew > 1<<20 {
		t.Errorf("%d Stat+DataRange calls on a %d byte image allocated %d bytes",
			calls, len(buf), grew)
	}
	t.Logf("%d calls allocated %d bytes from a %d byte image", calls, grew, len(buf))
}

// TestCopyFromImageSharesChunkMaps covers the memory a metadata-only copy
// retains for chunk maps. Nothing here is tampered: many hardlinks to one
// chunk-based file is an ordinary layer shape.
//
// parseChunks reserved capacity for the inode's *declared* chunk count, and a
// contiguous file coalesces to a single entry, so almost all of it was slack.
// copyFromImage then parsed the map again for every name, since a file nid is
// deliberately not de-duped. 20000 links to one 480-chunk file reserved 9.6 M
// chunk slots to hold 20001, retaining 162 MiB from a 384 KiB image.
func TestCopyFromImageSharesChunkMaps(t *testing.T) {
	const (
		links  = 2000
		blocks = 480
	)

	df, err := os.Create(filepath.Join(t.TempDir(), "data.bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = df.Close() }()

	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0), WithDataFile(df))
	f, err := w.Create("/f0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte{0xAB}, blocks*4096)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range links {
		if err := w.Link("/f0", fmt.Sprintf("/d/l%06d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	img, err := Open(bytes.NewReader(out.buf), WithExtraDevices(df))
	if err != nil {
		t.Fatal(err)
	}

	dst := Create(&seekBuf{})
	if err := dst.CopyFrom(img, MetadataOnly()); err != nil {
		t.Fatal(err)
	}

	var first *builder.Chunk
	entries := 0
	for _, e := range dst.byPath {
		if len(e.chunks) == 0 {
			continue
		}
		entries++
		if cap(e.chunks) != len(e.chunks) {
			t.Errorf("%s: chunk slice holds %d entries with capacity for %d",
				e.path, len(e.chunks), cap(e.chunks))
		}
		// Every name of one inode describes the same extents, so they must
		// all be looking at the same slice rather than a copy each.
		if first == nil {
			first = &e.chunks[0]
		} else if &e.chunks[0] != first {
			t.Errorf("%s: chunk map was parsed again instead of shared", e.path)
		}
	}
	if entries != links+1 {
		t.Fatalf("%d entries carry chunks, want %d", entries, links+1)
	}
	t.Logf("%d names share one %d-entry chunk map", entries, len(dst.byPath["/f0"].chunks))
}

// TestUnalignedDataFileStart covers a data file that does not begin on a block
// boundary. A chunk records a file's start as a block number, so the offset
// has to divide exactly; the first file inherited dataOff from a Seek to the
// end of a pre-existing data file, and the division truncated it onto an
// earlier block, so the file read back the bytes already there.
func TestUnalignedDataFileStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.bin")
	pre := []byte("PRE-EXISTING BYTES, NOT BLOCK ALIGNED")
	if err := os.WriteFile(path, pre, 0o644); err != nil {
		t.Fatal(err)
	}
	df, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = df.Close() }()

	out := &seekBuf{}
	w := Create(out, WithDataFile(df))
	f, err := w.Create("/a")
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{0xCD}, 4096)
	if _, err := f.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	dfr, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dfr.Close() }()
	img, err := Open(bytes.NewReader(out.buf), WithExtraDevices(dfr))
	if err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile(img, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("/a read back %d bytes starting %q, want the bytes written",
			len(got), got[:min(len(got), 24)])
	}
}

// TestShortReadIsNotZeroFilled covers a file whose data range runs past the
// end of the image. file.Read only clears io.EOF when it filled the buffer, so
// ReadFile used to swallow the EOF and hand back a zero-padded buffer with a
// nil error — a consumer validating a manifest would see attacker-chosen zeros
// instead of a failure.
func TestShortReadIsNotZeroFilled(t *testing.T) {
	buf, nid := buildTamperableImage(t)

	img0, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	inodeOff := img0.(*image).metaStartPos() + int64(nid)*disk.SizeInodeCompact

	// Flat-plain, one block past the end of the image.
	blocks := uint32(len(buf) >> img0.(*image).sb.BlkSizeBits)
	binary.LittleEndian.PutUint16(buf[inodeOff:], uint16(disk.LayoutFlatPlain)<<1|1)
	binary.LittleEndian.PutUint64(buf[inodeOff+8:], 4096)
	binary.LittleEndian.PutUint32(buf[inodeOff+16:], blocks)

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("tampered image failed to open: %v", err)
	}
	got, err := fs.ReadFile(img, "f")
	if err == nil {
		t.Fatalf("ReadFile returned %d bytes (all zero: %v) instead of an error",
			len(got), bytes.Equal(got, make([]byte, len(got))))
	}
	t.Logf("rejected: %v", err)
}

// TestXattrPrefixAndDuplicates covers the two ways an image can put the same
// xattr name in the map twice: an undefined name index, which used to map to
// the empty prefix and let "security.capability" be spelled out in full, and a
// plain repeat of a key, which the map silently took last-one-wins.
func TestXattrPrefixAndDuplicates(t *testing.T) {
	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0))
	f, err := w.Create("/f")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Setxattr("/f", "security.capability", "real"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	buf := out.buf

	img0, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := fs.Stat(img0, "f"); err != nil {
		t.Fatal(err)
	} else if st := fi.Sys().(*Stat); st.Xattrs["security.capability"] != "real" {
		t.Fatalf("xattr did not round-trip: %v", st.Xattrs)
	}

	// The 4-byte xattr entry precedes the stored name; its second byte is
	// the name index. Index 6 is "security."; 7 upward is undefined.
	i := bytes.Index(buf, []byte("capability"))
	if i < 0 {
		t.Fatal("could not find the stored xattr name")
	}
	if got := buf[i-3]; got != 6 {
		t.Fatalf("expected name index 6 before the name, got %d", got)
	}
	buf[i-3] = 7

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("tampered image failed to open: %v", err)
	}
	if _, err := fs.Stat(img, "f"); err == nil {
		t.Error("Stat accepted an undefined xattr name index")
	} else if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want it to wrap ErrInvalid", err)
	}

	// And a duplicate key must not be taken last-one-wins.
	stat := &Stat{Xattrs: map[string]string{"security.capability": "real"}}
	if err := setXattr(stat, 1, "security.capability", "spoofed"); err == nil {
		t.Error("setXattr accepted a duplicate key")
	}
	if stat.Xattrs["security.capability"] != "real" {
		t.Error("a rejected duplicate overwrote the original value")
	}
}

// TestCopyFromImageXattrParity covers the copyFromImage fast path, the other
// door an image's attributes come in through. The reader rejects an undefined
// xattr name index and a duplicated key (TestXattrPrefixAndDuplicates); the
// fast-path parser used to map the index to the empty prefix and take the
// duplicate last-wins, so an image that the reader refused would still be
// copied — spoofed security.capability included — by CopyFrom(MetadataOnly).
func TestCopyFromImageXattrParity(t *testing.T) {
	build := func(t *testing.T, xattrs map[string]string) []byte {
		t.Helper()
		out := &seekBuf{}
		w := Create(out, WithBuildTime(1000, 0))
		f, err := w.Create("/f")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		for k, v := range xattrs {
			if err := w.Setxattr("/f", k, v); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return out.buf
	}
	copyMeta := func(t *testing.T, buf []byte) error {
		t.Helper()
		img, err := Open(bytes.NewReader(buf))
		if err != nil {
			t.Fatalf("image failed to open: %v", err)
		}
		return Create(&seekBuf{}).CopyFrom(img, MetadataOnly())
	}

	t.Run("undefinedIndex", func(t *testing.T) {
		buf := build(t, map[string]string{"security.capability": "real"})
		if err := copyMeta(t, buf); err != nil {
			t.Fatalf("untampered image failed to copy: %v", err)
		}
		// The 4-byte entry precedes the stored name; its second byte is the
		// name index. 6 is "security."; 7 upward is undefined.
		i := bytes.Index(buf, []byte("capability"))
		if i < 0 || buf[i-3] != 6 {
			t.Fatalf("could not locate the stored xattr entry (index byte %d)", buf[i-3])
		}
		buf[i-3] = 7
		err := copyMeta(t, buf)
		if err == nil {
			t.Fatal("CopyFrom accepted an undefined xattr name index")
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("err = %v, want it to wrap ErrInvalid", err)
		}
	})

	t.Run("duplicateKey", func(t *testing.T) {
		// Two names under the same prefix, then rewrite the second's stored
		// suffix so both spell the same full key.
		buf := build(t, map[string]string{
			"user.aaaa": "first",
			"user.bbbb": "second",
		})
		if err := copyMeta(t, buf); err != nil {
			t.Fatalf("untampered image failed to copy: %v", err)
		}
		i := bytes.Index(buf, []byte("bbbb"))
		if i < 0 {
			t.Fatal("could not locate the second stored xattr name")
		}
		copy(buf[i:], "aaaa")
		err := copyMeta(t, buf)
		if err == nil {
			t.Fatal("CopyFrom accepted a duplicated xattr key")
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("err = %v, want it to wrap ErrInvalid", err)
		}
	})
}

// TestEmptyDirentNameIsRejected covers a dirent whose name is zero bytes long.
// No writer emits one, and as a path element it names the directory itself,
// so a lookup for it silently succeeds; the reader used to hand it out as an
// entry, and the copyFromImage fast path silently skipped over it — neither
// treated the image as the corrupt thing it is.
func TestEmptyDirentNameIsRejected(t *testing.T) {
	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0))
	for _, n := range []string{"/a", "/b"} {
		f, err := w.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	buf := out.buf

	img0, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	aNid, _, _, err := img0.(*image).resolve("a", "a", false)
	if err != nil {
		t.Fatal(err)
	}

	// Find a's dirent by its nid, then give it b's NameOff — the next dirent
	// is 12 bytes on — so a's name spans zero bytes. Nothing else changes.
	var want [8]byte
	binary.LittleEndian.PutUint64(want[:], aNid)
	off := -1
	for o := 0; o+disk.SizeDirent*2 <= len(buf); o += 4 {
		if bytes.Equal(buf[o:o+8], want[:]) {
			off = o
			break
		}
	}
	if off < 0 {
		t.Fatal("could not locate the dirent for /a")
	}
	nextNameOff := binary.LittleEndian.Uint16(buf[off+disk.SizeDirent+8:])
	binary.LittleEndian.PutUint16(buf[off+8:], nextNameOff)

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("tampered image failed to open: %v", err)
	}
	if ents, err := fs.ReadDir(img, "."); err == nil {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("ReadDir returned %q, want an error", names)
	} else if !errors.Is(err, ErrInvalid) {
		t.Errorf("ReadDir err = %v, want it to wrap ErrInvalid", err)
	}

	err = Create(&seekBuf{}).CopyFrom(img, MetadataOnly())
	if err == nil {
		t.Error("CopyFrom accepted an empty dirent name")
	} else if !errors.Is(err, ErrInvalid) {
		t.Errorf("CopyFrom err = %v, want it to wrap ErrInvalid", err)
	}
}
