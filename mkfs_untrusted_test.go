package erofs //nolint:testpackage // white-box: forges hostile images and writer states from unexported types

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

	"github.com/forkcloser/go-erofs/internal/disk"
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

	if _, err = f.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}

	if err = f.Close(); err != nil {
		t.Fatal(err)
	}

	if err = w.Chtimes("/f", time.Unix(2000, 0), time.Unix(2000, 0)); err != nil {
		t.Fatal(err)
	}

	if err = w.Close(); err != nil {
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

	nid := fi.Sys().(*Stat).Ino

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
//
//nolint:paralleltest // measures the process's TotalAlloc against a budget; a parallel test's allocations would count against it
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
				t.Fatal("CopyFrom accepted an inode claiming a 1 PiB chunk-based file")
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
//
//nolint:paralleltest // measures the process's TotalAlloc against a budget; a parallel test's allocations would count against it
func TestUntrustedInodeCountIsBounded(t *testing.T) {
	buf, _ := buildTamperableImage(t)

	// sb.Inos sits at offset 16 within the superblock. The tamper leaves the
	// superblock checksum stale; it is switched off so the bound under test,
	// not the checksum, is what Open judges.
	binary.LittleEndian.PutUint64(buf[disk.SuperBlockOffset+16:], 1<<62)
	buf[disk.SuperBlockOffset+8] &^= disk.FeatureCompatSBChksum

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
//
//nolint:paralleltest // measures the process's TotalAlloc against a budget; a parallel test's allocations would count against it
func TestUntrustedBlockCountIsRejected(t *testing.T) {
	buf, _ := buildTamperableImage(t)

	// sb.Blocks sits at offset 36 within the superblock. The checksum is
	// switched off for the same reason as above.
	binary.LittleEndian.PutUint32(buf[disk.SuperBlockOffset+36:], 0xFFFFFFFF)
	buf[disk.SuperBlockOffset+8] &^= disk.FeatureCompatSBChksum

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

	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("err = %v, want ErrCorrupt", err)
	}

	t.Logf("rejected with %v (allocated %d bytes)", err, grew)

	if grew > 64<<20 {
		t.Errorf("allocated %d bytes; want under 64 MiB", grew)
	}
}

// movedMeta serves an image whose metadata area sits far past its other
// bytes, as a sparse file holds it for nothing: reads from to on come from
// buf at from, and what is between buf's end and to reads as zeros. It
// reports no size, so the image is as long as its superblock declares.
type movedMeta struct {
	buf      []byte
	from, to int64
}

func (m *movedMeta) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, io.EOF
	}

	for n := range p {
		at := off + int64(n)
		if at >= m.to {
			at = at - m.to + m.from
		}

		switch {
		case at < int64(len(m.buf)):
			p[n] = m.buf[at]
		case off+int64(n) < m.to:
			p[n] = 0
		default:
			return n, io.EOF
		}
	}

	return len(p), nil
}

// TestUntrustedDirectorySizeIsBounded covers a directory's i_size, which
// sized the buffer the metadata-only copy read the directory into. The
// size is held to the image's declared extent, which is the superblock's
// word: a superblock that puts the metadata 4 GiB in, on a source of a few
// kilobytes that reports no length, let the root declare 2 GiB of entries
// and have them allocated before a byte was read.
//
//nolint:paralleltest // measures the process's TotalAlloc against a budget; a parallel test's allocations would count against it
func TestUntrustedDirectorySizeIsBounded(t *testing.T) {
	buf, _ := buildTamperableImage(t)

	img0, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}

	sb := img0.(*image).sb
	blkBits := sb.BlkSizeBits
	metaStart := img0.(*image).metaStartPos()

	// The metadata area moves 4 GiB in, the block count with it.
	const movedBy = int64(4 << 30)

	blocks := uint32(movedBy >> blkBits)

	binary.LittleEndian.PutUint32(buf[disk.SuperBlockOffset+40:], sb.MetaBlkAddr+blocks)
	binary.LittleEndian.PutUint32(buf[disk.SuperBlockOffset+36:], sb.Blocks+blocks)
	buf[disk.SuperBlockOffset+8] &^= disk.FeatureCompatSBChksum

	// The root becomes a flat-plain directory of 2 GiB at block 0. i_size is
	// 32 bits at offset 8 in a compact inode and 64 in an extended one.
	root := metaStart + int64(sb.RootNid)*disk.SizeInodeCompact
	format := binary.LittleEndian.Uint16(buf[root:])
	binary.LittleEndian.PutUint16(buf[root:], format&^disk.InodeFormatLayoutMask)
	binary.LittleEndian.PutUint32(buf[root+8:], 2<<30)

	if format&disk.InodeFormatExtended != 0 {
		binary.LittleEndian.PutUint32(buf[root+12:], 0)
	}

	binary.LittleEndian.PutUint32(buf[root+16:], 0)

	img, err := Open(&movedMeta{buf: buf, from: metaStart, to: metaStart + movedBy})
	if err != nil {
		t.Fatalf("tampered image failed to open: %v", err)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	err = Create(&seekBuf{}).CopyFrom(img, MetadataOnly())

	runtime.ReadMemStats(&after)
	grew := after.TotalAlloc - before.TotalAlloc

	// The limit's own refusal: ErrCorrupt is ErrInvalid too, and a copy
	// that never reached the directory answers with it.
	if err == nil {
		t.Fatal("a root declaring 2 GiB of entries was copied")
	}

	if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "directory") {
		t.Errorf("err = %v, want the directory limit's ErrInvalid", err)
	}

	t.Logf("rejected with %v (allocated %d bytes)", err, grew)

	if grew > 64<<20 {
		t.Errorf("allocated %d bytes for a root declaring 2 GiB of entries; want under 64 MiB", grew)
	}
}

// errWindowSource is windowSource's own error.
var errWindowSource = errors.New("windowSource: the source's own error")

// windowSource is a source of any length whose byte at off is off's low
// eight bits times three, and which counts what is read of it. Reads that
// reach short come back a byte short with no error; reads that reach fails
// fail.
type windowSource struct {
	calls, bytes int64
	short, fails int64
}

const windowSourceSize = 1 << 20

