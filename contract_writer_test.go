package erofs_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/erofsspec"
)

// The writer's contract, from the sources that state it: the Writer, File,
// CreateOpt and CopyOpt docs, the Stat and DataRange docs, the README's
// "Building an image programmatically" and "Limits", and the on-disk
// format as the reference reads it (erofs_fs.h and the kernel's reader).
// Where a sentence of the docs is held, it is quoted at the check.
//
// Every image the writer produces is parsed by the reference, accepted by
// fsck.erofs where the tool is present, and opened by the package: what
// the writer accepts, its own reader serves back. An image is built from
// a described tree or from a short sequence of calls; the reference reads
// it back field for field, and the package's Lstat agrees.

// TestContractWriterImageReadsBackAsTree: the three corpus trees, at
// every block size the writer takes ("a power of two between 512 and 64
// KiB"), read back to the tree through the reference and pass fsck. The
// nanoseconds Chtimes set are in the image: an inode carrying them is in
// a slot that has a field for them.
func TestContractWriterImageReadsBackAsTree(t *testing.T) {
	t.Parallel()

	trees := map[string]func() []corpus.Node{"tree": corpus.Tree, "full": corpus.Full, "small": corpus.Small}

	for name, tree := range trees {
		for bs := 512; bs <= 65536; bs *= 2 {
			t.Run(fmt.Sprintf("%s-b%d", name, bs), func(t *testing.T) {
				t.Parallel()

				nodes := tree()
				im := corpus.Image{Name: t.Name(), Img: writeImage(t, nodes, erofs.WithBlockSize(bs)), Tree: nodes}
				ref := parseImage(t, im)

				if ref.Super.BlockSize != bs {
					t.Errorf("block size %d under WithBlockSize(%d)", ref.Super.BlockSize, bs)
				}

				corpus.Check(t, im, ref, nodes, true)
				checkNanosecondsKept(t, ref, nodes)
				fsckUnlessPlatformLimit(t, im)
			})
		}
	}
}

// checkNanosecondsKept: every node with nanoseconds has them in the image.
// The corpus check accepts the superblock's fixed nanoseconds on a compact
// slot, which is the format; choosing that slot for an inode whose
// nanoseconds are set is the writer losing what Chtimes set.
func checkNanosecondsKept(t *testing.T, ref *erofsspec.Image, nodes []corpus.Node) {
	t.Helper()

	for _, n := range nodes {
		if n.Nsec == 0 || n.Type == tar.TypeLink {
			continue
		}

		in, err := ref.Resolve(n.Path, 0)
		if err != nil {
			t.Errorf("%s: %v", n.Path, err)

			continue
		}

		if int64(in.MtimeNs) != n.Nsec {
			t.Errorf("%s: nanoseconds %d in a %s slot, Chtimes set %d", n.Path, in.MtimeNs, slotName(in), n.Nsec)
		}
	}
}

func slotName(in *erofsspec.Inode) string {
	if in.Extended {
		return "extended"
	}

	return "compact"
}

// TestContractWriterDataFileImageReadsBackAsTree: with WithDataFile, the
// tree reads back through the data file as the one extra device, and "the
// image records its final length as the device's block count".
func TestContractWriterDataFileImageReadsBackAsTree(t *testing.T) {
	t.Parallel()

	for _, bs := range []int{512, 4096} {
		t.Run(fmt.Sprintf("b%d", bs), func(t *testing.T) {
			t.Parallel()

			df, err := os.CreateTemp(t.TempDir(), "data")
			if err != nil {
				t.Fatal(err)
			}

			defer func() { _ = df.Close() }()

			nodes := corpus.Tree()
			img := writeImage(t, nodes, erofs.WithBlockSize(bs), erofs.WithDataFile(df))

			blob, err := os.ReadFile(df.Name())
			if err != nil {
				t.Fatal(err)
			}

			im := corpus.Image{Name: t.Name(), Img: img, Blob: blob, Tree: nodes}
			ref := parseImage(t, im)

			if len(blob)%bs != 0 {
				t.Errorf("data file of %d bytes is not block-aligned at %d", len(blob), bs)
			}

			if len(ref.Devices) != 1 || ref.Devices[0].Blocks != uint64(len(blob)/bs) {
				t.Errorf("devices %+v, data file of %d blocks", ref.Devices, len(blob)/bs)
			}

			corpus.Check(t, im, ref, nodes, true)
			fsckUnlessPlatformLimit(t, im)
		})
	}
}

// snapshot is what one path reports through a Stat and an Open: the
// FileInfo, the Stat fields the accessors return, a regular file's content
// and a directory's entries. ino is compared by partition, not value.
type snapshot struct {
	mode    fs.FileMode
	size    int64
	mtime   time.Time
	uid     uint32
	gid     uint32
	nlink   uint64
	rdev    uint64
	xattrs  map[string]string
	content []byte
	entries []string
	ino     uint64
	bare    bool // no *erofs.Stat in Sys(): the fields above it are unset
}

// statOpener is what Writer and an image's fs.FS have in common for a
// snapshot: Writer.Stat and Writer.Open, or Lstat and Open through io/fs.
type statOpener interface {
	Stat(name string) (fs.FileInfo, error)
	Open(name string) (fs.File, error)
}

// lstatFS adapts an image's fs.FS so that Stat is Lstat.
type lstatFS struct{ fs.FS }

func (l lstatFS) Stat(name string) (fs.FileInfo, error) { return fs.Lstat(l.FS, name) }

// statFS adapts an image's fs.FS so that Stat follows symlinks.
type statFS struct{ fs.FS }

func (s statFS) Stat(name string) (fs.FileInfo, error) { return fs.Stat(s.FS, name) }

// take is the snapshot of name through so. The Stat fields come from
// Sys(), "the raw erofs stat data returned by Sys() on fs.FileInfo
// values"; a FileInfo without it is reported once and compared on what it
// has.
func take(tb testing.TB, so statOpener, name string) snapshot {
	tb.Helper()

	info, err := so.Stat(name)
	if err != nil {
		tb.Fatalf("%s: Stat: %v", name, err)
	}

	s := snapshot{
		mode:  info.Mode(),
		size:  info.Size(),
		mtime: info.ModTime(),
	}

	if st, ok := info.Sys().(*erofs.Stat); ok {
		s.uid, s.gid, s.nlink, s.rdev, s.xattrs, s.ino = st.UID, st.GID, st.Nlink, st.Rdev, st.Xattrs, st.Ino
	} else {
		s.bare = true
	}

	switch {
	case info.Mode().IsRegular():
		s.content = readAll(tb, so, name)
	case info.IsDir():
		s.entries = readEntries(tb, so, name)
	}

	return s
}

