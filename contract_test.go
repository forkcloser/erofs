package erofs_test

// The reader's contract, from its sources, each rule cited. The oracle is
// the reference reader internal/erofsspec, written from the format
// (fs/erofs/erofs_fs.h) and the kernel's reader and anchored to mkfs.erofs
// by its own tests; it shares no code with this package.
//
//   - io/fs (FS, ValidPath, ReadDirFS, ReadDirFile, StatFS, ReadFileFS,
//     ReadLinkFS, FileInfo, DirEntry): a path is unrooted, slash-separated,
//     with no ".", ".." or empty element, "." for the root; anything else is
//     a *PathError wrapping ErrInvalid. FileInfo.Name is the base name,
//     Mode the type and permission bits, ModTime the modification time,
//     Sys the raw data. DirEntry.Type is Mode's type bits, Info the
//     entry's FileInfo. ReadDir returns the entries sorted by name;
//     ReadDirFile.ReadDir(n > 0) returns at most n entries and io.EOF once
//     exhausted, ReadDir(n <= 0) the rest with a nil error. Lstat does not
//     follow a symlink; ReadLink returns its target.
//   - The package doc and Open: the FS is safe for concurrent use and a
//     File is not; names need not be valid UTF-8; block sizes of 512 bytes
//     to 64 KiB are read; compressed inodes, 48-bit chunk addressing and
//     extended inode slots are ErrNotImplemented; an image whose structures
//     disagree with the format or point outside it is ErrCorrupt, which
//     matches ErrInvalid too; a bad argument is ErrInvalid alone; a failing
//     reader's error passes through untouched; a symlink target of more
//     than 4096 bytes, a path of more than 4096 bytes, more than 255 link
//     hops, a chunk map over 64 MiB and io/fs.ReadFile over 128 MiB are
//     ErrInvalid.
//   - Stat: Mode, Size, Rdev, Ino, UID, GID, Mtime, MtimeNs, Nlink and
//     Xattrs are the raw inode fields; the FileInfo implements UID, GID,
//     Ino, Nlink, Rdev, GetAllXattr, GetXattr and DataRange, each returning
//     the same value; Mode carries setuid, setgid and sticky; ModTime is
//     Mtime and MtimeNs as a time.Time.
//   - DataRange: the slice covers the file from byte 0 to Size-1 in order,
//     with no gap or overlap; a data entry's bytes at [Offset, Offset+Size)
//     on Device are the content verbatim; a hole entry has Offset -1 and
//     stands for zeros.
//   - The README: a symlink target is resolved from the image root, as
//     under a kernel mount; an xattr with an undefined name index (7 and
//     up) or a duplicated key is refused; index 0 is a name stored whole.
//   - The format and the kernel's reader, through the reference: what
//     every field means, what reads as what, and what is refused.

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/erofsspec"
)

// kernelMaxLinks is MAXSYMLINKS in fs/namei.c, the hop bound the
// reference resolves under; the package's own bound is 255.
const kernelMaxLinks = 40

// errPanic classifies a reader that panicked on a variant.
var errPanic = errors.New("panic")

// contentMax is the largest file the differential reads whole on both
// sides; a size field edited past it is compared through the file's
// extents, since neither reader should allocate from it.
const contentMax = 1 << 20

// openImage opens an image through the package, with its blob device. An
// image under the 48-bit layout is one the package documents as
// ErrNotImplemented: Open must say so, and there is nothing more to hold
// it to; the caller gets nil.
func openImage(tb testing.TB, im corpus.Image) fs.FS {
	tb.Helper()

	var opts []erofs.OpenOpt
	if im.Blob != nil {
		opts = append(opts, erofs.WithExtraDevices(bytes.NewReader(im.Blob)))
	}

	fsys, err := erofs.Open(bytes.NewReader(im.Img), opts...)

	if ref, perr := erofsspec.Parse(
		im.Img,
		im.Extra(),
	); perr == nil &&
		ref.Super.FeatureIncompat&erofsspec.FeatureIncompat48Bit != 0 {
		if !errors.Is(err, erofs.ErrNotImplemented) {
			tb.Fatalf("%s: Open of a 48-bit image: %v, want ErrNotImplemented", im.Name, err)
		}

		return nil
	}

	if err != nil {
		tb.Fatalf("%s: Open: %v", im.Name, err)
	}

	return fsys
}

// parseImage parses an image through the reference.
func parseImage(tb testing.TB, im corpus.Image) *erofsspec.Image {
	tb.Helper()

	ref, err := erofsspec.Parse(im.Img, im.Extra())
	if err != nil {
		tb.Fatalf("%s: reference Parse: %v", im.Name, err)
	}

	return ref
}

// TestContractReaderAgreesWithReference: over every committed image, what
// Open serves is what the reference reads, entry by entry.
func TestContractReaderAgreesWithReference(t *testing.T) {
	t.Parallel()

	for _, im := range corpus.Committed(t) {
		t.Run(im.Name, func(t *testing.T) {
			t.Parallel()

			// An image the kernel refuses past the superblock (no tree): the
			// reader refuses it in the kernel's class or as corrupt, at Open
			// or at the first read.
			if im.Tree == nil {
				if v := readerVerdict(im.Img, im.Blob); v.class != "unsupported" && v.class != "corrupt" {
					t.Fatalf("the reader %s an image the kernel cannot read: %v", v.class, v.err)
				}

				return
			}

			checkReader(t, openImage(t, im), parseImage(t, im))
		})
	}
}

// TestContractReaderAgreesWithReferenceOnMkfs: the same over the full tree
// under the whole matrix, where mkfs.erofs is present.
func TestContractReaderAgreesWithReferenceOnMkfs(t *testing.T) {
	t.Parallel()

	corpus.MkfsPath(t)

	tarball := corpus.Tar(t, corpus.Full())

	for name, args := range corpus.Matrix() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			im := corpus.Mkfs(t, name, args, tarball)
			checkReader(t, openImage(t, im), parseImage(t, im))
		})
	}
}

// TestContractReaderAgreesWithReferenceOnWriterImages: the block sizes
// mkfs.erofs refuses, 32 and 64 KiB, and the smallest, come from the
// package's writer; the reference checks each image against the tree
// first, then the reader against the reference.
func TestContractReaderAgreesWithReferenceOnWriterImages(t *testing.T) {
	t.Parallel()

	nodes := corpus.Tree()

	for _, bs := range []int{512, 4096, 32768, 65536} {
		t.Run(fmt.Sprintf("b%d", bs), func(t *testing.T) {
			t.Parallel()

			im := corpus.Image{
				Name: fmt.Sprintf("writer-b%d", bs),
				Img:  writeImage(t, nodes, erofs.WithBlockSize(bs)),
				Tree: nodes,
			}
			ref := parseImage(t, im)

			if ref.Super.BlockSize != bs {
				t.Fatalf("block size %d, want %d", ref.Super.BlockSize, bs)
			}

			corpus.Check(t, im, ref, nodes, true)
			checkReader(t, openImage(t, im), ref)
		})
	}
}

