package erofs_test

import (
	"errors"
	"io/fs"
	"maps"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/erofsspec"
)

// TestContractSetxattrLeavesTheSourcesMapAlone: an attribute set on one
// entry after a copy is on that entry, and nowhere else. A source may hand
// several entries one map of attributes, and the map is the source's: the
// Writer kept it and wrote into it, so the attribute appeared on every
// entry that shared it, and in the source's own map.
func TestContractSetxattrLeavesTheSourcesMapAlone(t *testing.T) {
	t.Parallel()

	shared := map[string]string{"user.k": "v", "user.kept": "kept"}
	file := func() *fstest.MapFile {
		return &fstest.MapFile{Data: []byte("x"), Mode: 0o644, Sys: &erofs.Stat{Xattrs: shared}}
	}

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	if err := w.CopyFrom(fstest.MapFS{"a": file(), "b": file()}); err != nil {
		t.Fatal(err)
	}

	if err := errors.Join(
		w.Setxattr("/a", "user.added", "1"),
		w.Setxattr("/a", "user.k", "changed"),
		w.Close(),
	); err != nil {
		t.Fatal(err)
	}

	if !maps.Equal(shared, map[string]string{"user.k": "v", "user.kept": "kept"}) {
		t.Errorf("the source's own map is now %v", shared)
	}

	ref, err := erofsspec.Parse(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]map[string]string{
		// What a was handed and nobody set again is still on it.
		"a": {"user.k": "changed", "user.kept": "kept", "user.added": "1"},
		"b": {"user.k": "v", "user.kept": "kept"},
	} {
		in, err := ref.Resolve(name, 0)
		if err != nil {
			t.Fatal(err)
		}

		if got := xattrsOf(in); !maps.Equal(got, want) {
			t.Errorf("%s has %v; want %v", name, got, want)
		}
	}
}

// TestContractSetxattrOnTheRootLeavesTheSourcesMapAlone: the root is the one
// entry a copy does not replace but hands a map to, in place, and it may be
// set before the copy as well as after: a build's own order, a label on the
// root, a layer, a label. Neither label reaches the layer's map, nor the
// entries that share it.
func TestContractSetxattrOnTheRootLeavesTheSourcesMapAlone(t *testing.T) {
	t.Parallel()

	layer := func(shared map[string]string, name string) fstest.MapFS {
		return fstest.MapFS{
			".":  &fstest.MapFile{Mode: fs.ModeDir | 0o755, Sys: &erofs.Stat{Xattrs: shared}},
			name: &fstest.MapFile{Data: []byte("x"), Mode: 0o644, Sys: &erofs.Stat{Xattrs: shared}},
		}
	}

	first, second := map[string]string{"user.k": "first"}, map[string]string{"user.k": "second"}

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	if err := errors.Join(
		w.Setxattr("/", "user.before", "1"),
		w.CopyFrom(layer(first, "a")),
		w.Setxattr("/", "user.between", "1"),
		w.CopyFrom(layer(second, "b")),
		w.Setxattr("/", "user.after", "1"),
		w.Close(),
	); err != nil {
		t.Fatal(err)
	}

	if !maps.Equal(first, map[string]string{"user.k": "first"}) ||
		!maps.Equal(second, map[string]string{"user.k": "second"}) {
		t.Errorf("the sources' own maps are now %v and %v", first, second)
	}

	ref, err := erofsspec.Parse(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]map[string]string{
		// The root has the last layer's map and what was set after it.
		".": {"user.k": "second", "user.after": "1"},
		"a": {"user.k": "first"},
		"b": {"user.k": "second"},
	} {
		in, err := ref.Resolve(name, 0)
		if err != nil {
			t.Fatal(err)
		}

		if got := xattrsOf(in); !maps.Equal(got, want) {
			t.Errorf("%s has %v; want %v", name, got, want)
		}
	}
}