func readAll(tb testing.TB, so statOpener, name string) []byte {
	tb.Helper()

	f, err := so.Open(name)
	if err != nil {
		tb.Fatalf("%s: Open: %v", name, err)
	}

	defer func() { _ = f.Close() }()

	b, err := io.ReadAll(f)
	if err != nil {
		tb.Fatalf("%s: read: %v", name, err)
	}

	return b
}

// readEntries is a directory's entries as "name/type", sorted.
func readEntries(tb testing.TB, so statOpener, name string) []string {
	tb.Helper()

	f, err := so.Open(name)
	if err != nil {
		tb.Fatalf("%s: Open: %v", name, err)
	}

	defer func() { _ = f.Close() }()

	d, ok := f.(fs.ReadDirFile)
	if !ok {
		tb.Fatalf("%s: a directory's file is %T, not fs.ReadDirFile", name, f)
	}

	entries, err := d.ReadDir(-1)
	if err != nil {
		tb.Fatalf("%s: ReadDir: %v", name, err)
	}

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name()+"/"+e.Type().String())
	}

	slices.Sort(out)

	return out
}

// TestContractWriterServesBeforeCloseWhatOpenServesAfter: "Stat and Open
// read entries back before Close"; what they report for every path of the
// tree is what the package's reader reports from the image, the FileInfo,
// the Stat fields, a regular file's bytes and a directory's entries alike.
// Inode numbers and directory sizes exist at layout, so before Close they
// are 0 and not compared. A symlink is compared with Lstat or Stat,
// whichever the writer's Stat stands for.
func TestContractWriterServesBeforeCloseWhatOpenServesAfter(t *testing.T) {
	t.Parallel()

	nodes := corpus.Tree()
	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	for _, n := range corpus.Ordered(nodes) {
		writeNode(t, w, n)
	}

	paths := []string{"."}
	for _, n := range nodes {
		paths = append(paths, n.Path)
	}

	before := map[string]snapshot{}
	for _, p := range paths {
		before[p] = take(t, w, p)
	}

	// "Stat is the raw erofs stat data returned by Sys() on fs.FileInfo
	// values": the writer's FileInfo is one of the package's.
	if before["."].bare {
		t.Error(
			"Writer.Stat's FileInfo has no *erofs.Stat in Sys(): ids, nlink, rdev, xattrs and inode unreadable before Close",
		)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fsys := openImage(t, corpus.Image{Name: t.Name(), Img: out.Bytes()})

	after := map[string]snapshot{}

	for _, p := range paths {
		if before[p].mode&fs.ModeSymlink == 0 && isSymlinkNode(nodes, p) {
			after[p] = take(t, statFS{fsys}, p)
		} else {
			after[p] = take(t, lstatFS{fsys}, p)
		}
	}

	for _, p := range paths {
		compareSnapshots(t, p, before[p], after[p])
	}
}

func isSymlinkNode(nodes []corpus.Node, path string) bool {
	return slices.ContainsFunc(nodes, func(n corpus.Node) bool { return n.Path == path && n.Type == tar.TypeSymlink })
}

func compareSnapshots(t *testing.T, path string, b, a snapshot) {
	t.Helper()

	if b.mode != a.mode {
		t.Errorf("%s: mode %v before Close, %v after", path, b.mode, a.mode)
	}

	// io/fs promises Size for regular files alone; a directory's is its
	// dirent bytes, known at Close, and a symlink's is system-dependent.
	if b.mode.IsRegular() && b.size != a.size {
		t.Errorf("%s: size %d before Close, %d after", path, b.size, a.size)
	}

	if !b.mtime.Equal(a.mtime) {
		t.Errorf("%s: mtime %v before Close, %v after", path, b.mtime, a.mtime)
	}

	if !bytes.Equal(b.content, a.content) {
		t.Errorf("%s: %d content bytes before Close, %d after, or differing", path, len(b.content), len(a.content))
	}

	if !slices.Equal(b.entries, a.entries) {
		t.Errorf("%s: entries %v before Close, %v after", path, b.entries, a.entries)
	}

	if b.bare || a.bare {
		return
	}

	if b.uid != a.uid || b.gid != a.gid {
		t.Errorf("%s: uid:gid %d:%d before Close, %d:%d after", path, b.uid, b.gid, a.uid, a.gid)
	}

	if b.nlink != a.nlink {
		t.Errorf("%s: nlink %d before Close, %d after", path, b.nlink, a.nlink)
	}

	if b.rdev != a.rdev {
		t.Errorf("%s: rdev %#x before Close, %#x after", path, b.rdev, a.rdev)
	}

	if !equalXattrs(b.xattrs, a.xattrs) {
		t.Errorf("%s: xattrs %v before Close, %v after", path, corpus.Keys(b.xattrs), corpus.Keys(a.xattrs))
	}
}

func equalXattrs(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}

	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}

	return true
}

// TestContractWriterCleansPaths: "x", "/x" and "./x" name the same entry,
// a trailing slash is dropped, and "", "." and "/" all name the root.
func TestContractWriterCleansPaths(t *testing.T) {
	t.Parallel()

	w := erofs.Create(&testBuffer{})

	if err := w.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}

	if err := writeRegular(w, corpus.Node{Path: "d/x", Data: []byte("x"), Mode: 0o644}); err != nil {
		t.Fatal(err)
	}

	x := take(t, w, "d/x")

	for _, form := range []string{"/d/x", "./d/x", "d/x/", "/d/./x", "d//x"} {
		if got := take(t, w, form); got.mode != x.mode || got.size != x.size || !bytes.Equal(got.content, x.content) {
			t.Errorf("Stat(%q) is not Stat(\"d/x\")", form)
		}
	}

	root := take(t, w, ".")

	for _, form := range []string{"", "/", "./", "/."} {
		info, err := w.Stat(form)
		if err != nil {
			t.Errorf("Stat(%q): %v, want the root", form, err)

			continue
		}

		if got := take(t, w, form); !info.IsDir() || !slices.Equal(got.entries, root.entries) {
			t.Errorf("Stat(%q) is not the root", form)
		}
	}
}