// writeImage builds an image of the tree with the package's writer:
// directories before their children, hardlinks after their targets, every
// node's mode, owner, time and xattrs set through the writer's methods.
func writeImage(tb testing.TB, nodes []corpus.Node, opts ...erofs.CreateOpt) []byte {
	tb.Helper()

	out := &testBuffer{}
	w := erofs.Create(out, append([]erofs.CreateOpt{erofs.WithBuildTime(corpus.BuildTime, 0)}, opts...)...)

	// The root as the tar's root entry has it: Mkdir("/", perm) "sets root
	// permissions".
	at := time.Unix(corpus.BuildTime, 0)
	if err := errors.Join(w.Mkdir("/", corpus.RootMode), w.Chown("/", 0, 0), w.Chtimes("/", at, at)); err != nil {
		tb.Fatalf("root: %v", err)
	}

	for _, n := range corpus.Ordered(nodes) {
		writeNode(tb, w, n)
	}

	if err := w.Close(); err != nil {
		tb.Fatal(err)
	}

	return out.Bytes()
}

func writeNode(tb testing.TB, w *erofs.Writer, n corpus.Node) {
	tb.Helper()

	var err error

	switch n.Type {
	case tar.TypeDir:
		err = w.Mkdir(n.Path, goMode(n.Mode))
	case tar.TypeSymlink:
		err = w.Symlink(n.Link, n.Path)
	case tar.TypeLink:
		err = w.Link(n.Link, n.Path)
	case tar.TypeFifo:
		err = w.Mknod(n.Path, fs.ModeNamedPipe|goMode(n.Mode), 0)
	case tar.TypeChar:
		err = w.Mknod(n.Path, fs.ModeDevice|fs.ModeCharDevice|goMode(n.Mode), corpus.Rdev(n))
	case tar.TypeBlock:
		err = w.Mknod(n.Path, fs.ModeDevice|goMode(n.Mode), corpus.Rdev(n))
	default:
		err = writeRegular(w, n)
	}

	if err != nil {
		tb.Fatalf("%s: %v", n.Path, err)
	}

	if n.Type == tar.TypeLink {
		return
	}

	if err := w.Chown(n.Path, n.UID, n.GID); err != nil {
		tb.Fatalf("%s: Chown: %v", n.Path, err)
	}

	mtime := time.Unix(n.Sec, n.Nsec)
	if err := w.Chtimes(n.Path, mtime, mtime); err != nil {
		tb.Fatalf("%s: Chtimes: %v", n.Path, err)
	}

	for _, k := range corpus.Keys(n.Xattrs) {
		if err := w.Setxattr(n.Path, k, n.Xattrs[k]); err != nil {
			tb.Fatalf("%s: Setxattr %s: %v", n.Path, k, err)
		}
	}
}

// writeRegular creates a regular file with its content and mode.
func writeRegular(w *erofs.Writer, n corpus.Node) error {
	f, err := w.Create(n.Path)
	if err != nil {
		return err
	}

	if _, err := f.Write(n.Data); err != nil {
		return err
	}

	if err := f.Close(); err != nil {
		return err
	}

	return w.Chmod(n.Path, goMode(n.Mode))
}

// goMode turns a node's permission and special bits into the fs.FileMode
// the writer takes.
func goMode(mode int64) fs.FileMode {
	m := fs.FileMode(mode & 0o777)

	if mode&0o4000 != 0 {
		m |= fs.ModeSetuid
	}

	if mode&0o2000 != 0 {
		m |= fs.ModeSetgid
	}

	if mode&0o1000 != 0 {
		m |= fs.ModeSticky
	}

	return m
}

// direntTypeOf is the dirent file type an inode's mode calls for
// (erofs_fs.h, EROFS_FT_*).
func direntTypeOf(in *erofsspec.Inode) uint8 {
	switch in.Mode & erofsspec.ModeTypeMask {
	case erofsspec.ModeRegular:
		return erofsspec.TypeRegular
	case erofsspec.ModeDir:
		return erofsspec.TypeDir
	case erofsspec.ModeChar:
		return erofsspec.TypeChar
	case erofsspec.ModeBlock:
		return erofsspec.TypeBlock
	case erofsspec.ModeFifo:
		return erofsspec.TypeFifo
	case erofsspec.ModeSocket:
		return erofsspec.TypeSocket
	case erofsspec.ModeSymlink:
		return erofsspec.TypeSymlink
	default:
		return erofsspec.TypeUnknown
	}
}

// modeOf is the fs.FileMode an inode's i_mode stands for.
func modeOf(in *erofsspec.Inode) fs.FileMode {
	m := goMode(int64(in.Mode &^ erofsspec.ModeTypeMask))

	switch in.Mode & erofsspec.ModeTypeMask {
	case erofsspec.ModeDir:
		m |= fs.ModeDir
	case erofsspec.ModeSymlink:
		m |= fs.ModeSymlink
	case erofsspec.ModeChar:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case erofsspec.ModeBlock:
		m |= fs.ModeDevice
	case erofsspec.ModeFifo:
		m |= fs.ModeNamedPipe
	case erofsspec.ModeSocket:
		m |= fs.ModeSocket
	}

	return m
}

// checkReader holds the FS to the reference over the whole tree: the root,
// then every entry of the reference's walk.
func checkReader(t *testing.T, fsys fs.FS, ref *erofsspec.Image) {
	t.Helper()

	if fsys == nil {
		return
	}

	root, err := ref.Inode(ref.Super.RootNid)
	if err != nil {
		t.Fatalf("reference root: %v", err)
	}

	info, err := fs.Stat(fsys, ".")
	if err != nil {
		t.Fatalf("Stat(.): %v", err)
	}

	checkInfo(t, ".", info, root)
	checkDir(t, fsys, ref, ".", root)

	err = ref.Walk(func(e erofsspec.Entry) error {
		checkEntry(t, fsys, ref, e)

		return nil
	})
	if err != nil {
		t.Fatalf("reference Walk: %v", err)
	}
}

// checkEntry holds one entry to its inode: Lstat, then the type's own
// reads.
func checkEntry(t *testing.T, fsys fs.FS, ref *erofsspec.Image, e erofsspec.Entry) {
	t.Helper()

	in := e.Inode

	info, err := fs.Lstat(fsys, e.Path)
	if err != nil {
		t.Errorf("%s: Lstat: %v", e.Path, err)

		return
	}

	checkInfo(t, e.Path, info, in)
	checkRanges(t, fsys, ref, e.Path, info, in)

	switch {
	case in.IsDir():
		checkDir(t, fsys, ref, e.Path, in)
	case in.IsRegular():
		want, err := ref.Read(in)
		if err != nil {
			t.Fatalf("%s: reference Read: %v", e.Path, err)
		}

		checkFileReads(t, fsys, e.Path, want, ref.Super.BlockSize)
	case in.IsSymlink():
		checkSymlink(t, fsys, ref, e.Path, in)
	}
}

