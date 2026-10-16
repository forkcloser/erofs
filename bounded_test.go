package erofs_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/erofsspec"
)

// A bounded walk over the writer's call sequences: every sequence of up to
// three calls from a small alphabet over a few names, 2,379 of them, and a
// deterministic sample of the sequences of four, every thirty-first of the
// 28,561, by default; every sequence of four under EROFS_FULL_DIFFERENTIAL=1,
// the opt-in the reader's sweep uses, 30,940 in all. Each runs against a
// model of the tree the Writer docs describe. After
// every call its error class is the model's and Writer.Stat agrees with
// the model on what exists; after Close the reference reads the image back
// to the model, names, types, bits, ids, content, targets, xattrs, link
// counts and inode identity.
//
// Where the docs leave a call's outcome open (a name created over an
// existing one), the walk holds the writer to consistency instead: the
// call's result and the tree afterwards agree, whichever way it went, and
// the tally of what it did is logged.

// mnode is the model's inode; hard-linked names share one.
type mnode struct {
	typ    byte
	perm   int64 // -1 when the docs do not say (an implicit directory)
	uid    int   // -1 when unknown
	gid    int
	data   []byte
	link   string
	xattrs map[string]string
}

// model is the tree a sequence should have built, by path.
type model struct {
	nodes map[string]*mnode
}

func newModel() *model { return &model{nodes: map[string]*mnode{}} }

// parentOf is the directory a path sits in, "" for the root.
func parentOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}

	return ""
}

// childrenOf lists the paths directly under dir.
func (m *model) childrenOf(dir string) []string {
	var out []string

	for p := range m.nodes {
		if parentOf(p) == dir {
			out = append(out, p)
		}
	}

	return out
}

// isDir says whether path is a directory of the model.
func (m *model) isDir(path string) bool {
	n, ok := m.nodes[path]

	return ok && n != nil && n.typ == tar.TypeDir
}

// names counts the paths sharing n.
func (m *model) names(n *mnode) int {
	count := 0

	for _, o := range m.nodes {
		if o == n {
			count++
		}
	}

	return count
}

// place puts n at path, creating the implicit parents a Writer creates
// ("TestCreateFSImplicitDirs" of the inherited suite; the docs say nothing
// of their bits, so they are unknown) and dropping whatever the path held,
// children included.
func (m *model) place(path string, n *mnode) {
	m.drop(path)

	for dir := parentOf(path); dir != ""; dir = parentOf(dir) {
		if _, ok := m.nodes[dir]; !ok {
			m.nodes[dir] = &mnode{typ: tar.TypeDir, perm: -1, uid: -1, gid: -1}
		}
	}

	m.nodes[path] = n
}

// drop removes a path with everything under it.
func (m *model) drop(path string) {
	for p := range m.nodes {
		if p == path || strings.HasPrefix(p, path+"/") {
			delete(m.nodes, p)
		}
	}
}

// class is the error class a call's result is held to.
type class int

const (
	classOK class = iota
	classNotExist
	classExist
	classNotEmpty
	classNotDir
	classInvalid
	classAnyError
	classOpen // the docs do not say; the model follows the writer
)

func (c class) String() string {
	return [...]string{"ok", "ErrNotExist", "ErrExist", "ErrDirNotEmpty", "ErrNotDirectory", "ErrInvalid", "an error", "open"}[c]
}

// classOf is the class a result falls in; an error outside the named
// classes is classAnyError.
func classOf(err error) class {
	switch {
	case err == nil:
		return classOK
	case errors.Is(err, fs.ErrNotExist):
		return classNotExist
	case errors.Is(err, fs.ErrExist):
		return classExist
	case errors.Is(err, erofs.ErrDirNotEmpty):
		return classNotEmpty
	case errors.Is(err, erofs.ErrNotDirectory):
		return classNotDir
	case errors.Is(err, fs.ErrInvalid):
		return classInvalid
	default:
		return classAnyError
	}
}

// expectation is what the model says of a call before it runs: the class
// its result must fall in and the change to apply when it returns nil.
type expectation struct {
	class class
	apply func()
}

// wop is one letter of the alphabet: a call on the writer and the model's
// expectation of it.
type wop struct {
	name string
	run  func(w *erofs.Writer) error
	want func(m *model) expectation
}

