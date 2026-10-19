//go:build freebsd || linux

package erofs_test

import (
	"errors"
	"io/fs"
	"syscall"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
)

// TestContractCopyFromHostStatRdev: a device whose source stat is the
// platform's (syscall.Stat_t) keeps its rdev in the image, in the encoding
// Linux's dev_t and i_rdev share, and one whose rdev does not fit the
// 32 bits i_rdev holds is refused as ErrInvalid.
func TestContractCopyFromHostStatRdev(t *testing.T) {
	t.Parallel()

	const rdev = 0x801 // major 8, minor 1

	wide := fstest.MapFS{"dev": &fstest.MapFile{Mode: fs.ModeDevice | 0o600, Sys: &syscall.Stat_t{Rdev: 1<<32 | rdev}}}
	if err := erofs.Create(&testBuffer{}).CopyFrom(wide); !errors.Is(err, erofs.ErrInvalid) {
		t.Errorf("CopyFrom with a 64-bit rdev: %v, want ErrInvalid", err)
	}

	src := fstest.MapFS{"dev": &fstest.MapFile{Mode: fs.ModeDevice | 0o600, Sys: &syscall.Stat_t{Rdev: rdev}}}
	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	if err := errors.Join(w.CopyFrom(src), w.Close()); err != nil {
		t.Fatal(err)
	}

	im := corpus.Image{Name: "host-rdev", Img: out.Bytes()}

	in, err := parseImage(t, im).Resolve("dev", 0)
	if err != nil {
		t.Fatal(err)
	}

	if in.Rdev != rdev {
		t.Errorf("image rdev %#x, want %#x", in.Rdev, rdev)
	}

	info, err := fs.Lstat(openImage(t, im), "dev")
	if err != nil {
		t.Fatal(err)
	}

	if st, ok := info.Sys().(*erofs.Stat); !ok || st.Rdev != rdev {
		t.Errorf("Lstat Sys %T rdev %#x, want *erofs.Stat with %#x", info.Sys(), st.Rdev, rdev)
	}
}
