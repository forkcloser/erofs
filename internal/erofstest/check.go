// Package erofstest holds the fixtures and checks shared by the erofs tests:
// tar inputs, mkfs.erofs and fsck.erofs drivers, and conformance cases.
package erofstest

import (
	"io/fs"
	"path"
	"testing"

	"github.com/forkcloser/erofs"
)

// CheckXattrs verifies that the named path has exactly the expected xattrs.
func CheckXattrs(tb testing.TB, fsys fs.FS, name string, expected map[string]string) {
	tb.Helper()

	fi, err := fs.Stat(fsys, name)
	if err != nil {
		tb.Errorf("stat %s: %v", name, err)
		return
	}

	st, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		tb.Errorf("%s: expected *erofs.Stat from Sys(), got %T", name, fi.Sys())
		return
	}

	if len(st.Xattrs) != len(expected) {
		tb.Errorf("%s: xattr count %d, want %d", name, len(st.Xattrs), len(expected))
		return
	}

	for k, v := range expected {
		if actual, ok := st.Xattrs[k]; !ok {
			tb.Errorf("%s: missing xattr %q: %v", name, k, st.Xattrs)
		} else if actual != v {
			tb.Errorf("%s: xattr %q: got %q, want %q", name, k, actual, v)
		}
	}
}

// CheckDevice verifies that the named path is a device/fifo with the expected
// type and rdev.
func CheckDevice(tb testing.TB, fsys fs.FS, name string, ftype fs.FileMode, rdev uint32) {
	tb.Helper()

	f, err := fsys.Open(name)
	if err != nil {
		tb.Errorf("open %s: %v", name, err)
		return
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		tb.Errorf("stat %s: %v", name, err)
		return
	}

	st, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		tb.Errorf("%s: expected *erofs.Stat from Sys(), got %T", name, fi.Sys())
		return
	}

	if st.Mode&fs.ModeType != ftype {
		tb.Errorf("%s: type %v, want %v", name, st.Mode&fs.ModeType, ftype)
	}

	if st.Rdev != uint64(rdev) {
		tb.Errorf("%s: rdev %d, want %d", name, st.Rdev, rdev)
	}
}

// CheckMode verifies that the named path has the expected [fs.FileMode],
// including the setuid, setgid and sticky bits. Symlinks are not followed.
//
// The mode is checked on every path that reports one: Lstat, Stat, the
// parent directory's [fs.DirEntry] (as used by fs.WalkDir) and
// Sys().(*erofs.Stat).Mode, which must all agree.
func CheckMode(tb testing.TB, fsys fs.FS, name string, want fs.FileMode) {
	tb.Helper()

	lfs, ok := fsys.(lstatFS)
	if !ok {
		tb.Error("FS does not implement Lstat")
		return
	}

	fi, err := lfs.Lstat(name)
	if err != nil {
		tb.Errorf("lstat %s: %v", name, err)
		return
	}

	checkInfoMode(tb, "Lstat("+name+")", fi, want)

	// Stat follows symlinks, so it reports the target's mode instead.
	if want&fs.ModeSymlink == 0 {
		fi, err = fs.Stat(fsys, name)
		if err != nil {
			tb.Errorf("stat %s: %v", name, err)
		} else {
			checkInfoMode(tb, "Stat("+name+")", fi, want)
		}
	}

	ents, err := fs.ReadDir(fsys, path.Dir(name))
	if err != nil {
		tb.Errorf("readdir %s: %v", path.Dir(name), err)
		return
	}

	base := path.Base(name)
	for _, ent := range ents {
		if ent.Name() != base {
			continue
		}

		if got := ent.Type(); got != want&fs.ModeType {
			tb.Errorf("DirEntry(%s).Type() = %v, want %v", name, got, want&fs.ModeType)
		}

		fi, err := ent.Info()
		if err != nil {
			tb.Errorf("DirEntry(%s).Info(): %v", name, err)
			return
		}

		checkInfoMode(tb, "DirEntry("+name+").Info()", fi, want)

		return
	}

	tb.Errorf("%s: not listed in %s", name, path.Dir(name))
}

// checkInfoMode verifies fi.Mode() against want and against the raw mode in
// Sys(), which callers may use instead.
func checkInfoMode(tb testing.TB, what string, fi fs.FileInfo, want fs.FileMode) {
	tb.Helper()

	got := fi.Mode()
	if got != want {
		tb.Errorf("%s: mode %v (%#o), want %v (%#o)", what, got, uint32(got), want, uint32(want))
	}

	if fi.IsDir() != want.IsDir() {
		tb.Errorf("%s: IsDir() = %v, want %v", what, fi.IsDir(), want.IsDir())
	}

	st, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		tb.Errorf("%s: expected *erofs.Stat from Sys(), got %T", what, fi.Sys())
		return
	}

	if st.Mode != got {
		tb.Errorf("%s: Mode() = %v (%#o) disagrees with Sys().Mode = %v (%#o)",
			what, got, uint32(got), st.Mode, uint32(st.Mode))
	}
}

// Stat returns the *erofs.Stat for the named path, or calls tb.Fatal if it
// cannot be obtained.
func Stat(tb testing.TB, fsys fs.FS, name string) *erofs.Stat {
	tb.Helper()

	fi, err := fs.Stat(fsys, name)
	if err != nil {
		tb.Fatalf("stat %s: %v", name, err)
	}

	st, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		tb.Fatalf("%s: expected *erofs.Stat from Sys(), got %T", name, fi.Sys())
	}

	return st
}

// lstatFS is the interface for Lstat only, without requiring ReadLink.
type lstatFS interface {
	Lstat(name string) (fs.FileInfo, error)
}

// Lstat returns the *erofs.Stat for the named path without following symlinks,
// or calls tb.Fatal if it cannot be obtained.
func Lstat(tb testing.TB, fsys fs.FS, name string) *erofs.Stat {
	tb.Helper()

	lfs, ok := fsys.(lstatFS)
	if !ok {
		tb.Fatal("FS does not implement Lstat")
	}

	fi, err := lfs.Lstat(name)
	if err != nil {
		tb.Fatalf("lstat %s: %v", name, err)
	}

	st, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		tb.Fatalf("%s: expected *erofs.Stat from Sys(), got %T", name, fi.Sys())
	}

	return st
}
