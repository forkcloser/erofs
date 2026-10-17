package erofs_test

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/erofsspec"
)

// The package's fuzz targets, each held to the reference reader as its
// oracle rather than to "no panic": an edited image gets the verdict the
// kernel's rules give it, a path gets what the kernel's lookup finds, and
// a tree the writer is given is the tree the image holds. The seeds are
// the committed corpus and small programs; the inherited targets, four
// tree shapes by seven operations and one target per writer field, are
// folded into these three.

// FuzzReaderEdits: a committed image, the four small ones and the tree at
// 512, with up to two byte edits and a truncation. The reader's verdict on
// it (the tree it reads, or the class of its refusal) is the reference's,
// under the differential's rules in contract_test.go; a panic is a verdict
// of its own and never agrees.
func FuzzReaderEdits(f *testing.F) {
	images := editImages(f)

	for i, im := range images {
		// The seeds are the sweep's kinds of edit: the superblock's
		// checksum, its block size, the first inode slot, a data byte, and
		// a truncation inside the metadata.
		f.Add(uint8(i), uint32(1024+4), byte(0xff), uint32(0), byte(0), uint32(0))
		f.Add(uint8(i), uint32(1024+12), byte(17), uint32(0), byte(0), uint32(0))
		f.Add(uint8(i), uint32(1152), byte(0x7f), uint32(1152+4), byte(0x01), uint32(0))
		f.Add(uint8(i), uint32(len(im.Img)-1), byte(0), uint32(0), byte(0), uint32(0))
		f.Add(uint8(i), uint32(0), byte(0), uint32(0), byte(0), uint32(min(len(im.Img), 2048)))

		// And the directory blocks' own edges: the first byte of the first
		// and of the last name of a block zeroed (the last name then has
		// length zero, a bogus dirent to dir.c's erofs_fill_dentries; the
		// first keeps its length with a NUL inside) and a cut inside a
		// directory's second block.
		names, cuts := direntEdges(f, im)
		for _, off := range names {
			f.Add(uint8(i), off, byte(0), uint32(0), byte(0), uint32(0))
		}

		for _, cut := range cuts {
			f.Add(uint8(i), uint32(0), byte(0), uint32(0), byte(0), cut)
		}
	}

	f.Fuzz(func(t *testing.T, which uint8, off1 uint32, b1 byte, off2 uint32, b2 byte, truncate uint32) {
		im := images[int(which)%len(images)]
		img := bytes.Clone(im.Img)

		img[int(off1)%len(img)] = b1
		if off2 != off1 {
			img[int(off2)%len(img)] = b2
		}

		if truncate != 0 {
			img = img[:int(truncate)%(len(img)+1)]
		}

		ref, got := referenceVerdict(img, im.Blob), readerVerdict(img, im.Blob)
		if d := judge(ref, got); d != "" {
			t.Fatalf(
				"%s edited at %d=%#x, %d=%#x, cut to %d: %s (reference: %v; reader: %v)",
				im.Name,
				off1,
				b1,
				off2,
				b2,
				len(img),
				d,
				ref.err,
				got.err,
			)
		}
	})
}

