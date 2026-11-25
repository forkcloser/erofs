package erofs_test

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math/rand/v2"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
)

// modelNode is one name of the tree a sequence of Writer calls should leave:
// a directory, or a file by the number of its inode, which its other names
// share and its content is made of.
type modelNode struct {
	dir   bool
	inode int
}

// modelTree applies the calls of one random sequence to a Writer and to a
// map of what they should leave.
type modelTree struct {
	t     *testing.T
	w     *erofs.Writer
	nodes map[string]modelNode
	next  int
	log   []string
}

func (m *modelTree) failf(format string, args ...any) {
	m.t.Helper()
	m.t.Fatalf("%s\nafter:\n  %s", fmt.Sprintf(format, args...), strings.Join(m.log, "\n  "))
}

func (m *modelTree) under(dir string) []string {
	var names []string

	for p := range m.nodes {
		if strings.HasPrefix(p, dir+"/") {
			names = append(names, p)
		}
	}

	return names
}

// pick is one of the names for which keep holds, in an order that does not
// depend on the map's.
func (m *modelTree) pick(rnd *rand.Rand, keep func(string, modelNode) bool) (string, bool) {
	var names []string

	for p, n := range m.nodes {
		if keep(p, n) {
			names = append(names, p)
		}
	}

	if len(names) == 0 {
		return "", false
	}

	slices.Sort(names)

	return names[rnd.IntN(len(names))], true
}

// step makes one call, chosen by rnd among those the tree allows.
func (m *modelTree) step(rnd *rand.Rand) {
	m.t.Helper()

	isDir := func(_ string, n modelNode) bool { return n.dir }
	isFile := func(_ string, n modelNode) bool { return !n.dir }
	whatever := func(string, modelNode) bool { return true }

	// A new name under a directory there is, or under the root.
	fresh := func() string {
		parent, ok := m.pick(rnd, isDir)
		if !ok || rnd.IntN(4) == 0 {
			parent = ""
		}

		for {
			if p := parent + "/n" + strconv.Itoa(rnd.IntN(6)); m.nodes[p] == (modelNode{}) {
				return p
			}

			parent += "/n" + strconv.Itoa(rnd.IntN(6))
			if m.nodes[parent] == (modelNode{}) {
				return parent
			}

			if !m.nodes[parent].dir {
				return ""
			}
		}
	}

	switch rnd.IntN(8) {
	case 0, 1:
		p := fresh()
		if p == "" {
			return
		}

		m.next++
		m.log = append(m.log, fmt.Sprintf("Create(%s) inode %d", p, m.next))
		writeFile(m.t, m.w, p, []byte("content of "+strconv.Itoa(m.next)))
		m.nodes[p] = modelNode{inode: m.next}
	case 2:
		p := fresh()
		if p == "" {
			return
		}

		m.log = append(m.log, "Mkdir("+p+")")

		if err := m.w.Mkdir(p, 0o755); err != nil {
			m.failf("Mkdir(%s): %v", p, err)
		}

		m.nodes[p] = modelNode{dir: true}
	case 3, 4:
		old, ok := m.pick(rnd, isFile)
		p := fresh()

		if !ok || p == "" {
			return
		}

		m.log = append(m.log, fmt.Sprintf("Link(%s, %s)", old, p))

		if err := m.w.Link(old, p); err != nil {
			m.failf("Link(%s, %s): %v", old, p, err)
		}

		m.nodes[p] = m.nodes[old]
	case 5, 6:
		p, ok := m.pick(rnd, whatever)
		if !ok {
			return
		}

		m.log = append(m.log, "Remove("+p+")")

		err := m.w.Remove(p)

		switch full := len(m.under(p)) > 0; {
		case full && !errors.Is(err, erofs.ErrDirNotEmpty):
			m.failf("Remove(%s), a directory with entries: %v; want ErrDirNotEmpty", p, err)
		case !full && err != nil:
			m.failf("Remove(%s): %v", p, err)
		case !full:
			delete(m.nodes, p)
		}
	default:
		p, ok := m.pick(rnd, whatever)
		if !ok {
			return
		}

		m.log = append(m.log, "RemoveAll("+p+")")

		if err := m.w.RemoveAll(p); err != nil {
			m.failf("RemoveAll(%s): %v", p, err)
		}

		for _, under := range m.under(p) {
			delete(m.nodes, under)
		}

		delete(m.nodes, p)
	}
}

