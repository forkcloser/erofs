package erofs_test

import (
	"errors"
	"io/fs"
	"os"
	"runtime"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
)

// TestContractCopyKeepsTheHostsDeviceNumber: a device copied from the host
// is, to the kernel that mounts the image, the device it is on the host.
// The image holds a number in the kernel's own encoding (kdev_t.h:
// new_encode_dev), which a dev_t has on Linux alone: a darwin st_rdev
// copied as it was made /dev/null, (3, 2) there, the device (0, 12290).
func TestContractCopyKeepsTheHostsDeviceNumber(t *testing.T) {
	t.Parallel()

	// /dev/null, by the platform's own documentation of it.
	want, ok := map[string][2]uint64{"linux": {1, 3}, "darwin": {3, 2}}[runtime.GOOS]
	if !ok {
		t.Skipf("no device known by number on %s", runtime.GOOS)
	}

	host, err := os.Lstat("/dev/null")
	if err != nil {
		t.Fatal(err)
	}

	src := fstest.MapFS{"null": &fstest.MapFile{Mode: host.Mode(), Sys: host.Sys()}}
	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	if err = errors.Join(w.CopyFrom(src), w.Close()); err != nil {
		t.Fatal(err)
	}

	info, err := fs.Lstat(openWithDevices(t, out.Bytes()), "null")
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Type() != fs.ModeDevice|fs.ModeCharDevice {
		t.Fatalf("null is %v in the image; want a character device", info.Mode())
	}

	// The kernel's new_decode_dev.
	rdev := info.Sys().(*erofs.Stat).Rdev
	if got := [2]uint64{rdev & 0xfff00 >> 8, rdev&0xff | rdev>>12&0xfff00}; got != want {
		t.Errorf("the image's /dev/null is device %d:%d to the kernel; the host's is %d:%d",
			got[0], got[1], want[0], want[1])
	}
}
