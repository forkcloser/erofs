package erofs_test

import (
	"bytes"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/erofs"
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