// check closes the Writer and holds the image to the tree: its names, each
// file's content, one inode for the names that share one and for no others,
// and the link counts that follow.
func (m *modelTree) check(out *testBuffer) {
	m.t.Helper()

	if err := m.w.Close(); err != nil {
		m.failf("Close: %v", err)
	}

	fsys := openWithDevices(m.t, out.Bytes())

	got := map[string]modelNode{}
	inodes := map[uint64]int{}

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		st, ok := info.Sys().(*erofs.Stat)
		if !ok {
			m.failf("%s: Sys is %T", p, info.Sys())
		}

		want := m.nodes["/"+p]

		if d.IsDir() {
			got["/"+p] = modelNode{dir: true}

			subdirs := 0

			for q, n := range m.nodes {
				if n.dir && path.Dir(q) == "/"+p {
					subdirs++
				}
			}

			if int(st.Nlink) != 2+subdirs {
				m.failf("%s has a link count of %d; want %d", p, st.Nlink, 2+subdirs)
			}

			return nil
		}

		content, rerr := fs.ReadFile(fsys, p)
		if rerr != nil {
			return rerr
		}

		inode, _ := strconv.Atoi(strings.TrimPrefix(string(content), "content of "))
		got["/"+p] = modelNode{inode: inode}

		if seen, ok := inodes[st.Ino]; ok && seen != inode {
			m.failf("%s: inode %d of the image holds the content of %d and of %d", p, st.Ino, seen, inode)
		}

		inodes[st.Ino] = inode

		names := 0

		for _, n := range m.nodes {
			if !n.dir && n.inode == want.inode {
				names++
			}
		}

		if int(st.Nlink) != names {
			m.failf("%s has a link count of %d; want %d", p, st.Nlink, names)
		}

		return nil
	})
	if err != nil {
		m.failf("the walk of the image: %v", err)
	}

	if !maps.Equal(got, m.nodes) {
		m.failf("the image holds %v; want %v", got, m.nodes)
	}

	// One inode of the image for each of the tree's.
	files := map[int]bool{}

	for _, n := range m.nodes {
		if !n.dir {
			files[n.inode] = true
		}
	}

	if len(inodes) != len(files) {
		m.failf("the image holds %d inodes of files; want %d", len(inodes), len(files))
	}
}

// TestContractWriterLeavesWhatItsCallsLeave: random sequences of Create,
// Mkdir, Link, Remove and RemoveAll, each call one the tree allows, and the
// image is the tree they leave: no removed name back, none lost, a removed
// first name's file under its other names, the link counts of what is left.
func TestContractWriterLeavesWhatItsCallsLeave(t *testing.T) {
	t.Parallel()

	for seed := range uint64(600) {
		rnd := rand.New(rand.NewPCG(seed, 0x6c696e6b))

		out := &testBuffer{}
		m := &modelTree{
			t:     t,
			w:     erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0)),
			nodes: map[string]modelNode{},
			log:   []string{fmt.Sprintf("seed %d", seed)},
		}

		for range 10 + rnd.IntN(60) {
			m.step(rnd)
		}

		m.check(out)
	}
}

// namesOf closes w and lists what its image holds, directories with a
// slash after them.
func namesOf(t *testing.T, w *erofs.Writer, out *testBuffer) []string {
	t.Helper()

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var names []string

	err := fs.WalkDir(openWithDevices(t, out.Bytes()), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}

		if d.IsDir() {
			p += "/"
		}

		names = append(names, p)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return names
}