func (s *windowSource) ReadAt(p []byte, off int64) (int, error) {
	s.calls++

	if s.fails > 0 && off+int64(len(p)) > s.fails {
		return 0, errWindowSource
	}

	n := len(p)
	if s.short > 0 && off+int64(n) > s.short {
		n--
	}

	for i := range p[:n] {
		p[i] = byte(off+int64(i)) * 3
	}

	s.bytes += int64(n)

	return n, nil
}

// TestUntrustedCopyWindowsKeepToTheirRoom covers what a metadata-only copy
// holds of its source: every window it reads, so that none is read twice,
// and no more than its room, which is 1 GiB for a copy and 128 KiB here.
// Past it a range is read on its own, at its size, every time it is asked
// for; what is held is still served with no read. It is held to the type
// and not to a copy, against the package's rule for tests: the copy that
// shows the room is one that holds 1 GiB.
func TestUntrustedCopyWindowsKeepToTheirRoom(t *testing.T) {
	t.Parallel()

	src := &windowSource{}
	w := newImageWindows(src, windowSourceSize, 2*copyWindow)

	at := func(off, n int64) {
		t.Helper()

		got, err := w.span(off, n)
		if err != nil {
			t.Fatalf("span(%d, %d): %v", off, n, err)
		}

		for i, b := range got {
			if want := byte(off+int64(i)) * 3; b != want {
				t.Fatalf("span(%d, %d): byte %d is %d, want %d", off, n, i, b, want)
			}
		}

		if int64(len(got)) != n {
			t.Fatalf("span(%d, %d) is %d bytes", off, n, len(got))
		}
	}

	// Two windows, far apart: a read each, and the room is taken.
	at(100, 64)
	at(5*copyWindow+7, 32)

	if src.calls != 2 || src.bytes != 2*copyWindow || w.held != 2*copyWindow {
		t.Fatalf("two windows: %d reads of %d bytes, %d held; want 2 reads of %d, all held",
			src.calls, src.bytes, w.held, 2*copyWindow)
	}

	// Held windows answer with no read.
	at(200, 4096)
	at(5*copyWindow, copyWindow)

	if src.calls != 2 {
		t.Fatalf("ranges of held windows took %d more reads", src.calls-2)
	}

	// A third window has no room: its range is read alone, each time.
	at(9*copyWindow+1, 48)
	at(9*copyWindow+1, 48)

	if src.calls != 4 || src.bytes != 2*copyWindow+96 || w.held != 2*copyWindow {
		t.Fatalf("past the room: %d reads, %d bytes read, %d held; want 4, %d and %d",
			src.calls, src.bytes, w.held, 2*copyWindow+96, 2*copyWindow)
	}

	// So is a range that runs from a held window into one there is no
	// room for, and one in a window there is no room for before a held one.
	at(copyWindow-8, 16)
	at(4*copyWindow+1, 48)
	at(4*copyWindow, 48)

	if src.calls != 7 || src.bytes != 2*copyWindow+208 {
		t.Fatalf("beside held windows: %d reads, %d bytes read; want 7 and %d", src.calls, src.bytes, 2*copyWindow+208)
	}

	// Nothing is read for a range that is not in the image.
	for _, r := range [][2]int64{{-1, 4}, {windowSourceSize - 4, 5}, {windowSourceSize, 1}, {0, windowSourceSize + 1}, {8, -1}} {
		if _, err := w.span(r[0], r[1]); !errors.Is(err, ErrCorrupt) {
			t.Errorf("span(%d, %d) = %v; want ErrCorrupt", r[0], r[1], err)
		}
	}

	if src.calls != 7 {
		t.Errorf("ranges outside the image took %d reads", src.calls-7)
	}

	// A read that follows the last would take two windows, and takes the
	// one there is room for.
	src = &windowSource{}
	w = newImageWindows(src, windowSourceSize, 2*copyWindow)

	at(10, 10)
	at(copyWindow+10, 10)

	if src.calls != 2 || src.bytes != 2*copyWindow || w.held != 2*copyWindow {
		t.Fatalf("two windows in a row with room for two: %d reads of %d bytes, %d held; want 2 of %d, all held",
			src.calls, src.bytes, w.held, 2*copyWindow)
	}

	// A range across two held windows is their bytes, with no read: here
	// the windows of two reads.
	at(copyWindow-8, 16)

	if src.calls != 2 {
		t.Errorf("a range across two held windows took %d more reads", src.calls-2)
	}

	// With room, a range is read as the windows it runs through, in one
	// read where none of them is held, and they are held: a range across
	// two windows, and one longer than a window, each asked for twice.
	src = &windowSource{}
	w = newImageWindows(src, windowSourceSize, copyHeld)

	at(4*copyWindow-8, 16)
	at(4*copyWindow-8, 16)

	if src.calls != 1 || src.bytes != 2*copyWindow {
		t.Fatalf(
			"a range across two windows not held: %d reads of %d bytes; want 1 of %d",
			src.calls,
			src.bytes,
			2*copyWindow,
		)
	}

	at(8*copyWindow+5, 2*copyWindow+1)
	at(8*copyWindow+5, 2*copyWindow+1)

	if src.calls != 2 || src.bytes != 5*copyWindow || w.held != 5*copyWindow {
		t.Fatalf("a range through three windows not held: %d reads, %d bytes, %d held; want 2 and %d of each",
			src.calls, src.bytes, w.held, 5*copyWindow)
	}

	// A range from held windows into one not held reads that one, here
	// with the four after it to the source's end, which following the last
	// read earns; and a range through held windows and three between them
	// reads the three. Each is the bytes of all its reads.
	at(8*copyWindow+5, 3*copyWindow+1)
	at(3*copyWindow+9, 5*copyWindow+80)

	if src.calls != 4 || w.held != 13*copyWindow {
		t.Errorf(
			"ranges through held windows and others: %d reads, %d held; want 4 and %d",
			src.calls,
			w.held,
			13*copyWindow,
		)
	}

	// A window gigabytes into an image is held and found as the first is.
	src = &windowSource{}
	w = newImageWindows(src, 4<<30, copyHeld)

	at(3<<30+5, 8)
	at(5, 8)
	at(3<<30+9, 8)
	at(9, 8)

	if src.calls != 2 || w.held != 2*copyWindow {
		t.Errorf(
			"two windows 3 GiB apart, each asked twice: %d reads, %d held; want 2 and %d",
			src.calls,
			w.held,
			2*copyWindow,
		)
	}
}