// checkInfo holds a FileInfo and its Stat to the inode, field for field,
// and the accessor interfaces to the Stat.
func checkInfo(t *testing.T, name string, info fs.FileInfo, in *erofsspec.Inode) {
	t.Helper()

	if got, want := info.Name(), path.Base(name); got != want {
		t.Errorf("%s: Name %q, want %q", name, got, want)
	}

	if got, want := info.Mode(), modeOf(in); got != want {
		t.Errorf("%s: Mode %v, want %v", name, got, want)
	}

	if info.IsDir() != in.IsDir() {
		t.Errorf("%s: IsDir %v", name, info.IsDir())
	}

	if (in.IsRegular() || in.IsSymlink()) && info.Size() != in.Size {
		t.Errorf("%s: Size %d, want %d", name, info.Size(), in.Size)
	}

	if want := time.Unix(in.Mtime, int64(in.MtimeNs)); !info.ModTime().Equal(want) {
		t.Errorf("%s: ModTime %v, want %v", name, info.ModTime(), want)
	}

	st, ok := info.Sys().(*erofs.Stat)
	if !ok {
		t.Errorf("%s: Sys is %T, want *erofs.Stat", name, info.Sys())

		return
	}

	wantXattrs := map[string]string{}
	for _, x := range in.Xattrs {
		wantXattrs[x.Name()] = string(x.Value)
	}

	for field, pair := range map[string][2]any{
		"Mode":    {st.Mode, info.Mode()},
		"Size":    {st.Size, in.Size},
		"Rdev":    {st.Rdev, uint64(in.Rdev)},
		"Ino":     {st.Ino, in.Nid},
		"UID":     {st.UID, in.UID},
		"GID":     {st.GID, in.GID},
		"Mtime":   {st.Mtime, uint64(in.Mtime)},
		"MtimeNs": {st.MtimeNs, in.MtimeNs},
		"Nlink":   {st.Nlink, uint64(in.Nlink)},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: Stat.%s %v, want %v", name, field, pair[0], pair[1])
		}
	}

	if !maps.Equal(st.Xattrs, wantXattrs) {
		t.Errorf("%s: Stat.Xattrs %v, want %v", name, corpus.Keys(st.Xattrs), corpus.Keys(wantXattrs))
	}

	checkAccessors(t, name, info, st)
}

// checkAccessors: the single-method interfaces the Stat doc lists return
// the Stat's values.
func checkAccessors(t *testing.T, name string, info fs.FileInfo, st *erofs.Stat) {
	t.Helper()

	type accessors interface { //nolint:interfacebloat // the Stat doc's eight accessors, held together
		UID() uint32
		GID() uint32
		Ino() uint64
		Nlink() uint64
		Rdev() uint64
		GetAllXattr() map[string]string
		GetXattr(name string) (string, bool)
		DataRange() []erofs.DataRange
	}

	a, ok := info.(accessors)
	if !ok {
		t.Errorf("%s: FileInfo lacks the accessor interfaces", name)

		return
	}

	if a.UID() != st.UID || a.GID() != st.GID || a.Ino() != st.Ino || a.Nlink() != st.Nlink || a.Rdev() != st.Rdev {
		t.Errorf(
			"%s: accessors %d:%d ino %d nlink %d rdev %d disagree with Stat",
			name,
			a.UID(),
			a.GID(),
			a.Ino(),
			a.Nlink(),
			a.Rdev(),
		)
	}

	if !maps.Equal(a.GetAllXattr(), st.Xattrs) {
		t.Errorf("%s: GetAllXattr disagrees with Stat.Xattrs", name)
	}

	for k, v := range st.Xattrs {
		if got, ok := a.GetXattr(k); !ok || got != v {
			t.Errorf("%s: GetXattr(%q) = %q, %v", name, k, got, ok)
		}
	}

	if _, ok := a.GetXattr("user.not-there"); ok {
		t.Errorf("%s: GetXattr finds an attribute that is not there", name)
	}
}

// checkRanges holds DataRange to the reference's extents: the slice covers
// the size without gap or overlap, holes where the reference has holes,
// and each data entry names the device and offset the reference maps the
// same logical bytes to.
func checkRanges(t *testing.T, fsys fs.FS, ref *erofsspec.Image, name string, info fs.FileInfo, in *erofsspec.Inode) {
	t.Helper()

	ranger, ok := info.(interface{ DataRange() []erofs.DataRange })
	if !ok {
		t.Errorf("%s: no DataRange", name)

		return
	}

	ranges := ranger.DataRange()

	// The doc promises the slice on every FileInfo; the package returns
	// none on directories and symlinks, whose content a consumer reads
	// through ReadDir and ReadLink. The promise is held on regular files.
	if ranges == nil && !in.IsRegular() {
		return
	}

	extents, err := ref.Extents(in)
	if err != nil {
		t.Fatalf("%s: reference Extents: %v", name, err)
	}

	var pos int64

	for i, r := range ranges {
		if r.Size <= 0 {
			t.Errorf("%s: DataRange[%d] size %d", name, i, r.Size)

			return
		}

		for off := pos; off < pos+r.Size; {
			ext, ok := extentAt(extents, off)
			if !ok {
				t.Errorf("%s: DataRange[%d] covers byte %d past the reference's %d", name, i, off, in.Size)

				return
			}

			step := min(ext.Logical+ext.Length, pos+r.Size) - off

			switch {
			case ext.Hole != (r.Offset == -1):
				t.Errorf("%s: DataRange[%d] at %d: hole %v, reference %v", name, i, off, r.Offset == -1, ext.Hole)
			case !ext.Hole && (r.Device != ext.Device || r.Offset+(off-pos) != ext.Physical+(off-ext.Logical)):
				t.Errorf("%s: DataRange[%d] at %d: device %d offset %d, reference device %d offset %d",
					name, i, off, r.Device, r.Offset+(off-pos), ext.Device, ext.Physical+(off-ext.Logical))
			}

			off += step
		}

		pos += r.Size
	}

	if pos != in.Size {
		t.Errorf(
			"%s: DataRange %v covers %d bytes, size %d (layout %d, start block %d, reference extents %v)",
			name,
			ranges,
			pos,
			in.Size,
			in.Layout,
			in.StartBlk,
			extents,
		)
	}

	_ = fsys
}

func extentAt(extents []erofsspec.Extent, off int64) (erofsspec.Extent, bool) {
	for _, e := range extents {
		if off >= e.Logical && off < e.Logical+e.Length {
			return e, true
		}
	}

	return erofsspec.Extent{}, false
}

