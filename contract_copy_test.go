package erofs_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/erofsspec"
)

// The contract of CopyFrom, from the sources that state it: the CopyFrom,
// MetadataOnly and Merge docs, the README's "Merging multiple layers" and
// the paragraph on what CopyFrom takes from Sys(), the DataRange doc, and
// the on-disk format as the reference reads it. Where a sentence of the
// docs is held, it is quoted at the check.
//
// Three sources: an image the package opened (its FileInfo carries
// *erofs.Stat and DataRange), a host directory (os.DirFS, Sys() a
// *syscall.Stat_t where the platform has one), and a generic fs.FS
// (fstest.MapFS, "a source with neither still contributes mode, size and
// ModTime"). Two modes: the data copied, or MetadataOnly, "chunk-based
// layout referencing an external device". And Merge, "overlay merge
// semantics": whiteouts delete from prior layers, opaque markers delete a
// directory's prior children, the markers themselves never land.
//
// Hard-link identity is the source's link count: two names share one
// inode in the copy when the source reports more than one link for it;
// under a count of one, each name is copied as its own inode, content
// intact, whatever inode the names shared in the source.
//
// The oracle is the reference reader over the copy, held to the source's
// tree through corpus.Check, or, for an edited source, to the reference's
// own reading of that source.

// copied is one CopyFrom result: the image (with the source image as its
// Blob, the one extra device, when the copy is metadata-only) and the
// error CopyFrom or Close gave.
type copied struct {
	im  corpus.Image
	err error
}

// copyFromImage opens a committed or built image through the package and
// copies it into a fresh writer, with or without data.
func copyFromImage(tb testing.TB, src corpus.Image, metadataOnly bool) copied {
	tb.Helper()

	fsys := openImage(tb, src)
	if fsys == nil {
		tb.Skip("a 48-bit image: the package does not open it")
	}

	var opts []erofs.CopyOpt
	if metadataOnly {
		opts = append(opts, erofs.MetadataOnly())
	}

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	if err := w.CopyFrom(fsys, opts...); err != nil {
		return copied{err: fmt.Errorf("CopyFrom: %w", err)}
	}

	if err := w.Close(); err != nil {
		return copied{err: fmt.Errorf("Close: %w", err)}
	}

	c := copied{im: corpus.Image{Name: src.Name + copyMode(metadataOnly), Img: out.Bytes(), Tree: src.Tree}}

	// "chunk indexes reference those blocks with DeviceID=1": the source
	// image is the copy's one extra device.
	if metadataOnly {
		c.im.Blob = src.Img
	}

	return c
}

func copyMode(metadataOnly bool) string {
	if metadataOnly {
		return "-metadata-only"
	}

	return "-copy"
}

// TestContractCopyFromImageIsTheImage: an image copied through CopyFrom
// with its data reads back through the reference to the tree the source
// was built from, entry for entry: types, bits, ids, times, content,
// targets, link counts, xattrs. Every committed image with a tree, and the
// writer's own images of the three corpus trees. The metadata-only copy of
// an image source is not held here: what it addresses when the source's
// data is the image itself is a contract still to be stated, and the mode
// as coded refuses or holes such files.
func TestContractCopyFromImageIsTheImage(t *testing.T) {
	t.Parallel()

	var sources []corpus.Image

	for _, im := range corpus.Committed(t) {
		if im.Tree != nil {
			sources = append(sources, im)
		}
	}

	for name, tree := range map[string]func() []corpus.Node{"tree": corpus.Tree, "full": corpus.Full, "small": corpus.Small} {
		nodes := tree()
		sources = append(sources, corpus.Image{Name: "writer-" + name, Img: writeImage(t, nodes), Tree: nodes})
	}

	for _, src := range sources {
		t.Run(src.Name+copyMode(false), func(t *testing.T) {
			t.Parallel()

			c := copyFromImage(t, src, false)
			if c.err != nil {
				t.Fatalf("%s: %v", src.Name, c.err)
			}

			ref := parseImage(t, c.im)
			corpus.Check(t, c.im, ref, src.Tree, strings.HasPrefix(src.Name, "writer-"))
			fsckUnlessPlatformLimit(t, c.im)
		})
	}
}

// layer is one source of a merge: a generic fs.FS built from entries, with
// the whiteouts the Merge doc names.
type layer map[string]*fstest.MapFile