// TestUntrustedCopyWindowsDoNotTakeAShortReadForRead covers a source that
// answers a read short, or with an error of its own: the bytes it did not
// give are not zeros of the image, and its error is not the image's fault.
func TestUntrustedCopyWindowsDoNotTakeAShortReadForRead(t *testing.T) {
	t.Parallel()

	for _, room := range []int64{copyHeld, 0} {
		// The window's read and the range's own both end at the third
		// window's end, a byte past what the source gives.
		short := newImageWindows(&windowSource{short: 3*copyWindow - 1}, windowSourceSize, room)

		if _, err := short.span(3*copyWindow-64, 64); !errors.Is(err, ErrCorrupt) {
			t.Errorf("room %d: a read that came back short: %v; want ErrCorrupt", room, err)
		}

		failing := newImageWindows(&windowSource{fails: 3*copyWindow - 1}, windowSourceSize, room)

		if _, err := failing.span(3*copyWindow-64, 64); !errors.Is(err, errWindowSource) || errors.Is(err, ErrCorrupt) {
			t.Errorf("room %d: a read that failed: %v; want the source's error and not ErrCorrupt", room, err)
		}
	}
}

// TestUntrustedHoleSizeIsBounded covers a flat-plain file at the null
// address, a hole, whose i_size is all that sizes it: nothing in the image
// backs the bytes it declares. A hole of 2 GiB is copied, which also holds
// the directory's 1 GiB bound to directories; one of 1 PiB is refused by
// CopyFrom, with nothing built. The limit is 1<<27 blocks: a hole of
// exactly that is copied, and one a byte or a block longer is not. A hole
// that is copied is one chunk, an index entry whatever its size, so the
// copy is no larger than its source. The copy with its data is held to the
// same: the hole is a hole there too, and none of it is written out. So is
// the copy of the image behind a wrapper, which the walk reads and not
// copyFromImage: the bound and the hole are the file's, not the source's.
// A wrapper may hide the file's info and still open the image's file, where
// the hole is kept and bounded all the same; or hide the file and report
// the image's info, where the hole is zeros to the copy and only the bound
// holds, so that source is given the sizes that are refused and no other.
// Metadata-only no file is opened: behind a wrapper of its info the file is
// any source's file of that size with no range, copied as one hole whatever
// the size, which costs an entry for every 1<<31 blocks.
//
//nolint:paralleltest // measures the process's TotalAlloc against a budget; a parallel test's allocations would count against it
func TestUntrustedHoleSizeIsBounded(t *testing.T) {
	const limit = maxChunkIndexEntries

	// Sizes at the limit are in blocks of the image's own size.
	for _, tc := range []struct {
		name string
		size func(blockSize uint64) uint64
		want error
	}{
		{"2 GiB", func(uint64) uint64 { return 2 << 30 }, nil},
		{"1 PiB", func(uint64) uint64 { return 1 << 50 }, ErrInvalid},
		{"the limit", func(bs uint64) uint64 { return limit * bs }, nil},
		{"a byte past the limit", func(bs uint64) uint64 { return limit*bs + 1 }, ErrInvalid},
		{"a block past the limit", func(bs uint64) uint64 { return (limit + 1) * bs }, ErrInvalid},
	} {
		for _, mode := range []struct {
			name string
			opts []CopyOpt
			wrap func(fs.FS) fs.FS
		}{
			{"metadata-only", []CopyOpt{MetadataOnly()}, nil},
			{"with its data", nil, nil},
			{"metadata-only, wrapped", []CopyOpt{MetadataOnly()}, func(f fs.FS) fs.FS { return struct{ fs.FS }{f} }},
			{"with its data, wrapped", nil, func(f fs.FS) fs.FS { return struct{ fs.FS }{f} }},
			{"metadata-only, its info wrapped", []CopyOpt{MetadataOnly()}, func(f fs.FS) fs.FS { return infoWrapped{f} }},
			{"with its data, its info wrapped", nil, func(f fs.FS) fs.FS { return infoWrapped{f} }},
			{"with its data, its files wrapped", nil, func(f fs.FS) fs.FS { return filesWrapped{f} }},
		} {
			// Behind a wrapper of its files a hole that is copied is
			// written out, all of it.
			if strings.HasSuffix(mode.name, "files wrapped") && tc.want == nil {
				continue
			}

			want := tc.want
			if mode.name == "metadata-only, its info wrapped" {
				want = nil
			}

			t.Run(tc.name+", "+mode.name, func(t *testing.T) {
				holeSizeIsBounded(t, tc.size, want, mode.opts, mode.wrap)
			})
		}
	}
}

// infoWrapped is a source whose directories list the image's entries with
// their info behind a type of its own: the walk sees no range of theirs,
// and still opens the image's files.
type infoWrapped struct{ fs.FS }

func (w infoWrapped) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(w.FS, name)
	for i, e := range entries {
		entries[i] = infoWrappedEntry{e}
	}

	return entries, err
}

type infoWrappedEntry struct{ fs.DirEntry }

func (e infoWrappedEntry) Info() (fs.FileInfo, error) {
	info, err := e.DirEntry.Info()
	if err != nil {
		return nil, err
	}

	return struct{ fs.FileInfo }{info}, nil
}

// filesWrapped is a source that opens the image's regular files behind a
// type of its own: the walk sees their info, and reads them as any file.
type filesWrapped struct{ fs.FS }

func (w filesWrapped) Open(name string) (fs.File, error) {
	f, err := w.FS.Open(name)
	if err != nil {
		return nil, err
	}

	if _, dir := f.(fs.ReadDirFile); dir {
		return f, nil
	}

	return struct{ fs.File }{f}, nil
}

