package erofstest

import (
	"os"
	"os/exec"
	"testing"
)

// FsckErofs runs fsck.erofs on the image at path. Skips if fsck.erofs
// is not installed. Calls tb.Error on validation failure.
func FsckErofs(tb testing.TB, path string) {
	tb.Helper()

	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		return // silently skip
	}

	cmd := exec.CommandContext(tb.Context(), "fsck.erofs", path)

	out, err := cmd.CombinedOutput()
	if err != nil {
		tb.Errorf("fsck.erofs %s failed: %v\n%s", path, err, out)
	}
}

// FsckErofsBytes writes buf to a temp file, runs fsck.erofs, and cleans up.
func FsckErofsBytes(tb testing.TB, buf []byte) {
	tb.Helper()

	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		return
	}

	f, err := os.CreateTemp(tb.TempDir(), "fsck-*.erofs")
	if err != nil {
		tb.Fatal(err)
	}

	if _, err = f.Write(buf); err != nil {
		_ = f.Close()

		tb.Fatal(err)
	}

	if err = f.Close(); err != nil {
		tb.Fatal(err)
	}

	cmd := exec.CommandContext(tb.Context(), "fsck.erofs", f.Name())

	out, err := cmd.CombinedOutput()
	if err != nil {
		tb.Errorf("fsck.erofs failed: %v\n%s", err, out)
	}
}

// FsckAvailable returns true if fsck.erofs is on PATH.
func FsckAvailable() bool {
	_, err := exec.LookPath("fsck.erofs")
	return err == nil
}

// RequireFsck skips tb if fsck.erofs is not available.
func RequireFsck(tb testing.TB) {
	tb.Helper()

	if !FsckAvailable() {
		tb.Skip("fsck.erofs not available")
	}
}

// FsckErofsDevice runs fsck.erofs with --device to validate metadata-only
// images that reference external blob devices.
func FsckErofsDevice(tb testing.TB, imagePath string, devicePaths ...string) {
	tb.Helper()

	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		return
	}

	args := []string{imagePath}
	for _, d := range devicePaths {
		args = append(args, "--device="+d)
	}

	cmd := exec.CommandContext(tb.Context(), "fsck.erofs", args...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		tb.Errorf("fsck.erofs %s failed: %v\n%s", imagePath, err, out)
	}
}