// at is the time every layer entry carries, so that a generic source's
// ModTime is a fact the copy must keep.
func at() time.Time { return time.Unix(corpus.BuildTime+7, 0) }

func (l layer) file(name, content string) layer {
	l[name] = &fstest.MapFile{Data: []byte(content), Mode: 0o644, ModTime: at()}

	return l
}

func (l layer) dir(name string) layer {
	l[name] = &fstest.MapFile{Mode: fs.ModeDir | 0o755, ModTime: at()}

	return l
}

func (l layer) whiteout(name string) layer {
	return l.file(path.Join(path.Dir(name), ".wh."+path.Base(name)), "")
}

func (l layer) opaque(dir string) layer {
	return l.file(path.Join(dir, ".wh..wh..opq"), "")
}

// withParents is the layer with every missing parent directory made
// explicit, as fstest.MapFS would otherwise synthesize it with bits of its
// own; the model and the source then agree on what the layer holds.
func (l layer) withParents() layer {
	out := layer{}
	maps.Copy(out, l)

	for k := range l {
		for dir := path.Dir(k); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if _, ok := out[dir]; !ok {
				out.dir(dir)
			}
		}
	}

	return out
}

// fsys is the layer as an fs.FS with an explicit root, 0755 at the build
// time, so that nothing about the root is left to fstest.MapFS.
func (l layer) fsys() fstest.MapFS {
	m := fstest.MapFS{".": &fstest.MapFile{Mode: fs.ModeDir | corpus.RootMode, ModTime: time.Unix(corpus.BuildTime, 0)}}
	maps.Copy(m, l.withParents())

	return m
}

// merged is the model's outcome of layers applied in order under Merge:
// the overlay rules of the Merge doc over the entries of generic sources.
// Metadata-only, a generic source's file is "stored as chunk-based inodes
// with no physical mappings (all holes)": its size, zeros for content.
func merged(metadataOnly bool, layers ...layer) []corpus.Node {
	tree := map[string]*fstest.MapFile{}

	for i, l := range layers {
		l = l.withParents()

		names := make([]string, 0, len(l))
		for k := range l {
			names = append(names, k)
		}

		slices.Sort(names)

		for _, name := range names {
			mf := l[name]
			if mf == nil {
				continue
			}

			base := path.Base(name)

			switch {
			case i > 0 && base == ".wh..wh..opq":
				// "opaque markers (.wh..wh..opq) delete all children of
				// their parent directory" from prior layers.
				dropChildren(tree, path.Dir(name), l)
			case i > 0 && strings.HasPrefix(base, ".wh."):
				// "whiteout files (.wh.<name>) delete the named entry from
				// prior layers".
				drop(tree, path.Join(path.Dir(name), strings.TrimPrefix(base, ".wh.")))
			case strings.HasPrefix(base, ".wh.") && i == 0:
				// A whiteout in the first copy, which is not a Merge, is an
				// ordinary name.
				tree[name] = mf
			case mf.Mode.IsDir() && tree[name] != nil && tree[name].Mode.IsDir():
				// A directory over a directory merges: the lower's children
				// stay, the upper's bits win.
				tree[name] = mf
			default:
				// Anything else replaces what was there, children included.
				drop(tree, name)
				tree[name] = mf
			}
		}
	}

	nodes := nodesOf(tree)

	if metadataOnly {
		for i := range nodes {
			nodes[i].Data = make([]byte, len(nodes[i].Data))
		}
	}

	return nodes
}

// drop removes a path and everything under it.
func drop(tree map[string]*fstest.MapFile, name string) {
	for k := range tree {
		if k == name || strings.HasPrefix(k, name+"/") {
			delete(tree, k)
		}
	}
}

// dropChildren removes a directory's children that the layer did not
// itself contribute.
func dropChildren(tree map[string]*fstest.MapFile, dir string, l layer) {
	for k := range tree {
		if path.Dir(k) != dir && !strings.HasPrefix(k, dir+"/") {
			continue
		}

		if _, own := l[k]; !own {
			delete(tree, k)
		}
	}
}