// holeSizeIsBounded copies an image whose file is a flat hole of the size
// given, itself or behind a wrapper, and holds the copy to its error, its
// allocations and its size.
func holeSizeIsBounded(
	t *testing.T, sizeOf func(blockSize uint64) uint64, want error, opts []CopyOpt, wrap func(fs.FS) fs.FS,
) {
	t.Helper()

	buf, nid := buildTamperableImage(t)

	img0, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}

	size := sizeOf(uint64(img0.(*image).blockSize()))

	// An extended inode's start block is 48 bits, i_nb its high 16: all
	// ones is the null address.
	inodeOff := img0.(*image).metaStartPos() + int64(nid)*disk.SizeInodeCompact
	binary.LittleEndian.PutUint16(buf[inodeOff:], disk.InodeFormatExtended|disk.LayoutFlatPlain<<1)
	binary.LittleEndian.PutUint16(buf[inodeOff+6:], 0xffff)
	binary.LittleEndian.PutUint64(buf[inodeOff+8:], size)
	binary.LittleEndian.PutUint32(buf[inodeOff+16:], 0xffffffff)
	buf[disk.SuperBlockOffset+8] &^= disk.FeatureCompatSBChksum

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("tampered image failed to open: %v", err)
	}

	if wrap != nil {
		img = wrap(img)
	}

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)

	out := &seekBuf{}
	dst := Create(out)

	err = dst.CopyFrom(img, opts...)
	if !errors.Is(err, want) {
		t.Errorf("CopyFrom: %v, want %v", err, want)
	}

	// A copy that should have been refused is not written out: it may be
	// the hole, as zeros.
	if err == nil && want == nil {
		if cerr := dst.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	}

	runtime.ReadMemStats(&after)
	grew := after.TotalAlloc - before.TotalAlloc

	t.Logf("CopyFrom: %v (allocated %d bytes, wrote %d)", err, grew, len(out.buf))

	if grew > 64<<20 {
		t.Errorf("allocated %d bytes copying a hole of %d bytes; want under 64 MiB", grew, size)
	}

	if len(out.buf) > len(buf) {
		t.Errorf("the copy of a %d byte image holding a hole of %d bytes is %d bytes", len(buf), size, len(out.buf))
	}
}

// patchDirentNid rewrites every 8-byte dirent nid slot equal to from so it
// points at to, turning a valid tree into a cyclic graph. The slots are
// found by their bytes, so their number is the caller's to state: the
// entry that names the inode, and for a directory its own "." and the
// ".." of each directory in it, which the rewritten entry leaves out of
// reach. Another count is bytes that were no dirent's.
func patchDirentNid(t *testing.T, buf []byte, from, to uint64, slots int) {
	t.Helper()

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

	if n != slots {
		t.Fatalf("test premise broken: nid %d is in %d slots of the image, want %d", from, n, slots)
	}
}

// direntNameMax is EROFS_NAME_LEN: the kernel refuses to look up a longer
// name, though the format itself carries any length.
const direntNameMax = 255

// buildCyclicImage returns an image whose directory /d contains a dirent
// named "e" that points back at /d itself, both names nameLen bytes long.
func buildCyclicImage(t *testing.T, nameLen int) []byte {
	t.Helper()

	return buildCycle(t, nameLen, false)
}