// wBuilt is one image a sequence of calls produced: its bytes, the
// reference's parse and the package's open, fsck.erofs having accepted it.
type wBuilt struct {
	img  []byte
	ref  *erofsspec.Image
	fsys fs.FS
}

// wBuild runs fn over a fresh writer and closes it; a failing call inside
// fn is fn's to judge.
func wBuild(tb testing.TB, fn func(w *erofs.Writer), opts ...erofs.CreateOpt) wBuilt {
	tb.Helper()

	out := &testBuffer{}
	w := erofs.Create(out, append([]erofs.CreateOpt{erofs.WithBuildTime(corpus.BuildTime, 0)}, opts...)...)

	fn(w)

	if err := w.Close(); err != nil {
		tb.Fatalf("Close: %v", err)
	}

	im := corpus.Image{Name: tb.Name(), Img: out.Bytes()}
	b := wBuilt{img: im.Img, ref: parseImage(tb, im), fsys: openImage(tb, im)}

	fsckUnlessPlatformLimit(tb, im)

	return b
}

// fsckUnlessPlatformLimit runs fsck.erofs on the image, except where the
// tool cannot read the block size at all: the macOS build of erofs-utils
// refuses one over its page size ("blksize N isn't supported on this
// platform"), which says nothing of the image.
func fsckUnlessPlatformLimit(tb testing.TB, im corpus.Image) {
	tb.Helper()

	err := corpus.FsckVerdict(tb, im)
	if err == nil {
		return
	}

	if strings.Contains(err.Error(), "isn't supported on this platform") {
		tb.Logf("%s: fsck.erofs cannot read this block size here; no verdict", im.Name)

		return
	}

	tb.Fatalf("%s: fsck.erofs refuses the image: %v", im.Name, err)
}

// inodeOf finds a path through the reference the kernel's way, without
// following a symlink at its last name.
func (b wBuilt) inodeOf(tb testing.TB, path string) *erofsspec.Inode {
	tb.Helper()

	dir, name := "", path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		dir, name = path[:i], path[i+1:]
	}

	parent, err := b.ref.Resolve(dir, kernelMaxLinks)
	if err != nil {
		tb.Fatalf("%s: reference Resolve: %v", path, err)
	}

	if name == "." || name == "" {
		return parent
	}

	de, err := b.ref.Lookup(parent, []byte(name))
	if err != nil {
		tb.Fatalf("%s: reference Lookup: %v", path, err)
	}

	in, err := b.ref.Inode(de.Nid)
	if err != nil {
		tb.Fatalf("%s: reference Inode: %v", path, err)
	}

	return in
}

// statOf is the package's Lstat of a path with its Stat.
func (b wBuilt) statOf(tb testing.TB, name string) (fs.FileInfo, *erofs.Stat) {
	tb.Helper()

	info, err := fs.Lstat(b.fsys, name)
	if err != nil {
		tb.Fatalf("%s: Lstat: %v", name, err)
	}

	st, ok := info.Sys().(*erofs.Stat)
	if !ok {
		tb.Fatalf("%s: Sys is %T", name, info.Sys())
	}

	return info, st
}

// mustCreate writes a small regular file, failing the test on any error.
func mustCreate(tb testing.TB, w *erofs.Writer, name string, data []byte) {
	tb.Helper()

	if err := writeRegular(w, corpus.Node{Path: name, Data: data, Mode: 0o644}); err != nil {
		tb.Fatalf("%s: %v", name, err)
	}
}

// TestContractWriterChown: "Both must fit the 32-bit unsigned fields EROFS
// stores; a negative value is rejected with ErrInvalid". Every id that
// fits is stored exactly, through Writer.Chown and File.Chown alike; the
// 16-bit boundary of the compact slot is the writer's to cross.
func TestContractWriterChown(t *testing.T) {
	t.Parallel()

	cases := []struct {
		uid, gid int
		ok       bool
	}{
		{0, 0, true},
		{65535, 65535, true},
		{65536, 65536, true},
		{math.MaxUint32, math.MaxUint32, true},
		{-1, 0, false},
		{0, -1, false},
		{math.MaxUint32 + 1, 0, false},
		{0, math.MaxUint32 + 1, false},
	}

	for _, c := range cases {
		for _, via := range []string{"Writer.Chown", "File.Chown"} {
			t.Run(fmt.Sprintf("%s/%d:%d", via, c.uid, c.gid), func(t *testing.T) {
				t.Parallel()

				var err error

				b := wBuild(t, func(w *erofs.Writer) {
					err = chownVia(t, w, via, c.uid, c.gid)
				})

				if !c.ok {
					if !errors.Is(err, fs.ErrInvalid) {
						t.Fatalf("%d:%d: %v, want ErrInvalid", c.uid, c.gid, err)
					}

					return
				}

				if err != nil {
					t.Fatalf("%d:%d: %v", c.uid, c.gid, err)
				}

				in := b.inodeOf(t, "f")
				if int(in.UID) != c.uid || int(in.GID) != c.gid {
					t.Errorf("image has %d:%d, want %d:%d", in.UID, in.GID, c.uid, c.gid)
				}

				if _, st := b.statOf(t, "f"); int(st.UID) != c.uid || int(st.GID) != c.gid {
					t.Errorf("Lstat has %d:%d, want %d:%d", st.UID, st.GID, c.uid, c.gid)
				}
			})
		}
	}
}

// chownVia creates f and chowns it through the writer or the open file.
func chownVia(tb testing.TB, w *erofs.Writer, via string, uid, gid int) error {
	tb.Helper()

	f, err := w.Create("f")
	if err != nil {
		tb.Fatal(err)
	}

	if via == "File.Chown" {
		err = f.Chown(uid, gid)
	}

	if cerr := f.Close(); cerr != nil {
		tb.Fatal(cerr)
	}

	if via == "Writer.Chown" {
		err = w.Chown("f", uid, gid)
	}

	return err
}

// TestContractWriterSetNlink: "SetNlink overrides the computed link count";
// the value is in the image, across the 16-bit boundary of the compact
// slot.
func TestContractWriterSetNlink(t *testing.T) {
	t.Parallel()

	for _, n := range []uint32{1, 2, 65535, 65536, math.MaxUint32} {
		t.Run(strconv.FormatUint(uint64(n), 10), func(t *testing.T) {
			t.Parallel()

			b := wBuild(t, func(w *erofs.Writer) {
				mustCreate(t, w, "f", []byte("n"))

				if err := w.SetNlink("f", n); err != nil {
					t.Fatal(err)
				}
			})

			if in := b.inodeOf(t, "f"); in.Nlink != n {
				t.Errorf("image nlink %d, want %d", in.Nlink, n)
			}

			if _, st := b.statOf(t, "f"); st.Nlink != uint64(n) {
				t.Errorf("Lstat nlink %d, want %d", st.Nlink, n)
			}
		})
	}
}