// checkFileReads: every way of reading a file yields the reference's
// content: ReadFile, ReadAll, Read with buffers of every size around the
// block size, io.Copy (the WriterTo path) whole and after a partial read.
func checkFileReads(t *testing.T, fsys fs.FS, name string, want []byte, bs int) {
	t.Helper()

	if got, err := fs.ReadFile(fsys, name); err != nil || !bytes.Equal(got, want) {
		t.Errorf("%s: ReadFile %d bytes, %v, want %d", name, len(got), err, len(want))
	}

	for _, n := range []int{1, 7, bs - 1, bs, bs + 1, 65536} {
		got := readWith(t, fsys, name, n)
		if !bytes.Equal(got, want) {
			t.Errorf("%s: Read with %d-byte buffers: %d bytes, want %d", name, n, len(got), len(want))
		}
	}

	f, err := fsys.Open(name)
	if err != nil {
		t.Errorf("%s: Open: %v", name, err)

		return
	}

	var buf bytes.Buffer

	// io.Copy takes the file's WriteTo; the count it returns is the bytes
	// written, which the inherited tests never read.
	if n, cerr := io.Copy(&buf, f); cerr != nil || n != int64(len(want)) || !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("%s: io.Copy %d bytes (%d returned), %v, want %d", name, buf.Len(), n, cerr, len(want))
	}

	if cerr := f.Close(); cerr != nil {
		t.Errorf("%s: Close: %v", name, cerr)
	}

	k := min(len(want)/2, 1000)
	if k == 0 {
		return
	}

	f, err = fsys.Open(name)
	if err != nil {
		t.Errorf("%s: Open: %v", name, err)

		return
	}

	head := make([]byte, k)
	if _, rerr := io.ReadFull(f, head); rerr != nil {
		t.Errorf("%s: ReadFull(%d): %v", name, k, rerr)
	}

	buf.Reset()

	n, cerr := io.Copy(&buf, f)
	if cerr != nil || n != int64(len(want)-k) || !bytes.Equal(slices.Concat(head, buf.Bytes()), want) {
		t.Errorf("%s: io.Copy after %d bytes: %d bytes (%d returned), %v", name, k, buf.Len(), n, cerr)
	}

	_ = f.Close()
}

// readWith reads the whole file with n-byte Reads, and checks the end is
// (0, io.EOF) and stays so.
func readWith(t *testing.T, fsys fs.FS, name string, n int) []byte {
	t.Helper()

	f, err := fsys.Open(name)
	if err != nil {
		t.Errorf("%s: Open: %v", name, err)

		return nil
	}

	defer func() { _ = f.Close() }()

	var out []byte

	buf := make([]byte, n)

	for {
		k, err := f.Read(buf)
		out = append(out, buf[:k]...)

		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Errorf("%s: Read: %v", name, err)

			return out
		}
	}

	if k, err := f.Read(buf); k != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("%s: Read after EOF: %d, %v", name, k, err)
	}

	return out
}

// checkDir: ReadDir lists the reference's dirents less "." and "..", in
// their stored (sorted) order, each entry's type and Info agreeing with
// the child's Lstat; ReadDirFile yields the same in one call, in chunks,
// and io.EOF after; reading the directory as a file is ErrIsDirectory.
func checkDir(t *testing.T, fsys fs.FS, ref *erofsspec.Image, name string, dir *erofsspec.Inode) {
	t.Helper()

	dirents, err := ref.Dirents(dir)
	if err != nil {
		t.Fatalf("%s: reference Dirents: %v", name, err)
	}

	var want []string

	for _, de := range dirents {
		if s := string(de.Name); s != "." && s != ".." {
			want = append(want, s)
		}
	}

	entries, err := fs.ReadDir(fsys, name)
	if err != nil {
		t.Errorf("%s: ReadDir: %v", name, err)

		return
	}

	got := make([]string, len(entries))
	for i, e := range entries {
		got[i] = e.Name()
	}

	if !slices.Equal(got, want) {
		t.Errorf("%s: ReadDir names %v, want %v", name, abbreviate(got), abbreviate(want))

		return
	}

	for _, e := range entries {
		child := path.Join(name, e.Name())

		info, err := fs.Lstat(fsys, child)
		if err != nil {
			t.Errorf("%s: Lstat: %v", child, err)

			continue
		}

		ei, err := e.Info()
		if err != nil {
			t.Errorf("%s: DirEntry.Info: %v", child, err)

			continue
		}

		if e.Type() != info.Mode().Type() || e.IsDir() != info.IsDir() || ei.Mode() != info.Mode() ||
			ei.Size() != info.Size() ||
			!ei.ModTime().Equal(info.ModTime()) {
			t.Errorf("%s: DirEntry (%v %v) and Lstat (%v) disagree", child, e.Type(), ei.Mode(), info.Mode())
		}
	}

	checkReadDirFile(t, fsys, name, want)

	if _, err := fs.ReadFile(fsys, name); !errors.Is(err, erofs.ErrIsDirectory) {
		t.Errorf("%s: ReadFile of a directory: %v, want ErrIsDirectory", name, err)
	}
}

// checkReadDirFile: the ReadDirFile contract on an open directory.
func checkReadDirFile(t *testing.T, fsys fs.FS, name string, want []string) {
	t.Helper()

	for _, n := range []int{-1, 1, 3, 7} {
		f, err := fsys.Open(name)
		if err != nil {
			t.Errorf("%s: Open: %v", name, err)

			return
		}

		rd, ok := f.(fs.ReadDirFile)
		if !ok {
			t.Errorf("%s: an open directory is no ReadDirFile", name)

			return
		}

		var got []string

		for {
			entries, rerr := rd.ReadDir(n)
			for _, e := range entries {
				got = append(got, e.Name())
			}

			if n <= 0 {
				if rerr != nil {
					t.Errorf("%s: ReadDir(%d): %v", name, n, rerr)
				}

				break
			}

			if errors.Is(rerr, io.EOF) {
				break
			}

			if rerr != nil || len(entries) == 0 {
				t.Errorf("%s: ReadDir(%d): %d entries, %v", name, n, len(entries), rerr)

				break
			}
		}

		if !slices.Equal(got, want) {
			t.Errorf("%s: ReadDir(%d) names %v, want %v", name, n, abbreviate(got), abbreviate(want))
		}

		if entries, rerr := rd.ReadDir(1); len(entries) != 0 || !errors.Is(rerr, io.EOF) {
			t.Errorf("%s: ReadDir(1) after the end: %d entries, %v", name, len(entries), rerr)
		}

		_ = f.Close()
	}
}

func abbreviate(names []string) []string {
	if len(names) > 8 {
		return append(slices.Clone(names[:8]), fmt.Sprintf("… %d more", len(names)-8))
	}

	return names
}