// buildCycle is buildCyclicImage, or with toRoot an image whose root holds
// a dirent that points back at the root itself: a cycle through no other
// directory.
func buildCycle(t *testing.T, nameLen int, toRoot bool) []byte {
	t.Helper()

	d := strings.Repeat("d", nameLen)
	e := strings.Repeat("e", nameLen)

	out := &seekBuf{}

	w := Create(out, WithBuildTime(1000, 0))
	if err := w.Mkdir("/"+d, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := w.Mkdir("/"+d+"/"+e, 0o755); err != nil {
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

	i, ok := img.(*image)
	if !ok {
		t.Fatalf("Open returned %T, want *image", img)
	}

	dNid, _, _, err := i.resolve("x", d, false)
	if err != nil {
		t.Fatal(err)
	}

	eNid, _, _, err := i.resolve("x", d+"/"+e, false)
	if err != nil {
		t.Fatal(err)
	}

	// e's entry and its ".", or d's with the ".." of e besides.
	slots := 2

	if toRoot {
		eNid, dNid, slots = dNid, uint64(i.sb.RootNid), 3
	}

	patchDirentNid(t, buf, eNid, dNid, slots)

	return buf
}

// TestUntrustedDirectoryCycleThroughTheRoot: a root that holds itself is
// refused at the first return like any other cycle, in both modes, though
// the root of an image this package writes is nid 0 and 0 is what a source
// with no inode identity reports.
func TestUntrustedDirectoryCycleThroughTheRoot(t *testing.T) {
	t.Parallel()

	buf := buildCycle(t, 1, true)

	for _, metadataOnly := range []bool{false, true} {
		img, err := Open(bytes.NewReader(buf))
		if err != nil {
			t.Fatalf("cyclic image failed to open: %v", err)
		}

		if root := img.(*image).sb.RootNid; root != 0 {
			t.Fatalf("the premise: the root is nid %d, want 0", root)
		}

		var opts []CopyOpt
		if metadataOnly {
			opts = append(opts, MetadataOnly())
		}

		err = Create(&seekBuf{}).CopyFrom(img, opts...)
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("CopyFrom (metadata only: %t) = %v, want ErrCorrupt", metadataOnly, err)
		}
	}
}

// TestCopyFromAWriterNotClosed: a Writer not yet closed is a source whose
// entries carry no inode identity; its directories are not taken for one
// directory named twice.
func TestCopyFromAWriterNotClosed(t *testing.T) {
	t.Parallel()

	source := Create(&seekBuf{})

	for _, dir := range []string{"/a", "/a/b", "/c"} {
		if err := source.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	out := &seekBuf{}
	w := Create(out)

	if err := w.CopyFrom(source); err != nil {
		t.Fatalf("CopyFrom a Writer not closed: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	img, err := Open(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatal(err)
	}

	for _, dir := range []string{"a", "a/b", "c"} {
		if info, err := fs.Stat(img, dir); err != nil || !info.IsDir() {
			t.Errorf("%s in the copy: %v", dir, err)
		}
	}
}

// TestUntrustedDirectoryCycleTerminates covers a dirent graph that is not a
// tree. Without a visited set the BFS revisits the same subtree forever,
// growing the queue and the path strings without bound.
//
//nolint:paralleltest // measures the process's TotalAlloc against a budget; a parallel test's allocations would count against it
func TestUntrustedDirectoryCycleTerminates(t *testing.T) {
	buf := buildCyclicImage(t, 1)

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

		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("err = %v, want ErrCorrupt", err)
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
// is the writer's own record of the directories it placed, which refuses the
// cycle's first return to one; the path length bound stands behind it for a
// source with no inode identity.
//
//nolint:paralleltest // measures the process's heap in use against a budget; a parallel test's live heap would count against it
func TestUntrustedDirectoryCycleFullImageTerminates(t *testing.T) {
	// Names at the dirent maximum hit the path bound seventeen levels down.
	// Shorter ones reach it thousands of levels down, and since every step
	// re-resolves its path from the root the walk turns quadratic — slow
	// enough under the race detector to blow the deadline below.
	buf := buildCyclicImage(t, direntNameMax)

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("cyclic image failed to open: %v", err)
	}

	done := make(chan error, 1)

	go func() {
		dst := Create(&seekBuf{})
		done <- dst.CopyFrom(img)
	}()

	select {
	case err := <-done:
		var after runtime.MemStats

		//revive:disable-next-line:call-to-gc resident memory is measured from a collected heap
		runtime.GC()
		runtime.ReadMemStats(&after)

		if err == nil {
			t.Fatal("full-image CopyFrom accepted an image with a directory cycle")
		}

		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("err = %v, want ErrCorrupt", err)
		}
		// What has to stay bounded is resident memory, which is what an OOM
		// is made of.
		t.Logf("rejected with %v (heap in use after GC %d bytes)", err, after.HeapInuse)

		if after.HeapInuse > 256<<20 {
			t.Errorf("heap in use is %d bytes after a cyclic image; want well under 256 MiB", after.HeapInuse)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("full-image CopyFrom did not terminate on a cyclic image")
	}
}

// errBudget stops the cyclic walk below once it has visited enough.
var errBudget = errors.New("visit budget exhausted")

// TestReaderWalkOnCyclicImage records the reader's behaviour on the same
// image. fs.WalkDir has no cycle detection of its own, so the walk is
// expected to keep descending; the callback bounds it so this test can
// assert the exposure without running away.
func TestReaderWalkOnCyclicImage(t *testing.T) {
	t.Parallel()

	buf := buildCyclicImage(t, 1)

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("cyclic image failed to open: %v", err)
	}

	const budget = 200

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
func (badSizeInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (i badSizeInfo) IsDir() bool      { return i.dir }
func (badSizeInfo) Sys() any           { return nil }

type badSizeDirent struct{ info badSizeInfo }

func (d badSizeDirent) Name() string               { return d.info.name }
func (d badSizeDirent) IsDir() bool                { return d.info.dir }
func (d badSizeDirent) Type() fs.FileMode          { return d.info.Mode().Type() }
func (d badSizeDirent) Info() (fs.FileInfo, error) { return d.info, nil }

type badSizeDir struct{ fsys badSizeFS }

func (badSizeDir) Close() error             { return nil }
func (badSizeDir) Read([]byte) (int, error) { return 0, fs.ErrInvalid }
func (badSizeDir) Stat() (fs.FileInfo, error) {
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
	t.Parallel()

	out := &seekBuf{}
	w := Create(out)

	err := w.CopyFrom(badSizeFS{size: -1})
	if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrCorrupt) {
		t.Fatalf("CopyFrom of a source with Size() == -1: %v, want ErrInvalid and not ErrCorrupt", err)
	}
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
	t.Parallel()

	out := &seekBuf{}
	w := Create(out)

	f, err := w.Create("/secret")
	if err != nil {
		t.Fatal(err)
	}

	if _, err = f.Write([]byte("TOPSECRET")); err != nil {
		t.Fatal(err)
	}

	if err = f.Close(); err != nil {
		t.Fatal(err)
	}

	if err = w.Mkdir("/sub", 0o755); err != nil {
		t.Fatal(err)
	}

	if err = w.Symlink("x", "/sub/el"); err != nil {
		t.Fatal(err)
	}

	if err = w.Close(); err != nil {
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

	nid := fi.Sys().(*Stat).Ino

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
	} else if !errors.Is(err, ErrCorrupt) {
		t.Errorf("ReadLink err = %v, want ErrCorrupt", err)
	}

	if fi, err := fs.Stat(img, "sub/el"); err == nil {
		t.Errorf("Stat(sub/el) = mode=%v isdir=%v, want an error (it resolved to the root)",
			fi.Mode(), fi.IsDir())
	} else if !errors.Is(err, ErrCorrupt) {
		t.Errorf("Stat(sub/el) err = %v, want ErrCorrupt", err)
	}

	if data, err := fs.ReadFile(img, "sub/el/secret"); err == nil {
		t.Errorf("ReadFile(sub/el/secret) = %q, want an error (the path was re-rooted at /)", data)
	} else if !errors.Is(err, ErrCorrupt) {
		t.Errorf("ReadFile(sub/el/secret) err = %v, want ErrCorrupt", err)
	}
}

// TestWriterRejectsEmptySymlinkTarget checks both entry points: the direct API
// and a CopyFrom source that reports an empty target.
func TestWriterRejectsEmptySymlinkTarget(t *testing.T) {
	t.Parallel()

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

	i, ok := img.(*image)
	if !ok {
		t.Fatalf("Open returned %T, want *image", img)
	}

	aNid, _, _, err := i.resolve("x", "a", false)
	if err != nil {
		t.Fatal(err)
	}

	patchDirentNid(t, buf, aNid, uint64(i.sb.RootNid), 2)

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
	t.Parallel()

	// maxSymlinks hops over at most maxPathLen/2 components each, at a couple
	// of reads per component. Clear of the real figure (~1M) and far below the
	// tens of millions the unbounded walk reached.
	const budget = 4 * maxSymlinks * (maxPathLen / 2)

	for _, tc := range []struct {
		name   string
		target string
		want   error
	}{
		// Relative: every hop prepends the prefix already walked, so the
		// path grows and the work per hop grows with it, until the path is
		// over its limit.
		{"relative", strings.Repeat("a/", 500) + "l", ErrInvalid},
		// Absolute: the prefix is not prepended, so the path keeps its
		// length. Just under PATH_MAX is the most expensive shape that
		// survives the per-hop cap — the steady state the budget must hold.
		{"absolute", "/" + strings.Repeat("a/", (maxPathLen-4)/2) + "l", ErrLoop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			buf := buildSymlinkCycleImage(t, tc.target)

			cr := &countingReaderAt{ra: bytes.NewReader(buf)}

			img, err := Open(cr)
			if err != nil {
				t.Fatal(err)
			}

			// A chain of links that never ends is a valid image.
			if _, err := img.Open("l"); !errors.Is(err, tc.want) || errors.Is(err, ErrCorrupt) {
				t.Fatalf("Open of a cyclic symlink chain: %v, want %v and not ErrCorrupt", err, tc.want)
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
	t.Parallel()

	// Not a target of four bytes: the link's i_size would read as "a"'s
	// nid, 4, and be patched with it.
	buf := buildSymlinkCycleImage(t, "/a/a/l")

	cr := &countingReaderAt{ra: bytes.NewReader(buf)}

	img, err := Open(cr)
	if err != nil {
		t.Fatal(err)
	}

	before := cr.calls

	// The caller's argument, not the image's fault.
	_, err = img.Open(strings.Repeat("a/", maxPathLen) + "l")
	if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open of a path over PATH_MAX: %v, want ErrInvalid and not ErrCorrupt", err)
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
	t.Parallel()

	out := &seekBuf{}
	w := Create(out)

	f, err := w.Create("/AAAAAAAAAA")
	if err != nil {
		t.Fatal(err)
	}

	if _, err = f.Write([]byte("pwned")); err != nil {
		t.Fatal(err)
	}

	if err = f.Close(); err != nil {
		t.Fatal(err)
	}

	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	patchDirentName(t, out.buf, "AAAAAAAAAA", "../../evil")

	img, err := Open(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatalf("tampered image failed to open: %v", err)
	}

	var ents []fs.DirEntry
	if ents, err = fs.ReadDir(img, "."); err == nil {
		for _, e := range ents {
			t.Errorf("ReadDir returned name %q, want an error", e.Name())
		}
	} else if !errors.Is(err, ErrCorrupt) {
		t.Errorf("ReadDir err = %v, want ErrCorrupt", err)
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
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("WalkDir of an image with a separator in a dirent name: %v, want ErrCorrupt", err)
	}
}

// TestMergeWhiteoutCannotEscapeItsDirectory covers a dirent name carrying a
// path traversal in merge mode. copyFromImage built childPath by plain
// concatenation, so a nested entry named "y/../../../.wh..wh..opq" produced a
// path whose path.Dir collapses to "/" — and the opaque-whiteout branch then
// removed every entry contributed by every prior layer.
func TestMergeWhiteoutCannotEscapeItsDirectory(t *testing.T) {
	t.Parallel()

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

	if _, err = keep.Write([]byte("PRIOR LAYER DATA")); err != nil {
		t.Fatal(err)
	}

	if err = keep.Close(); err != nil {
		t.Fatal(err)
	}

	if err = dst.Mkdir("/etc", 0o755); err != nil {
		t.Fatal(err)
	}

	err = dst.CopyFrom(hostile, MetadataOnly(), Merge())
	if err == nil {
		t.Fatal("merge accepted a dirent name containing a path separator")
	}

	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("err = %v, want ErrCorrupt", err)
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
// The image has to open for any of them to be driven.
func exerciseUntrusted(t *testing.T, buf []byte) {
	t.Helper()

	img, err := Open(&strictReaderAt{t: t, buf: buf})
	if err != nil {
		t.Fatalf("test premise broken: the tampered image does not open: %v", err)
	}

	_ = fs.WalkDir(img, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // what the call returns is not the point; the walk goes on
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
// a panic for a slice-backed one.
func TestUntrustedNidStaysInBounds(t *testing.T) {
	t.Parallel()

	buf, _ := buildTamperableImage(t)

	img0, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}

	fNid, _, _, err := img0.(*image).resolve("x", "f", false)
	if err != nil {
		t.Fatal(err)
	}

	patchDirentNid(t, buf, fNid, 1<<58, 1)

	exerciseUntrusted(t, buf)
}

// TestUntrustedChunkAddrStaysInBounds covers a chunk index whose physical
// block address, shifted by BlkSizeBits, overflows int64. The 16-bit high half
// only reaches that at the maximum block size, which is why the default-sized
// fuzz corpus never produced it.
func TestUntrustedChunkAddrStaysInBounds(t *testing.T) {
	t.Parallel()

	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0), WithBlockSize(65536))

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

	if err = w.Chtimes("/f", time.Unix(2000, 0), time.Unix(2000, 0)); err != nil {
		t.Fatal(err)
	}

	if err = w.Close(); err != nil {
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

	nid := fi.Sys().(*Stat).Ino
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
//
//nolint:paralleltest // measures the process's TotalAlloc against a budget; a parallel test's allocations would count against it
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
	t.Parallel()

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

	if _, err = f.Write(bytes.Repeat([]byte{0xAB}, blocks*4096)); err != nil {
		t.Fatal(err)
	}

	if err = f.Close(); err != nil {
		t.Fatal(err)
	}

	if err = w.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}

	for i := range links {
		if err = w.Link("/f0", fmt.Sprintf("/d/l%06d", i)); err != nil {
			t.Fatal(err)
		}
	}

	if err = w.Close(); err != nil {
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

	// The source names one inode 2001 times. The copy reproduces that as
	// one inode — one entry carrying the chunk map — and 2000 aliases of
	// it, so the map is parsed and held exactly once.
	owner := dst.byPath["/f0"]
	if owner == nil || owner.linkTo != nil || len(owner.chunks) == 0 {
		t.Fatalf("/f0 = %+v, want the entry that owns the chunk map", owner)
	}

	if cap(owner.chunks) != len(owner.chunks) {
		t.Errorf("chunk slice holds %d entries with capacity for %d", len(owner.chunks), cap(owner.chunks))
	}

	entries, aliases := 0, 0

	for _, e := range dst.byPath {
		switch {
		case len(e.chunks) > 0:
			entries++
		case e.linkTo == owner:
			aliases++
		}
	}

	if entries != 1 || aliases != links {
		t.Fatalf("%d entries carry chunks and %d alias /f0, want 1 and %d", entries, aliases, links)
	}

	if owner.extraLinks != links {
		t.Errorf("/f0 has %d extra links, want %d", owner.extraLinks, links)
	}

	t.Logf("%d names share one %d-entry chunk map", aliases+1, len(owner.chunks))
}

// TestUnalignedDataFileStart covers a data file that does not begin on a block
// boundary. A chunk records a file's start as a block number, so the offset
// has to divide exactly; the first file inherited dataOff from a Seek to the
// end of a pre-existing data file, and the division truncated it onto an
// earlier block, so the file read back the bytes already there.
func TestUnalignedDataFileStart(t *testing.T) {
	t.Parallel()

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
	if _, err = f.Write(want); err != nil {
		t.Fatal(err)
	}

	if err = f.Close(); err != nil {
		t.Fatal(err)
	}

	if err = w.Close(); err != nil {
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
	t.Parallel()

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

	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("ReadFile err = %v, want ErrCorrupt", err)
	}
}

// TestXattrPrefixAndDuplicates covers the two ways an image can put the same
// xattr name in the map twice: an undefined name index, which used to map to
// the empty prefix and let "security.capability" be spelled out in full, and a
// plain repeat of a key, which the map silently took last-one-wins. An entry
// under an undefined index is left out, as the kernel lists it on no mount.
func TestXattrPrefixAndDuplicates(t *testing.T) {
	t.Parallel()

	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0))

	f, err := w.Create("/f")
	if err != nil {
		t.Fatal(err)
	}

	if err = f.Close(); err != nil {
		t.Fatal(err)
	}

	if err = w.Setxattr("/f", "security.capability", "real"); err != nil {
		t.Fatal(err)
	}

	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	buf := out.buf

	img0, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}

	var fi fs.FileInfo
	if fi, err = fs.Stat(img0, "f"); err != nil {
		t.Fatal(err)
	}

	st, ok := fi.Sys().(*Stat)
	if !ok {
		t.Fatalf("Sys returned %T, want *Stat", fi.Sys())
	}

	if st.Xattrs["security.capability"] != "real" {
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

	hidden, err := fs.Stat(img, "f")
	if err != nil {
		t.Fatalf("Stat of a file with an entry under an undefined name index: %v", err)
	}

	if got := hidden.Sys().(*Stat).Xattrs; len(got) != 0 {
		t.Errorf("an entry under an undefined name index is listed: %v", got)
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
// door an image's attributes come in through. The reader leaves out an entry
// under an undefined name index and rejects a duplicated key
// (TestXattrPrefixAndDuplicates); the fast-path parser used to map the index
// to the empty prefix and take the duplicate last-wins, so a spoofed
// security.capability would be copied by CopyFrom(MetadataOnly).
func TestCopyFromImageXattrParity(t *testing.T) {
	t.Parallel()

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
		t.Parallel()

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

		// The copy holds what a mount of the source shows: the file,
		// without the entry no mount lists.
		img, err := Open(bytes.NewReader(buf))
		if err != nil {
			t.Fatal(err)
		}

		out := &seekBuf{}
		w := Create(out, WithBuildTime(1000, 0))

		if err = errors.Join(w.CopyFrom(img, MetadataOnly()), w.Close()); err != nil {
			t.Fatalf("copy of an image with an entry under an undefined name index: %v", err)
		}

		copied, err := Open(bytes.NewReader(out.buf), WithExtraDevices(bytes.NewReader(buf)))
		if err != nil {
			t.Fatal(err)
		}

		info, err := fs.Stat(copied, "f")
		if err != nil {
			t.Fatal(err)
		}

		if got := info.Sys().(*Stat).Xattrs; len(got) != 0 {
			t.Errorf("the copy lists an entry under an undefined name index: %v", got)
		}
	})

	// A hidden entry is read over, not skipped unread: one whose header
	// claims more than its area holds is corrupt in a copy as in the reader.
	t.Run("hiddenEntryPastItsArea", func(t *testing.T) {
		t.Parallel()

		for name, edit := range map[string]func(header []byte){
			"a value of 65535 bytes": func(h []byte) { h[2], h[3] = 0xff, 0xff },
			"a name of 255 bytes":    func(h []byte) { h[0] = 0xff },
		} {
			buf := build(t, map[string]string{"security.capability": "real"})

			// e_name_len, e_name_index, e_value_size: the four bytes
			// before the stored name.
			i := bytes.Index(buf, []byte("capability"))
			if i < 4 || buf[i-3] != 6 {
				t.Fatal("could not locate the stored xattr entry")
			}

			buf[i-3] = 7
			edit(buf[i-4 : i])

			if err := copyMeta(t, buf); !errors.Is(err, ErrCorrupt) {
				t.Errorf("%s on a hidden entry: the copy is %v, want ErrCorrupt", name, err)
			}
		}
	})

	t.Run("duplicateKey", func(t *testing.T) {
		t.Parallel()

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

		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("err = %v, want ErrCorrupt", err)
		}
	})
}

// TestEmptyDirentNameIsRejected covers a dirent whose name is zero bytes long.
// No writer emits one, and as a path element it names the directory itself,
// so a lookup for it silently succeeds; the reader used to hand it out as an
// entry, and the copyFromImage fast path silently skipped over it — neither
// treated the image as the corrupt thing it is.
func TestEmptyDirentNameIsRejected(t *testing.T) {
	t.Parallel()

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

	var ents []fs.DirEntry
	if ents, err = fs.ReadDir(img, "."); err == nil {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name())
		}

		t.Errorf("ReadDir returned %q, want an error", names)
	} else if !errors.Is(err, ErrCorrupt) {
		t.Errorf("ReadDir err = %v, want ErrCorrupt", err)
	}

	err = Create(&seekBuf{}).CopyFrom(img, MetadataOnly())
	if err == nil {
		t.Error("CopyFrom accepted an empty dirent name")
	} else if !errors.Is(err, ErrCorrupt) {
		t.Errorf("CopyFrom err = %v, want ErrCorrupt", err)
	}
}

// buildFlatPlainImage writes an image with one file large enough to be laid
// out flat-plain (its data in whole blocks at a block address), and returns
// the image plus the byte offset of that file's inode.
func buildFlatPlainImage(t *testing.T) ([]byte, int64) {
	t.Helper()

	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0))

	f, err := w.Create("/big")
	if err != nil {
		t.Fatal(err)
	}

	if _, err = f.Write(bytes.Repeat([]byte("x"), 2*4096)); err != nil {
		t.Fatal(err)
	}

	if err = f.Close(); err != nil {
		t.Fatal(err)
	}

	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	img, err := Open(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatal(err)
	}

	fi, err := fs.Stat(img, "big")
	if err != nil {
		t.Fatal(err)
	}

	if fi.(*fileInfo).layout != disk.LayoutFlatPlain {
		t.Fatalf("test premise broken: layout = %d, want flat-plain", fi.(*fileInfo).layout)
	}

	nid := fi.Sys().(*Stat).Ino

	return out.buf, img.(*image).metaStartPos() + int64(nid)*disk.SizeInodeCompact
}