// creating is the expectation of a call that creates path with n: fine
// into a free name under directories, an error through a non-directory,
// open over an existing name.
func creating(m *model, path string, n *mnode) expectation {
	for dir := parentOf(path); dir != ""; dir = parentOf(dir) {
		if _, ok := m.nodes[dir]; ok && !m.isDir(dir) {
			return expectation{class: classAnyError}
		}
	}

	e := expectation{class: classOK, apply: func() { m.place(path, n) }}
	if _, ok := m.nodes[path]; ok {
		e.class = classOpen
	}

	return e
}

// touching is the expectation of a metadata call on path: ErrNotExist
// for a missing one, else fine with the change.
func touching(m *model, path string, apply func(n *mnode)) expectation {
	n, ok := m.nodes[path]
	if !ok {
		return expectation{class: classNotExist}
	}

	return expectation{class: classOK, apply: func() { apply(n) }}
}

func regular(data []byte) *mnode {
	return &mnode{typ: tar.TypeReg, perm: 0o644, data: data}
}

func createOp(path string, data []byte) wop {
	return wop{
		name: "create " + path,
		run: func(w *erofs.Writer) error {
			return writeRegular(w, corpus.Node{Path: path, Data: data, Mode: 0o644})
		},
		want: func(m *model) expectation { return creating(m, path, regular(data)) },
	}
}

// alphabet is the walk's calls, over the names a, a/x, d, d/b, l, h and p.
//
//nolint:gochecknoglobals // the walk's fixed alphabet
var alphabet = []wop{
	createOp("a", corpus.Pattern(700)),
	createOp("d/b", corpus.Pattern(10)),
	createOp("a/x", corpus.Pattern(5000)),
	{
		name: "mkdir d",
		run:  func(w *erofs.Writer) error { return w.Mkdir("d", 0o750) },
		want: func(m *model) expectation {
			e := creating(m, "d", &mnode{typ: tar.TypeDir, perm: 0o750})
			if m.isDir("d") {
				// Over a directory the children stay whatever the writer does.
				e.apply = func() { m.nodes["d"].perm = 0o750 }
			}

			return e
		},
	},
	{
		name: "symlink l",
		run:  func(w *erofs.Writer) error { return w.Symlink("a", "l") },
		want: func(m *model) expectation {
			return creating(m, "l", &mnode{typ: tar.TypeSymlink, perm: 0o777, link: "a"})
		},
	},
	{
		name: "link a h",
		run:  func(w *erofs.Writer) error { return w.Link("a", "h") },
		want: func(m *model) expectation {
			a, ok := m.nodes["a"]

			switch {
			case !ok:
				return expectation{class: classNotExist}
			case a.typ == tar.TypeDir:
				return expectation{class: classAnyError}
			default:
				return creating(m, "h", a)
			}
		},
	},
	{
		name: "mknod p",
		run:  func(w *erofs.Writer) error { return w.Mknod("p", fs.ModeNamedPipe|0o600, 0) },
		want: func(m *model) expectation { return creating(m, "p", &mnode{typ: tar.TypeFifo, perm: 0o600}) },
	},
	{
		name: "remove a",
		run:  func(w *erofs.Writer) error { return w.Remove("a") },
		want: func(m *model) expectation { return removing(m, "a") },
	},
	{
		name: "remove d",
		run:  func(w *erofs.Writer) error { return w.Remove("d") },
		want: func(m *model) expectation { return removing(m, "d") },
	},
	{
		name: "removeall d",
		run:  func(w *erofs.Writer) error { return w.RemoveAll("d") },
		want: func(m *model) expectation { return expectation{class: classOK, apply: func() { m.drop("d") }} },
	},
	{
		name: "chmod a",
		run:  func(w *erofs.Writer) error { return w.Chmod("a", fs.ModeSetuid|0o711) },
		want: func(m *model) expectation { return touching(m, "a", func(n *mnode) { n.perm = 0o4711 }) },
	},
	{
		name: "chown a",
		run:  func(w *erofs.Writer) error { return w.Chown("a", 65536, 7) },
		want: func(m *model) expectation { return touching(m, "a", func(n *mnode) { n.uid, n.gid = 65536, 7 }) },
	},
	{
		name: "setxattr a",
		run:  func(w *erofs.Writer) error { return w.Setxattr("a", "user.k", "v") },
		want: func(m *model) expectation {
			return touching(m, "a", func(n *mnode) { n.xattrs = map[string]string{"user.k": "v"} })
		},
	},
}