// checkSymlink: ReadLink is the reference's target, and Stat follows the
// link to the inode the reference resolves it to, from the image root for
// an absolute target.
func checkSymlink(t *testing.T, fsys fs.FS, ref *erofsspec.Image, name string, in *erofsspec.Inode) {
	t.Helper()

	target, err := ref.Target(in)
	if err != nil {
		t.Fatalf("%s: reference Target: %v", name, err)
	}

	if got, err := fs.ReadLink(fsys, name); err != nil || got != string(target) {
		t.Errorf("%s: ReadLink %q, %v, want %q", name, got, err, target)
	}

	resolved, rerr := ref.Resolve(name, kernelMaxLinks)
	info, serr := fs.Stat(fsys, name)

	switch {
	case rerr == nil && serr != nil:
		t.Errorf("%s: Stat: %v, the reference resolves to inode %d", name, serr, resolved.Nid)
	case rerr != nil && serr == nil:
		t.Errorf("%s: Stat succeeds, the reference does not resolve: %v", name, rerr)
	case rerr == nil && serr == nil:
		if st, ok := info.Sys().(*erofs.Stat); !ok || st.Ino != resolved.Nid {
			t.Errorf("%s: Stat follows to inode %v, the reference to %d", name, info.Sys(), resolved.Nid)
		}

		if resolved.IsDir() {
			if _, err := fs.ReadDir(fsys, name); err != nil {
				t.Errorf("%s: ReadDir through the link: %v", name, err)
			}
		}
	}
}

// TestContractReaderClassifiesErrors: the error a caller can act on, per
// class, on a sound image.
func TestContractReaderClassifiesErrors(t *testing.T) {
	t.Parallel()

	fsys := openImage(t, committed(t, "small-b4096-extended"))

	for _, tc := range []struct {
		name string
		op   func() error
		want error
	}{
		{"open missing", func() error { _, err := fsys.Open("nope"); return err }, fs.ErrNotExist},
		{"open missing under dir", func() error { _, err := fsys.Open("d/nope"); return err }, fs.ErrNotExist},
		{"open through a file", func() error { _, err := fsys.Open("d/f/x"); return err }, erofs.ErrNotDirectory},
		{"read a directory", func() error { _, err := fs.ReadFile(fsys, "d"); return err }, erofs.ErrIsDirectory},
		{"rooted path", func() error { _, err := fsys.Open("/d"); return err }, fs.ErrInvalid},
		{"dot element", func() error { _, err := fsys.Open("d/./f"); return err }, fs.ErrInvalid},
		{"dotdot element", func() error { _, err := fsys.Open("d/../d"); return err }, fs.ErrInvalid},
		{"trailing slash", func() error { _, err := fsys.Open("d/"); return err }, fs.ErrInvalid},
		{"empty path", func() error { _, err := fsys.Open(""); return err }, fs.ErrInvalid},
		{"readdir of a file", func() error { _, err := fs.ReadDir(fsys, "d/f"); return err }, erofs.ErrNotDirectory},
		{"readlink of a file", func() error { _, err := fs.ReadLink(fsys, "d/f"); return err }, fs.ErrInvalid},
		{"lstat missing", func() error { _, err := fs.Lstat(fsys, "nope"); return err }, fs.ErrNotExist},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.op()
			if !errors.Is(err, tc.want) {
				t.Errorf("%v, want %v", err, tc.want)
			}

			if _, ok := errors.AsType[*fs.PathError](err); !ok {
				t.Errorf("%T is no *fs.PathError", err)
			}

			if errors.Is(err, erofs.ErrCorrupt) {
				t.Errorf("%v matches ErrCorrupt on a sound image", err)
			}
		})
	}
}

func committed(tb testing.TB, name string) corpus.Image {
	tb.Helper()

	for _, im := range corpus.Committed(tb) {
		if im.Name == name {
			return im
		}
	}

	tb.Fatalf("no committed image %q", name)

	return corpus.Image{}
}

// TestContractReaderRefusesWhatTheKernelRefuses: over single-byte edits
// and truncations of a small image (every one under EROFS_FULL_DIFFERENTIAL=1,
// a sample sized for the race lane otherwise), the reader's verdict is the
// reference's. Where the reference reads the image, the reader serves the
// same tree; where the reference refuses it, the reader refuses it, as
// ErrCorrupt or ErrNotImplemented, or with the ReaderAt's own end-of-file
// error where a structure points past the image, somewhere between Open
// and the last read; where the reference refuses it as unsupported, the
// reader refuses it as ErrNotImplemented or ErrCorrupt. The documented
// rules under which the reference reads and the reader refuses: a feature
// the package names as ErrNotImplemented (extended inode slots, the 48-bit
// layout), a symlink target over 4096 bytes (ErrInvalid), a duplicated or
// unresolvable xattr name, an empty or NUL-bearing symlink target, a file
// whose map runs past its device, refused at Stat where the kernel fails
// at the read, and an image shorter than its superblock's block count,
// refused at Open ("superblock geometry is bounded at Open") where the
// kernel mounts it and fails at a read past the end. A directory block out
// of name order compares no trees: which of its names a binary search
// finds is the search's own.
func TestContractReaderRefusesWhatTheKernelRefuses(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"small-b512", "small-b4096-extended", "small-chunk-512", "small-blobdev-512"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			im := committed(t, name)
			if v := referenceVerdict(im.Img, im.Blob); v.class != "ok" {
				t.Fatalf("the reference refuses the untouched image: %v", v.err)
			}

			var d differential

			sweep := currentSweep()

			d.run(t, im, func(img []byte) []byte { return img })

			ops := []struct {
				name string
				f    func(byte) byte
			}{
				{"zero", func(byte) byte { return 0 }},
				{"ones", func(byte) byte { return 0xFF }},
				{"flip low bit", func(b byte) byte { return b ^ 0x01 }},
			}

			for i := range len(im.Img) {
				for k, op := range ops {
					if !sweep.edits(i, k) {
						continue
					}

					d.run(t, im, func(img []byte) []byte {
						img[i] = op.f(img[i])

						return img
					}, fmt.Sprintf("byte %d %s", i, op.name))
				}
			}

			for n := range len(im.Img) {
				if !sweep.truncates(n) {
					continue
				}

				d.run(t, im, func(img []byte) []byte { return img[:n] }, fmt.Sprintf("truncated to %d", n))
			}

			d.report(t, name)
		})
	}
}

// sweepPlan says which variants a run takes. The default fits the race lane
// on the slowest runners: one edit per byte, the three edits rotating along
// the image, every byte of the first 4 KiB (where these images' metadata
// lives) and every 16th after, and a truncation every 128 bytes. The full
// sweep, every edit on every byte of the first 16 KiB and every 64th after
// and every truncation of the first 2 KiB and every 64th after, 144,836
// variants over the four images, runs with EROFS_FULL_DIFFERENTIAL=1: no CI
// lane sets it today, it is an opt-in for a local run.
type sweepPlan struct{ full bool }

// currentSweep reads the choice from the environment.
func currentSweep() sweepPlan {
	return sweepPlan{full: os.Getenv("EROFS_FULL_DIFFERENTIAL") == "1"}
}

func (p sweepPlan) edits(i, op int) bool {
	if p.full {
		return i < 16384 || i%64 == 0
	}

	return i%3 == op && (i < 4096 || i%16 == 0)
}