// TestContractWriterSetxattr: "The name (after any standard prefix) must be
// at most 255 bytes and the value at most 65535"; what is accepted is in
// the image under its name, once, with its last value.
func TestContractWriterSetxattr(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("n", 255)

	cases := []struct {
		attr, value string
		ok          bool
	}{
		{"user." + long, "v", true},
		{"user." + long + "n", "v", false},
		{"user.v", strings.Repeat("x", 65535), true},
		{"user.v", strings.Repeat("x", 65536), false},
		{"user.empty", "", true},
		{"trusted.t", "1", true},
		{"security.s", "1", true},
		{"system.posix_acl_access", "\x02\x00\x00\x00", true},
		{long, "v", true},
		{long + "n", "v", false},
		{"user.", "v", false},
		{"", "v", false},
	}

	for _, c := range cases {
		t.Run(fmt.Sprintf("%.20s(%d)=%d", c.attr, len(c.attr), len(c.value)), func(t *testing.T) {
			t.Parallel()

			var err error

			b := wBuild(t, func(w *erofs.Writer) {
				mustCreate(t, w, "f", []byte("x"))
				err = w.Setxattr("f", c.attr, c.value)
			})

			if !c.ok {
				if !errors.Is(err, fs.ErrInvalid) {
					t.Fatalf("Setxattr(%q, %d bytes): %v, want ErrInvalid", c.attr, len(c.value), err)
				}

				return
			}

			if err != nil {
				t.Fatalf("Setxattr: %v", err)
			}

			want := map[string]string{c.attr: c.value}
			if got := xattrsOf(b.inodeOf(t, "f")); !equalXattrs(got, want) {
				t.Errorf("image xattrs %v, want %v", corpus.Keys(got), corpus.Keys(want))
			}

			if _, st := b.statOf(t, "f"); !equalXattrs(st.Xattrs, want) {
				t.Errorf("Lstat xattrs %v, want %v", corpus.Keys(st.Xattrs), corpus.Keys(want))
			}
		})
	}
}

// xattrsOf is an inode's xattrs by full name; a name stored twice shows
// as the test's failure through the count.
func xattrsOf(in *erofsspec.Inode) map[string]string {
	m := map[string]string{}
	for _, x := range in.Xattrs {
		m[x.Name()] = string(x.Value)
	}

	return m
}

// TestContractWriterSetxattrOverwrites: a name set twice is in the image
// once, with the last value.
func TestContractWriterSetxattrOverwrites(t *testing.T) {
	t.Parallel()

	b := wBuild(t, func(w *erofs.Writer) {
		mustCreate(t, w, "f", []byte("x"))

		for _, v := range []string{"1", "22", "3"} {
			if err := w.Setxattr("f", "user.k", v); err != nil {
				t.Fatal(err)
			}
		}
	})

	in := b.inodeOf(t, "f")
	if len(in.Xattrs) != 1 || in.Xattrs[0].Name() != "user.k" || string(in.Xattrs[0].Value) != "3" {
		t.Errorf("image xattrs %v, want user.k=3 once", xattrsOf(in))
	}
}

// TestContractWriterMknod: each type the doc names lands with its mode bits
// and, for the two device types, its rdev; "Any other type, including a
// regular file, is rejected with ErrInvalid".
func TestContractWriterMknod(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		mode     fs.FileMode
		rdev     uint32
		wantType uint16
		ok       bool
	}{
		{"fifo", fs.ModeNamedPipe | 0o644, 0, erofsspec.ModeFifo, true},
		{"socket", fs.ModeSocket | 0o600, 0, erofsspec.ModeSocket, true},
		{"char", fs.ModeDevice | fs.ModeCharDevice | 0o660, math.MaxUint32, erofsspec.ModeChar, true},
		{"block", fs.ModeDevice | 0o660, 0x0801, erofsspec.ModeBlock, true},
		{
			"char-special-bits",
			fs.ModeDevice | fs.ModeCharDevice | fs.ModeSetuid | fs.ModeSticky | 0o600,
			1,
			erofsspec.ModeChar,
			true,
		},
		{"regular", 0o644, 0, 0, false},
		{"dir", fs.ModeDir | 0o755, 0, 0, false},
		{"symlink", fs.ModeSymlink | 0o777, 0, 0, false},
		{"irregular", fs.ModeIrregular | 0o644, 0, 0, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			var err error

			b := wBuild(t, func(w *erofs.Writer) {
				err = w.Mknod("n", c.mode, c.rdev)
			})

			if !c.ok {
				if !errors.Is(err, fs.ErrInvalid) {
					t.Fatalf("Mknod(%v): %v, want ErrInvalid", c.mode, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("Mknod(%v): %v", c.mode, err)
			}

			in := b.inodeOf(t, "n")
			if in.Mode&erofsspec.ModeTypeMask != c.wantType || modeOf(in) != c.mode {
				t.Errorf("image mode %#o (%v), want type %#o and %v", in.Mode, modeOf(in), c.wantType, c.mode)
			}

			wantRdev := uint64(0)
			if c.wantType == erofsspec.ModeChar || c.wantType == erofsspec.ModeBlock {
				wantRdev = uint64(c.rdev)
			}

			if uint64(in.Rdev) != wantRdev {
				t.Errorf("image rdev %#x, want %#x", in.Rdev, wantRdev)
			}

			if info, st := b.statOf(t, "n"); info.Mode() != c.mode || st.Rdev != wantRdev {
				t.Errorf("Lstat mode %v rdev %#x, want %v and %#x", info.Mode(), st.Rdev, c.mode, wantRdev)
			}
		})
	}
}

