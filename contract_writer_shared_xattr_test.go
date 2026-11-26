package erofs_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"strings"
	"testing"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
	"github.com/forkcloser/go-erofs/internal/erofsspec"
)

// attributed writes an image of the files of tree, each empty and with its
// attributes, in the order given.
func attributed(t *testing.T, order []string, tree map[string]map[string]string) []byte {
	t.Helper()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	for _, name := range order {
		writeFile(t, w, "/"+name, nil)

		for attr, value := range tree[name] {
			if err := w.Setxattr("/"+name, attr, value); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	return out.Bytes()
}

// sharedOf reads img by the reference: each file's attributes, and, of
// them, the ones its inode names in the shared area, with their place there.
func sharedOf(
	t *testing.T,
	img []byte,
	names []string,
) (all map[string]map[string]string, shared map[string]map[string]uint32) {
	t.Helper()

	ref, err := erofsspec.Parse(img)
	if err != nil {
		t.Fatal(err)
	}

	all, shared = map[string]map[string]string{}, map[string]map[string]uint32{}

	for _, name := range names {
		in, err := ref.Resolve(name, 0)
		if err != nil {
			t.Fatal(err)
		}

		all[name], shared[name] = map[string]string{}, map[string]uint32{}

		for _, x := range in.Xattrs {
			if x.Unresolved {
				t.Fatalf("%s: an attribute of index %d the reference cannot name", name, x.Index)
			}

			all[name][x.Name()] = string(x.Value)

			if x.Shared {
				shared[name][x.Name()] = x.SharedID
			}
		}
	}

	return all, shared
}

// TestContractWriterSharesAnAttributeMoreThanTwoFilesCarry: an attribute,
// name and value, that more than two files carry is stored once, in the
// image's shared area, and each of them names it there, as mkfs.erofs
// stores it by default. One that two carry, or one, is stored with each.
// Every file reads its own, by the reference and by the package, and
// fsck.erofs passes the image.
func TestContractWriterSharesAnAttributeMoreThanTwoFilesCarry(t *testing.T) {
	t.Parallel()

	const label = "system_u:object_r:usr_t:s0"

	tree := map[string]map[string]string{
		"f0": {"security.selinux": label, "user.three": "v", "user.own": "f0's"},
		"f1": {"security.selinux": label, "user.three": "v", "user.k": "a"},
		"f2": {"security.selinux": label, "user.three": "v", "user.k": "a"},
		"f3": {"security.selinux": label, "user.two": "w", "user.k": "a"},
		"f4": {"security.selinux": label, "user.two": "w", "user.k": "b"},
		"f5": {"user.k": "b"},
		"f6": {"user.k": "b"},
	}
	names := []string{"f0", "f1", "f2", "f3", "f4", "f5", "f6"}

	img := attributed(t, names, tree)
	all, shared := sharedOf(t, img, names)

	fsys := openWithDevices(t, img)

	for _, name := range names {
		if !maps.Equal(all[name], tree[name]) {
			t.Errorf("%s has %v by the reference; want %v", name, all[name], tree[name])
		}

		info, err := fs.Stat(fsys, name)
		if err != nil {
			t.Fatal(err)
		}

		if st, ok := info.Sys().(*erofs.Stat); !ok || !maps.Equal(st.Xattrs, tree[name]) {
			t.Errorf("%s: Sys() is %v; want the attributes %v", name, info.Sys(), tree[name])
		}

		for attr := range tree[name] {
			_, is := shared[name][attr]

			// Five carry the label, three user.three, three each value of user.k.
			want := attr == "security.selinux" || attr == "user.three" || attr == "user.k"
			if is != want {
				t.Errorf("%s: %s in the shared area: %v; want %v", name, attr, is, want)
			}
		}
	}

	if shared["f0"]["security.selinux"] != shared["f4"]["security.selinux"] {
		t.Errorf(
			"the label is at %d for f0 and %d for f4; want one place",
			shared["f0"]["security.selinux"],
			shared["f4"]["security.selinux"],
		)
	}

	if shared["f1"]["user.k"] == shared["f5"]["user.k"] {
		t.Errorf("user.k=a and user.k=b are both at %d; want a place for each value", shared["f1"]["user.k"])
	}

	fsckUnlessPlatformLimit(t, corpus.Image{Name: "shared attributes", Img: img})
}

// TestContractWriterImageIsTheSameWhateverOrderItsFilesCame: the shared
// area is laid out in the order of the attributes' names and values, not in
// the order the files were written: the same tree gives the same bytes.
func TestContractWriterImageIsTheSameWhateverOrderItsFilesCame(t *testing.T) {
	t.Parallel()

	tree := map[string]map[string]string{}
	for i := range 12 {
		tree[fmt.Sprintf("f%02d", i)] = map[string]string{
			"user.b":           fmt.Sprintf("one of three, %d", i%3),
			"security.selinux": fmt.Sprintf("one of two, %d", i%2),
			"user.a":           "the same on all",
		}
	}

	forward := make([]string, 0, len(tree))
	for i := range 12 {
		forward = append(forward, fmt.Sprintf("f%02d", i))
	}

	backward := make([]string, len(forward))
	for i, name := range forward {
		backward[len(forward)-1-i] = name
	}

	if !bytes.Equal(attributed(t, forward, tree), attributed(t, backward, tree)) {
		t.Error("the same files written in another order are another image")
	}
}

// TestContractWriterSharesAtMost255AttributesOfAFile: an inode names at
// most 255 attributes in the shared area, its count of them being one byte.
// Four files that carry the same 300 name 255 there and hold 45 themselves,
// and every one of the 300 reads right, in a shared area of several blocks.
func TestContractWriterSharesAtMost255AttributesOfAFile(t *testing.T) {
	t.Parallel()

	attrs := map[string]string{}
	for i := range 300 {
		attrs[fmt.Sprintf("user.a%03d", i)] = strings.Repeat(fmt.Sprintf("value %03d ", i), 4)
	}

	names := []string{"f0", "f1", "f2", "f3"}
	tree := map[string]map[string]string{"f0": attrs, "f1": attrs, "f2": attrs, "f3": attrs}

	img := attributed(t, names, tree)
	all, shared := sharedOf(t, img, names)

	fsys := openWithDevices(t, img)

	for _, name := range names {
		if !maps.Equal(all[name], attrs) {
			t.Errorf("%s has %d attributes by the reference; want the 300", name, len(all[name]))
		}

		if len(shared[name]) != 255 {
			t.Errorf("%s names %d attributes in the shared area; want 255", name, len(shared[name]))
		}

		info, err := fs.Stat(fsys, name)
		if err != nil {
			t.Fatal(err)
		}

		if st, ok := info.Sys().(*erofs.Stat); !ok || !maps.Equal(st.Xattrs, attrs) {
			t.Errorf("%s: Sys() has not the 300 attributes", name)
		}
	}

	fsckUnlessPlatformLimit(t, corpus.Image{Name: "300 shared attributes", Img: img})
}

// TestContractWriterCountsAFileOnceWhateverItsNames: what is counted is the
// files that carry an attribute, not their names: a file of three names
// and one other are two, and their attribute stays with each.
func TestContractWriterCountsAFileOnceWhateverItsNames(t *testing.T) {
	t.Parallel()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	writeFile(t, w, "/a", nil)
	writeFile(t, w, "/other", nil)

	if err := errors.Join(
		w.Link("/a", "/b"),
		w.Link("/a", "/c"),
		w.Setxattr("/a", "user.k", "v"),
		w.Setxattr("/other", "user.k", "v"),
		w.Close(),
	); err != nil {
		t.Fatal(err)
	}

	all, shared := sharedOf(t, out.Bytes(), []string{"a", "b", "c", "other"})

	for name, attrs := range all {
		if attrs["user.k"] != "v" || len(shared[name]) != 0 {
			t.Errorf(
				"%s: user.k is %q, in the shared area: %v; want it with the file",
				name,
				attrs["user.k"],
				len(shared[name]) != 0,
			)
		}
	}
}

// TestContractWriterStoresOnceAnAttributeManyFilesCarry: two thousand files
// with one label of a kilobyte are an image of under 200 bytes a file,
// where the label with each is over a kilobyte a file.
func TestContractWriterStoresOnceAnAttributeManyFilesCarry(t *testing.T) {
	t.Parallel()

	const files = 2000

	label := map[string]string{"security.selinux": strings.Repeat("l", 1000)}
	tree := map[string]map[string]string{}
	names := make([]string, 0, files)

	for i := range files {
		name := fmt.Sprintf("f%05d", i)
		names = append(names, name)
		tree[name] = label
	}

	img := attributed(t, names, tree)

	if len(img) > files*200 {
		t.Errorf("the image is %d bytes, %d a file; want the label once, under 200 a file", len(img), len(img)/files)
	}

	all, _ := sharedOf(t, img, []string{"f00000", "f01999"})
	for name, attrs := range all {
		if !maps.Equal(attrs, label) {
			t.Errorf("%s has not the label", name)
		}
	}
}

// TestContractWriterSharesWhatAnInodeCannotHold: an inode's own attributes
// are sized by a count of 65535 units of four bytes. One file with 255
// attributes of 1100 bytes holds more than that, and nothing else carries
// them: they are named in the shared area all the same, as an image may
// store them, and every one reads right. With 300, 255 are named there and
// the file holds the 45 that are left. With 600 the 345 left are more than
// an inode holds, and Close refuses the image, ErrInvalid.
func TestContractWriterSharesWhatAnInodeCannotHold(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		attrs, shared int
		refused       bool
	}{
		{255, 255, false},
		{300, 255, false},
		{600, 0, true},
	} {
		t.Run(fmt.Sprintf("%d attributes", tc.attrs), func(t *testing.T) {
			t.Parallel()

			attrs := map[string]string{}
			for i := range tc.attrs {
				attrs[fmt.Sprintf("user.a%03d", i)] = strings.Repeat(fmt.Sprintf("%03d ", i), 275)
			}

			out := &testBuffer{}
			w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
			writeFile(t, w, "/f", nil)

			for attr, value := range attrs {
				if err := w.Setxattr("/f", attr, value); err != nil {
					t.Fatal(err)
				}
			}

			err := w.Close()
			if tc.refused {
				if !errors.Is(err, erofs.ErrInvalid) {
					t.Fatalf("Close: %v; want ErrInvalid", err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			all, shared := sharedOf(t, out.Bytes(), []string{"f"})
			if !maps.Equal(all["f"], attrs) || len(shared["f"]) != tc.shared {
				t.Errorf(
					"f has %d attributes by the reference, %d of them in the shared area; want its %d, and %d there",
					len(all["f"]),
					len(shared["f"]),
					tc.attrs,
					tc.shared,
				)
			}

			info, err := fs.Stat(openWithDevices(t, out.Bytes()), "f")
			if err != nil {
				t.Fatal(err)
			}

			if st, ok := info.Sys().(*erofs.Stat); !ok || !maps.Equal(st.Xattrs, attrs) {
				t.Errorf("f: Sys() has not its %d attributes", tc.attrs)
			}

			fsckUnlessPlatformLimit(t, corpus.Image{Name: "attributes one inode cannot hold", Img: out.Bytes()})
		})
	}
}

// TestContractWriterSharesFromTheFirstUnitAnInodeCannotHold: an inode's
// own attributes are held to 65535 units of four bytes, the twelve of the
// body's head counting for one. Five attributes of one file that come to
// that count exactly stay with the file; with four bytes more they are
// named in the shared area.
func TestContractWriterSharesFromTheFirstUnitAnInodeCannotHold(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		last   int
		shared int
	}{
		// Four entries of 65528 bytes and one of 24: 65534 units after the head's.
		{"65535 units", 19, 0},
		{"65536 units", 23, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			attrs := map[string]string{"user.e": strings.Repeat("e", tc.last)}
			for _, name := range []string{"user.a", "user.b", "user.c", "user.d"} {
				attrs[name] = strings.Repeat(name[5:], 65523)
			}

			out := &testBuffer{}
			w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
			writeFile(t, w, "/f", nil)

			for attr, value := range attrs {
				if err := w.Setxattr("/f", attr, value); err != nil {
					t.Fatal(err)
				}
			}

			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			all, shared := sharedOf(t, out.Bytes(), []string{"f"})
			if !maps.Equal(all["f"], attrs) || len(shared["f"]) != tc.shared {
				t.Errorf(
					"f has %d attributes by the reference, %d of them in the shared area; want its 5, and %d there",
					len(all["f"]),
					len(shared["f"]),
					tc.shared,
				)
			}

			fsckUnlessPlatformLimit(t, corpus.Image{Name: tc.name, Img: out.Bytes()})
		})
	}
}

// TestContractWriterSharesNoAttributeThatTwoFilesCarryHoweverMany: what
// two files carry stays with each of them, however many such attributes a
// tree has: fifteen hundred values, each on two files, and no file names
// one in a shared area, which the image does not have. Each on three
// files, every file names its own there.
func TestContractWriterSharesNoAttributeThatTwoFilesCarryHoweverMany(t *testing.T) {
	t.Parallel()

	const values = 1500

	for _, carriers := range []int{2, 3} {
		tree := map[string]map[string]string{}
		names := make([]string, 0, carriers*values)

		for i := range carriers * values {
			name := fmt.Sprintf("f%05d", i)
			names = append(names, name)
			tree[name] = map[string]string{"user.k": fmt.Sprintf("value %d", i/carriers)}
		}

		img := attributed(t, names, tree)
		all, shared := sharedOf(t, img, names)

		named := 0

		for _, name := range names {
			if !maps.Equal(all[name], tree[name]) {
				t.Fatalf("%s has %v; want %v", name, all[name], tree[name])
			}

			named += len(shared[name])
		}

		if want := (carriers - 2) * carriers * values; named != want {
			t.Errorf(
				"%d values on %d files each: %d files name theirs in a shared area; want %d",
				values, carriers, named, want,
			)
		}
	}
}

// TestContractWriterOrdersTheSharedAreaByNameThenValue: the shared area
// holds its attributes in the order of their names, and of their values
// under one name: the places of six attributes, three names with two
// values each, rise in that order, where the values alone would order them
// otherwise.
func TestContractWriterOrdersTheSharedAreaByNameThenValue(t *testing.T) {
	t.Parallel()

	// In the order they must lie; by value alone the order is another.
	inOrder := [][2]string{
		{"user.a", "y"},
		{"user.a", "z"},
		{"user.b", "w"},
		{"user.b", "x"},
		{"user.c", "u"},
		{"user.c", "v"},
	}

	tree := map[string]map[string]string{}

	var names []string

	// Three files carry each attribute, and nothing else.
	for i, x := range inOrder {
		for carrier := range 3 {
			name := fmt.Sprintf("f%d%d", i, carrier)
			names = append(names, name)
			tree[name] = map[string]string{x[0]: x[1]}
		}
	}

	_, shared := sharedOf(t, attributed(t, names, tree), names)

	last, lastAt := "", uint32(0)

	for i, x := range inOrder {
		at, is := shared[fmt.Sprintf("f%d0", i)][x[0]]
		if !is {
			t.Fatalf("%s=%s is not in the shared area", x[0], x[1])
		}

		if i > 0 && at <= lastAt {
			t.Errorf("%s=%s is at %d, not after %s at %d", x[0], x[1], at, last, lastAt)
		}

		last, lastAt = x[0]+"="+x[1], at
	}
}
