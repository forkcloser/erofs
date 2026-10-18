package erofs_test

import (
	"archive/tar"
	"bytes"
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
	"testing/fstest"
	"time"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/builder"
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
// layout referencing an external device"; of an image source, the mode
// "registers the image as an extra device of the result, followed by the
// image's own extra devices: every file's blocks stay where they are and
// the result's chunk indexes point at them; only a file's inline tail,
// shorter than a block, is copied, as one block", and the result is opened
// "over the source images in copy order, each followed by its own devices"
// (the README). And Merge, "overlay merge semantics": whiteouts delete from
// prior layers, opaque markers delete a directory's prior children, the
// markers themselves never land.
//
// Hard-link identity is the source's link count: two names share one
// inode in the copy when the source reports more than one link for it;
// under a count of one, each name is copied as its own inode, content
// intact, whatever inode the names shared in the source. The identity is
// the source's device and inode number together, and it is scoped to one
// CopyFrom call: a later source whose numbers coincide links nothing to
// the earlier one's files.
//
// The oracle is the reference reader over the copy, held to the source's
// tree through corpus.Check, or, for an edited source, to the reference's
// own reading of that source.

// copied is one CopyFrom result: the image (with the source image and its
// devices as the copy's devices when the copy is metadata-only) and the
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

	// "registers the image as an extra device of the result, followed by
	// the image's own extra devices".
	if metadataOnly {
		c.im.Devices = append([][]byte{src.Img}, src.Extra()...)
	}

	return c
}

func copyMode(metadataOnly bool) string {
	if metadataOnly {
		return "-metadata-only"
	}

	return "-copy"
}

// TestContractCopyFromImageIsTheImage: an image copied through CopyFrom,
// with its data or metadata-only, reads back through the reference to the
// tree the source was built from, entry for entry: types, bits, ids, times,
// content, targets, link counts, xattrs; the metadata-only copy over the
// source image and its devices, as the README has it opened. Every
// committed image with a tree, and the writer's own images of the three
// corpus trees; each copy passes fsck.erofs over the same devices.
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
		for _, metadataOnly := range []bool{false, true} {
			t.Run(src.Name+copyMode(metadataOnly), func(t *testing.T) {
				t.Parallel()

				c := copyFromImage(t, src, metadataOnly)
				if c.err != nil {
					t.Fatalf("%s: %v", src.Name, c.err)
				}

				ref := parseImage(t, c.im)
				corpus.Check(t, c.im, ref, src.Tree, strings.HasPrefix(src.Name, "writer-"))
				fsckUnlessPlatformLimit(t, c.im)
			})
		}
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
				upper = upper.dir("d").opaque("d")

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

// layerImage is the layer written as an image of its own: the same entries,
// whiteout names included, the root 0755 at the build time.
func layerImage(tb testing.TB, l layer) corpus.Image {
	tb.Helper()

	return corpus.Image{Name: "layer", Img: writeImage(tb, nodesOf(l.withParents()))}
}

// mergeImages copies the layers as images, the first plain and the rest
// with Merge, in one of the two modes; metadata-only, the layer images are
// the copy's devices "in copy order".
func mergeImages(tb testing.TB, metadataOnly bool, layers ...layer) (corpus.Image, *erofsspec.Image) {
	tb.Helper()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	var devices [][]byte

	for i, l := range layers {
		src := layerImage(tb, l)

		var opts []erofs.CopyOpt
		if metadataOnly {
			opts = append(opts, erofs.MetadataOnly())
		}

		if i > 0 {
			opts = append(opts, erofs.Merge())
		}

		if err := w.CopyFrom(openImage(tb, src), opts...); err != nil {
			tb.Fatalf("layer %d: CopyFrom: %v", i, err)
		}

		devices = append(devices, src.Img)
	}

	if err := w.Close(); err != nil {
		tb.Fatalf("Close: %v", err)
	}

	im := corpus.Image{Name: "merge-images" + copyMode(metadataOnly), Img: out.Bytes()}
	if metadataOnly {
		im.Devices = devices
	}

	return im, parseImage(tb, im)
}

