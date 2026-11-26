//go:build unix

package erofs_test

import (
	"archive/tar"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
)

// TestContractCopyFromHostTree: a host directory holding the tree's files,
// directories, symlinks and hardlinks, copied through os.DirFS: "CopyFrom
// takes ownership, times and link identity from Sys() when it is a
// *syscall.Stat_t (linux, darwin and freebsd)", so every entry of the copy
// carries what Lstat reported for it on the host (its owner, its bits, its
// time, a symlink's own), the root included, and "two names that share a
// source inode become one inode with two names".
func TestContractCopyFromHostTree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	nodes := hostNodes(t, root, corpus.Tree())

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	if err := w.CopyFrom(os.DirFS(root)); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	im := corpus.Image{Name: "host", Img: out.Bytes(), Tree: nodes}
	ref := parseImage(t, im)

	corpus.CheckTree(t, im, ref, nodes, true)

	rootNode := lstatNode(t, root, corpus.Node{Type: tar.TypeDir})

	in, err := ref.Inode(ref.Super.RootNid)
	if err != nil {
		t.Fatal(err)
	}

	if int64(in.Mode&^0o170000) != rootNode.Mode || int(in.UID) != rootNode.UID || int(in.GID) != rootNode.GID ||
		in.Mtime != rootNode.Sec || int64(in.MtimeNs) != rootNode.Nsec {
		t.Errorf(
			"root: mode %#o %d:%d mtime %d.%09d, host %#o %d:%d %d.%09d",
			in.Mode&^0o170000, in.UID, in.GID, in.Mtime, in.MtimeNs,
			rootNode.Mode, rootNode.UID, rootNode.GID, rootNode.Sec, rootNode.Nsec,
		)
	}
}

// hostNodes materializes the tree's regular files, directories, symlinks
// and hardlinks under root (devices, FIFOs, xattrs and setuid bits need
// privileges or platform calls a host tree cannot rely on; a symlink target
// over 1023 bytes exceeds symlink(2)'s PATH_MAX), sets the tree's bits and
// times where the host lets a process set them, the root to bits and a
// time that are not the writer's defaults, and returns what Lstat then
// reports for each: the nodes the copy must show.
func hostNodes(t *testing.T, root string, tree []corpus.Node) []corpus.Node {
	t.Helper()

	var made []corpus.Node

	for _, n := range corpus.Ordered(tree) {
		if n.Xattrs != nil || n.Type == tar.TypeChar || n.Type == tar.TypeBlock || n.Type == tar.TypeFifo ||
			n.Mode&0o7000 != 0 ||
			len(n.Link) > 1023 {
			continue
		}

		p := filepath.Join(root, filepath.FromSlash(n.Path))

		var err error

		switch n.Type {
		case tar.TypeDir:
			err = os.Mkdir(p, 0o700)
		case tar.TypeSymlink:
			err = os.Symlink(n.Link, p)
		case tar.TypeLink:
			err = os.Link(filepath.Join(root, filepath.FromSlash(n.Link)), p)
		default:
			err = os.WriteFile(p, n.Data, 0o600)
		}

		if err != nil {
			t.Fatalf("%s: %v", n.Path, err)
		}

		if n.Type != tar.TypeSymlink && n.Type != tar.TypeLink {
			at := time.Unix(n.Sec, n.Nsec)
			if err := errors.Join(os.Chmod(p, fs.FileMode(n.Mode)), os.Chtimes(p, at, at)); err != nil {
				t.Fatalf("%s: %v", n.Path, err)
			}
		}

		made = append(made, n)
	}

	// Directories' times last: creating their children changed them.
	for _, n := range made {
		if n.Type == tar.TypeDir {
			at := time.Unix(n.Sec, n.Nsec)
			if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(n.Path)), at, at); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The root's bits and time are set apart from the writer's defaults
	// (0755 at the build time), so that the copy is seen to take them from
	// the host and not to fall back.
	at := time.Unix(corpus.BuildTime+11, 500_000_000)
	if err := errors.Join(os.Chmod(root, 0o750), os.Chtimes(root, at, at)); err != nil {
		t.Fatal(err)
	}

	out := make([]corpus.Node, 0, len(made))
	for _, n := range made {
		out = append(out, lstatNode(t, filepath.Join(root, filepath.FromSlash(n.Path)), n))
	}

	return out
}

// lstatNode is the node as the host reports it: Lstat's owner, bits and
// time on the node's type, content and target.
func lstatNode(t *testing.T, p string, n corpus.Node) corpus.Node {
	t.Helper()

	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: Sys is %T, not *syscall.Stat_t", p, info.Sys())
	}

	n.UID, n.GID = int(st.Uid), int(st.Gid)
	n.Sec, n.Nsec = info.ModTime().Unix(), int64(info.ModTime().Nanosecond())

	if n.Type != tar.TypeLink {
		n.Mode = int64(info.Mode().Perm())
	}

	return n
}

// TestContractCopyFromAHostDirectoryReachedTwice: a host directory that two
// paths reach, which is what a bind mount shows (one device and inode under
// both), is a directory at each path of the copy, with its content under
// both, in both modes: mkfs.erofs never matches a directory by its identity
// and gives each path its own. The copy was refused as ErrCorrupt, "the
// source is not a tree", for a tree that is one.
func TestContractCopyFromAHostDirectoryReachedTwice(t *testing.T) {
	t.Parallel()

	shared := func() *fstest.MapFile {
		return &fstest.MapFile{Mode: fs.ModeDir | 0o755, Sys: &syscall.Stat_t{Dev: 5, Ino: 7, Nlink: 2}}
	}

	src := fstest.MapFS{
		"data":        shared(),
		"data/f":      &fstest.MapFile{Data: []byte("content\n"), Mode: 0o644},
		"mnt":         &fstest.MapFile{Mode: fs.ModeDir | 0o755},
		"mnt/bound":   shared(),
		"mnt/bound/f": &fstest.MapFile{Data: []byte("content\n"), Mode: 0o644},
		"mnt/other":   &fstest.MapFile{Mode: fs.ModeDir | 0o755},
		"mnt/other/g": &fstest.MapFile{Data: []byte("g\n"), Mode: 0o644},
	}

	for _, metadataOnly := range []bool{false, true} {
		var opts []erofs.CopyOpt
		if metadataOnly {
			opts = append(opts, erofs.MetadataOnly())
		}

		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

		if err := errors.Join(w.CopyFrom(src, opts...), w.Close()); err != nil {
			t.Fatalf("CopyFrom%s: %v", copyMode(metadataOnly), err)
		}

		copied := openImage(t, corpus.Image{Name: "host-twice", Img: out.Bytes()})

		for _, name := range []string{"data", "mnt/bound"} {
			if info, err := fs.Stat(copied, name); err != nil || !info.IsDir() {
				t.Errorf("CopyFrom%s: %s: %v, %v, want a directory", copyMode(metadataOnly), name, info, err)
			}
		}

		for _, name := range []string{"data/f", "mnt/bound/f"} {
			if metadataOnly {
				if _, err := fs.Stat(copied, name); err != nil {
					t.Errorf("CopyFrom%s: %s: %v", copyMode(metadataOnly), name, err)
				}

				continue
			}

			if data, err := fs.ReadFile(copied, name); err != nil || string(data) != "content\n" {
				t.Errorf("CopyFrom%s: %s holds %q, %v", copyMode(metadataOnly), name, data, err)
			}
		}
	}
}