// nodesOf turns the model's entries into corpus nodes: ids 0, the layer
// time, the entry's bits.
func nodesOf(tree map[string]*fstest.MapFile) []corpus.Node {
	var out []corpus.Node

	for name, mf := range tree {
		if mf == nil {
			continue
		}

		n := corpus.Node{Path: name, Mode: int64(mf.Mode.Perm()), Sec: at().Unix()}
		if mf.Mode.IsDir() {
			n.Type = tar.TypeDir
		} else {
			n.Type = tar.TypeReg
			n.Data = mf.Data
		}

		out = append(out, n)
	}

	slices.SortFunc(out, func(a, b corpus.Node) int { return strings.Compare(a.Path, b.Path) })

	return out
}

// mergeImage copies the layers in order, the first plain and the rest
// with Merge, in one of the two modes, and parses the result.
func mergeImage(tb testing.TB, metadataOnly bool, layers ...layer) (corpus.Image, *erofsspec.Image) {
	tb.Helper()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	for i, l := range layers {
		var opts []erofs.CopyOpt
		if metadataOnly {
			opts = append(opts, erofs.MetadataOnly())
		}

		if i > 0 {
			opts = append(opts, erofs.Merge())
		}

		if err := w.CopyFrom(l.fsys(), opts...); err != nil {
			tb.Fatalf("layer %d: CopyFrom: %v", i, err)
		}
	}

	if err := w.Close(); err != nil {
		tb.Fatalf("Close: %v", err)
	}

	im := corpus.Image{Name: "merge" + copyMode(metadataOnly), Img: out.Bytes()}

	return im, parseImage(tb, im)
}

// TestContractMergeIsOverlay walks the upper layer's shapes over one lower
// layer: for each of the lower's names (a file at the root, a directory,
// a file in it, an empty directory) and one new name, the upper holds a
// file, a directory, a whiteout, or nothing, and the directory may carry
// an opaque marker; every combination, in both modes. The image reads back
// to the model.
func TestContractMergeIsOverlay(t *testing.T) {
	t.Parallel()

	lower := layer{}.file("a", "A").dir("d").file("d/b", "B").file("d/c", "C").dir("e")

	names := []string{"a", "d", "d/b", "e", "n"}
	choices := []string{"none", "file", "dir", "whiteout"}

	total := 1
	for range names {
		total *= len(choices)
	}

	for k := range total {
		for _, opaque := range []bool{false, true} {
			upper := layer{}
			label := ""

			for i, v := len(names)-1, k; i >= 0; i, v = i-1, v/len(choices) {
				c := choices[v%len(choices)]
				label = names[i] + "=" + c + " " + label

				switch c {
				case "file":
					upper.file(names[i], "U"+names[i])
				case "dir":
					upper.dir(names[i])
				case "whiteout":
					upper.whiteout(names[i])
				}
			}

			if opaque {
				upper.dir("d").opaque("d")

				label += "d opaque"
			}

			// A layer with an entry under a file is no layer: nothing an
			// fs.FS can present, nothing the model says anything about.
			if d, ok := upper["d"]; ok && !d.Mode.IsDir() {
				if _, under := upper["d/b"]; under || opaque {
					continue
				}
			}

			for _, metadataOnly := range []bool{false, true} {
				want := merged(metadataOnly, lower, upper)

				t.Run(strings.TrimSpace(label)+copyMode(metadataOnly), func(t *testing.T) {
					t.Parallel()

					im, ref := mergeImage(t, metadataOnly, lower, upper)
					corpus.Check(t, im, ref, want, true)
				})
			}
		}
	}
}

// TestContractMergeWhiteoutOfHardlinkedName: a whiteout on one name of a
// hard-linked pair leaves the other with the inode, "as unlink(2) does".
func TestContractMergeWhiteoutOfHardlinkedName(t *testing.T) {
	t.Parallel()

	nodes := []corpus.Node{
		{Path: "orig", Type: tar.TypeReg, Mode: 0o644, Data: []byte("linked"), Sec: corpus.BuildTime},
		{Path: "d", Type: tar.TypeDir, Mode: 0o755, Sec: corpus.BuildTime},
		{Path: "d/alias", Type: tar.TypeLink, Link: "orig", Sec: corpus.BuildTime},
	}

	src := corpus.Image{Name: "linked", Img: writeImage(t, nodes), Tree: nodes}

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	if err := errors.Join(
		w.CopyFrom(openImage(t, src)),
		w.CopyFrom(layer{}.whiteout("orig").fsys(), erofs.Merge()),
	); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	want := []corpus.Node{
		{Path: "d", Type: tar.TypeDir, Mode: 0o755, Sec: corpus.BuildTime},
		{Path: "d/alias", Type: tar.TypeReg, Mode: 0o644, Data: []byte("linked"), Sec: corpus.BuildTime},
	}

	im := corpus.Image{Name: "whiteout-of-link", Img: out.Bytes(), Tree: want}
	corpus.Check(t, im, parseImage(t, im), want, true)
}