// TestContractMergeOfImagesIsOverlay: the upper's shapes over one lower
// layer, both layers images of their own: for a file at the root, the
// directory and a file in it, the upper holds a file, a directory, a
// whiteout or nothing, with or without an opaque marker, in both modes. The
// model is the overlay's with the content intact either way: metadata-only,
// "every file's blocks stay where they are" on the layer images, the tails
// copied, and the result reads over the layers in copy order.
func TestContractMergeOfImagesIsOverlay(t *testing.T) {
	t.Parallel()

	lower := layer{}.file("a", "A").dir("d").file("d/b", "B").file("d/c", "C").dir("e")

	names := []string{"a", "d", "d/b"}
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
				upper = upper.dir("d").opaque("d")

				label += "d opaque"
			}

			// A layer with an entry under a file is no layer: no image
			// holds it, and the model says nothing about it.
			if d, ok := upper["d"]; ok && !d.Mode.IsDir() {
				if slices.ContainsFunc(
					slices.Collect(maps.Keys(upper)),
					func(k string) bool { return strings.HasPrefix(k, "d/") },
				) {
					continue
				}
			}

			want := merged(false, lower, upper)

			for _, metadataOnly := range []bool{false, true} {
				t.Run(strings.TrimSpace(label)+copyMode(metadataOnly), func(t *testing.T) {
					t.Parallel()

					im, ref := mergeImages(t, metadataOnly, lower, upper)
					corpus.Check(t, im, ref, want, true)
				})
			}
		}
	}
}

// TestContractMergeOverwriteOfHardlinkedName: an upper layer's file over
// one name of a hard-linked pair replaces that name alone; the other keeps
// the inode and the content, with one link, as a rename over a linked
// name does. Either name: the copy meets d/alias first in its walk, so it
// holds the inode and orig is the link to it; replacing each exercises
// one side of the group.
func TestContractMergeOverwriteOfHardlinkedName(t *testing.T) {
	t.Parallel()

	nodes := []corpus.Node{
		{Path: "orig", Type: tar.TypeReg, Mode: 0o644, Data: []byte("linked"), Sec: corpus.BuildTime},
		{Path: "d", Type: tar.TypeDir, Mode: 0o755, Sec: corpus.BuildTime},
		{Path: "d/alias", Type: tar.TypeLink, Link: "orig", Sec: corpus.BuildTime},
	}

	src := corpus.Image{Name: "linked", Img: writeImage(t, nodes), Tree: nodes}

	for _, replaced := range []string{"orig", "d/alias"} {
		t.Run(replaced, func(t *testing.T) {
			t.Parallel()

			out := &testBuffer{}
			w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

			if err := errors.Join(
				w.CopyFrom(openImage(t, src)),
				w.CopyFrom(layer{}.file(replaced, "replaced").fsys(), erofs.Merge()),
			); err != nil {
				t.Fatal(err)
			}

			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			want := []corpus.Node{
				{Path: "orig", Type: tar.TypeReg, Mode: 0o644, Data: []byte("linked"), Sec: corpus.BuildTime},
				{Path: "d", Type: tar.TypeDir, Mode: 0o755, Sec: corpus.BuildTime},
				{Path: "d/alias", Type: tar.TypeReg, Mode: 0o644, Data: []byte("linked"), Sec: corpus.BuildTime},
			}

			// The upper layer carries the replaced name's parents too, so a
			// directory over a directory: "the upper's bits win".
			for i := range want {
				switch want[i].Path {
				case replaced:
					want[i].Data, want[i].Sec = []byte("replaced"), at().Unix()
				case path.Dir(replaced):
					want[i].Sec = at().Unix()
				}
			}

			im := corpus.Image{Name: "overwrite-of-link", Img: out.Bytes(), Tree: want}
			corpus.Check(t, im, parseImage(t, im), want, true)
		})
	}
}

// TestContractCopyHardLinkIdentity: a generic source's names share an inode
// in the copy when their Sys() reports the same device and inode number
// with a link count above one; the same inode number on another device,
// no inode number at all, or a count of one makes each name its own
// inode; a count higher than the names the source holds is not copied.
func TestContractCopyHardLinkIdentity(t *testing.T) {
	t.Parallel()

	entry := func(nlink uint32, dev, ino uint64) *builder.Entry {
		return &builder.Entry{Nlink: nlink, Dev: dev, Ino: ino}
	}
	file := func(content string, sys *builder.Entry) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(content), Mode: 0o644, ModTime: at(), Sys: sys}
	}

	src := fstest.MapFS{
		".":       &fstest.MapFile{Mode: fs.ModeDir | corpus.RootMode, ModTime: time.Unix(corpus.BuildTime, 0)},
		"a":       file("dev1", entry(2, 1, 100)),
		"a2":      file("ignored: the same inode as a", entry(2, 1, 100)),
		"b":       file("dev2", entry(2, 2, 100)),
		"noino":   file("n1", entry(3, 0, 0)),
		"noino2":  file("n2", entry(3, 0, 0)),
		"single":  file("s1", entry(1, 1, 200)),
		"single2": file("s2", entry(1, 1, 200)),
		"partial": file("p", entry(5, 1, 300)),
	}

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	if err := errors.Join(w.CopyFrom(src), w.Close()); err != nil {
		t.Fatal(err)
	}

	reg := func(name, content string) corpus.Node {
		return corpus.Node{Path: name, Type: tar.TypeReg, Mode: 0o644, Data: []byte(content), Sec: at().Unix()}
	}

	want := []corpus.Node{
		reg("a", "dev1"),
		{Path: "a2", Type: tar.TypeLink, Link: "a", Sec: at().Unix()},
		reg("b", "dev2"),
		reg("noino", "n1"),
		reg("noino2", "n2"),
		reg("single", "s1"),
		reg("single2", "s2"),
		reg("partial", "p"),
	}

	im := corpus.Image{Name: "hard-link-identity", Img: out.Bytes(), Tree: want}
	corpus.Check(t, im, parseImage(t, im), want, true)
}

