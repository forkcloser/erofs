package erofs_test

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/erofsspec"
)

// hiddenAndListed is an image of one file, f, whose attributes are of both
// kinds, inline and shared: those the kernel lists, and entries under every
// sort of name index it has no handler for.
func hiddenAndListed() []byte {
	return corpus.Assemble(corpus.Layout{
		BlockBits:  12,
		NoChecksum: true,
		Prefixes: []corpus.APrefix{
			{BaseIndex: 1, Infix: "long."},                // 0x80: user.long.
			{BaseIndex: 0, Infix: "erofs.fingerprint.v1"}, // 0x81: what mkfs.erofs --xattr-inode-digest writes
			{BaseIndex: 9, Infix: "nine."},                // 0x82: built on an undefined index
		},
		Shared: []corpus.AXattr{
			{Index: 6, Suffix: "shared", Value: []byte("listed")},
			{Index: 0, Suffix: "shared.whole", Value: []byte("hidden")},
		},
		Entries: []corpus.AEntry{{
			Name: "f", Mode: 0o100644, Nlink: 1, Mtime: corpus.AssembledBuildTime, Data: []byte("f\n"),
			SharedIDs: []int{0, 1},
			Inline: []corpus.AXattr{
				{Index: 1, Suffix: "k", Value: []byte("v")},
				{Index: 0, Suffix: "com.apple.provenance", Value: []byte{1, 2, 3}},
				{Index: 0, Suffix: "security.capability", Value: []byte("spelled whole")},
				{Index: 4, Suffix: "t", Value: []byte("trusted")},
				{Index: 5, Suffix: "lov", Value: []byte("lustre")},
				{Index: 7, Suffix: "seven", Value: []byte("undefined")},
				{Index: 2, Value: []byte("\x02\x00\x00\x00")},
				{Index: 0},
				{Index: 0},
				{Index: 0x80, Suffix: "name", Value: []byte("long")},
				{Index: 0x81, Value: []byte("digest")},
				{Index: 0x82, Suffix: "x", Value: []byte("hidden")},
				{Index: 0x85, Suffix: "past", Value: []byte("the table")},
				{Index: 6, Suffix: "capability", Value: []byte("real")},
			},
		}},
	})
}

// listedOfIt is what a mount of hiddenAndListed shows on f.
func listedOfIt() map[string]string {
	return map[string]string{
		"user.k":                  "v",
		"trusted.t":               "trusted",
		"system.posix_acl_access": "\x02\x00\x00\x00",
		"user.long.name":          "long",
		"security.capability":     "real",
		"security.shared":         "listed",
	}
}

// TestContractReaderListsTheAttributesTheKernelLists: an entry whose name
// index the kernel has no handler for (0, the format's lustre index 5, 7
// and up, a long prefix past the table or built on such an index) is on no
// mount: listxattr leaves it out and getxattr cannot name it (xattr.c:
// erofs_listxattr_foreach). The reader leaves it out too. It listed index 0
// with the name whole, so that an image could spell "security.capability"
// out under it, and refused the whole inode for an undefined index.
func TestContractReaderListsTheAttributesTheKernelLists(t *testing.T) {
	t.Parallel()

	img := hiddenAndListed()

	// The reference's own word on which entries the kernel lists.
	ref, err := erofsspec.Parse(img)
	if err != nil {
		t.Fatal(err)
	}

	in, err := ref.Resolve("f", 0)
	if err != nil {
		t.Fatal(err)
	}

	if got := xattrsOf(in); !maps.Equal(got, listedOfIt()) {
		t.Fatalf("the reference lists %v; the image is not what the test means", corpus.Keys(got))
	}

	if len(in.Xattrs) != 16 {
		t.Fatalf("the image holds %d entries on f; want 16, ten of them hidden", len(in.Xattrs))
	}

	fsys, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	info, err := fs.Stat(fsys, "f")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if got := info.Sys().(*erofs.Stat).Xattrs; !maps.Equal(got, listedOfIt()) {
		t.Errorf("Stat.Xattrs lists %v\nwant %v", corpus.Keys(got), corpus.Keys(listedOfIt()))
	}
}

// TestContractCopyHoldsTheAttributesTheKernelLists: a copy of an image holds
// what a mount of it shows, with its data and metadata-only: the entries no
// mount lists are not carried over, and do not fail the copy.
func TestContractCopyHoldsTheAttributesTheKernelLists(t *testing.T) {
	t.Parallel()

	img := hiddenAndListed()

	for name, opts := range map[string][]erofs.CopyOpt{"with its data": nil, "metadata-only": {erofs.MetadataOnly()}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			src, err := erofs.Open(bytes.NewReader(img))
			if err != nil {
				t.Fatal(err)
			}

			out := &testBuffer{}
			w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

			if err = errors.Join(w.CopyFrom(src, opts...), w.Close()); err != nil {
				t.Fatalf("the copy: %v", err)
			}

			ref, err := erofsspec.Parse(out.Bytes())
			if err != nil {
				t.Fatal(err)
			}

			in, err := ref.Resolve("f", 0)
			if err != nil {
				t.Fatal(err)
			}

			if len(in.Xattrs) != len(listedOfIt()) || !maps.Equal(xattrsOf(in), listedOfIt()) {
				t.Errorf("the copy stores %d entries, listing %v\nwant the %d a mount of the source shows: %v",
					len(in.Xattrs), corpus.Keys(xattrsOf(in)), len(listedOfIt()), corpus.Keys(listedOfIt()))
			}
		})
	}
}