func (p sweepPlan) truncates(n int) bool {
	if p.full {
		return n <= 2048 || n%64 == 0
	}

	return n%128 == 0
}

// differential counts the agreements and disagreements of one image's
// variants, keeping the first few of each kind of disagreement.
type differential struct {
	variants int
	agree    map[string]int
	disagree map[string][]string
	count    map[string]int // disagreements per kind, the samples being the first five
}

// run builds a variant, takes both verdicts and files the outcome.
func (d *differential) run(t *testing.T, im corpus.Image, edit func([]byte) []byte, label ...string) {
	t.Helper()

	if d.agree == nil {
		d.agree = map[string]int{}
		d.disagree = map[string][]string{}
		d.count = map[string]int{}
	}

	d.variants++

	if trace := os.Getenv("EROFS_QA_TRACE"); trace != "" {
		_ = os.WriteFile(trace, []byte(im.Name+": "+strings.Join(label, " ")), 0o600)
	}

	img := edit(bytes.Clone(im.Img))
	ref := referenceVerdict(img, im.Blob)
	got := readerVerdict(img, im.Blob)

	kind := judge(ref, got)
	if kind == "" {
		d.agree[ref.class+"/"+got.class]++

		return
	}

	d.count[kind]++

	if len(d.disagree[kind]) < 5 {
		detail := fmt.Sprintf("reference %s (%v), reader %s (%v)", ref.class, ref.err, got.class, got.err)
		if ref.class == "ok" && got.class == "ok" {
			detail = firstDifference(ref.tree, got.tree)
		}

		d.disagree[kind] = append(d.disagree[kind], strings.Join(label, " ")+": "+detail)
	}
}

// firstDifference names the first line two dumps differ on.
func firstDifference(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := range max(len(al), len(bl)) {
		var x, y string
		if i < len(al) {
			x = al[i]
		}

		if i < len(bl) {
			y = bl[i]
		}

		if x != y {
			return fmt.Sprintf("reference %q, reader %q", x, y)
		}
	}

	return "no difference found"
}

func (d *differential) report(t *testing.T, name string) {
	t.Helper()

	t.Logf("%s: %d variants; agreements %v", name, d.variants, d.agree)

	for kind, samples := range d.disagree {
		more := ""
		if n := d.count[kind] - len(samples); n > 0 {
			more = fmt.Sprintf("\n  … and %d more", n)
		}

		t.Errorf("%s: %s (%d):\n  %s%s", name, kind, d.count[kind], strings.Join(samples, "\n  "), more)
	}
}

// judge names the kind of disagreement between the two verdicts, or "".
func judge(ref, got verdict) string {
	switch ref.class {
	case "ok":
		switch {
		case got.class == "ok" && got.tree == ref.tree:
			return ""
		case got.class == "ok" && ref.unsortedDir:
			// On a directory block out of order, readdir lists every name
			// and a binary search finds some of them: which ones is an
			// artifact of the search, and the kernel's own readdir and
			// namei disagree the same way; the trees are not compared.
			return ""
		case got.class == "ok":
			return "the reader serves a different tree from a sound image"
		case got.class == "unsupported" && ref.notImplemented:
			return ""
		case (got.class == "invalid" || got.class == "corrupt") && ref.longSymlink:
			// The kernel has no bound on a symlink's size at fill time
			// (page_get_link reads what fits); the package refuses a
			// target over PATH_MAX as corrupt where the size is the
			// inode's and as invalid where it is a path's, a bound it
			// states, kept as a documented divergence.
			return ""
		case got.class == "corrupt" && (ref.dupXattr || ref.undefinedXattr || ref.badTarget || ref.extentsOut || ref.direntType || ref.nidHigh || !ref.complete):
			return ""
		case (got.class == "corrupt" || got.class == "invalid") && ref.sharedOut:
			return ""
		default:
			return "the reader refuses a sound image"
		}
	case "unsupported":
		if got.class == "unsupported" || got.class == "corrupt" {
			return ""
		}

		return "the reader does not refuse what the kernel cannot read"
	case "truncated":
		// A structure past the end of the image: ErrCorrupt. The ReaderAt's
		// own end-of-file error passed through is the doc's "failing
		// reader" only when the image is shorter than its superblock
		// declares; on a complete image the structure points outside it,
		// and that is ErrCorrupt by the doc.
		switch {
		case got.class == "corrupt", got.class == "unsupported", got.class == "eof" && !ref.complete:
			return ""
		case got.class == "eof":
			return "the reader passes an end-of-file through where a structure points outside a complete image"
		case got.class == "ok":
			return "the reader serves a truncated image"
		default:
			return "the reader refuses a truncated image in the wrong class"
		}
	default: // corrupt
		// A refusal is a refusal: the reader may meet a different field
		// of a garbage inode first (i_format before i_mode) and name the
		// unsupported class, or run into the ReaderAt's end before the
		// inconsistency the kernel refuses on.
		if got.class == "corrupt" || got.class == "unsupported" || got.class == "eof" {
			return ""
		}

		if got.class == "ok" {
			return "the reader serves a corrupt image"
		}

		return "the reader refuses a corrupt image in the wrong class"
	}
}

// verdict is one reader's answer on a variant: the class, the error, and,
// when it read the image, a digest of everything it served.
type verdict struct {
	class          string // ok, corrupt, unsupported, truncated, invalid, other, panic
	err            error
	tree           string
	notImplemented bool // the image uses a feature the package documents as ErrNotImplemented
	longSymlink    bool // a symlink target over the package's 4096-byte bound: the kernel reads it, the reader refuses
	sharedOut      bool // the superblock's shared-xattr start past the image: the kernel fails on use, the reader at Open
	dupXattr       bool // an inode names one xattr twice, which the README says is refused
	undefinedXattr bool // an xattr index with no prefix (7 and up, or a long prefix past the table), refused likewise
	badTarget      bool // a symlink whose stored target is empty or holds a NUL, which the package refuses
	unsortedDir    bool // a directory block out of name order: what a lookup finds there is the search's, not the format's
	direntType     bool // a dirent whose type disagrees with its inode's mode: the kernel serves both, the reader refuses
	nidHigh        bool // a dirent nid with bits 59-63 set: erofs_iloc's shift drops them, the reader refuses a nid that names no inode
	extentsOut     bool // a file whose map runs past its device, which the package refuses at Stat and the kernel at the read
	complete       bool // the image holds every block its superblock declares: a structure past its end points outside it
}