// TestContractCopyFromEditedImage: a copy of an edited image, with its
// data, is refused with ErrInvalid (ErrCorrupt included) or with
// ErrNotImplemented, the reader's two classes, or is an image
// the reference reads to what it reads from the source; the copy never
// invents a tree, and never panics. The edits are the differential's kinds
// over the small images: one byte, every sixteenth offset of the first 4
// KiB, two values; and a truncation every 256 bytes.
func TestContractCopyFromEditedImage(t *testing.T) {
	t.Parallel()

	for _, im := range editImages(t) {
		if im.Name == "b512" {
			continue
		}

		t.Run(im.Name, func(t *testing.T) {
			t.Parallel()

			count := 0

			for off := 0; off < min(len(im.Img), 4096); off += 16 {
				for _, b := range []byte{0x00, 0xff} {
					img := bytes.Clone(im.Img)
					img[off] = b
					count += checkEditedCopy(t, im, img, fmt.Sprintf("%d=%#x", off, b))
				}
			}

			for cut := 256; cut < len(im.Img); cut += 256 {
				count += checkEditedCopy(t, im, im.Img[:cut], fmt.Sprintf("cut to %d", cut))
			}

			t.Logf("%s: %d edited sources the package opened, each copied with its data", im.Name, count)
		})
	}
}

// checkEditedCopy copies one edited image with its data; it returns 1 when
// the package opened the source and the copy was judged.
func checkEditedCopy(t *testing.T, im corpus.Image, img []byte, label string) int {
	t.Helper()

	var opts []erofs.OpenOpt
	if im.Blob != nil {
		opts = append(opts, erofs.WithExtraDevices(bytes.NewReader(im.Blob)))
	}

	fsys, err := erofs.Open(bytes.NewReader(img), opts...)
	if err != nil {
		return 0
	}

	// With its data alone: the metadata-only copy of an image source
	// joins when its contract is decided.
	got, cerr := copyOpened(fsys, false, img)

	switch {
	case errors.Is(cerr, errPanic):
		t.Fatalf("%s %s: %v", im.Name, label, cerr)
	case cerr != nil && (errors.Is(cerr, io.ErrUnexpectedEOF) || errors.Is(cerr, io.EOF)) &&
		referenceVerdict(img, im.Blob).class == "truncated":
		// A structure the reference finds past the end of the image or of
		// a device (whose length no field declares): the reader's Read
		// passes the ReaderAt's own end through, "a failing reader", and
		// the copy carries it as is.
		return 1
	case cerr != nil && !errors.Is(cerr, fs.ErrInvalid) && !errors.Is(cerr, erofs.ErrNotImplemented) && !errors.Is(cerr, fs.ErrNotExist):
		// ErrNotExist is a directory block out of name order: ReadDir
		// lists a name the lookup's binary search does not find, the
		// reader's documented divergence, met by the copy's own Open.
		t.Fatalf("%s %s: refused with %v, want ErrInvalid, ErrNotImplemented or ErrNotExist", im.Name, label, cerr)
	case cerr != nil:
		return 1
	}

	// A directory block out of name order, a name stored twice included:
	// ReadDir lists every name and the lookups find some of them, the
	// differential's own rule for the reader; what a copy holds of such a
	// directory is the search's, and the copy is not compared.
	var extra [][]byte
	if im.Blob != nil {
		extra = [][]byte{im.Blob}
	}

	if ref, perr := erofsspec.Parse(img, extra); perr == nil {
		if unsorted, _ := referenceWalk(ref, func(erofsspec.Entry) error { return nil }); unsorted {
			return 1
		}
	}

	if d := copyAgrees(img, im.Blob, got.Img, got.Blob); d != "" {
		t.Fatalf("%s %s: the copy is not the source: %s", im.Name, label, d)
	}

	return 1
}

