package erofs_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"runtime"
	"strings"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/erofsspec"
)

// attributedImage is an image of empty files "f00000".. in its root, each
// with the attributes attrs gives its number.
func attributedImage(t *testing.T, files int, attrs func(i int) map[string]string) []byte {
	t.Helper()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	for i := range files {
		name := fmt.Sprintf("/f%05d", i)
		writeFile(t, w, name, nil)

		for attr, value := range attrs(i) {
			if err := w.Setxattr(name, attr, value); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return out.Bytes()
}

// copyRoutes are the two ways a copy reads an image: the walk, which a copy
// with its data takes, and the image's own metadata, which a metadata-only
// copy takes.
func copyRoutes() map[string][]erofs.CopyOpt {
	return map[string][]erofs.CopyOpt{
		"with its data": nil,
		"metadata-only": {erofs.MetadataOnly()},
	}
}

// liveHeap is the bytes the process holds once what nothing refers to is
// collected.
func liveHeap() int64 {
	// Twice: the first run's finalizers free what the second collects.
	for range 2 {
		//revive:disable-next-line:call-to-gc // the measure is of what is live, which only a collection tells
		runtime.GC()
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return int64(m.HeapAlloc)
}

// heldByCopy is what a Writer holds once it has copied img, until Close.
func heldByCopy(t *testing.T, img []byte, opts ...erofs.CopyOpt) int64 {
	t.Helper()

	src := openWithDevices(t, img)

	before := liveHeap()
	w := erofs.Create(&testBuffer{})

	if err := w.CopyFrom(src, opts...); err != nil {
		t.Fatal(err)
	}

	held := liveHeap() - before

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return held
}

// TestContractCopyHoldsOnceTheAttributesItsFilesShare: a copy of a tree
// whose files all carry the same attributes, as the files of a labelled tree
// do, holds them once and not once a file. Twenty thousand files with a
// label of 216 bytes cost the copy under sixteen bytes a file more than the
// same files without one.
//
//nolint:paralleltest // measures the process's live heap; a parallel test's would count in it
func TestContractCopyHoldsOnceTheAttributesItsFilesShare(t *testing.T) {
	const files = 20000

	label := map[string]string{
		"security.selinux": strings.Repeat("system_u:object_r:usr_t:s0 ", 8),
		"user.origin":      "the same on every file",
	}

	plain := attributedImage(t, files, func(int) map[string]string { return nil })
	labelled := attributedImage(t, files, func(int) map[string]string { return label })

	for route, opts := range copyRoutes() {
		t.Run(route, func(t *testing.T) {
			extra := heldByCopy(t, labelled, opts...) - heldByCopy(t, plain, opts...)
			if extra > files*16 {
				t.Errorf("the attributes cost the copy %d bytes, %d a file; want them held once, under 16 a file",
					extra, extra/files)
			}
		})
	}
}

// TestContractSetxattrOnOneOfTheFilesACopyGaveTheSameAttributes: files a
// copy found with the same attributes are apart afterwards: one set on a
// file, a new name or a new value, is on that file and on no other.
func TestContractSetxattrOnOneOfTheFilesACopyGaveTheSameAttributes(t *testing.T) {
	t.Parallel()

	img := attributedImage(t, 3, func(int) map[string]string {
		return map[string]string{"user.k": "v", "user.kept": "kept"}
	})

	for route, opts := range copyRoutes() {
		t.Run(route, func(t *testing.T) {
			t.Parallel()

			src := openWithDevices(t, img)

			out := &testBuffer{}
			w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

			if err := errors.Join(
				w.CopyFrom(src, opts...),
				w.Setxattr("/f00000", "user.k", "changed"),
				// One name that sorts before the copy's, one after.
				w.Setxattr("/f00001", "user.a", "first"),
				w.Setxattr("/f00001", "user.z", "last"),
			); err != nil {
				t.Fatal(err)
			}

			want := map[string]map[string]string{
				"f00000": {"user.k": "changed", "user.kept": "kept"},
				"f00001": {"user.a": "first", "user.k": "v", "user.kept": "kept", "user.z": "last"},
				"f00002": {"user.k": "v", "user.kept": "kept"},
			}

			// Before Close, as Stat reads an entry back.
			for name, attrs := range want {
				info, err := w.Stat(name)
				if err != nil {
					t.Fatal(err)
				}

				if st, ok := info.Sys().(*erofs.Stat); !ok || !maps.Equal(st.Xattrs, attrs) {
					t.Errorf("Stat(%s) before Close: Sys() is %v; want the attributes %v", name, info.Sys(), attrs)
				}

				by, ok := info.(interface {
					GetAllXattr() map[string]string
					GetXattr(name string) (string, bool)
				})
				if !ok {
					t.Fatalf("Stat(%s) before Close lacks the xattr accessors", name)
				}

				if all := by.GetAllXattr(); !maps.Equal(all, attrs) {
					t.Errorf("Stat(%s) before Close: GetAllXattr is %v; want %v", name, all, attrs)
				}

				for attr, value := range attrs {
					if got, has := by.GetXattr(attr); !has || got != value {
						t.Errorf("Stat(%s) before Close: GetXattr(%s) is %q, %v; want %q", name, attr, got, has, value)
					}
				}

				if got, has := by.GetXattr("user.none"); has {
					t.Errorf("Stat(%s) before Close: GetXattr of a name it lacks is %q", name, got)
				}
			}

			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			ref, err := erofsspec.Parse(out.Bytes())
			if err != nil {
				t.Fatal(err)
			}

			for name, attrs := range want {
				in, err := ref.Resolve(name, 0)
				if err != nil {
					t.Fatal(err)
				}

				if got := xattrsOf(in); !maps.Equal(got, attrs) {
					t.Errorf("%s has %v in the image; want %v", name, got, attrs)
				}
			}
		})
	}
}

// TestContractCopyWritesAttributesInTheOrderOfTheirNames: the attributes of
// a file are in the image in the order of their names, whatever order its
// source had them in: a map has none, and the image of a tree is the same
// bytes each time it is written.
func TestContractCopyWritesAttributesInTheOrderOfTheirNames(t *testing.T) {
	t.Parallel()

	attrs := map[string]string{}
	for _, name := range []string{"user.e", "trusted.b", "user.a", "security.d", "user.c", "trusted.f"} {
		attrs[name] = "the value of " + name
	}

	// The format's name indexes: user 1, trusted 4, security 6.
	unordered := corpus.Assemble(corpus.Layout{BlockBits: 12, Entries: []corpus.AEntry{{
		Name: "f00000", Mode: 0o100644, Nlink: 1,
		Inline: []corpus.AXattr{
			{Index: 1, Suffix: "e", Value: []byte("the value of user.e")},
			{Index: 4, Suffix: "b", Value: []byte("the value of trusted.b")},
			{Index: 1, Suffix: "a", Value: []byte("the value of user.a")},
			{Index: 6, Suffix: "d", Value: []byte("the value of security.d")},
			{Index: 1, Suffix: "c", Value: []byte("the value of user.c")},
			{Index: 4, Suffix: "f", Value: []byte("the value of trusted.f")},
		},
	}}})

	for source, img := range map[string][]byte{
		"an image in their order":   attributedImage(t, 1, func(int) map[string]string { return attrs }),
		"an image in another order": unordered,
	} {
		for route, opts := range copyRoutes() {
			t.Run(source+", "+route, func(t *testing.T) {
				t.Parallel()

				src := openWithDevices(t, img)

				out := &testBuffer{}
				w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

				if err := errors.Join(w.CopyFrom(src, opts...), w.Close()); err != nil {
					t.Fatal(err)
				}

				ref, err := erofsspec.Parse(out.Bytes())
				if err != nil {
					t.Fatal(err)
				}

				in, err := ref.Resolve("f00000", 0)
				if err != nil {
					t.Fatal(err)
				}

				var got []string
				for _, x := range in.Xattrs {
					got = append(got, x.Name()+"="+string(x.Value))
				}

				var want []string
				for _, name := range []string{"security.d", "trusted.b", "trusted.f", "user.a", "user.c", "user.e"} {
					want = append(want, name+"="+attrs[name])
				}

				if strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf(
						"the image has the attributes\n%s\nwant\n%s",
						strings.Join(got, "\n"),
						strings.Join(want, "\n"),
					)
				}
			})
		}
	}
}

// TestContractCopyRefusesAnAttributeListedTwiceWhereverItIs: an inode that
// lists one attribute name twice is refused by a copy, ErrCorrupt, as it is
// by Stat, with other names between the two as well as side by side.
func TestContractCopyRefusesAnAttributeListedTwiceWhereverItIs(t *testing.T) {
	t.Parallel()

	one := func(suffix string) corpus.AXattr {
		return corpus.AXattr{Index: 1, Suffix: suffix, Value: []byte("a value")}
	}

	for layout, inline := range map[string][]corpus.AXattr{
		"side by side": {one("a"), one("a"), one("b")},
		"apart":        {one("b"), one("a"), one("c"), one("b")},
	} {
		img := corpus.Assemble(corpus.Layout{BlockBits: 12, Entries: []corpus.AEntry{{
			Name: "f", Mode: 0o100644, Nlink: 1, Inline: inline,
		}}})

		for route, opts := range copyRoutes() {
			t.Run(layout+", "+route, func(t *testing.T) {
				t.Parallel()

				src := openWithDevices(t, img)

				err := erofs.Create(&testBuffer{}).CopyFrom(src, opts...)
				if !errors.Is(err, erofs.ErrCorrupt) {
					t.Errorf("CopyFrom: %v; want ErrCorrupt", err)
				}
			})
		}
	}
}

// TestContractAFileRemovedAfterACopyTakesItsAttributesWithIt: what a Writer
// holds is what its tree has. Two thousand files that each have four
// kilobytes of attribute of their own are eight megabytes held after the
// copy, and under a tenth of that once the files are removed.
//
//nolint:paralleltest // measures the process's live heap; a parallel test's would count in it
func TestContractAFileRemovedAfterACopyTakesItsAttributesWithIt(t *testing.T) {
	const files = 2000

	img := attributedImage(t, files, func(i int) map[string]string {
		return map[string]string{"user.own": strings.Repeat(fmt.Sprintf("%07d ", i), 500)}
	})

	for route, opts := range copyRoutes() {
		t.Run(route, func(t *testing.T) {
			src := openWithDevices(t, img)

			before := liveHeap()
			w := erofs.Create(&testBuffer{})

			if err := w.CopyFrom(src, opts...); err != nil {
				t.Fatal(err)
			}

			held := liveHeap() - before
			if held < files*4000 {
				t.Fatalf("the copy holds %d bytes; the test wants the attributes held, %d at least", held, files*4000)
			}

			for i := range files {
				if err := w.Remove(fmt.Sprintf("/f%05d", i)); err != nil {
					t.Fatal(err)
				}
			}

			if left := liveHeap() - before; left > held/10 {
				t.Errorf(
					"the Writer holds %d bytes of the %d it held before its files were removed; want under a tenth",
					left,
					held,
				)
			}

			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestContractCopyOfFilesWithAttributesOfTheirOwn: files that have one
// attribute in common and one of their own are copied each with its own,
// among files that have the common one alone.
func TestContractCopyOfFilesWithAttributesOfTheirOwn(t *testing.T) {
	t.Parallel()

	const files = 40

	// Every other file has the label alone; the rest add one of their own.
	attrs := func(i int) map[string]string {
		m := map[string]string{"security.selinux": "system_u:object_r:usr_t:s0"}
		if i%2 == 1 {
			m["user.id"] = fmt.Sprintf("file %d", i)
		}

		return m
	}

	img := attributedImage(t, files, attrs)

	for route, opts := range copyRoutes() {
		t.Run(route, func(t *testing.T) {
			t.Parallel()

			src := openWithDevices(t, img)

			out := &testBuffer{}
			w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

			if err := errors.Join(w.CopyFrom(src, opts...), w.Close()); err != nil {
				t.Fatal(err)
			}

			copied, err := erofs.Open(bytes.NewReader(out.Bytes()))
			if err != nil {
				t.Fatal(err)
			}

			for i := range files {
				name := fmt.Sprintf("f%05d", i)

				info, err := fs.Stat(copied, name)
				if err != nil {
					t.Fatal(err)
				}

				if st, ok := info.Sys().(*erofs.Stat); !ok || !maps.Equal(st.Xattrs, attrs(i)) {
					t.Errorf("%s: Sys() is %v; want the attributes %v", name, info.Sys(), attrs(i))
				}
			}
		})
	}
}
