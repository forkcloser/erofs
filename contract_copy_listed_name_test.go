package erofs_test

import (
	"errors"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/go-erofs"
)

// misnaming is a tree one of whose directories lists one of its entries
// under another name, which need not be a name: what a source that breaks
// fs.DirEntry's "the base name" hands a walk.
type misnaming struct {
	fstest.MapFS

	dir, from, to string
}

type misnamed struct {
	fs.DirEntry

	name string
}

func (m misnamed) Name() string { return m.name }

func (m misnaming) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := m.MapFS.ReadDir(name)
	if err != nil || name != m.dir {
		return entries, err
	}

	for i, e := range entries {
		if e.Name() == m.from {
			entries[i] = misnamed{e, m.to}
		}
	}

	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })

	return entries, nil
}

// TestContractCopyRefusesAListedNameThatIsNotOne: a directory entry's name
// is one path element. A source that lists anything else, an empty name,
// "." or "..", or one with a separator, is refused as it is listed, with
// ErrCorrupt and before anything of that directory is copied: the walk
// joins a name to its directory, and such a name is another path.
func TestContractCopyRefusesAListedNameThatIsNotOne(t *testing.T) {
	t.Parallel()

	file := &fstest.MapFile{Data: []byte("x")}

	for _, name := range []string{"", ".", "..", "a/", "/a", "sub/a", "../a", "a/../b"} {
		for _, dir := range []string{".", "d"} {
			tree := fstest.MapFS{"a": file, "d/a": file, "d/b": file}
			src := misnaming{MapFS: tree, dir: dir, from: "a", to: name}

			for mode, opts := range map[string][]erofs.CopyOpt{"a copy": nil, "a merge": {erofs.Merge()}} {
				err := erofs.Create(&testBuffer{}).CopyFrom(src, opts...)
				if err == nil || !errors.Is(err, erofs.ErrCorrupt) ||
					!strings.Contains(err.Error(), strconv.Quote(name)) {
					t.Errorf(
						"%s of a tree whose %q lists the name %q: %v; want ErrCorrupt, naming it",
						mode,
						dir,
						name,
						err,
					)
				}
			}
		}
	}
}

// TestContractMergeAppliesNoMarkerThatALayerDidNotName: a layer's listing
// that names "../.wh.secret" in its directory "x" names no marker of the
// root: the merge is refused, and "secret", which the layer below has, is
// not taken out of the tree for a path the layer never had.
func TestContractMergeAppliesNoMarkerThatALayerDidNotName(t *testing.T) {
	t.Parallel()

	file := func(content string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(content)} }

	lower := fstest.MapFS{"secret": file("kept"), "x/old": file("old")}
	upper := misnaming{MapFS: fstest.MapFS{"x/y": file("")}, dir: "x", from: "y", to: "../.wh.secret"}

	w := erofs.Create(&testBuffer{})

	if err := w.CopyFrom(lower); err != nil {
		t.Fatal(err)
	}

	if err := w.CopyFrom(upper, erofs.Merge()); !errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf("the merge of a layer that lists %q: %v; want ErrCorrupt", "../.wh.secret", err)
	}

	if _, err := w.Stat("/secret"); err != nil {
		t.Errorf("after that merge, the lower layer's secret: %v; want it still there", err)
	}
}

// TestContractCopyRefusesANameListedTwiceWhereverItIs: a name a directory
// lists twice is refused whether the two are its first entries, its last or
// in between.
func TestContractCopyRefusesANameListedTwiceWhereverItIs(t *testing.T) {
	t.Parallel()

	file := &fstest.MapFile{Data: []byte("x")}

	for _, pair := range [][2]string{{"b", "a"}, {"c", "b"}, {"d", "c"}} {
		src := misnaming{
			MapFS: fstest.MapFS{"a": file, "b": file, "c": file, "d": file},
			dir:   ".", from: pair[0], to: pair[1],
		}

		err := erofs.Create(&testBuffer{}).CopyFrom(src)
		if err == nil || !errors.Is(err, erofs.ErrCorrupt) || !strings.Contains(err.Error(), "twice") {
			t.Errorf(
				"a copy of a tree that lists %q twice among a, b, c and d: %v; want ErrCorrupt, for a name listed twice",
				pair[1],
				err,
			)
		}
	}
}