// FuzzCopyFromEdits: a committed image with up to two byte edits and a
// truncation, opened by the package and copied with its data. A copy the
// package refuses is refused as the reader's
// classes or ErrNotExist (a directory out of order, met by the copy's own
// Open); a copy it makes is the source as the reference reads it, every
// entry with the link count the writer computes; a panic never agrees.
func FuzzCopyFromEdits(f *testing.F) {
	images := editImages(f)

	for i, im := range images {
		f.Add(uint8(i), uint32(0), byte(0), uint32(0), byte(0), uint32(0))
		f.Add(uint8(i), uint32(1152), byte(0x7f), uint32(1152+4), byte(0x01), uint32(0))
		f.Add(uint8(i), uint32(0), byte(0), uint32(0), byte(0), uint32(min(len(im.Img), 2048)))

		names, cuts := direntEdges(f, im)
		for _, off := range names[:min(len(names), 4)] {
			f.Add(uint8(i), off, byte(0), uint32(0), byte(0), uint32(0))
		}

		for _, cut := range cuts {
			f.Add(uint8(i), uint32(0), byte(0), uint32(0), byte(0), cut)
		}
	}

	f.Fuzz(func(t *testing.T, which uint8, off1 uint32, b1 byte, off2 uint32, b2 byte, truncate uint32) {
		im := images[int(which)%len(images)]
		img := bytes.Clone(im.Img)

		img[int(off1)%len(img)] = b1
		if off2 != off1 {
			img[int(off2)%len(img)] = b2
		}

		if truncate != 0 {
			img = img[:int(truncate)%(len(img)+1)]
		}

		checkEditedCopy(t, im, img, fmt.Sprintf("%d=%#x, %d=%#x, cut to %d", off1, b1, off2, b2, len(img)))
	})
}

// editImages are the committed images of the small tree, the sweep's, and
// the tree at 512 for its many-block directory.
func editImages(tb testing.TB) []corpus.Image {
	tb.Helper()

	var out []corpus.Image

	for _, im := range corpus.Committed(tb) {
		if strings.HasPrefix(im.Name, "small-") || im.Name == "b512" {
			out = append(out, im)
		}
	}

	if len(out) < 2 {
		tb.Fatal("the committed small images and b512 are missing")
	}

	return out
}

// direntEdges finds, through the reference, the first byte of the first
// and of the last name in every directory block of an image, and a point
// inside the second block of every directory that has one. A dirent is
// twelve bytes, its nameoff at offset 8 (erofs_fs.h, erofs_dirent); the
// first nameoff, over twelve, counts the block's entries.
func direntEdges(tb testing.TB, im corpus.Image) (names, cuts []uint32) {
	tb.Helper()

	ref := parseImage(tb, im)
	bs := int64(ref.Super.BlockSize)

	err := ref.Walk(func(e erofsspec.Entry) error {
		if !e.Inode.IsDir() {
			return nil
		}

		extents, err := ref.Extents(e.Inode)
		if err != nil {
			return err
		}

		for _, x := range extents {
			if x.Hole || x.Device != 0 {
				continue
			}

			if x.Length > bs {
				cuts = append(cuts, uint32(x.Physical+bs+7))
			}

			for off := x.Physical; off < x.Physical+x.Length; off += bs {
				blk := im.Img[off:min(off+bs, x.Physical+x.Length)]
				if len(blk) < 12 {
					continue
				}

				first := binary.LittleEndian.Uint16(blk[8:])
				count := int(first) / 12

				if count == 0 || int(first) >= len(blk) {
					continue
				}

				last := binary.LittleEndian.Uint16(blk[(count-1)*12+8:])
				names = append(names, uint32(off)+uint32(first), uint32(off)+uint32(last))
			}
		}

		return nil
	})
	if err != nil {
		tb.Fatalf("%s: %v", im.Name, err)
	}

	return names, cuts
}

// pathImage is an image FuzzReaderPaths looks paths up in.
type pathImage struct {
	name string
	ref  *erofsspec.Image
	fsys fs.FS
}