// TestContractWriterChmodKeepsType: "Chmod sets permission bits on the
// named path, preserving type bits", setuid, setgid and sticky included,
// through Writer.Chmod and File.Chmod; a missing path is ErrNotExist.
func TestContractWriterChmodKeepsType(t *testing.T) {
	t.Parallel()

	var missing error

	b := wBuild(t, func(w *erofs.Writer) {
		f, err := w.Create("open")
		if err != nil {
			t.Fatal(err)
		}

		if err := f.Chmod(fs.ModeDir | fs.ModeSetgid | 0o710); err != nil {
			t.Fatal(err)
		}

		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		mustCreate(t, w, "f", []byte("x"))

		if err := w.Chmod("f", fs.ModeSymlink|fs.ModeSetuid|0o755); err != nil {
			t.Fatal(err)
		}

		if err := w.Mkdir("d", 0o755); err != nil {
			t.Fatal(err)
		}

		if err := w.Chmod("d", fs.ModeSticky|0o777); err != nil {
			t.Fatal(err)
		}

		missing = w.Chmod("missing", 0o644)
	})

	if !errors.Is(missing, fs.ErrNotExist) {
		t.Errorf("Chmod of a missing path: %v, want ErrNotExist", missing)
	}

	want := map[string]fs.FileMode{
		"open": fs.ModeSetgid | 0o710,
		"f":    fs.ModeSetuid | 0o755,
		"d":    fs.ModeDir | fs.ModeSticky | 0o777,
	}

	for name, mode := range want {
		if got := modeOf(b.inodeOf(t, name)); got != mode {
			t.Errorf("%s: image mode %v, want %v", name, got, mode)
		}

		if info, _ := b.statOf(t, name); info.Mode() != mode {
			t.Errorf("%s: Lstat mode %v, want %v", name, info.Mode(), mode)
		}
	}
}

// TestContractWriterChtimes: "EROFS only stores mtime"; seconds and
// nanoseconds set are the seconds and nanoseconds read back, past 32 bits
// of seconds, before 1970 (the kernel reads the extended slot's seconds as
// a signed time64_t) and when the seconds equal the build time.
func TestContractWriterChtimes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		sec  int64
		nsec int64
	}{
		{0, 0},
		{-1, 0},
		{-1 << 40, 5},
		{corpus.BuildTime, 123},
		{1<<32 + 5, 999_999_999},
		{math.MaxInt32, 1},
	}

	for _, c := range cases {
		t.Run(fmt.Sprintf("%d.%09d", c.sec, c.nsec), func(t *testing.T) {
			t.Parallel()

			b := wBuild(t, func(w *erofs.Writer) {
				mustCreate(t, w, "f", []byte("x"))

				at := time.Unix(c.sec, c.nsec)
				if err := w.Chtimes("f", at, at); err != nil {
					t.Fatal(err)
				}
			})

			if in := b.inodeOf(t, "f"); in.Mtime != c.sec || int64(in.MtimeNs) != c.nsec {
				t.Errorf("image mtime %d.%09d, want %d.%09d", in.Mtime, in.MtimeNs, c.sec, c.nsec)
			}

			if info, _ := b.statOf(t, "f"); !info.ModTime().Equal(time.Unix(c.sec, c.nsec)) {
				t.Errorf("Lstat mtime %v, want %v", info.ModTime(), time.Unix(c.sec, c.nsec))
			}
		})
	}
}

// TestContractWriterSymlinkTargets: a target the kernel delivers whole
// (page_get_link reads at most a page less its terminator, 4095 bytes)
// round-trips through ReadLink and the reference alike; a longer target,
// an empty one, or one with a NUL byte is refused, the kernel delivering
// it cut and the reader refusing the image.
func TestContractWriterSymlinkTargets(t *testing.T) {
	t.Parallel()

	for _, n := range []int{1, 100, 4095} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()

			target := strings.Repeat("t", n)

			b := wBuild(t, func(w *erofs.Writer) {
				if err := w.Symlink(target, "l"); err != nil {
					t.Fatal(err)
				}
			})

			if got, err := fs.ReadLink(b.fsys, "l"); err != nil || got != target {
				t.Errorf("ReadLink: %d bytes, %v; want %d", len(got), err, n)
			}

			if got, err := b.ref.Target(b.inodeOf(t, "l")); err != nil || string(got) != target {
				t.Errorf("reference target: %d bytes, %v; want %d", len(got), err, n)
			}
		})
	}

	refused := map[string]string{
		"4096":  strings.Repeat("t", erofsspec.TargetMax+1),
		"4097":  strings.Repeat("t", erofsspec.TargetMax+2),
		"empty": "",
		"nul":   "a\x00b",
	}

	for name, target := range refused {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out := &testBuffer{}
			w := erofs.Create(out)

			if err := w.Symlink(target, "l"); !errors.Is(err, fs.ErrInvalid) {
				t.Errorf(
					"Symlink(%d bytes): %v, want ErrInvalid; the image then reads: %v",
					len(target),
					err,
					readLinkAfter(w, out),
				)
			}
		})
	}
}

// readLinkAfter closes the writer and reads "l" back through the package,
// as evidence of what an accepted target became.
func readLinkAfter(w *erofs.Writer, out *testBuffer) error {
	if err := w.Close(); err != nil {
		return err
	}

	fsys, err := erofs.Open(bytes.NewReader(out.Bytes()))
	if err != nil {
		return err
	}

	_, err = fs.ReadLink(fsys, "l")

	return err
}