// removing is the expectation of Remove on path: ErrNotExist for a
// missing one, ErrDirNotEmpty for a directory with children, else fine.
func removing(m *model, path string) expectation {
	if _, ok := m.nodes[path]; !ok {
		return expectation{class: classNotExist}
	}

	if m.isDir(path) && len(m.childrenOf(path)) > 0 {
		return expectation{class: classNotEmpty}
	}

	return expectation{class: classOK, apply: func() { delete(m.nodes, path) }}
}

// walkNames are the paths Writer.Stat is asked about after every call; a
// symlink is left out, the docs not saying whether Stat follows it.
//
//nolint:gochecknoglobals // the walk's fixed names
var walkNames = []string{"a", "a/x", "d", "d/b", "h", "p"}

// TestBoundedWriterSequences runs the walk.
func TestBoundedWriterSequences(t *testing.T) {
	t.Parallel()

	tally := map[string]int{}
	count := 0

	for seq := range sequences() {
		count++

		runSequence(t, seq, tally)

		if t.Failed() && count > 50 {
			t.Fatalf("stopping after %d sequences", count)
		}
	}

	keys := make([]string, 0, len(tally))
	for k := range tally {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	for _, k := range keys {
		t.Logf("%s: %d", k, tally[k])
	}

	t.Logf("%d sequences", count)
}

// sequences yields every sequence of up to three letters, then every
// thirty-first of four, or every one under EROFS_FULL_DIFFERENTIAL=1.
func sequences() func(yield func([]int) bool) {
	return func(yield func([]int) bool) {
		n := len(alphabet)
		full := os.Getenv("EROFS_FULL_DIFFERENTIAL") == "1"

		for depth := 1; depth <= 4; depth++ {
			total := 1
			for range depth {
				total *= n
			}

			step := 1
			if depth == 4 && !full {
				step = 31
			}

			for k := 0; k < total; k += step {
				seq := make([]int, depth)
				for i, v := depth-1, k; i >= 0; i, v = i-1, v/n {
					seq[i] = v % n
				}

				if !yield(seq) {
					return
				}
			}
		}
	}
}

func labelOf(seq []int) string {
	parts := make([]string, len(seq))
	for i, k := range seq {
		parts[i] = alphabet[k].name
	}

	return strings.Join(parts, "; ")
}

// runSequence runs one sequence against a fresh writer and the model.
func runSequence(t *testing.T, seq []int, tally map[string]int) {
	t.Helper()

	label := labelOf(seq)
	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
	m := newModel()

	for _, k := range seq {
		op := alphabet[k]
		e := op.want(m)
		err := op.run(w)

		if e.class == classOpen {
			tally[op.name+" over an existing name: "+classOf(err).String()]++
		} else if got := classOf(err); !fits(got, e.class) {
			t.Errorf("%s: %s: %v, want %v", label, op.name, err, e.class)

			return
		}

		if err == nil && e.apply != nil {
			e.apply()
		}

		if !checkExistence(t, label, w, m) {
			return
		}
	}

	if err := w.Close(); err != nil {
		t.Errorf("%s: Close: %v", label, err)

		return
	}

	ref, err := erofsspec.Parse(out.Bytes())
	if err != nil {
		t.Errorf("%s: reference: %v", label, err)

		return
	}

	compareModel(t, label, ref, m)
}

// fits says whether a result's class satisfies the expected one.
func fits(got, want class) bool {
	return got == want || (want == classAnyError && got != classOK)
}

// checkExistence: Writer.Stat says a name exists exactly when the model
// has it, with the model's type.
func checkExistence(t *testing.T, label string, w *erofs.Writer, m *model) bool {
	t.Helper()

	for _, name := range walkNames {
		info, err := w.Stat(name)
		n, ok := m.nodes[name]

		var typ fs.FileMode
		if info != nil {
			typ = info.Mode().Type()
		}

		switch {
		case !ok && !errors.Is(err, fs.ErrNotExist):
			t.Errorf("%s: Stat(%s): %v, want ErrNotExist", label, name, err)

			return false
		case ok && err != nil:
			t.Errorf("%s: Stat(%s): %v, want %c", label, name, err, n.typ)

			return false
		case ok && (info == nil || typ != typeBits(n.typ)):
			t.Errorf("%s: Stat(%s) type %v, want %v", label, name, typ, typeBits(n.typ))

			return false
		}
	}

	return true
}

// typeBits is the fs.FileMode type of a tar type.
func typeBits(typ byte) fs.FileMode {
	switch typ {
	case tar.TypeDir:
		return fs.ModeDir
	case tar.TypeSymlink:
		return fs.ModeSymlink
	case tar.TypeFifo:
		return fs.ModeNamedPipe
	default:
		return 0
	}
}

// compareModel holds the reference's walk of the image to the model.
func compareModel(t *testing.T, label string, ref *erofsspec.Image, m *model) {
	t.Helper()

	nids := map[string]uint64{}

	err := ref.Walk(func(e erofsspec.Entry) error {
		nids[e.Path] = e.Inode.Nid

		n, ok := m.nodes[e.Path]
		if !ok {
			t.Errorf("%s: %s in the image, not in the model", label, e.Path)

			return nil
		}

		compareNode(t, label, ref, e, n, m)

		return nil
	})
	if err != nil {
		t.Errorf("%s: walk: %v", label, err)

		return
	}

	for p, n := range m.nodes {
		if _, ok := nids[p]; !ok {
			t.Errorf("%s: %s (%c) in the model, not in the image", label, p, n.typ)
		}

		for q, o := range m.nodes {
			if shared := nids[p] == nids[q]; (n == o) != shared {
				t.Errorf("%s: %s and %s share an inode in the model: %v, in the image: %v", label, p, q, n == o, shared)
			}
		}
	}
}

// compareNode holds one entry to its model node.
func compareNode(t *testing.T, label string, ref *erofsspec.Image, e erofsspec.Entry, n *mnode, m *model) {
	t.Helper()

	in := e.Inode
	path := label + ": " + e.Path

	if wantType, wantDirent := corpus.TypeOf(
		n.typ,
	); in.Mode&erofsspec.ModeTypeMask != wantType ||
		e.Dir.Type != wantDirent {
		t.Errorf("%s: mode %#o dirent type %d, want %#o and %d", path, in.Mode, e.Dir.Type, wantType, wantDirent)
	}

	if perm := int64(in.Mode &^ erofsspec.ModeTypeMask); n.perm >= 0 && perm != n.perm {
		t.Errorf("%s: permissions %#o, want %#o", path, perm, n.perm)
	}

	if n.uid >= 0 && (int(in.UID) != n.uid || int(in.GID) != n.gid) {
		t.Errorf("%s: uid:gid %d:%d, want %d:%d", path, in.UID, in.GID, n.uid, n.gid)
	}

	if want := wantNlink(m, e.Path, n); in.Nlink != want {
		t.Errorf("%s: nlink %d, want %d", path, in.Nlink, want)
	}

	if got := xattrsOf(in); !equalXattrs(got, n.xattrs) {
		t.Errorf("%s: xattrs %v, want %v", path, corpus.Keys(got), corpus.Keys(n.xattrs))
	}

	switch n.typ {
	case tar.TypeReg:
		if got, err := ref.Read(in); err != nil || !bytes.Equal(got, n.data) {
			t.Errorf("%s: content %d bytes (%v), want %d", path, len(got), err, len(n.data))
		}
	case tar.TypeSymlink:
		if got, err := ref.Target(in); err != nil || string(got) != n.link {
			t.Errorf("%s: target %q (%v), want %q", path, got, err, n.link)
		}
	}
}

// wantNlink is a directory's 2 plus its subdirectories, or the number of
// names sharing the inode.
func wantNlink(m *model, path string, n *mnode) uint32 {
	if n.typ != tar.TypeDir {
		return uint32(m.names(n))
	}

	count := uint32(2)

	for _, c := range m.childrenOf(path) {
		if m.isDir(c) {
			count++
		}
	}

	return count
}