// pathImages are the committed tree at 4096 and a writer-built chain forty
// directories deep with a file and a symlink at its bottom: the inherited
// targets' flat, nested, wide and deep shapes.
func pathImages(tb testing.TB) []pathImage {
	tb.Helper()

	var out []pathImage

	for _, im := range corpus.Committed(tb) {
		if im.Name == "b4096" {
			out = append(out, pathImage{name: im.Name, ref: parseImage(tb, im), fsys: openImage(tb, im)})
		}
	}

	var nodes []corpus.Node

	dir := ""
	for i := range 40 {
		dir += fmt.Sprintf("l%d/", i)
		nodes = append(
			nodes,
			corpus.Node{Path: strings.TrimSuffix(dir, "/"), Type: tar.TypeDir, Mode: 0o755, Sec: corpus.BuildTime},
		)
	}

	nodes = append(
		nodes,
		corpus.Node{Path: dir + "f", Type: tar.TypeReg, Mode: 0o644, Data: corpus.Pattern(5000), Sec: corpus.BuildTime},
		corpus.Node{
			Path: dir + "up",
			Type: tar.TypeSymlink,
			Mode: 0o777,
			Link: strings.Repeat("../", 40) + "l0",
			Sec:  corpus.BuildTime,
		},
	)

	im := corpus.Image{Name: "deep", Img: writeImage(tb, nodes)}
	out = append(out, pathImage{name: im.Name, ref: parseImage(tb, im), fsys: openImage(tb, im)})

	return out
}

// FuzzReaderPaths: a path against an image. A path of invalid fs.FS shape
// is ErrInvalid from every call; a valid one gets what the kernel's lookup
// finds through the reference: not there, a file in the way, or an entry
// whose type, size, content, entries or target the package serves the
// same.
func FuzzReaderPaths(f *testing.F) {
	images := pathImages(f)

	for _, p := range []string{".", "d", "d/sym", "d/symdir/", "many/f000", "l0/l1/l2", strings.Repeat("l0/", 2), strings.Repeat("p", 4097), "../x", "/abs", "d//x", "l0/l1/l2/l3/l4/l5/l6/l7/l8/l9/l10/l11/l12/l13/l14/l15/l16/l17/l18/l19/l20/l21/l22/l23/l24/l25/l26/l27/l28/l29/l30/l31/l32/l33/l34/l35/l36/l37/l38/l39/up/l1", "\x00", "d/f\xff"} {
		for i := range images {
			f.Add(uint8(i), p)
		}
	}

	f.Fuzz(func(t *testing.T, which uint8, path string) {
		im := images[int(which)%len(images)]

		// The reader keeps a non-UTF-8 name reachable, as the kernel does
		// (TestNonUTF8NamesRemainReachable); fs.ValidPath alone would call
		// such a path invalid, so validity is judged on its shape. A path
		// over the documented 4096 bytes is ErrInvalid, a bound of the
		// package ("a path of at most 4096 bytes").
		if !fs.ValidPath(strings.ToValidUTF8(path, "x")) || len(path) > 4096 {
			checkInvalidPath(t, im, path)

			return
		}

		in, err := im.ref.Resolve(path, kernelMaxLinks)

		switch {
		case errors.Is(err, erofsspec.ErrNotExist):
			checkPathError(t, im, path, fs.ErrNotExist)
		case errors.Is(err, erofsspec.ErrNotDir):
			checkPathError(t, im, path, erofs.ErrNotDirectory)
		case errors.Is(err, erofsspec.ErrLoop):
			checkPathError(t, im, path, erofs.ErrLoop)
		case err != nil:
			t.Fatalf("%s %q: reference: %v", im.name, path, err)
		default:
			checkPathEntry(t, im, path, in)
		}
	})
}

// checkInvalidPath: every call refuses a path io/fs calls invalid.
func checkInvalidPath(t *testing.T, im pathImage, path string) {
	t.Helper()

	_, openErr := im.fsys.Open(path)
	_, statErr := fs.Stat(im.fsys, path)
	_, readErr := fs.ReadFile(im.fsys, path)
	_, dirErr := fs.ReadDir(im.fsys, path)

	for call, err := range map[string]error{"Open": openErr, "Stat": statErr, "ReadFile": readErr, "ReadDir": dirErr} {
		if !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("%s %q (invalid): %s: %v, want ErrInvalid", im.name, path, call, err)
		}
	}
}