// TestContractCopyLeavesOutASourcesHiddenAttributes: a source may hand any
// attribute name through its Sys. One the kernel would list on no mount is
// left out of the copy and fails nothing: the kernel refuses one attribute
// in one call (setxattr, EOPNOTSUPP), never a tree for it, and Setxattr is
// that call here.
func TestContractCopyLeavesOutASourcesHiddenAttributes(t *testing.T) {
	t.Parallel()

	for _, hidden := range []string{
		"btrfs.compression", "com.apple.provenance", "lustre.lov", "system.nfs4_acl",
		// An ACL name with anything after it is no ACL to the kernel.
		"system.posix_acl_accessx", "system.posix_acl_default.more",
	} {
		t.Run(hidden, func(t *testing.T) {
			t.Parallel()

			// The same on the file and on the root, which the copy takes
			// by a path of its own.
			handed := map[string]string{"user.ok": "1", hidden: "x"}
			src := fstest.MapFS{
				".": &fstest.MapFile{Mode: fs.ModeDir | 0o755, Sys: &erofs.Stat{Xattrs: handed}},
				"f": &fstest.MapFile{Data: []byte("f"), Mode: 0o644, Sys: &erofs.Stat{Xattrs: handed}},
			}

			out := &testBuffer{}
			w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

			if err := errors.Join(w.CopyFrom(src), w.Close()); err != nil {
				t.Fatalf("the copy: %v", err)
			}

			if len(handed) != 2 {
				t.Errorf("the source's own map was changed: %v", handed)
			}

			ref, err := erofsspec.Parse(out.Bytes())
			if err != nil {
				t.Fatal(err)
			}

			for _, p := range []string{"f", "."} {
				in, err := ref.Resolve(p, 0)
				if err != nil {
					t.Fatal(err)
				}

				if len(in.Xattrs) != 1 || !maps.Equal(xattrsOf(in), map[string]string{"user.ok": "1"}) {
					t.Errorf(
						"the image stores %d entries on %s, listing %v; want user.ok alone",
						len(in.Xattrs),
						p,
						corpus.Keys(xattrsOf(in)),
					)
				}
			}

			// The one call that refuses.
			if err := erofs.Create(&testBuffer{}).Setxattr("/", hidden, "x"); !errors.Is(err, erofs.ErrInvalid) {
				t.Errorf("Setxattr(%s) = %v; want ErrInvalid", hidden, err)
			}
		})
	}
}

// TestContractCopyLeavesOutAnAclNameWithASuffix: an image can hold an entry
// under an ACL's index with a name after it. The kernel lists it, prefix
// and name, and serves it to no call: an ACL is read under the empty name
// alone (xattr.c: erofs_get_acl). The reader lists it as the kernel does; a
// copy, which could write it only as it is or hidden, leaves it out.
func TestContractCopyLeavesOutAnAclNameWithASuffix(t *testing.T) {
	t.Parallel()

	img := corpus.Assemble(corpus.Layout{
		BlockBits:  12,
		NoChecksum: true,
		Entries: []corpus.AEntry{{
			Name: "f", Mode: 0o100644, Nlink: 1, Mtime: corpus.AssembledBuildTime, Data: []byte("f\n"),
			Inline: []corpus.AXattr{
				{Index: 1, Suffix: "k", Value: []byte("v")},
				{Index: 2, Suffix: "x", Value: []byte("no acl")},
				{Index: 3, Value: []byte("\x02\x00\x00\x00")},
			},
		}},
	})

	src, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	info, err := fs.Stat(src, "f")
	if err != nil {
		t.Fatal(err)
	}

	listed := map[string]string{
		"user.k":                   "v",
		"system.posix_acl_accessx": "no acl",
		"system.posix_acl_default": "\x02\x00\x00\x00",
	}

	st, ok := info.Sys().(*erofs.Stat)
	if !ok || !maps.Equal(st.Xattrs, listed) {
		t.Fatalf("the reader lists %v; want %v", info.Sys(), corpus.Keys(listed))
	}

	delete(listed, "system.posix_acl_accessx")

	for name, opts := range map[string][]erofs.CopyOpt{"with its data": nil, "metadata-only": {erofs.MetadataOnly()}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out := &testBuffer{}
			w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

			if err := errors.Join(w.CopyFrom(src, opts...), w.Close()); err != nil {
				t.Fatalf("the copy: %v", err)
			}

			ref, err := erofsspec.Parse(out.Bytes())
			if err != nil {
				t.Fatal(err)
			}

			in, err := ref.Resolve("f", 0)
			if err != nil {
				t.Fatal(err)
			}

			if len(in.Xattrs) != len(listed) || !maps.Equal(xattrsOf(in), listed) {
				t.Errorf("the copy stores %d entries, listing %v; want %v",
					len(in.Xattrs), corpus.Keys(xattrsOf(in)), corpus.Keys(listed))
			}
		})
	}
}