// TestContractWriterSizesAtBlockBoundaries: files one byte either side of
// every block boundary, at the smallest, the default and the largest
// block size, some with an xattr moving the inline tail: each reads back
// whole through the reference and the package, and DataRange is the
// reference's extents.
func TestContractWriterSizesAtBlockBoundaries(t *testing.T) {
	t.Parallel()

	for _, bs := range []int{512, 4096, 65536} {
		t.Run(fmt.Sprintf("b%d", bs), func(t *testing.T) {
			t.Parallel()

			sizes := []int{0, 1, bs - 1, bs, bs + 1, 2*bs - 1, 2 * bs, 2*bs + 1, 3*bs + bs/2}
			data := map[string][]byte{}

			b := wBuild(t, func(w *erofs.Writer) {
				for i, n := range sizes {
					for _, x := range []bool{false, true} {
						name := fmt.Sprintf("f%d-%v", n, x)
						data[name] = corpus.Pattern(n + i)
						mustCreate(t, w, name, data[name])

						if x {
							if err := w.Setxattr(name, "user.pad", strings.Repeat("p", 100)); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
			}, erofs.WithBlockSize(bs))

			for name, want := range data {
				in := b.inodeOf(t, name)

				if got, err := b.ref.Read(in); err != nil || !bytes.Equal(got, want) {
					t.Errorf("%s: reference reads %d bytes (%v), want %d", name, len(got), err, len(want))
				}

				if got, err := fs.ReadFile(b.fsys, name); err != nil || !bytes.Equal(got, want) {
					t.Errorf("%s: ReadFile %d bytes (%v), want %d", name, len(got), err, len(want))
				}

				info, _ := b.statOf(t, name)
				checkRanges(t, b.fsys, b.ref, name, info, in)
			}
		})
	}
}

// TestContractWriterDirectoryBlocks: a directory whose entries fill a
// block exactly, one short of it and one over, at two block sizes: the
// kernel's lookup finds every name, the directory passes fsck's rules and
// ReadDir lists them all. A 255-byte name is stored; a 256-byte one, which
// no lookup could ever reach, is refused.
func TestContractWriterDirectoryBlocks(t *testing.T) {
	t.Parallel()

	for _, bs := range []int{512, 4096} {
		t.Run(fmt.Sprintf("b%d", bs), func(t *testing.T) {
			t.Parallel()

			dirs := map[string][]string{}

			b := wBuild(t, func(w *erofs.Writer) {
				for dir, delta := range map[string]int{"full": 0, "short": -1, "over": 1} {
					dirs[dir] = fillingNames(bs, delta)

					for _, n := range dirs[dir] {
						mustCreate(t, w, dir+"/"+n, []byte("d"))
					}
				}
			}, erofs.WithBlockSize(bs))

			for dir, names := range dirs {
				checkDirectory(t, b, dir, names)
			}

			// The names hold the directory to its name: "full" is one block of
			// dirents exactly, "over" that block and, in a second, the one
			// dirent that no longer fit (the last name in order, as a dirent
			// never straddles blocks), "short" under a block.
			last := slices.Max(dirs["over"])
			sizes := map[string]int64{"full": int64(bs), "over": int64(bs + 12 + len(last)), "short": int64(bs) - 1}

			for dir, want := range sizes {
				got := b.inodeOf(t, dir).Size
				if (dir == "short" && got >= int64(bs)) || (dir != "short" && got != want) {
					t.Errorf("%s: directory size %d, want %d", dir, got, want)
				}
			}
		})
	}

	t.Run("name-length", func(t *testing.T) {
		t.Parallel()

		long := strings.Repeat("n", 255)

		b := wBuild(t, func(w *erofs.Writer) {
			mustCreate(t, w, long, []byte("l"))
		})

		checkDirectory(t, b, ".", []string{long})

		// A name of 256 bytes is one the kernel never resolves (namei.c,
		// erofs_namei returns ENAMETOOLONG past EROFS_NAME_LEN), so the
		// writer has nothing to store it for. Each call gets its own name
		// and its own writer, so that one acceptance hides no other.
		for name, call := range map[string]func(w *erofs.Writer, n string) error{
			"Create":  func(w *erofs.Writer, n string) error { return writeRegular(w, corpus.Node{Path: n, Mode: 0o644}) },
			"Mkdir":   func(w *erofs.Writer, n string) error { return w.Mkdir(n, 0o755) },
			"Symlink": func(w *erofs.Writer, n string) error { return w.Symlink("x", n) },
			"Mknod":   func(w *erofs.Writer, n string) error { return w.Mknod(n, fs.ModeNamedPipe|0o644, 0) },
			"CopyFrom": func(w *erofs.Writer, n string) error {
				return w.CopyFrom(fstest.MapFS{n: &fstest.MapFile{Data: []byte("c"), Mode: 0o644}})
			},
		} {
			w := erofs.Create(&testBuffer{})

			if err := call(w, long+"n"); !errors.Is(err, fs.ErrInvalid) {
				t.Errorf("%s of a 256-byte name: %v, want ErrInvalid (Close then says: %v)", name, err, w.Close())
			}
		}
	})
}

// fillingNames are names whose dirents, with "." and "..", fill one block
// of bs bytes exactly, plus delta entries: a dirent is 12 bytes and its
// name.
func fillingNames(bs, delta int) []string {
	const dirent, short = 12, 8

	room := bs - (dirent + 1) - (dirent + 2)
	count := room / (dirent + short)
	rest := room - count*(dirent+short)

	// The remainder, under a dirent's minimum, goes into one longer name.
	if rest > 0 && rest < dirent+1 {
		count--
		rest += dirent + short
	}

	names := make([]string, 0, count+2)
	for i := range count {
		names = append(names, fmt.Sprintf("n%07d", i))
	}

	if rest > 0 {
		names = append(names, "x"+strings.Repeat("y", rest-dirent-1))
	}

	for i := range max(delta, 0) {
		names = append(names, fmt.Sprintf("o%07d", i))
	}

	return names[:len(names)+min(delta, 0)]
}

// checkDirectory holds one directory to the format: every name is found
// by the kernel's lookup with a regular file's nid, the directory passes
// fsck's rules, and ReadDir lists the names.
func checkDirectory(t *testing.T, b wBuilt, dir string, names []string) {
	t.Helper()

	in := b.inodeOf(t, dir)

	for _, n := range names {
		de, err := b.ref.Lookup(in, []byte(n))
		if err != nil || de.Type != erofsspec.TypeRegular {
			t.Errorf("%s/%s: lookup %+v, %v", dir, n, de, err)
		}
	}

	entries, err := b.ref.Dirents(in)
	if err != nil {
		t.Fatalf("%s: dirents: %v", dir, err)
	}

	if len(entries) != len(names)+2 {
		t.Errorf("%s: %d dirents, want %d", dir, len(entries), len(names)+2)
	}

	parent := b.inodeOf(t, ".").Nid
	for _, err := range b.ref.CheckDir(in, parent, entries) {
		t.Errorf("%s: %v", dir, err)
	}

	listed, err := fs.ReadDir(b.fsys, dir)
	if err != nil {
		t.Fatalf("%s: ReadDir: %v", dir, err)
	}

	got := make([]string, 0, len(listed))
	for _, e := range listed {
		got = append(got, e.Name())
	}

	want := slices.Clone(names)
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Errorf("%s: ReadDir %d names, want %d", dir, len(got), len(want))
	}
}

// TestContractWriterHoles: "In MetadataOnly mode ... files without
// DataRange() (or pre-built chunks) are stored as chunk-based inodes with
// no physical mappings (all holes)": such a file is all hole in the
// image, zeros through the reference and the package, and its DataRange
// is hole entries summing to its size.
func TestContractWriterHoles(t *testing.T) {
	t.Parallel()

	sizes := map[string]int{"empty": 0, "block": 4096, "odd": 10000, "chunks": 4096*3 + 1}
	src := fstest.MapFS{}

	for name, n := range sizes {
		src[name] = &fstest.MapFile{Data: make([]byte, n), Mode: 0o644}
	}

	b := wBuild(t, func(w *erofs.Writer) {
		if err := w.CopyFrom(src, erofs.MetadataOnly()); err != nil {
			t.Fatal(err)
		}
	}, erofs.WithBlockSize(4096))

	for name, n := range sizes {
		in := b.inodeOf(t, name)

		extents, err := b.ref.Extents(in)
		if err != nil {
			t.Fatalf("%s: extents: %v", name, err)
		}

		if n > 0 && in.Layout != erofsspec.LayoutChunkBased {
			t.Errorf("%s: layout %d, want chunk-based", name, in.Layout)
		}

		if slices.ContainsFunc(extents, func(e erofsspec.Extent) bool { return !e.Hole }) {
			t.Errorf("%s: an extent is not a hole: %+v", name, extents)
		}

		if got, err := fs.ReadFile(b.fsys, name); err != nil || !bytes.Equal(got, make([]byte, n)) {
			t.Errorf("%s: ReadFile %d bytes (%v), want %d zeros", name, len(got), err, n)
		}

		info, _ := b.statOf(t, name)
		checkRanges(t, b.fsys, b.ref, name, info, in)
	}
}

// TestContractWriterBlockSizeOption: "An invalid size causes subsequent
// Writer operations to return an error": every call, Close included; zero
// is the default of 4096.
func TestContractWriterBlockSizeOption(t *testing.T) {
	t.Parallel()

	t.Run("0", func(t *testing.T) {
		t.Parallel()

		b := wBuild(t, func(w *erofs.Writer) { mustCreate(t, w, "f", []byte("x")) }, erofs.WithBlockSize(0))
		if b.ref.Super.BlockSize != 4096 {
			t.Errorf("block size %d under WithBlockSize(0), want the default 4096", b.ref.Super.BlockSize)
		}
	})

	for _, n := range []int{-1, 256, 3000, 4095, 1 << 17} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()

			w := erofs.Create(&testBuffer{}, erofs.WithBlockSize(n))

			if err := w.Mkdir("d", 0o755); err == nil {
				t.Errorf("Mkdir under WithBlockSize(%d) succeeded", n)
			}

			if _, err := w.Create("f"); err == nil {
				t.Errorf("Create under WithBlockSize(%d) succeeded", n)
			}

			if err := w.Close(); err == nil {
				t.Errorf("Close under WithBlockSize(%d) succeeded", n)
			}
		})
	}
}