// TestFlatPlainAddressIsBounded patches a flat-plain inode's data block
// address to lie past the image. Chunk addresses were already bounded; the
// flat address was handed to the reader as-is — and published as
// DataRange.Offset — so a slice-backed io.ReaderAt indexed with it.
func TestFlatPlainAddressIsBounded(t *testing.T) {
	t.Parallel()

	buf, inodeOff := buildFlatPlainImage(t)
	// i_u (the data block address for a flat-plain inode) sits at byte 16 of
	// both inode formats.
	binary.LittleEndian.PutUint32(buf[inodeOff+16:], 0xFFFFFFF0)

	for _, tc := range []struct {
		name string
		ra   io.ReaderAt
	}{
		{"sized", bytes.NewReader(buf)},
		{"strict", &strictReaderAt{t: t, buf: buf}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			img, err := Open(tc.ra)
			if err != nil {
				t.Fatalf("tampered image failed to open: %v", err)
			}

			if _, err := fs.Stat(img, "big"); !errors.Is(err, ErrCorrupt) {
				t.Errorf("Stat err = %v, want ErrCorrupt (the address is published as DataRange.Offset)", err)
			}

			if _, err := fs.ReadFile(img, "big"); !errors.Is(err, ErrCorrupt) {
				t.Errorf("ReadFile err = %v, want ErrCorrupt", err)
			}

			if err := Create(&seekBuf{}).CopyFrom(img); !errors.Is(err, ErrCorrupt) {
				t.Errorf("CopyFrom of a file whose data lies outside the image: %v, want ErrCorrupt", err)
			}
		})
	}
}

