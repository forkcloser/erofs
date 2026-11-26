package erofs_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/go-erofs"
)

// heldTree is a hundred directories of a hundred files of ten bytes, the
// directories' names that much longer than five bytes; names is how many
// names it has, its root with them, and paths the bytes of their paths, a
// slash before each.
func heldTree(longer int) (tree fstest.MapFS, names, paths int) {
	tree = fstest.MapFS{}

	for d := range 100 {
		dir := fmt.Sprintf("d%04d%s", d, strings.Repeat("x", longer))

		for f := range 100 {
			tree[fmt.Sprintf("%s/f%05d", dir, f)] = &fstest.MapFile{Data: []byte("ten bytes."), Mode: 0o644}
		}

		paths += 1 + len(dir) + 100*(1+len(dir)+len("/f00000"))
	}

	return tree, 100*100 + 100 + 1, paths + 1
}

// TestContractWriterHoldsUnder512BytesANameAndItsPath: what a Writer holds
// of a tree until Close is bounded by the tree's names, not by its files'
// data: under 2 KiB, and for each name under 512 bytes and the name's path,
// once, or under 768 for a file of an image copied with its data: two
// thousand files of 5000 bytes, and as many of a block of data and a hole.
// That is so of a Writer given nothing, and, for a tree of ten
// thousand files, of a source that is not an image, of an image copied
// with its data and of one copied metadata-only, with paths of 13 bytes
// and of 213. A symbolic link's target comes on top, in the size the
// allocator rounds it up to, a quarter more at most: ten thousand links to
// targets of 3900 bytes, which it rounds to 4096. So do sixteen bytes for
// each run of blocks that follow each other in a file a metadata-only copy
// leaves in its source, a file of 65536 chunks in runs of two, with a block
// map, or in a file that has holes, copied with its data: 16384 chunks,
// data and a hole in turn. An image that was copied stays open, whichever
// way it was: under 1 KiB of its own, three thousand images of one file
// each; and under 1.5 MiB of what it kept of its shared attributes, three
// hundred files that carry, three by three, 25500 attributes of the image's
// shared area, read by the caller, copied both ways and let go of.
//
//nolint:paralleltest // measures the process's live heap and allocations; a parallel test's would count in them
func TestContractWriterHoldsUnder512BytesANameAndItsPath(t *testing.T) {
	const fixed = 2 << 10

	// A thousand of them: one Writer's two kilobytes are under what the
	// rest of the process moves between two measures.
	t.Run("nothing but the root", func(t *testing.T) {
		const writers = 1000

		before := liveHeap()
		held := make([]*erofs.Writer, 0, writers)

		for range writers {
			held = append(held, erofs.Create(&sink{}))
		}

		if each := (liveHeap() - before) / writers; each > fixed+512+1 {
			t.Errorf("a Writer given nothing holds %d bytes; want %d at most", each, fixed+512+1)
		}

		for _, w := range held {
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		}
	})

	// Files of an image that are not inline, copied with their data.
	fiveThousand := fstest.MapFS{}
	for i := range 2000 {
		fiveThousand[fmt.Sprintf("f%04d", i)] = &fstest.MapFile{Data: bytes.Repeat([]byte{'x'}, 5000), Mode: 0o644}
	}

	stored := &testBuffer{}
	sw := erofs.Create(stored)

	if err := errors.Join(sw.CopyFrom(fiveThousand), sw.Close()); err != nil {
		t.Fatal(err)
	}

	for name, source := range map[string]func() fs.FS{
		"files of 5000 bytes of an image, with their data":      func() fs.FS { return openWithDevices(t, stored.Bytes()) },
		"files of data and a hole of an image, with their data": func() fs.FS { return holedFiles(t, 2000) },
	} {
		t.Run(name, func(t *testing.T) {
			const files = 2000

			from := source()

			before := liveHeap()
			w := erofs.Create(&sink{})

			if err := w.CopyFrom(from); err != nil {
				t.Fatal(err)
			}

			// A slash and five bytes of name each, the root's one; two runs a
			// file at most, sixteen bytes each and their quarter.
			held := liveHeap() - before
			if most := int64(fixed + 512 + files*768 + 1 + files*6 + files*2*16*5/4); held > most {
				t.Errorf(
					"the Writer holds %d bytes for %d files, %d each; want %d at most",
					held,
					files,
					held/files,
					most,
				)
			}

			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}

	// What an image that was copied holds, of its own and of its shared
	// attributes: the Writer keeps it open, whichever way it was copied.
	const image = 1<<10 + 3<<19

	// Three hundred files carry, three by three, the 25500 attributes of the
	// image's shared area, more than the image keeps. The caller reads them
	// all, copies the image and lets go of it.
	for way, opts := range map[string][]erofs.CopyOpt{
		"with its data": nil,
		"metadata-only": {erofs.MetadataOnly()},
	} {
		t.Run("an image whose files share their attributes, "+way+", let go of", func(t *testing.T) {
			const (
				groups = 100
				attrs  = 255
			)

			made := &testBuffer{}
			mw := erofs.Create(made)
			paths := 1

			for g := range groups {
				for f := range 3 {
					name := fmt.Sprintf("/g%03df%d", g, f)
					paths += len(name)

					writeFile(t, mw, name, []byte("ten bytes."))

					for a := range attrs {
						if err := mw.Setxattr(name, fmt.Sprintf("user.g%03da%03d", g, a), "a value"); err != nil {
							t.Fatal(err)
						}
					}
				}
			}

			if err := mw.Close(); err != nil {
				t.Fatal(err)
			}

			before := liveHeap()
			w := erofs.Create(&sink{})

			// In a function of its own: the image is in no variable of
			// this one once it returns.
			func() {
				from := openWithDevices(t, made.Bytes())

				if err := fs.WalkDir(from, ".", func(name string, _ fs.DirEntry, err error) error {
					if err != nil {
						return err
					}

					_, err = fs.Stat(from, name)

					return err
				}); err != nil {
					t.Fatal(err)
				}

				if err := w.CopyFrom(from, opts...); err != nil {
					t.Fatal(err)
				}
			}()

			// A list for each group, 32 bytes an attribute and the
			// allocator's quarter; each name of 13 bytes once, held in 16;
			// the value once.
			held := liveHeap() - before
			most := int64(fixed + (3*groups+1)*768 + paths + groups*attrs*32*5/4 + groups*attrs*16 + 8 + image)

			if held > most {
				t.Errorf(
					"the Writer holds %d bytes for %d files of %d attributes that three share; want %d at most",
					held,
					3*groups,
					attrs,
					most,
				)
			}

			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}

	// An image for each file, each let go of once copied: the Writer keeps
	// them all.
	t.Run("three thousand images of one file, metadata-only, let go of", func(t *testing.T) {
		const images = 3000

		// The images' bytes are the caller's, and made before the measure.
		sources := make([][]byte, 0, images)
		paths := 1

		for i := range images {
			made := &testBuffer{}
			mw := erofs.Create(made)
			name := fmt.Sprintf("/f%04d", i)
			paths += len(name)

			writeFile(t, mw, name, []byte("ten bytes."))

			if err := mw.Close(); err != nil {
				t.Fatal(err)
			}

			sources = append(sources, made.Bytes())
		}

		before := liveHeap()
		w := erofs.Create(&sink{})

		for _, source := range sources {
			if err := w.CopyFrom(openWithDevices(t, source), erofs.MetadataOnly()); err != nil {
				t.Fatal(err)
			}
		}

		held := liveHeap() - before

		runtime.KeepAlive(sources)

		most := int64(fixed + (images+1)*512 + paths + images*(1<<10))

		if held > most {
			t.Errorf("the Writer holds %d bytes for %d images of one file; want %d at most", held, images, most)
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("links to targets of 3900 bytes", func(t *testing.T) {
		const (
			links  = 10000
			target = 3900
		)

		tail := strings.Repeat("t", target-6)
		paths := 1

		before := liveHeap()
		w := erofs.Create(&sink{})

		for i := range links {
			name := fmt.Sprintf("/s%05d", i)
			paths += len(name)

			if err := w.Symlink(fmt.Sprintf("%05d/%s", i, tail), name); err != nil {
				t.Fatal(err)
			}
		}

		held := liveHeap() - before
		if most := int64(fixed + (links+1)*512 + paths + links*target*5/4); held > most {
			t.Errorf(
				"the Writer holds %d bytes for %d links to targets of %d bytes; want %d at most",
				held,
				links,
				target,
				most,
			)
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a file of 32768 runs left in its source", func(t *testing.T) {
		const chunks = 1 << 16

		// Pairs of chunks that follow each other, a block between two pairs.
		starts := make([]int, 0, chunks)
		for i := range chunks {
			starts = append(starts, i/2%8*3+i%2)
		}

		img, _ := laidChunkedImage(0, 0, starts)
		fsys := openWithDevices(t, img)

		before := liveHeap()
		w := erofs.Create(&sink{})

		if err := w.CopyFrom(fsys, erofs.MetadataOnly()); err != nil {
			t.Fatal(err)
		}

		held := liveHeap() - before
		if most := int64(fixed + 2*512 + len("/") + len("/f") + chunks/2*16*5/4); held > most {
			t.Errorf(
				"the Writer holds %d bytes for a file of %d runs left in its source; want %d at most",
				held,
				chunks/2,
				most,
			)
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a file of 16384 runs, data and a hole in turn, copied with its data", func(t *testing.T) {
		const chunks = 1 << 14

		starts := make([]int, 0, chunks)
		for i := range chunks {
			if i%2 == 0 {
				starts = append(starts, i/2%16)
			} else {
				starts = append(starts, -1)
			}
		}

		img, _ := laidChunkedImage(0, 0, starts)
		fsys := openWithDevices(t, img)

		before := liveHeap()
		w := erofs.Create(&sink{})

		if err := w.CopyFrom(fsys); err != nil {
			t.Fatal(err)
		}

		held := liveHeap() - before
		if most := int64(fixed + 2*512 + len("/") + len("/f") + chunks*16*5/4); held > most {
			t.Errorf(
				"the Writer holds %d bytes for a file of %d runs, half of them holes; want %d at most",
				held,
				chunks,
				most,
			)
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	})

	for _, longer := range []int{0, 200} {
		tree, names, paths := heldTree(longer)

		made := &testBuffer{}
		mw := erofs.Create(made)

		if err := mw.CopyFrom(tree); err != nil {
			t.Fatal(err)
		}

		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}

		for route, source := range map[string]func() (fs.FS, []erofs.CopyOpt){
			"a source that is not an image": func() (fs.FS, []erofs.CopyOpt) { return tree, nil },
			"an image, with its data": func() (fs.FS, []erofs.CopyOpt) {
				return openWithDevices(t, made.Bytes()), nil
			},
			"an image, metadata-only": func() (fs.FS, []erofs.CopyOpt) {
				return openWithDevices(t, made.Bytes()), []erofs.CopyOpt{erofs.MetadataOnly()}
			},
		} {
			t.Run(fmt.Sprintf("%s, paths %d bytes longer", route, longer), func(t *testing.T) {
				from, opts := source()

				before := liveHeap()
				w := erofs.Create(&sink{})

				if err := w.CopyFrom(from, opts...); err != nil {
					t.Fatal(err)
				}

				held := liveHeap() - before
				if most := int64(fixed + names*512 + paths); held > most {
					t.Errorf(
						"the Writer holds %d bytes for %d names of %d bytes of path in all, %d a name over its path; want under 512",
						held,
						names,
						paths,
						(held-int64(paths))/int64(names),
					)
				}

				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// TestContractCloseAllocatesByTheNamesAndTheAttributes: a budget for what
// Close allocates while it writes the image, by what the tree has: 1 MiB,
// 640 bytes for each name, 16 for each attribute of each entry, 256 for
// each attribute that goes to the shared xattr area, and 256 for each extra
// device. The documentation promises none of it: a count of allocations
// takes in the allocator's rounding and the pools of the images read. It
// is here so that a list made twice, a string copied to be written, or an
// entry allocated beside its slab shows as one of these trees going over:
// no name but the root, with blocks of 4 KiB and of 64 KiB; twenty
// thousand devices for one name; directories with nothing in them; files of
// ten names; links to targets of three thousand bytes; a file of 65536
// chunks copied with its data; a hundred attributes on every file;
// attributes that three files share and no more; and files that cannot
// hold their own attributes.
//
//nolint:paralleltest // measures the process's allocations against a budget; a parallel test's would count against it
func TestContractCloseAllocatesByTheNamesAndTheAttributes(t *testing.T) {
	const (
		fixed     = 1 << 20
		perName   = 640
		perAttr   = 16
		perShared = 256
		perDevice = 256
	)

	file := func(w *erofs.Writer, name string) {
		writeFile(t, w, name, []byte("ten bytes."))
	}

	chunked, _ := laidChunkedImage(0, 0, repeated(1<<16))

	one := &testBuffer{}
	oneFile := erofs.Create(one)
	writeFile(t, oneFile, "/f", bytes.Repeat([]byte{'x'}, 4096))

	if err := oneFile.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		opts []erofs.CreateOpt
		// devices is how many extra devices the image has.
		devices int
		// fill gives the Writer its tree and says how many names that is,
		// the root with them, how many attributes its entries carry in all,
		// and how many of those go to the shared area.
		fill func(w *erofs.Writer) (names, carried, shared int)
	}{
		{name: "the root alone", fill: func(*erofs.Writer) (int, int, int) { return 1, 0, 0 }},
		{
			name:    "one file copied metadata-only from twenty thousand sources",
			devices: 20000,
			fill: func(w *erofs.Writer) (int, int, int) {
				for range 20000 {
					if err := w.CopyFrom(openWithDevices(t, one.Bytes()), erofs.MetadataOnly(), erofs.Merge()); err != nil {
						t.Fatal(err)
					}
				}

				return 2, 0, 0
			},
		},
		{
			name: "the root alone, blocks of 64 KiB",
			opts: []erofs.CreateOpt{erofs.WithBlockSize(1 << 16)},
			fill: func(*erofs.Writer) (int, int, int) { return 1, 0, 0 },
		},
		{name: "ten thousand links to targets of 3000 bytes", fill: func(w *erofs.Writer) (int, int, int) {
			tail := strings.Repeat("t", 2994)

			for i := range 10000 {
				if err := w.Symlink(fmt.Sprintf("%05d/%s", i, tail), fmt.Sprintf("/s%05d", i)); err != nil {
					t.Fatal(err)
				}
			}

			return 10001, 0, 0
		}},
		{name: "a file of 65536 chunks copied with its data", fill: func(w *erofs.Writer) (int, int, int) {
			if err := w.CopyFrom(openWithDevices(t, chunked)); err != nil {
				t.Fatal(err)
			}

			return 2, 0, 0
		}},
		{name: "nine files", fill: func(w *erofs.Writer) (int, int, int) {
			for f := range 9 {
				file(w, fmt.Sprintf("/f%d", f))
			}

			return 10, 0, 0
		}},
		{name: "ten thousand empty directories", fill: func(w *erofs.Writer) (int, int, int) {
			for d := range 10000 {
				if err := w.Mkdir(fmt.Sprintf("/d%05d", d), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			return 10001, 0, 0
		}},
		{name: "a thousand files of ten names", fill: func(w *erofs.Writer) (int, int, int) {
			for f := range 1000 {
				file(w, fmt.Sprintf("/f%04d-0", f))

				for n := 1; n < 10; n++ {
					if err := w.Link(fmt.Sprintf("/f%04d-0", f), fmt.Sprintf("/f%04d-%d", f, n)); err != nil {
						t.Fatal(err)
					}
				}
			}

			return 10001, 0, 0
		}},
		{name: "a hundred attributes, the same on two thousand files", fill: func(w *erofs.Writer) (int, int, int) {
			for f := range 2000 {
				file(w, fmt.Sprintf("/f%04d", f))

				for a := range 100 {
					if err := w.Setxattr(fmt.Sprintf("/f%04d", f), fmt.Sprintf("user.a%03d", a), "the same on every file"); err != nil {
						t.Fatal(err)
					}
				}
			}

			return 2001, 2000 * 100, 100
		}},
		{name: "twenty attributes, each value on three files", fill: func(w *erofs.Writer) (int, int, int) {
			for f := range 9999 {
				file(w, fmt.Sprintf("/f%04d", f))

				for a := range 20 {
					if err := w.Setxattr(fmt.Sprintf("/f%04d", f), fmt.Sprintf("user.a%03d", a), fmt.Sprintf("v%06d", f/3)); err != nil {
						t.Fatal(err)
					}
				}
			}

			return 10000, 9999 * 20, 3333 * 20
		}},
		{name: "ten files that cannot hold their own attributes", fill: func(w *erofs.Writer) (int, int, int) {
			for f := range 1000 {
				file(w, fmt.Sprintf("/f%04d", f))
			}

			// 300 attributes of 1100 bytes are over the 262140 bytes an
			// inode's body holds.
			for f := range 10 {
				file(w, fmt.Sprintf("/full%d", f))

				for a := range 300 {
					value := fmt.Sprintf("%d-%03d-", f, a) + strings.Repeat("v", 1094)
					if err := w.Setxattr(fmt.Sprintf("/full%d", f), fmt.Sprintf("user.a%03d", a), value); err != nil {
						t.Fatal(err)
					}
				}
			}

			return 1011, 10 * 300, 10 * 300
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := erofs.Create(&sink{}, tc.opts...)
			names, carried, shared := tc.fill(w)

			allocated := allocatedSoFar()

			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			allocated = allocatedSoFar() - allocated

			most := uint64(fixed + names*perName + carried*perAttr + shared*perShared + tc.devices*perDevice)
			if allocated > most {
				t.Errorf(
					"Close allocated %d bytes for %d names, %d attributes carried, %d in the shared area and %d devices; want %d at most",
					allocated,
					names,
					carried,
					shared,
					tc.devices,
					most,
				)
			}
		})
	}
}