// checkPathError: Open and Stat refuse the path the way the reference's
// lookup did.
func checkPathError(t *testing.T, im pathImage, path string, want error) {
	t.Helper()

	if _, err := im.fsys.Open(path); !errors.Is(err, want) {
		t.Fatalf("%s %q: Open: %v, want %v", im.name, path, err, want)
	}

	if _, err := fs.Stat(im.fsys, path); !errors.Is(err, want) {
		t.Fatalf("%s %q: Stat: %v, want %v", im.name, path, err, want)
	}
}

// checkPathEntry holds what the package serves for a path to the inode the
// reference resolved: type and size, then the content, the entries or the
// target.
func checkPathEntry(t *testing.T, im pathImage, path string, in *erofsspec.Inode) {
	t.Helper()

	info, err := fs.Stat(im.fsys, path)
	if err != nil {
		t.Fatalf("%s %q: Stat: %v, reference resolves it", im.name, path, err)
	}

	if info.Mode().Type() != modeOf(in).Type() {
		t.Fatalf("%s %q: type %v, reference %v", im.name, path, info.Mode().Type(), modeOf(in).Type())
	}

	switch {
	case in.IsRegular():
		if info.Size() != in.Size {
			t.Fatalf("%s %q: size %d, reference %d", im.name, path, info.Size(), in.Size)
		}

		want, err := im.ref.Read(in)
		if err != nil {
			t.Fatalf("%s %q: reference read: %v", im.name, path, err)
		}

		if got, err := fs.ReadFile(im.fsys, path); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s %q: ReadFile %d bytes (%v), reference %d", im.name, path, len(got), err, len(want))
		}
	case in.IsDir():
		checkPathDir(t, im, path, in)
	}

	checkPathLink(t, im, path)
}

// checkPathDir: ReadDir lists the reference's dirents, less "." and "..".
func checkPathDir(t *testing.T, im pathImage, path string, in *erofsspec.Inode) {
	t.Helper()

	dirents, err := im.ref.Dirents(in)
	if err != nil {
		t.Fatalf("%s %q: reference dirents: %v", im.name, path, err)
	}

	var want []string

	for _, de := range dirents {
		if n := string(de.Name); n != "." && n != ".." {
			want = append(want, n)
		}
	}

	slices.Sort(want)

	entries, err := fs.ReadDir(im.fsys, path)
	if err != nil {
		t.Fatalf("%s %q: ReadDir: %v", im.name, path, err)
	}

	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}

	if !slices.Equal(got, want) {
		t.Fatalf("%s %q: ReadDir %d names, reference %d", im.name, path, len(got), len(want))
	}
}

// checkPathLink: when the last name is a symlink, ReadLink is the
// reference's target; otherwise ReadLink is ErrInvalid.
func checkPathLink(t *testing.T, im pathImage, path string) {
	t.Helper()

	last, err := lookupNoFollow(im.ref, path)
	if err != nil {
		t.Fatalf("%s %q: reference lookup: %v", im.name, path, err)
	}

	got, err := fs.ReadLink(im.fsys, path)

	if !last.IsSymlink() {
		if !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("%s %q: ReadLink of a non-symlink: %v, want ErrInvalid", im.name, path, err)
		}

		return
	}

	want, terr := im.ref.Target(last)
	if terr != nil || err != nil || got != string(want) {
		t.Fatalf("%s %q: ReadLink %q (%v), reference %q (%v)", im.name, path, got, err, want, terr)
	}
}

// lookupNoFollow resolves a path's directory the kernel's way and looks the
// last name up without following it.
func lookupNoFollow(ref *erofsspec.Image, path string) (*erofsspec.Inode, error) {
	if path == "." {
		return ref.Inode(ref.Super.RootNid)
	}

	dir, name := "", path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		dir, name = path[:i], path[i+1:]
	}

	parent, err := ref.Resolve(dir, kernelMaxLinks)
	if err != nil {
		return nil, err
	}

	de, err := ref.Lookup(parent, []byte(name))
	if err != nil {
		return nil, err
	}

	return ref.Inode(de.Nid)
}

