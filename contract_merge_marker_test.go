package erofs_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
)

// mergedLayers is lower with upper merged over it, by each path a copy
// takes: the walk of a source tree, and the copy of an image's metadata.
// It returns the two results, each ready to read.
func mergedLayers(t *testing.T, lower, upper fstest.MapFS) map[string]fs.FS {
	t.Helper()

	image := func(tree fstest.MapFS) []byte {
		out := &testBuffer{}
		w := erofs.Create(out)

		if err := w.CopyFrom(tree); err != nil {
			t.Fatal(err)
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		return out.Bytes()
	}

	walked := &testBuffer{}
	w := erofs.Create(walked)

	if err := w.CopyFrom(lower); err != nil {
		t.Fatal(err)
	}

	if err := w.CopyFrom(upper, erofs.Merge()); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// The upper layer as an image keeps its markers: it is built without
	// Merge.
	lowerImage, upperImage := image(lower), image(upper)

	copied := &testBuffer{}
	w = erofs.Create(copied)

	for i, layer := range [][]byte{lowerImage, upperImage} {
		source, err := erofs.Open(bytes.NewReader(layer))
		if err != nil {
			t.Fatal(err)
		}

		opts := []erofs.CopyOpt{erofs.MetadataOnly()}
		if i > 0 {
			opts = append(opts, erofs.Merge())
		}

		if err = w.CopyFrom(source, opts...); err != nil {
			t.Fatal(err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return map[string]fs.FS{
		"the walk":         openWithDevices(t, walked.Bytes()),
		"the image's copy": openWithDevices(t, copied.Bytes(), lowerImage, upperImage),
	}
}

// TestContractMergeMarkerSparesItsOwnLayer: a whiteout and an opaque marker
// "delete the named entry from prior layers", and nothing of the layer they
// are in, whichever side of the marker a name sorts on: a walk goes by name,
// and ".bashrc", ".cache", ".ssh" and any name starting with "-" come before
// ".wh..wh..opq". A name that is the whiteout prefix alone names nothing and
// is a file like another. "The markers themselves are not added to the
// image": nor is what lies under a marker that is a directory. All of it
// holds through the walk of a tree and through the copy of an image's
// metadata.
func TestContractMergeMarkerSparesItsOwnLayer(t *testing.T) {
	t.Parallel()

	file := func(content string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(content)} }

	for name, tc := range map[string]struct {
		lower, upper fstest.MapFS
		want         []string
		content      map[string]string
	}{
		"an opaque marker, the layer's own names before and after it": {
			lower: fstest.MapFS{"d/old": file("old"), "d/.bashrc": file("lower"), "d/.abc": file("lower")},
			upper: fstest.MapFS{
				"d/.bashrc": file("upper"), "d/.cache/x": file("x"), "d/-dash": file("dash"),
				"d/.wh..wh..opq": file(""), "d/new": file("new"),
			},
			want:    []string{"d", "d/-dash", "d/.bashrc", "d/.cache", "d/.cache/x", "d/new"},
			content: map[string]string{"d/.bashrc": "upper", "d/.cache/x": "x"},
		},
		"a whiteout of a name the layer has too": {
			lower:   fstest.MapFS{"d/-x": file("lower"), "d/keep": file("keep")},
			upper:   fstest.MapFS{"d/-x": file("upper"), "d/.wh.-x": file("")},
			want:    []string{"d", "d/-x", "d/keep"},
			content: map[string]string{"d/-x": "upper"},
		},
		"a whiteout of a directory of the layer below": {
			lower: fstest.MapFS{"d/sub/a": file("a"), "d/sub/deep/b": file("b"), "d/keep": file("keep")},
			upper: fstest.MapFS{"d/.wh.sub": file("")},
			want:  []string{"d", "d/keep"},
		},
		"the prefix alone, in a directory": {
			lower: fstest.MapFS{"a/keep": file("keep")},
			upper: fstest.MapFS{"a/.wh.": file("")},
			want:  []string{"a", "a/.wh.", "a/keep"},
		},
		"the prefix alone, at the root": {
			lower: fstest.MapFS{"a/keep": file("keep")},
			upper: fstest.MapFS{".wh.": file("")},
			want:  []string{".wh.", "a", "a/keep"},
		},
		// A marker is a name, whatever bears it: one that is a directory
		// is applied and left out, with everything under it.
		"a directory named as a whiteout, with entries": {
			lower: fstest.MapFS{"keep/a": file("a"), "other": file("other")},
			upper: fstest.MapFS{".wh.keep/x": file("x"), ".wh.keep/sub/y": file("y")},
			want:  []string{"other"},
		},
		"a directory named as the opaque marker, with entries": {
			lower: fstest.MapFS{"keep/a": file("a"), "keep/b": file("b")},
			upper: fstest.MapFS{"keep/.wh..wh..opq/x": file("x"), "keep/new": file("new")},
			want:  []string{"keep", "keep/new"},
		},
	} {
		for route, fsys := range mergedLayers(t, tc.lower, tc.upper) {
			var got []string

			err := fs.WalkDir(fsys, ".", func(p string, _ fs.DirEntry, err error) error {
				if err == nil && p != "." {
					got = append(got, p)
				}

				return err
			})
			if err != nil {
				t.Errorf("%s, by %s: %v", name, route, err)

				continue
			}

			slices.Sort(got)

			if !slices.Equal(got, tc.want) {
				t.Errorf("%s, by %s:\n got  %q\n want %q", name, route, got, tc.want)
			}

			for p, want := range tc.content {
				if data, err := fs.ReadFile(fsys, p); err != nil || string(data) != want {
					t.Errorf("%s, by %s: %s reads %q, %v; want %q", name, route, p, data, err, want)
				}
			}
		}
	}
}

// TestContractMergeMarkerSparesEveryNameOfItsLayersFile: a whiteout that
// names a file of its own layer, one that sorts before it, takes the file
// of the layer below and leaves the layer's own with every name it has: the
// name the marker bears and the one that comes after the marker are one
// file of two links, through the walk of a tree and through the copy of an
// image's metadata.
func TestContractMergeMarkerSparesEveryNameOfItsLayersFile(t *testing.T) {
	t.Parallel()

	lower := writeImage(t, []corpus.Node{
		{Path: "-a", Type: tar.TypeReg, Mode: 0o644, Data: []byte("lower"), Sec: corpus.BuildTime},
	})
	upper := writeImage(t, []corpus.Node{
		{Path: "-a", Type: tar.TypeReg, Mode: 0o644, Data: []byte("upper"), Sec: corpus.BuildTime},
		{Path: ".wh.-a", Type: tar.TypeReg, Mode: 0o644, Sec: corpus.BuildTime},
		{Path: "zlink", Type: tar.TypeLink, Link: "-a", Sec: corpus.BuildTime},
	})

	for route, opts := range map[string][]erofs.CopyOpt{
		"the walk":         nil,
		"the image's copy": {erofs.MetadataOnly()},
	} {
		out := &testBuffer{}
		w := erofs.Create(out)

		if err := errors.Join(
			w.CopyFrom(openWithDevices(t, lower), opts...),
			w.CopyFrom(openWithDevices(t, upper), append([]erofs.CopyOpt{erofs.Merge()}, opts...)...),
		); err != nil {
			t.Fatalf("by %s: %v", route, err)
		}

		if err := w.Close(); err != nil {
			t.Fatalf("by %s: %v", route, err)
		}

		var devices [][]byte
		if opts != nil {
			devices = [][]byte{lower, upper}
		}

		fsys := openWithDevices(t, out.Bytes(), devices...)

		entries, err := fs.ReadDir(fsys, ".")
		if err != nil {
			t.Fatalf("by %s: %v", route, err)
		}

		if len(entries) != 2 || entries[0].Name() != "-a" || entries[1].Name() != "zlink" {
			t.Errorf("by %s: the root lists %v; want -a and zlink", route, entries)

			continue
		}

		var inodes []uint64

		for _, name := range []string{"-a", "zlink"} {
			info, err := fs.Stat(fsys, name)
			if err != nil {
				t.Fatalf("by %s: %v", route, err)
			}

			st, ok := info.Sys().(*erofs.Stat)
			if !ok || st.Nlink != 2 {
				t.Errorf("by %s: %s has Sys() %v; want a file of 2 links", route, name, info.Sys())

				continue
			}

			inodes = append(inodes, st.Ino)

			if data, err := fs.ReadFile(fsys, name); err != nil || string(data) != "upper" {
				t.Errorf("by %s: %s reads %q, %v; want the upper layer's file", route, name, data, err)
			}
		}

		if len(inodes) == 2 && inodes[0] != inodes[1] {
			t.Errorf("by %s: -a is inode %d and zlink %d; want one file", route, inodes[0], inodes[1])
		}
	}
}