// TestContractCopyHardLinkScopeIsOneCall: two image sources of the same
// shape, whose inode numbers coincide, each with a linked pair, copied by
// two CopyFrom calls into one writer: each pair shares its own inode, and
// nothing of the second source links to the first's files.
func TestContractCopyHardLinkScopeIsOneCall(t *testing.T) {
	t.Parallel()

	shape := func(prefix, content string) []corpus.Node {
		return []corpus.Node{
			{Path: prefix + "-orig", Type: tar.TypeReg, Mode: 0o644, Data: []byte(content), Sec: corpus.BuildTime},
			{Path: prefix + "-link", Type: tar.TypeLink, Link: prefix + "-orig", Sec: corpus.BuildTime},
			{
				Path: prefix + "-other",
				Type: tar.TypeReg,
				Mode: 0o644,
				Data: []byte(prefix + " other"),
				Sec:  corpus.BuildTime,
			},
		}
	}

	nodesA, nodesB := shape("a", "a shared"), shape("b", "b shared")
	srcA := corpus.Image{Name: "a", Img: writeImage(t, nodesA), Tree: nodesA}
	srcB := corpus.Image{Name: "b", Img: writeImage(t, nodesB), Tree: nodesB}

	// The case holds only when the two sources' inode numbers coincide.
	refA, refB := parseImage(t, srcA), parseImage(t, srcB)
	inA, errA := refA.Resolve("a-orig", 0)
	inB, errB := refB.Resolve("b-orig", 0)

	if errA != nil || errB != nil || inA.Nid != inB.Nid {
		t.Fatalf("the two sources do not share their inode numbers (%v %v): the case exercises nothing", errA, errB)
	}

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	if err := errors.Join(w.CopyFrom(openImage(t, srcA)), w.CopyFrom(openImage(t, srcB)), w.Close()); err != nil {
		t.Fatal(err)
	}

	want := slices.Concat(nodesA, nodesB)
	im := corpus.Image{Name: "hard-link-scope", Img: out.Bytes(), Tree: want}
	corpus.Check(t, im, parseImage(t, im), want, true)
}

// TestContractMetadataOnlyCopyAfterTheCopysOwnDevice: the source image is
// registered "after the copy's earlier devices": a writer with its own data
// file (device 1) that copies an image metadata-only places the image at
// device 2 and the image's own devices after it, and the result reads the
// writer's own file from its data file and the source's files from the
// source, each at its own slot.
func TestContractMetadataOnlyCopyAfterTheCopysOwnDevice(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"small-b512", "small-blobdev-512"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			src := committed(t, name)

			df, err := os.CreateTemp(t.TempDir(), "data")
			if err != nil {
				t.Fatal(err)
			}

			defer func() { _ = df.Close() }()

			out := &testBuffer{}
			w := erofs.Create(
				out,
				erofs.WithBuildTime(corpus.BuildTime, 0),
				erofs.WithBlockSize(512),
				erofs.WithDataFile(df),
			)

			at := time.Unix(corpus.BuildTime, 0)
			if err := errors.Join(
				w.Mkdir("/", corpus.RootMode),
				w.Chown("/", 0, 0),
				w.Chtimes("/", at, at),
			); err != nil {
				t.Fatal(err)
			}

			own := corpus.Node{
				Path: "own",
				Type: tar.TypeReg,
				Mode: 0o644,
				Data: bytes.Repeat([]byte("o"), 5000),
				Sec:  corpus.BuildTime,
			}
			writeNode(t, w, own)

			if err := errors.Join(w.CopyFrom(openImage(t, src), erofs.MetadataOnly()), w.Close()); err != nil {
				t.Fatal(err)
			}

			blob, err := os.ReadFile(df.Name())
			if err != nil {
				t.Fatal(err)
			}

			want := append([]corpus.Node{own}, src.Tree...)
			im := corpus.Image{Name: name + "-after-own-device", Img: out.Bytes(), Tree: want}
			im.Devices = append([][]byte{blob, src.Img}, src.Extra()...)

			ref := parseImage(t, im)
			if got := len(ref.Devices); got != len(im.Devices) {
				t.Fatalf("%d device slots, want %d (the data file, the image, its devices)", got, len(im.Devices))
			}

			corpus.Check(t, im, ref, want, true)
			fsckUnlessPlatformLimit(t, im)
		})
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