// copyOpened copies an opened image into a fresh writer, a panic returned
// as errPanic; the source bytes are the copy's extra device when
// metadata-only.
func copyOpened(fsys fs.FS, metadataOnly bool, src []byte) (im corpus.Image, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", errPanic, r)
		}
	}()

	var opts []erofs.CopyOpt
	if metadataOnly {
		opts = append(opts, erofs.MetadataOnly())
	}

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	if err := w.CopyFrom(fsys, opts...); err != nil {
		return corpus.Image{}, err
	}

	if err := w.Close(); err != nil {
		return corpus.Image{}, err
	}

	im = corpus.Image{Name: "copy", Img: out.Bytes()}
	if metadataOnly {
		im.Blob = src
	}

	return im, nil
}

// copyView is what a copy must reproduce of an image, by path: the
// entry's type and bits, ids, times, device, xattrs, content, target, and
// the link count the writer computes: two plus its subdirectories for a
// directory, and for a file the names sharing its inode when the source
// reports more than one link (the copy keeps the inode shared), else one
// (each name is copied as its own inode), whatever the field says.
type copyView struct {
	line  string
	nlink int
}

// viewOf reads an image through the reference into its copy view; an
// error is the reference's refusal, which the copy must then have shared.
func viewOf(img, blob []byte) (map[string]copyView, error) {
	var extra [][]byte
	if blob != nil {
		extra = [][]byte{blob}
	}

	ref, err := erofsspec.Parse(img, extra)
	if err != nil {
		return nil, err
	}

	out := map[string]copyView{}
	names := map[uint64]int{}
	linked := map[uint64]bool{}
	subdirs := map[string]int{}
	nids := map[string]uint64{}

	err = ref.Walk(func(e erofsspec.Entry) error {
		in := e.Inode
		names[in.Nid]++
		nids[e.Path] = in.Nid
		linked[in.Nid] = in.Nlink > 1

		if in.IsDir() && e.Path != "." {
			subdirs[path.Dir(e.Path)]++
		}

		content := ""

		switch {
		case in.IsRegular() && in.Size <= contentMax:
			b, rerr := ref.Read(in)
			if rerr != nil {
				return fmt.Errorf("%s: %w", e.Path, rerr)
			}

			content = corpus.Digest(b)
		case in.IsSymlink():
			b, rerr := ref.Target(in)
			if rerr != nil {
				return fmt.Errorf("%s: %w", e.Path, rerr)
			}

			content = string(b)
		}

		xattrs := xattrsOf(in)

		// A directory's size is the writer's layout of its names, not the
		// source's field: the names themselves are held by the path set.
		size := in.Size
		if in.IsDir() {
			size = 0
		}

		out[e.Path] = copyView{
			line: fmt.Sprintf(
				"%v|%d|%d|%d|%d|%d|%d|%v|%s",
				modeOf(in),
				size,
				in.UID,
				in.GID,
				in.Mtime,
				in.MtimeNs,
				in.Rdev,
				corpus.Keys(xattrs),
				content,
			),
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// The computed counts, now that every name is seen.
	for p, v := range out {
		switch {
		case strings.HasPrefix(v.line, "d"):
			v.nlink = 2 + subdirs[p]
		case linked[nids[p]]:
			v.nlink = names[nids[p]]
		default:
			v.nlink = 1
		}

		out[p] = v
	}

	return out, nil
}

// copyAgrees holds a copy to its source: the same paths, each with the
// same view and the computed link count; the two must share one name
// partition over inodes as well, which the computed counts of a file
// express.
func copyAgrees(src, srcBlob, dst, dstBlob []byte) string {
	want, err := viewOf(src, srcBlob)
	if err != nil {
		return fmt.Sprintf("the reference cannot read the source the package copied: %v", err)
	}

	got, err := viewOf(dst, dstBlob)
	if err != nil {
		return fmt.Sprintf("the reference cannot read the copy: %v", err)
	}

	for p, w := range want {
		g, ok := got[p]

		switch {
		case !ok:
			return p + " is missing from the copy"
		case g.line != w.line:
			return fmt.Sprintf("%s differs\nsource: %s\ncopy:   %s", p, w.line, g.line)
		case g.nlink != w.nlink:
			return fmt.Sprintf("%s: link count %d in the copy, %d by the source's names and count", p, g.nlink, w.nlink)
		}
	}

	for p := range got {
		if _, ok := want[p]; !ok {
			return p + " is in the copy and not in the source"
		}
	}

	return ""
}
