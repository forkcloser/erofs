package erofs_test

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"testing"

	"github.com/forkcloser/erofs"
)

// testBuffer is an in-memory io.WriteSeeker for tests.
type testBuffer struct {
	buf []byte
	pos int
}

func (b *testBuffer) Write(p []byte) (int, error) {
	end := b.pos + len(p)
	if end > len(b.buf) {
		if end > cap(b.buf) {
			newBuf := make([]byte, end, end*2)
			copy(newBuf, b.buf)
			b.buf = newBuf
		} else {
			b.buf = b.buf[:end]
		}
	}

	copy(b.buf[b.pos:], p)
	b.pos = end

	return len(p), nil
}

func (b *testBuffer) Seek(offset int64, whence int) (int64, error) {
	var abs int64

	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = int64(b.pos) + offset
	case io.SeekEnd:
		abs = int64(len(b.buf)) + offset
	default:
		return 0, fmt.Errorf("testbuf: invalid whence %d", whence) //nolint:err113 // a fake's failure fixture
	}

	if abs < 0 {
		return 0, fmt.Errorf("testbuf: negative position %d", abs) //nolint:err113 // a fake's failure fixture
	}

	b.pos = int(abs)

	return abs, nil
}

func (b *testBuffer) Bytes() []byte {
	return b.buf
}

// writeFile creates, fills and closes one file.
func writeFile(t *testing.T, w *erofs.Writer, name string, data []byte) {
	t.Helper()

	f, err := w.Create(name)
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}

	if _, err := f.Write(data); err != nil {
		t.Fatalf("Write(%q): %v", name, err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close(%q): %v", name, err)
	}
}

func firstByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}

	return b[0]
}

// openWithDevices opens img over its extra devices.
func openWithDevices(t *testing.T, img []byte, devices ...[]byte) fs.FS {
	t.Helper()

	var opts []erofs.OpenOpt

	if len(devices) > 0 {
		readers := make([]io.ReaderAt, len(devices))
		for i, d := range devices {
			readers[i] = bytes.NewReader(d)
		}

		opts = append(opts, erofs.WithExtraDevices(readers...))
	}

	fsys, err := erofs.Open(bytes.NewReader(img), opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return fsys
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

// sameTree holds got to want: the same paths, each with the same type,
// bits, size, time and ownership, the same bytes for a file and the same
// target for a symlink.
func sameTree(t *testing.T, want, got fs.FS) {
	t.Helper()

	seen := 0

	err := fs.WalkDir(want, ".", func(name string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		seen++

		w, err := fs.Lstat(want, name)
		if err != nil {
			return err
		}

		g, err := fs.Lstat(got, name)
		if err != nil {
			t.Errorf("%s: %v in the copy", name, err)

			return nil
		}

		ws, gs := w.Sys().(*erofs.Stat), g.Sys().(*erofs.Stat)
		if w.Mode() != g.Mode() || w.Size() != g.Size() || !w.ModTime().Equal(g.ModTime()) ||
			ws.UID != gs.UID || ws.GID != gs.GID || ws.Rdev != gs.Rdev {
			t.Errorf("%s: copy %v %d %v %d:%d, source %v %d %v %d:%d",
				name, g.Mode(), g.Size(), g.ModTime(), gs.UID, gs.GID,
				w.Mode(), w.Size(), w.ModTime(), ws.UID, ws.GID)
		}

		switch {
		case w.Mode().IsRegular():
			wb, err := fs.ReadFile(want, name)
			if err != nil {
				return err
			}

			gb, err := fs.ReadFile(got, name)
			if err != nil {
				t.Errorf("%s: copy: %v", name, err)
			} else if !bytes.Equal(wb, gb) {
				t.Errorf(
					"%s: copy reads %d bytes %q, source %d %q; bytes %s",
					name,
					len(gb),
					gb[:min(8, len(gb))],
					len(wb),
					wb[:min(8, len(wb))],
					sameBytes(wb, gb),
				)
			}
		case w.Mode()&fs.ModeSymlink != 0:
			wl, err := fs.ReadLink(want, name)
			if err != nil {
				return err
			}

			if gl, err := fs.ReadLink(got, name); err != nil || gl != wl {
				t.Errorf("%s: copy links to %q (%v), source %q", name, gl, err, wl)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking the source: %v", err)
	}

	count := 0

	if err := fs.WalkDir(got, ".", func(string, fs.DirEntry, error) error { count++; return nil }); err != nil {
		t.Fatalf("walking the copy: %v", err)
	}

	if count != seen {
		t.Errorf("the copy holds %d entries, the source %d", count, seen)
	}
}

func sameBytes(a, b []byte) string {
	if bytes.Equal(a, b) {
		return "equal"
	}

	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return "differ from offset " + strconv.Itoa(i)
		}
	}

	return "differ in length"
}
