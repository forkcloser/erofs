package erofs_test

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"
	"time"

	erofs "github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/disk"
	"github.com/forkcloser/erofs/internal/erofstest"
)

// linkStat fetches the raw erofs stat for a path in a read-back image,
// without following symlinks (link groups can be symlink inodes too).
func linkStat(t testing.TB, fsys fs.FS, name string) *erofs.Stat {
	t.Helper()
	lfs, ok := fsys.(interface {
		Lstat(name string) (fs.FileInfo, error)
	})
	if !ok {
		t.Fatal("FS does not implement Lstat")
	}
	fi, err := lfs.Lstat(name)
	if err != nil {
		t.Fatalf("lstat %s: %v", name, err)
	}
	st, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		t.Fatalf("lstat %s: Sys() is %T, want *erofs.Stat", name, fi.Sys())
	}
	return st
}

// TestWriterLink verifies that hardlinked names share one inode on disk:
// same ino, shared data, a correct link count, and shared metadata.
func TestWriterLink(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	f, err := fsys.Create("/data.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("shared content\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Chmod("/data.txt", 0o640); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Chown("/data.txt", 12, 34); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Setxattr("/data.txt", "user.tag", "v"); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1700000000, 0)
	if err := fsys.Chtimes("/data.txt", mtime, mtime); err != nil {
		t.Fatal(err)
	}

	// One link beside the target, one in a (implicitly created) subdir, and
	// one through an existing link (must resolve to the same inode, not chain).
	if err := fsys.Link("/data.txt", "/hard.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Link("/data.txt", "/dir/hard2.txt"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Link("/hard.txt", "/hard3.txt"); err != nil {
		t.Fatal(err)
	}

	// Metadata through an alias must hit the shared inode.
	if err := fsys.Chown("/hard.txt", 56, 78); err != nil {
		t.Fatal(err)
	}

	// Read-back through the Writer resolves aliases too.
	wfi, err := fsys.Stat("/hard3.txt")
	if err != nil {
		t.Fatal(err)
	}
	if wfi.Mode().Perm() != 0o640 {
		t.Errorf("writer stat via alias: mode %v, want 0640", wfi.Mode())
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())

	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("Open:", err)
	}

	names := []string{"data.txt", "hard.txt", "dir/hard2.txt", "hard3.txt"}
	for _, name := range names {
		erofstest.CheckReadFile(t, efs, name, "shared content\n")
	}

	base := linkStat(t, efs, "data.txt")
	if base.Nlink != len(names) {
		t.Errorf("nlink = %d, want %d", base.Nlink, len(names))
	}
	if base.UID != 56 || base.GID != 78 {
		t.Errorf("uid:gid = %d:%d, want 56:78 (chown through alias)", base.UID, base.GID)
	}
	for _, name := range names[1:] {
		st := linkStat(t, efs, name)
		if st.Ino != base.Ino {
			t.Errorf("%s: ino %d != target ino %d", name, st.Ino, base.Ino)
		}
		if st.Nlink != base.Nlink || st.Mode != base.Mode || st.UID != base.UID ||
			st.GID != base.GID || st.Mtime != base.Mtime {
			t.Errorf("%s: stat %+v differs from target %+v", name, st, base)
		}
	}

	erofstest.CheckXattrs(t, efs, "hard.txt", map[string]string{"user.tag": "v"})
	erofstest.CheckDirEntries(t, efs, ".", []string{"data.txt", "dir", "hard.txt", "hard3.txt"})
	erofstest.CheckDirEntries(t, efs, "dir", []string{"hard2.txt"})
}

// TestWriterLinkSpecial links non-regular inodes (symlink, device): the tar
// world produces these, and the dirent file type must follow the target.
func TestWriterLinkSpecial(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	if err := fsys.Symlink("target", "/sym"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Link("/sym", "/sym2"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Mknod("/null", disk.StatTypeChrdev|0o666, 1<<8|3); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Link("/null", "/null2"); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	erofstest.FsckErofsBytes(t, buf.Bytes())

	efs, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal("Open:", err)
	}

	erofstest.CheckSymlink(t, efs, "sym2", "target")
	erofstest.CheckDevice(t, efs, "null2", fs.ModeDevice|fs.ModeCharDevice, 1<<8|3)

	if s1, s2 := linkStat(t, efs, "sym"), linkStat(t, efs, "sym2"); s1.Ino != s2.Ino || s2.Nlink != 2 {
		t.Errorf("symlink pair: ino %d/%d nlink %d, want shared ino nlink 2", s1.Ino, s2.Ino, s2.Nlink)
	}
	if d1, d2 := linkStat(t, efs, "null"), linkStat(t, efs, "null2"); d1.Ino != d2.Ino || d2.Nlink != 2 {
		t.Errorf("device pair: ino %d/%d nlink %d, want shared ino nlink 2", d1.Ino, d2.Ino, d2.Nlink)
	}
}

// TestWriterLinkErrors covers the refusal cases.
func TestWriterLinkErrors(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf)

	if err := fsys.Mkdir("/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := fsys.Create("/file")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := fsys.Link("/missing", "/x"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("link to missing target: err %v, want ErrNotExist", err)
	}
	if err := fsys.Link("/dir", "/x"); err == nil {
		t.Error("link to directory succeeded, want error")
	}
	if err := fsys.Link("/file", "/dir"); err == nil {
		t.Error("link over existing path succeeded, want error")
	}
	if err := fsys.Link("/file", "/"); err == nil {
		t.Error("link at root succeeded, want error")
	}

	if err := fsys.Close(); err != nil {
		t.Fatal("Close:", err)
	}
}

// TestWriterLinkDeterministic builds the same linked tree twice and expects
// byte-identical images (link handling must not disturb reproducibility).
func TestWriterLinkDeterministic(t *testing.T) {
	build := func() []byte {
		var buf testBuffer
		fsys := erofs.Create(&buf, erofs.WithBuildTime(0, 0))
		f, err := fsys.Create("/a")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		for _, l := range []string{"/b", "/c", "/d/e"} {
			if err := fsys.Link("/a", l); err != nil {
				t.Fatal(err)
			}
		}
		if err := fsys.Close(); err != nil {
			t.Fatal(err)
		}
		return append([]byte(nil), buf.Bytes()...)
	}

	if !bytes.Equal(build(), build()) {
		t.Error("two identical linked builds produced different bytes")
	}
}