// TestContractRemovingWhatACopyPlaced: a name is removed as any other after
// a copy wrote over it, after a marker sifted its directory around it, and
// after it stopped being a directory: the removal takes that name and no
// other, and what is made afterwards is there.
func TestContractRemovingWhatACopyPlaced(t *testing.T) {
	t.Parallel()

	file := func(content string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(content)} }

	for name, tc := range map[string]struct {
		layers []fstest.MapFS
		merge  bool
		remove []string
		want   []string
	}{
		"a name a later copy wrote over": {
			layers: []fstest.MapFS{
				{"a": file("a"), "b": file("b"), "c": file("c"), "d": file("d")},
				{"c": file("again")},
			},
			remove: []string{"/c"},
			want:   []string{"a", "b", "d", "new"},
		},
		"a name its layer's opaque marker spared": {
			layers: []fstest.MapFS{
				{"d/a": file("a"), "d/b": file("b"), "d/c": file("c")},
				{"d/-kept": file("kept"), "d/.wh..wh..opq": file(""), "d/z": file("z")},
			},
			merge:  true,
			remove: []string{"/d/-kept"},
			want:   []string{"d/", "d/z", "new"},
		},
		"a directory spared with its marker's layer, what it held before gone": {
			layers: []fstest.MapFS{
				{"d/-sub/old": file("old"), "d/-sub/older": file("older"), "d/b": file("b")},
				{"d/-sub/mine": file("mine"), "d/.wh..wh..opq": file("")},
			},
			merge:  true,
			remove: []string{"/d/-sub/mine", "/d/-sub"},
			want:   []string{"d/", "new"},
		},
		"a name that stopped being a directory": {
			layers: []fstest.MapFS{
				{"a/b": file("b"), "a/d/e": file("e"), "keep": file("keep")},
				{"a": file("now a file")},
			},
			remove: []string{"/a"},
			want:   []string{"keep", "new"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out := &testBuffer{}
			w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

			for i, layer := range tc.layers {
				var opts []erofs.CopyOpt
				if tc.merge && i > 0 {
					opts = append(opts, erofs.Merge())
				}

				if err := w.CopyFrom(layer, opts...); err != nil {
					t.Fatalf("the copy of layer %d: %v", i, err)
				}
			}

			for _, p := range tc.remove {
				if err := w.Remove(p); err != nil {
					t.Fatalf("Remove(%s): %v", p, err)
				}
			}

			writeFile(t, w, "/new", []byte("new"))

			if got := namesOf(t, w, out); !slices.Equal(got, tc.want) {
				t.Errorf("the image holds %v; want %v", got, tc.want)
			}
		})
	}
}

// heapAfter is the heap in use once what can be collected is.
func heapAfter() int64 {
	var stats runtime.MemStats

	//revive:disable-next-line:call-to-gc what a Writer holds is measured from a collected heap
	runtime.GC()
	runtime.ReadMemStats(&stats)

	return int64(stats.HeapInuse)
}

// TestContractWriterHoldsNothingOfARemovedName: a name made and removed over
// and over costs a Writer nothing that stays: a directory, a file's other
// name beside one that stays, and a directory removed with what it holds.
// Before, every removed name stayed in its directory's list for the Writer's
// life: 300,000 rounds of the first shape held about 90 MiB, and as many
// names made and then removed about 108 MiB, where the map and the list
// that held them now keep 16.
//
//nolint:paralleltest // measures the process's heap in use; a parallel test's live heap would count against the bound
func TestContractWriterHoldsNothingOfARemovedName(t *testing.T) {
	const (
		rounds = 300_000
		bound  = 8 << 20
		// What a map and a list keep of the most they held at once, which
		// neither gives back: far under the names themselves.
		boundAfterAll = 32 << 20
	)

	for _, shape := range []struct {
		name   string
		rounds int
		bound  int64
		round  func(w *erofs.Writer) error
	}{
		{"a directory made and removed", rounds, bound, func(w *erofs.Writer) error {
			return errors.Join(w.Mkdir("/d/x", 0o755), w.Remove("/d/x"))
		}},
		{"a file's other name made and removed", rounds, bound, func(w *erofs.Writer) error {
			return errors.Join(w.Link("/d/f", "/d/x"), w.Remove("/d/x"))
		}},
		{"a directory removed with what it holds", rounds, bound, func(w *erofs.Writer) error {
			return errors.Join(w.Mkdir("/d/x", 0o755), w.Mkdir("/d/x/y", 0o755), w.Link("/d/f", "/d/x/l"), w.RemoveAll("/d/x"))
		}},
		// Not one name over and over: as many at once, then each removed,
		// the directory that held them staying.
		{"every name made, then each removed", 1, boundAfterAll, func(w *erofs.Writer) error {
			for i := range rounds {
				if err := w.Mkdir("/d/x"+strconv.Itoa(i), 0o755); err != nil {
					return err
				}
			}

			for i := range rounds {
				if err := w.Remove("/d/x" + strconv.Itoa(i)); err != nil {
					return err
				}
			}

			return nil
		}},
	} {
		w := erofs.Create(&testBuffer{}, erofs.WithBuildTime(corpus.BuildTime, 0))
		writeFile(t, w, "/d/f", []byte("content"))

		if err := w.Link("/d/f", "/d/stays"); err != nil {
			t.Fatal(err)
		}

		before := heapAfter()

		for range shape.rounds {
			if err := shape.round(w); err != nil {
				t.Fatalf("%s: %v", shape.name, err)
			}
		}

		if grown := heapAfter() - before; grown > shape.bound {
			t.Errorf(
				"%s, %d names: the heap in use grew by %d MiB; want under %d",
				shape.name,
				rounds,
				grown>>20,
				shape.bound>>20,
			)
		}

		runtime.KeepAlive(w)
	}
}