// checkEditedCopy copies one edited image, with its data and metadata-only;
// it returns 1 when the package opened the source and the copies were
// judged.
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

	// An image shorter than its superblock declares: no field tells the
	// reader, so a read past the end is the ReaderAt's own end-of-file,
	// passed through ("a failing reader"). Every other map past its image
	// or device is a declared bound the reader refuses at Stat.
	short := false
	if ref, perr := erofsspec.Parse(img, im.Extra()); perr == nil {
		short = uint64(len(img)) < ref.Super.Blocks*uint64(ref.Super.BlockSize)
	}

	truncated := referenceVerdict(img, im.Blob).class == "truncated"

	// A directory block out of name order, a name stored twice included:
	// ReadDir lists every name and the lookups find some of them, the
	// differential's own rule for the reader; what a copy holds of such a
	// directory is the search's, and the copy is not compared.
	unsorted := false
	if ref, perr := erofsspec.Parse(img, im.Extra()); perr == nil {
		unsorted, _ = referenceWalk(ref, func(erofsspec.Entry) error { return nil })
	}

	for _, metadataOnly := range []bool{false, true} {
		mode := label + copyMode(metadataOnly)
		got, cerr := copyOpened(fsys, metadataOnly, img, im.Extra())

		switch {
		case errors.Is(cerr, errPanic):
			t.Fatalf("%s %s: %v", im.Name, mode, cerr)
		case cerr != nil && (errors.Is(cerr, io.ErrUnexpectedEOF) || errors.Is(cerr, io.EOF)) && short:
			continue
		case cerr != nil && !errors.Is(cerr, fs.ErrInvalid) && !errors.Is(cerr, erofs.ErrNotImplemented) && !errors.Is(cerr, fs.ErrNotExist):
			// ErrNotExist is a directory block out of name order: ReadDir
			// lists a name the lookup's binary search does not find, the
			// reader's documented divergence, met by the copy's own Open.
			t.Fatalf("%s %s: refused with %v, want ErrInvalid, ErrNotImplemented or ErrNotExist", im.Name, mode, cerr)
		case cerr != nil, unsorted:
			continue
		}

		if metadataOnly && truncated {
			// The source's map runs past its image or a device, and the
			// metadata-only copy's chunk indexes point where the source's
			// blocks were: the copy is as unreadable as the source, at the
			// same place, and nothing else.
			if _, verr := viewOf(got.Img, got.Extra()); !errors.Is(verr, erofsspec.ErrTruncated) {
				t.Fatalf("%s %s: the source reads past its end and the copy does not: %v", im.Name, mode, verr)
			}

			continue
		}

		if d := copyAgrees(img, im.Extra(), got.Img, got.Extra()); d != "" {
			t.Fatalf("%s %s: the copy is not the source: %s", im.Name, mode, d)
		}
	}

	return 1
}

// copyOpened copies an opened image into a fresh writer, a panic returned
// as errPanic; the source image and its devices are the copy's devices
// when metadata-only.
func copyOpened(fsys fs.FS, metadataOnly bool, src []byte, srcExtra [][]byte) (im corpus.Image, err error) {
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
		im.Devices = append([][]byte{src}, srcExtra...)
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
func viewOf(img []byte, extra [][]byte) (map[string]copyView, error) {
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
func copyAgrees(src []byte, srcExtra [][]byte, dst []byte, dstExtra [][]byte) string {
	want, err := viewOf(src, srcExtra)
	if err != nil {
		return fmt.Sprintf("the reference cannot read the source the package copied: %v", err)
	}

	got, err := viewOf(dst, dstExtra)
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