// TestCopyFromImageInlineCrossingBlock bumps an inline symlink's size so its
// target would run past the inode's block. The reader refuses that; the
// metadata-only copy used to take the bytes that followed as the target, or
// an empty one when nothing followed. The bytes that follow are made a
// target anything would take, so that nothing but the block's edge refuses
// them.
func TestCopyFromImageInlineCrossingBlock(t *testing.T) {
	t.Parallel()

	out := &seekBuf{}

	w := Create(out, WithBuildTime(1000, 0))
	if err := w.Symlink("target", "/l"); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// One block more, for the target to run into: the link's inode is the
	// last thing in the image. sb.Blocks sits at offset 36 within the
	// superblock, and the checksum is switched off, the tamper leaving it
	// stale.
	buf := make([]byte, len(out.buf)+4096)
	copy(buf, out.buf)
	binary.LittleEndian.PutUint32(buf[disk.SuperBlockOffset+36:], uint32(len(buf)/4096))
	buf[disk.SuperBlockOffset+8] &^= disk.FeatureCompatSBChksum

	img0, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}

	nid, _, _, err := img0.(*image).resolve("l", "l", false)
	if err != nil {
		t.Fatal(err)
	}

	inodeOff := img0.(*image).metaStartPos() + int64(nid)*disk.SizeInodeCompact

	format := binary.LittleEndian.Uint16(buf[inodeOff:])
	if (format&0x0E)>>1 != disk.LayoutFlatInline {
		t.Fatalf("test premise broken: format %#x, want a flat-inline inode", format)
	}
	// i_size starts at byte 8 of both inode formats: a uint32 in a compact
	// inode, a uint64 in an extended one. Claim a byte short of a block:
	// still inline, still a target the package takes, and past the end of
	// the block from anywhere after an inode.
	inline := inodeOff + disk.SizeInodeCompact

	if format&0x01 == 0 {
		binary.LittleEndian.PutUint32(buf[inodeOff+8:], 4095)
	} else {
		binary.LittleEndian.PutUint64(buf[inodeOff+8:], 4095)
		inline = inodeOff + disk.SizeInodeExtended
	}

	if xattrs := binary.LittleEndian.Uint16(buf[inodeOff+2:]); xattrs != 0 {
		t.Fatalf("test premise broken: the link has xattrs (i_xattr_icount %d)", xattrs)
	}

	if inline+4095 > int64(len(buf)) || inline%4096+4095 <= 4096 {
		t.Fatalf("test premise broken: 4095 bytes from %d do not cross a block inside the %d byte image",
			inline, len(buf))
	}

	copy(buf[inline:], bytes.Repeat([]byte{'a'}, 4095))

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("tampered image failed to open: %v", err)
	}

	var target string
	if target, err = img.(interface {
		ReadLink(name string) (string, error)
	}).ReadLink("l"); !errors.Is(err, ErrCorrupt) {
		t.Errorf("ReadLink returned %q, %v, want ErrCorrupt", target, err)
	}

	err = Create(&seekBuf{}).CopyFrom(img, MetadataOnly())
	if err == nil {
		t.Error("CopyFrom accepted inline data crossing its block")
	} else if !errors.Is(err, ErrCorrupt) {
		t.Errorf("CopyFrom err = %v, want ErrCorrupt", err)
	}
}