// TestContractWriterBuildTime: WithBuildTime is the superblock's build
// time, seconds and nanoseconds.
func TestContractWriterBuildTime(t *testing.T) {
	t.Parallel()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(1_800_000_000, 42))

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	ref := parseImage(t, corpus.Image{Name: t.Name(), Img: out.Bytes()})

	if got := ref.Super.Epoch + int64(ref.Super.BuildTime); got != 1_800_000_000 || ref.Super.FixedNsec != 42 {
		t.Errorf("superblock build time %d.%09d, want 1800000000.000000042", got, ref.Super.FixedNsec)
	}
}

// TestContractWriterRemoveAndLink: the Remove, RemoveAll and Link docs,
// sentence by sentence: Remove of "/" and of a missing path, of a
// non-empty directory; RemoveAll of a missing path, of "/", through a
// file; Link of a missing or directory target; two names of one inode
// with a count of two; Remove of one name leaving the other, the original
// name included ("as unlink(2) does").
func TestContractWriterRemoveAndLink(t *testing.T) {
	t.Parallel()

	errs := map[string]error{}

	b := wBuild(t, func(w *erofs.Writer) {
		mustCreate(t, w, "f", []byte("f"))
		mustCreate(t, w, "d/child", []byte("c"))
		mustCreate(t, w, "orig", []byte("o"))

		errs["Remove /"] = w.Remove("/")
		errs["Remove missing"] = w.Remove("missing")
		errs["Remove non-empty"] = w.Remove("d")
		errs["RemoveAll missing"] = w.RemoveAll("missing")
		errs["RemoveAll /"] = w.RemoveAll("/")
		errs["RemoveAll through file"] = w.RemoveAll("f/x")
		errs["Link missing"] = w.Link("missing", "h")
		errs["Link dir"] = w.Link("d", "h")
		errs["Link"] = w.Link("f", "h")
		errs["Link orig"] = w.Link("orig", "alias")
		errs["Remove orig"] = w.Remove("orig")
		errs["Link f twice"] = w.Link("f", "h2")
		errs["Remove h2"] = w.Remove("h2")
	})

	want := map[string]error{
		"Remove /":               fs.ErrInvalid,
		"Remove missing":         fs.ErrNotExist,
		"Remove non-empty":       erofs.ErrDirNotEmpty,
		"RemoveAll missing":      nil,
		"RemoveAll /":            fs.ErrInvalid,
		"RemoveAll through file": erofs.ErrNotDirectory,
		"Link missing":           fs.ErrNotExist,
		"Link":                   nil,
		"Link orig":              nil,
		"Remove orig":            nil,
		"Link f twice":           nil,
		"Remove h2":              nil,
	}

	for call, target := range want {
		if err := errs[call]; !errors.Is(err, target) && !(target == nil && err == nil) {
			t.Errorf("%s: %v, want %v", call, err, target)
		}
	}

	if errs["Link dir"] == nil {
		t.Error("Link of a directory succeeded")
	}

	f, h := b.inodeOf(t, "f"), b.inodeOf(t, "h")
	if f.Nid != h.Nid || f.Nlink != 2 {
		t.Errorf("f nid %d nlink %d, h nid %d: want one inode with two names", f.Nid, f.Nlink, h.Nid)
	}

	if alias := b.inodeOf(t, "alias"); alias.Nlink != 1 {
		t.Errorf("alias nlink %d after its original name was removed, want 1", alias.Nlink)
	}

	if got, err := fs.ReadFile(b.fsys, "alias"); err != nil || string(got) != "o" {
		t.Errorf("alias reads %q, %v", got, err)
	}

	for _, gone := range []string{"orig", "h2"} {
		if _, err := fs.Lstat(b.fsys, gone); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: Lstat %v after Remove, want ErrNotExist", gone, err)
		}
	}
}