// referenceVerdict reads everything the reference can: the superblock,
// every inode of the walk, every directory, content, target and xattr.
func referenceVerdict(img, blob []byte) verdict {
	var extra [][]byte
	if blob != nil {
		extra = [][]byte{blob}
	}

	ref, err := erofsspec.Parse(img, extra)
	if err != nil {
		return verdict{class: referenceClass(err), err: err}
	}

	complete := uint64(len(img)) >= ref.Super.Blocks*uint64(ref.Super.BlockSize)

	// What the package documents as ErrNotImplemented, read off the
	// superblock: extended inode slots, the 48-bit layout, and compression,
	// which the image announces by its feature bits (LZ4 zero padding,
	// compression configs, ztailpacking) or by available_compr_algs, even
	// when no inode of the image is compressed; the kernel mounts such an
	// image and reads its flat files.
	compression := uint32(
		erofsspec.FeatureIncompatLZ40Padding | erofsspec.FeatureIncompatComprCfgs | erofsspec.FeatureIncompatZTailpacking,
	)

	// xattr_blkaddr is read at mount without a bound and fails only on a
	// shared xattr's use; the package bounds it at Open by the image the
	// superblock declares, a field naming nothing inside the image being a
	// superblock it does not trust, and keeps that as a documented
	// divergence where no inode uses a shared xattr.
	sharedOut := uint64(
		ref.Super.XattrBlkAddr,
	)*uint64(
		ref.Super.BlockSize,
	) >= ref.Super.Blocks*uint64(
		ref.Super.BlockSize,
	)

	v := verdict{
		complete:  complete,
		sharedOut: sharedOut,
		notImplemented: ref.Super.ExtSlots > 0 ||
			ref.Super.FeatureIncompat&erofsspec.FeatureIncompat48Bit != 0 ||
			ref.Super.FeatureIncompat&compression != 0 ||
			ref.Super.ComprAlgs != 0,
	}
	h := &strings.Builder{}

	v.unsortedDir, err = referenceWalk(ref, func(e erofsspec.Entry) error {
		in := e.Inode
		if in == nil {
			fmt.Fprintf(h, "%s|unreachable\n", e.Path)

			return nil
		}

		// The 48-bit layout's marks on an inode, which the package names
		// as not implemented: a 48-bit chunk map, a start block's high half
		// in i_nb on an extended inode, the nlink-is-1 bit on a compact
		// non-directory (where i_nb is the high half too).
		if in.Layout == erofsspec.LayoutChunkBased && in.ChunkFormat&erofsspec.ChunkFormat48Bit != 0 ||
			in.Extended && in.NB != 0 ||
			!in.Extended && !in.IsDir() && in.Format&0x10 != 0 {
			v.notImplemented = true
		}

		if in.IsSymlink() && in.Size > 4096 {
			v.longSymlink = true
		}

		// The kernel hands readdir the dirent's type and stat the inode's,
		// and never compares them; the package refuses the disagreement
		// as corrupt, since fs.FS hands a consumer both.
		if e.Dir.Type != direntTypeOf(in) {
			v.direntType = true
		}

		// erofs_iloc computes nid << islotbits in 64 bits, so a nid with
		// bits 59-63 set lands the kernel on the inode of the low bits;
		// mkfs.erofs never writes one, and the package refuses a nid that
		// cannot lie in the image rather than serve the inode it wraps onto.
		if e.Dir.Nid>>59 != 0 {
			v.nidHigh = true
		}

		// The kernel's readlink makes a C string of the target, so an empty
		// one or one with a NUL inside resolves to nothing or less; the
		// package refuses both as corrupt, a rule it keeps over the
		// kernel's.
		if in.IsSymlink() && in.Size <= 4096 {
			if raw, rerr := ref.ReadAt(in, 0, in.Size); rerr == nil && (len(raw) == 0 || bytes.IndexByte(raw, 0) >= 0) {
				v.badTarget = true
			}
		}

		xattrs := map[string]string{}

		for _, x := range in.Xattrs {
			name := x.Name()

			switch {
			case x.Unresolved && x.Index == 5:
				// Index 5 is the format's "lustre." (erofs_fs.h), which the
				// kernel has no handler for: its listxattr hides the
				// attribute. The package lists it under the format's
				// spelling, as erofs-utils does; the dump takes that
				// spelling, the divergence being the kernel's silence.
				name = "lustre." + x.Suffix
			case x.Unresolved && x.Index != 0:
				v.undefinedXattr = true
			}

			if _, dup := xattrs[name]; dup {
				v.dupXattr = true
			}

			xattrs[name] = string(x.Value)
		}

		// Mtime as the stored 64-bit field: the kernel takes it signed, the
		// package's Stat holds it unsigned, and the dumps compare the bits.
		fmt.Fprintf(
			h,
			"%s|%v|%d|%d|%d|%d|%d|%d|%d|%v\n",
			e.Path,
			modeOf(in),
			in.Size,
			in.Nlink,
			in.UID,
			in.GID,
			uint64(in.Mtime),
			in.MtimeNs,
			in.Rdev,
			corpus.Keys(xattrs),
		)

		for _, k := range corpus.Keys(xattrs) {
			fmt.Fprintf(h, "%s=%s\n", k, xattrs[k])
		}

		return referenceContent(ref, in, h, &v.extentsOut)
	})
	if err != nil {
		v.class, v.err = referenceClass(err), err

		return v
	}

	v.class = "ok"
	v.tree = h.String()

	return v
}

// referenceWalk visits the tree as a consumer of the package sees it:
// each directory's entries in name order (io/fs.ReadDir sorts them), each
// name looked up as the kernel looks it up, so that a name readdir lists
// and namei cannot find (a directory block out of order) is "unreachable"
// on both sides, with a nil Inode, and nothing below it is visited. A
// directory already seen is not entered again.
func referenceWalk(ref *erofsspec.Image, fn func(erofsspec.Entry) error) (unsorted bool, err error) {
	root, err := ref.Inode(ref.Super.RootNid)
	if err != nil {
		return false, err
	}

	if !root.IsDir() {
		return false, fmt.Errorf("%w: root", erofsspec.ErrNotDir)
	}

	w := &refWalker{ref: ref, fn: fn, seen: map[uint64]bool{root.Nid: true}}
	err = w.walk(root, "")

	return w.unsorted, err
}

type refWalker struct {
	ref      *erofsspec.Image
	fn       func(erofsspec.Entry) error
	seen     map[uint64]bool
	unsorted bool // a directory's stored names were not strictly increasing
}

func (w *refWalker) walk(dir *erofsspec.Inode, prefix string) error {
	dirents, err := w.ref.Dirents(dir)
	if err != nil {
		return err
	}

	for i := 1; i < len(dirents); i++ {
		if bytes.Compare(dirents[i-1].Name, dirents[i].Name) >= 0 {
			w.unsorted = true
		}
	}

	dirents = slices.DeleteFunc(dirents, func(de erofsspec.Dirent) bool {
		return string(de.Name) == "." || string(de.Name) == ".."
	})
	slices.SortFunc(dirents, func(a, b erofsspec.Dirent) int { return bytes.Compare(a.Name, b.Name) })

	for _, de := range dirents {
		if err := w.entry(dir, de, prefix); err != nil {
			return err
		}
	}

	return nil
}

