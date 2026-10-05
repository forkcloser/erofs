//go:build freebsd || linux

package erofs_test

import (
	"bytes"
	"errors"
	"io/fs"
	"syscall"
	"testing"
	"testing/fstest"

	erofs "github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/erofstest"
)

// A 64-bit dev_t keeps a major past 4095 or a minor past 2^20-1 above bit 31,
// where the 32-bit i_rdev cannot hold it: CopyFrom refuses such a device
// rather than store the low half, which names another one.
func TestCopyFromHostStatRejectsWideRdev(t *testing.T) {
	t.Parallel()

	src := fstest.MapFS{
		"dev": &fstest.MapFile{
			Mode: fs.ModeDevice | 0o600,
			Sys:  &syscall.Stat_t{Rdev: 1<<32 | 0x801},
		},
	}

	var buf testBuffer

	err := erofs.Create(&buf).CopyFrom(src)
	if !errors.Is(err, erofs.ErrInvalid) {
		t.Fatalf("CopyFrom with a 64-bit rdev: %v, want ErrInvalid", err)
	}
}

func TestCopyFromHostStatKeepsRdev(t *testing.T) {
	t.Parallel()

	// Major 8, minor 1 (sda1), in the encoding Linux's dev_t and i_rdev share.
	const rdev = 0x801

	src := fstest.MapFS{
		"dev": &fstest.MapFile{
			Mode: fs.ModeDevice | 0o600,
			Sys:  &syscall.Stat_t{Rdev: rdev},
		},
	}

	var buf testBuffer

	w := erofs.Create(&buf)
	if err := w.CopyFrom(src); err != nil {
		t.Fatal("CopyFrom:", err)
	}

	if err := w.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("Open:", err)
	}

	if st := erofstest.Stat(t, efs, "dev"); st.Rdev != rdev {
		t.Errorf("rdev = %#x, want %#x", st.Rdev, rdev)
	}
}