// TestCopyFromImageTruncatedXattr overstates an inline xattr's value length
// so the entry runs past the inode's xattr area. The reader errors; the
// metadata-only copy used to stop parsing and drop the attribute silently.
func TestCopyFromImageTruncatedXattr(t *testing.T) {
	t.Parallel()

	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0))

	f, err := w.Create("/f")
	if err != nil {
		t.Fatal(err)
	}

	if err = f.Close(); err != nil {
		t.Fatal(err)
	}

	if err = w.Setxattr("/f", "security.capability", "real"); err != nil {
		t.Fatal(err)
	}

	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	buf := out.buf
	// The 4-byte entry precedes the stored name: name length, name index,
	// then the little-endian value length.
	i := bytes.Index(buf, []byte("capability"))
	if i < 0 || buf[i-3] != 6 {
		t.Fatal("could not locate the stored xattr entry")
	}

	binary.LittleEndian.PutUint16(buf[i-2:], 4000)

	img, err := Open(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("tampered image failed to open: %v", err)
	}

	if _, err = fs.Stat(img, "f"); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Stat of an xattr running past its area: %v, want ErrCorrupt", err)
	}

	err = Create(&seekBuf{}).CopyFrom(img, MetadataOnly())
	if err == nil {
		t.Error("CopyFrom accepted an xattr running past its area")
	} else if !errors.Is(err, ErrCorrupt) {
		t.Errorf("CopyFrom err = %v, want ErrCorrupt", err)
	}
}