// TestContractWriterLifecycle: what a closed writer or file refuses, as
// fs.ErrClosed ("FS already closed", "write to closed file"): a write or a
// second Close of a closed file, a second Close of the writer, a Create
// after it; a second Create of a name already written is refused and the
// first file stands; the file open for writing is refused at Open until
// its Close and served after; a directory handle refuses Read.
func TestContractWriterLifecycle(t *testing.T) {
	t.Parallel()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	f, ferr := w.Create("/f")
	if ferr != nil {
		t.Fatal(ferr)
	}

	if _, err := f.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}

	// The one file open for writing is not served: Open of it is refused
	// until its Close, and a PathError names the path.
	if _, err := w.Open("/f"); err == nil {
		t.Error("Open of the file still open for writing succeeded")
	} else if pe := (*fs.PathError)(nil); !errors.As(err, &pe) || pe.Path != "/f" {
		t.Errorf("Open of the file still open for writing: %v, want a PathError for /f", err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if h, err := w.Open("/f"); err != nil {
		t.Errorf("Open after the file's Close: %v", err)
	} else if data, rerr := io.ReadAll(h); rerr != nil || string(data) != "first" {
		t.Errorf("Open after the file's Close reads %q, %v; want \"first\"", data, rerr)
	} else {
		_ = h.Close()
	}

	if _, err := f.Write([]byte("more")); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("Write after Close: %v, want fs.ErrClosed", err)
	}

	if err := f.Close(); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("second Close of a file: %v, want fs.ErrClosed", err)
	}

	if _, err := w.Create("/f"); err == nil {
		t.Error("a second Create of a written name succeeded")
	}

	if err := w.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}

	d, derr := w.Open("/d")
	if derr != nil {
		t.Fatal(derr)
	}

	if _, err := d.Read(make([]byte, 8)); err == nil {
		t.Error("Read from a directory handle succeeded")
	}

	_ = d.Close()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("second Close of the writer: %v, want fs.ErrClosed", err)
	}

	if _, err := w.Create("/g"); !errors.Is(err, fs.ErrClosed) {
		t.Errorf("Create after Close: %v, want fs.ErrClosed", err)
	}

	// An entry never given a time has none: zero, not the build time.
	want := []corpus.Node{
		{Path: "f", Type: tar.TypeReg, Mode: 0o644, Data: []byte("first")},
		{Path: "d", Type: tar.TypeDir, Mode: 0o755},
	}
	im := corpus.Image{Name: "lifecycle", Img: out.Bytes(), Tree: want}
	corpus.CheckTree(t, im, parseImage(t, im), want, true)
}

// TestContractWriterEmptyImage: a writer closed with nothing added is an
// image of one empty root, which the reference and fsck.erofs read.
func TestContractWriterEmptyImage(t *testing.T) {
	t.Parallel()

	im := corpus.Image{Name: "empty", Img: writeImage(t, nil)}
	ref := parseImage(t, im)

	root, err := ref.Inode(ref.Super.RootNid)
	if err != nil {
		t.Fatal(err)
	}

	ents, err := ref.Dirents(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range ents {
		if n := string(e.Name); n != "." && n != ".." {
			t.Errorf("the empty image's root lists %q", n)
		}
	}

	if ents, err := fs.ReadDir(openImage(t, im), "."); err != nil || len(ents) != 0 {
		t.Errorf("ReadDir(\".\") = %v, %v; want none", ents, err)
	}

	fsckUnlessPlatformLimit(t, im)
}

// TestContractWriterTempDirHoldsTheSpool: "WithTempDir (where the spool
// lives otherwise)": with no data file, a file's bytes are spooled under the
// given directory, so a directory that does not exist fails the write, and
// an existing one carries the image to a whole read-back.
func TestContractWriterTempDirHoldsTheSpool(t *testing.T) {
	t.Parallel()

	data := corpus.Pattern(200_000)
	nodes := []corpus.Node{{Path: "big", Type: tar.TypeReg, Mode: 0o644, Data: data, Sec: corpus.BuildTime}}

	t.Run("missing", func(t *testing.T) {
		t.Parallel()

		w := erofs.Create(&testBuffer{}, erofs.WithTempDir(filepath.Join(t.TempDir(), "missing")))

		f, err := w.Create("/big")
		if err == nil {
			_, err = f.Write(data)
		}

		if err == nil {
			err = f.Close()
		}

		if err == nil {
			err = w.Close()
		}

		if err == nil {
			t.Fatal("a writer whose temp dir does not exist wrote a 200 kB file without an error")
		}
	})

	t.Run("present", func(t *testing.T) {
		t.Parallel()

		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0), erofs.WithTempDir(t.TempDir()))
		writeNode(t, w, nodes[0])

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		im := corpus.Image{Name: "spooled", Img: out.Bytes(), Tree: nodes}
		corpus.CheckTree(t, im, parseImage(t, im), nodes, true)
	})
}

// TestContractMetadataOnlyWritesNoData: "file data is not copied": a generic
// source copied metadata-only leaves none of its bytes in the image, which
// holds the file as holes (the model's rule, read back elsewhere); here the
// bytes themselves are looked for.
func TestContractMetadataOnlyWritesNoData(t *testing.T) {
	t.Parallel()

	marker := bytes.Repeat([]byte("EROFS_DATA_LEAK_CHECK!"), 200)
	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	src := fstest.MapFS{"f": &fstest.MapFile{Data: marker, Mode: 0o644, ModTime: time.Unix(corpus.BuildTime, 0)}}
	if err := errors.Join(w.CopyFrom(src, erofs.MetadataOnly()), w.Close()); err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(out.Bytes(), marker[:64]) {
		t.Error("the metadata-only image holds the source's bytes")
	}
}

// TestContractWriterOneOpenFile: "Only one file may be open for writing at
// a time", and "The caller must Close the returned File ... before calling
// Writer.Close".
func TestContractWriterOneOpenFile(t *testing.T) {
	t.Parallel()

	w := erofs.Create(&testBuffer{})

	a, err := w.Create("a")
	if err != nil {
		t.Fatal(err)
	}

	if _, second := w.Create("b"); second == nil {
		t.Error("a second Create while a is open succeeded")
	}

	if early := w.Close(); early == nil {
		t.Error("Close with a open succeeded")
	}

	if err = a.Close(); err != nil {
		t.Fatal(err)
	}

	b, err := w.Create("b")
	if err != nil {
		t.Fatalf("Create after a's Close: %v", err)
	}

	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
}