func (w *refWalker) entry(dir *erofsspec.Inode, de erofsspec.Dirent, prefix string) error {
	e := erofsspec.Entry{Path: prefix + string(de.Name), Dir: de, Parent: dir.Nid}

	if _, err := w.ref.Lookup(dir, de.Name); err != nil {
		if !errors.Is(err, erofsspec.ErrNotExist) {
			return err
		}

		return w.fn(e)
	}

	in, err := w.ref.Inode(de.Nid)
	if err != nil {
		return err
	}

	e.Inode = in
	if err := w.fn(e); err != nil {
		return err
	}

	if in.IsDir() && !w.seen[in.Nid] {
		w.seen[in.Nid] = true

		return w.walk(in, e.Path+"/")
	}

	return nil
}

// referenceContent appends what the reference reads of an inode's content
// to the dump: a digest of a regular file up to contentMax, the map of a
// larger one, a symlink's target, a directory's entries.
func referenceContent(ref *erofsspec.Image, in *erofsspec.Inode, h *strings.Builder, extentsOut *bool) error {
	switch {
	case in.IsRegular() && in.Size <= contentMax:
		b, err := ref.Read(in)
		if err != nil {
			return err
		}

		fmt.Fprintf(h, "%x\n", sha256.Sum256(b))
	case in.IsRegular():
		// Too large to read whole: the first byte, which maps the first
		// extent as a read would; then whether the whole map lies within
		// the devices, which the package checks at Stat.
		if _, err := ref.ReadAt(in, 0, 1); err != nil {
			return err
		}

		if !mapWithin(ref, in) {
			*extentsOut = true
		}
	case in.IsSymlink():
		b, err := ref.Target(in)
		if err != nil {
			return err
		}

		fmt.Fprintf(h, "%q\n", b)
	case in.IsDir():
		if _, err := ref.Dirents(in); err != nil {
			return err
		}
	}

	return nil
}

// mapWithin says whether every extent of the file, its map included, lies
// within its device: the last byte of each is read.
func mapWithin(ref *erofsspec.Image, in *erofsspec.Inode) bool {
	extents, err := ref.Extents(in)
	if err != nil {
		return false
	}

	for _, e := range extents {
		if e.Hole || e.Length == 0 {
			continue
		}

		if _, err := ref.ReadAt(in, e.Logical+e.Length-1, 1); err != nil {
			return false
		}
	}

	return true
}

func referenceClass(err error) string {
	switch {
	case errors.Is(err, erofsspec.ErrTruncated):
		return "truncated"
	case errors.Is(err, erofsspec.ErrUnsupported):
		return "unsupported"
	case errors.Is(err, erofsspec.ErrCorrupt),
		errors.Is(err, erofsspec.ErrNotDir),
		errors.Is(err, erofsspec.ErrNotExist):
		return "corrupt"
	default:
		return "other"
	}
}

// readerVerdict reads everything the package can, the way a consumer
// would: Open, then a walk by ReadDir with Lstat, ReadFile and ReadLink on
// every entry, bounded against a cyclic image. A panic is a class of its
// own.
func readerVerdict(img, blob []byte) (v verdict) {
	defer func() {
		if r := recover(); r != nil {
			v = verdict{class: "panic", err: fmt.Errorf("%w: %v", errPanic, r)}
		}
	}()

	var opts []erofs.OpenOpt
	if blob != nil {
		opts = append(opts, erofs.WithExtraDevices(bytes.NewReader(blob)))
	}

	fsys, err := erofs.Open(bytes.NewReader(img), opts...)
	if err != nil {
		return verdict{class: readerClass(err), err: err}
	}

	h := &strings.Builder{}
	seen := map[uint64]bool{}
	visits := 0

	var walk func(dir string) error

	walk = func(dir string) error {
		entries, rerr := fs.ReadDir(fsys, dir)
		if rerr != nil {
			return rerr
		}

		for _, e := range entries {
			visits++
			if visits > 100_000 {
				return errors.New("walk over 100000 entries") //nolint:err113 // a bound of the test
			}

			name := path.Join(dir, e.Name())

			descend, eerr := readerEntry(fsys, name, h, seen)
			if eerr != nil {
				return eerr
			}

			if descend {
				if werr := walk(name); werr != nil {
					return werr
				}
			}
		}

		return nil
	}

	root, err := fs.Stat(fsys, ".")
	if err != nil {
		return verdict{class: readerClass(err), err: err}
	}

	if st, ok := root.Sys().(*erofs.Stat); ok {
		seen[st.Ino] = true
	}

	if err := walk("."); err != nil {
		return verdict{class: readerClass(err), err: err}
	}

	return verdict{class: "ok", tree: h.String()}
}

// readerEntry appends one entry to the dump as the reference does, and says
// whether the walk descends into it: a directory not yet seen.
func readerEntry(fsys fs.FS, name string, h *strings.Builder, seen map[uint64]bool) (bool, error) {
	info, err := fs.Lstat(fsys, name)
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(h, "%s|unreachable\n", name)

		return false, nil
	}

	if err != nil {
		return false, err
	}

	st, ok := info.Sys().(*erofs.Stat)
	if !ok {
		return false, fmt.Errorf("%s: Sys is %T", name, info.Sys()) //nolint:err113 // a finding of the test
	}

	fmt.Fprintf(
		h,
		"%s|%v|%d|%d|%d|%d|%d|%d|%d|%v\n",
		name,
		info.Mode(),
		st.Size,
		st.Nlink,
		st.UID,
		st.GID,
		st.Mtime,
		st.MtimeNs,
		st.Rdev,
		corpus.Keys(st.Xattrs),
	)

	for _, k := range corpus.Keys(st.Xattrs) {
		fmt.Fprintf(h, "%s=%s\n", k, st.Xattrs[k])
	}

	switch {
	case info.Mode().IsRegular() && st.Size <= contentMax:
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return false, err
		}

		fmt.Fprintf(h, "%x\n", sha256.Sum256(b))
	case info.Mode().IsRegular():
		if err := readFirstByte(fsys, name); err != nil {
			return false, err
		}
	case info.Mode()&fs.ModeSymlink != 0:
		b, err := fs.ReadLink(fsys, name)
		if err != nil {
			return false, err
		}

		fmt.Fprintf(h, "%q\n", b)
	case info.IsDir():
		if seen[st.Ino] {
			return false, nil
		}

		seen[st.Ino] = true

		return true, nil
	}

	return false, nil
}

// readFirstByte opens the file and reads one byte, as the reference does
// for a file too large to read whole.
func readFirstByte(fsys fs.FS, name string) error {
	f, err := fsys.Open(name)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	_, err = f.Read(make([]byte, 1))

	return err
}

func readerClass(err error) string {
	switch {
	case errors.Is(err, erofs.ErrCorrupt):
		return "corrupt"
	case errors.Is(err, erofs.ErrNotImplemented):
		return "unsupported"
	case errors.Is(err, fs.ErrInvalid):
		return "invalid"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	default:
		return "other"
	}
}
