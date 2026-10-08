package erofs //nolint:testpackage // white-box: forges hostile images from unexported offsets

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"testing"
	"time"

	"github.com/forkcloser/erofs/internal/disk"
)

// errDiskOnFire is the transport failure errReader reports.
var errDiskOnFire = errors.New("disk on fire")

// errReader is a transport failure: a ReaderAt whose every read fails with
// its own error, which the reader must pass through untouched.
type errReader struct{ err error }

func (r errReader) ReadAt([]byte, int64) (int, error) { return 0, r.err }

// TestErrCorruptClassifies pins what ErrCorrupt means: a malformed image
// matches it (and still ErrInvalid), a bad argument matches ErrInvalid alone,
// and a failing reader's error is passed through and matches neither.
func TestErrCorruptClassifies(t *testing.T) {
	t.Parallel()

	t.Run("bad magic is a corrupt superblock", func(t *testing.T) {
		t.Parallel()

		buf, _ := buildTamperableImage(t)
		binary.LittleEndian.PutUint32(buf[disk.SuperBlockOffset:], 0xdeadbeef)

		_, err := Open(bytes.NewReader(buf))
		if err == nil {
			t.Fatal("Open accepted a superblock with a bad magic number")
		}

		for _, want := range []error{ErrInvalidSuperblock, ErrCorrupt, ErrInvalid} {
			if !errors.Is(err, want) {
				t.Errorf("err = %v; want it to match %v", err, want)
			}
		}
	})

	t.Run("inode data past the image is corrupt", func(t *testing.T) {
		t.Parallel()

		buf, nid := buildTamperableImage(t)

		img0, err := Open(bytes.NewReader(buf))
		if err != nil {
			t.Fatal(err)
		}

		inodeOff := img0.(*image).metaStartPos() + int64(nid)*disk.SizeInodeCompact
		// The extended inode keeps its layout but claims a 1 MiB file: its
		// data can no longer sit where the layout says it does.
		binary.LittleEndian.PutUint64(buf[inodeOff+8:], 1<<20)

		img, err := Open(bytes.NewReader(buf))
		if err != nil {
			t.Fatalf("tampered image failed to open: %v", err)
		}

		_, err = fs.ReadFile(img, "f")
		if err == nil {
			t.Fatal("ReadFile accepted an inode whose data lies past the image")
		}

		if !errors.Is(err, ErrCorrupt) || !errors.Is(err, ErrInvalid) {
			t.Errorf("err = %v; want it to match ErrCorrupt and ErrInvalid", err)
		}
	})

	t.Run("xattr header past its area is corrupt", func(t *testing.T) {
		t.Parallel()

		buf, nid := buildXattrImage(t)

		img0, err := Open(bytes.NewReader(buf))
		if err != nil {
			t.Fatal(err)
		}

		// The xattr body header follows the extended inode; its shared
		// count claims more references than i_xattr_icount's area holds.
		headerOff := img0.(*image).metaStartPos() + int64(nid)*disk.SizeInodeCompact + disk.SizeInodeExtended
		buf[headerOff+4] = 255

		img, err := Open(bytes.NewReader(buf))
		if err != nil {
			t.Fatalf("tampered image failed to open: %v", err)
		}

		_, err = fs.Stat(img, "f")
		if err == nil {
			t.Fatal("Stat accepted an xattr header claiming shared references past its area")
		}

		if !errors.Is(err, ErrCorrupt) || !errors.Is(err, ErrInvalid) {
			t.Errorf("err = %v; want it to match ErrCorrupt and ErrInvalid", err)
		}

		t.Logf("rejected with %v", err)
	})

	t.Run("a bad path is ErrInvalid alone", func(t *testing.T) {
		t.Parallel()

		buf, _ := buildTamperableImage(t)

		img, err := Open(bytes.NewReader(buf))
		if err != nil {
			t.Fatal(err)
		}

		_, err = fs.ReadFile(img, "../f")
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("ReadFile(\"../f\") = %v; want ErrInvalid", err)
		}

		if errors.Is(err, ErrCorrupt) {
			t.Errorf("ReadFile(\"../f\") = %v; a bad argument must not match ErrCorrupt", err)
		}
	})

	t.Run("a failing reader is passed through", func(t *testing.T) {
		t.Parallel()

		_, err := Open(errReader{err: errDiskOnFire})
		if !errors.Is(err, errDiskOnFire) {
			t.Fatalf("Open = %v; want the reader's own error", err)
		}

		if errors.Is(err, ErrCorrupt) || errors.Is(err, ErrInvalid) {
			t.Errorf("Open = %v; a transport failure must not match ErrCorrupt or ErrInvalid", err)
		}

		if errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("Open = %v; the reader's error was replaced by a short read", err)
		}
	})
}

// buildXattrImage is buildTamperableImage's image with one xattr on /f, so
// the inode carries an xattr body to tamper with.
func buildXattrImage(t *testing.T) ([]byte, uint64) {
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

	if err = w.Setxattr("/f", "user.k", "v"); err != nil {
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