// FuzzWriterTree: a byte program decoded into a tree of up to sixty-four
// entries (files with content, directories, symlinks, hardlinks, FIFOs,
// xattrs, ids and times from the bytes), given to the writer; the reference
// reads the image back to the tree field for field, and the package serves
// what the reference reads. fsck.erofs is not run per input; the contract
// tests run it over the corpus trees.
func FuzzWriterTree(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 1, 5})
	f.Add([]byte{1, 0, 0, 0, 2, 1, 1, 0, 7, 2, 0, 0, 3, 0, 4, 1, 5, 0, 0})
	f.Add(bytes.Repeat([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 250, 251, 252, 253, 254, 255}, 8))

	f.Fuzz(func(t *testing.T, program []byte) {
		nodes := decodeTree(program)

		im := corpus.Image{Name: "fuzz", Img: writeImage(t, nodes), Tree: nodes}
		ref := parseImage(t, im)

		corpus.Check(t, im, ref, nodes, true)
		checkReader(t, openImage(t, im), ref)
	})
}

// treeReader hands out a program's bytes, zero past its end.
type treeReader struct {
	b []byte
	i int
}

func (r *treeReader) next() byte {
	if r.i >= len(r.b) {
		r.i++

		return 0
	}

	r.i++

	return r.b[r.i-1]
}

func (r *treeReader) done() bool { return r.i >= len(r.b) }

// nameAlphabet is what a fuzzed name is made of: letters, a digit, a dash,
// a space, and two bytes no UTF-8 decoder accepts.
const nameAlphabet = "abcdefghij7- \xff\x80"

// decodeTree turns a byte program into a tree: each step picks a kind, a
// parent among the directories so far, a name, and the kind's fields.
func decodeTree(program []byte) []corpus.Node {
	r := &treeReader{b: program}

	var nodes []corpus.Node

	dirs := []string{""}

	var regulars []string

	for len(nodes) < 64 && !r.done() {
		kind := r.next() % 6
		parent := dirs[int(r.next())%len(dirs)]
		name := decodeName(r)

		path := name
		if parent != "" {
			path = parent + "/" + name
		}

		if slices.ContainsFunc(nodes, func(n corpus.Node) bool { return n.Path == path }) {
			continue
		}

		n := corpus.Node{
			Path: path,
			Mode: int64(r.next()) & 0o777,
			UID:  int(r.next()) << int(r.next()%3*8),
			GID:  int(r.next()),
			Sec:  corpus.BuildTime + int64(r.next()),
			Nsec: int64(r.next()) * 1_000_000,
		}

		switch kind {
		case 0:
			n.Type = tar.TypeReg
			n.Data = corpus.Pattern(int(r.next())<<int(r.next()%2*8) + int(r.next()))

			regulars = append(regulars, path)
		case 1:
			n.Type = tar.TypeDir
			n.Mode |= 0o700

			dirs = append(dirs, path)
		case 2:
			n.Type = tar.TypeSymlink
			n.Mode = 0o777
			n.Link = "../" + decodeName(r)
		case 3:
			if len(regulars) == 0 {
				continue
			}

			n.Type = tar.TypeLink
			n.Link = regulars[int(r.next())%len(regulars)]
		case 4:
			n.Type = tar.TypeFifo
		default:
			n.Type = tar.TypeReg
			n.Xattrs = map[string]string{"user." + decodeName(r): string(corpus.Pattern(int(r.next())))}
		}

		nodes = append(nodes, n)
	}

	return nodes
}

// decodeName is one to eight bytes of the alphabet.
func decodeName(r *treeReader) string {
	n := int(r.next())%8 + 1
	b := make([]byte, n)

	for i := range b {
		b[i] = nameAlphabet[int(r.next())%len(nameAlphabet)]
	}

	return string(b)
}
