package corpus

import (
	"archive/tar"
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/forkcloser/erofs/internal/erofsspec"
)

// Check holds the reference's view of an image to the tree it was built
// from: every entry of the walk is in the tree with its type, permission
// bits, ids, mtime, link count, content, target, device number and xattrs,
// every node of the tree is in the walk, the directories pass fsck's rules,
// and the superblock carries what the options asked for. nsec says whether
// the builder stored nanoseconds (the Darwin build of erofs-utils does not).
func Check(tb testing.TB, im Image, ref *erofsspec.Image, nodes []Node, nsec bool) {
	tb.Helper()

	checkSuper(tb, im, ref)
	checkRoot(tb, ref)

	want := ByPath(nodes)
	seen := map[string]bool{}
	nids := map[uint64]int{}

	err := ref.Walk(func(e erofsspec.Entry) error {
		seen[e.Path] = true
		nids[e.Inode.Nid]++

		n, ok := want[e.Path]
		if !ok {
			tb.Errorf("%s: not in the tree", e.Path)

			return nil
		}

		checkEntry(tb, ref, e, n, want, nsec)

		return nil
	})
	if err != nil {
		tb.Fatalf("Walk: %v", err)
	}

	for path := range want {
		if !seen[path] {
			tb.Errorf("%s: missing from the image", path)
		}
	}

	// inos counts inodes, which hardlinks share (dump.erofs -s, "inode count"),
	// and the packed inode, which no path reaches.
	got := uint64(len(nids)) + 1
	if ref.Super.FeatureIncompat&erofsspec.FeatureIncompatFragments != 0 && ref.Super.PackedNid != 0 {
		got++
	}

	if ref.Super.Inos != 0 && got != ref.Super.Inos {
		tb.Errorf("inode count %d, superblock says %d", got, ref.Super.Inos)
	}
}

// checkRoot: the root directory is root's, RootMode, at the build time,
// whichever tool wrote it.
func checkRoot(tb testing.TB, ref *erofsspec.Image) {
	tb.Helper()

	root, err := ref.Inode(ref.Super.RootNid)
	if err != nil {
		tb.Errorf("root inode: %v", err)

		return
	}

	if !root.IsDir() || root.Mode&^erofsspec.ModeTypeMask != RootMode || root.UID != 0 || root.GID != 0 {
		tb.Errorf("root: mode %#o uid %d gid %d, want a %#o directory of 0:0", root.Mode, root.UID, root.GID, RootMode)
	}

	if root.Mtime != BuildTime {
		tb.Errorf("root: mtime %d, want the build time %d", root.Mtime, BuildTime)
	}
}

// checkSuper: what the options put in the superblock, one check per
// option, and the chunked-file feature where the options chunk.
func checkSuper(tb testing.TB, im Image, ref *erofsspec.Image) {
	tb.Helper()

	for _, a := range im.Args {
		if msg := optionCheck(im, ref, a); msg != "" {
			tb.Error(msg)
		}
	}

	chunked := strings.Contains(im.Name, "chunk") || strings.Contains(im.Name, "blob")
	if len(im.Args) > 0 && chunked != (ref.Super.FeatureIncompat&erofsspec.FeatureIncompatChunkedFile != 0) {
		tb.Errorf("chunked-file feature %#x for %s", ref.Super.FeatureIncompat, im.Name)
	}
}

// optionCheck is one option's mark on the superblock, or "" when the
// option leaves none or the mark is there.
func optionCheck(im Image, ref *erofsspec.Image, a string) string {
	s := ref.Super

	switch {
	case strings.HasPrefix(a, "-b"):
		if bs, _ := strconv.Atoi(a[2:]); bs != s.BlockSize {
			return fmt.Sprintf("block size %d, want %d", s.BlockSize, bs)
		}
	case a == "-Enosbcrc":
		if s.FeatureCompat&erofsspec.FeatureCompatSBChecksum != 0 {
			return "superblock checksum feature set under -Enosbcrc"
		}
	case a == "-Edot-omitted":
		if s.FeatureIncompat&erofsspec.FeatureIncompat48Bit == 0 {
			return "dot-omitted without the 48-bit layout"
		}
	case strings.HasPrefix(a, "--blobdev="):
		return blobCheck(im, ref)
	case strings.HasPrefix(a, "--xattr-prefix="):
		if !slices.ContainsFunc(ref.Prefixes, func(p erofsspec.Prefix) bool {
			return xattrPrefixName(p) == a[len("--xattr-prefix="):]
		}) {
			return fmt.Sprintf("long prefix %q not in the table %v", a, ref.Prefixes)
		}
	case a == "-Lvolume":
		if got := string(bytes.TrimRight(s.VolumeName[:], "\x00")); got != "volume" {
			return fmt.Sprintf("volume name %q", got)
		}
	case a == UUID:
		if got := fmt.Sprintf(
			"%x-%x-%x-%x-%x",
			s.UUID[0:4],
			s.UUID[4:6],
			s.UUID[6:8],
			s.UUID[8:10],
			s.UUID[10:16],
		); got != a {
			return "uuid " + got
		}
	case a == "-T"+strconv.Itoa(BuildTime):
		if s.Epoch+int64(s.BuildTime) != BuildTime {
			return fmt.Sprintf("build time %d + %d", s.Epoch, s.BuildTime)
		}
	}

	return ""
}

// blobCheck: one extra device, sized as the blob.
func blobCheck(im Image, ref *erofsspec.Image) string {
	if len(ref.Devices) != 1 || ref.Super.ExtraDevices != 1 {
		return fmt.Sprintf("devices %d, superblock says %d, want 1", len(ref.Devices), ref.Super.ExtraDevices)
	}

	blocks := int64(len(im.Blob)) / int64(ref.Super.BlockSize)
	if ref.Devices[0].Blocks != uint64(blocks) { // #nosec G115 -- a length is not negative
		return fmt.Sprintf("blob device of %d blocks, slot says %d", blocks, ref.Devices[0].Blocks)
	}

	return ""
}

func xattrPrefixName(p erofsspec.Prefix) string {
	return map[uint8]string{1: "user.", 2: "system.posix_acl_access", 3: "system.posix_acl_default", 4: "trusted.", 6: "security."}[p.BaseIndex] + p.Infix
}

// checkEntry holds one walked entry to its node: the type in the dirent
// and the mode, the permission bits, ids, times, size, content, target,
// link count, device number and xattrs.
func checkEntry(tb testing.TB, ref *erofsspec.Image, e erofsspec.Entry, n Node, all map[string]Node, nsec bool) {
	tb.Helper()

	in := e.Inode
	path := e.Path

	// A hardlink is its target's inode under another name.
	if n.Type == tar.TypeLink {
		n = all[n.Link]
	}

	wantType, wantDirent := TypeOf(n.Type)
	if in.Mode&erofsspec.ModeTypeMask != wantType || e.Dir.Type != wantDirent {
		tb.Errorf("%s: mode %#o dirent type %d, want type %#o and %d", path, in.Mode, e.Dir.Type, wantType, wantDirent)
	}

	if perm := int64(in.Mode &^ erofsspec.ModeTypeMask); perm != n.Mode {
		tb.Errorf("%s: permissions %#o, want %#o", path, perm, n.Mode)
	}

	if int(in.UID) != n.UID || int(in.GID) != n.GID {
		tb.Errorf("%s: uid:gid %d:%d, want %d:%d", path, in.UID, in.GID, n.UID, n.GID)
	}

	// A compact inode slot has no nanosecond field: the kernel reads the
	// superblock's fixed nanoseconds for it (erofs_fs.h, erofs_inode_compact;
	// inode.c, erofs_read_inode). mkfs.erofs keeps an inode compact under
	// the 48-bit layout whatever its mtime (lib/inode.c,
	// erofs_should_use_inode_extended), so there the tree's nanoseconds are gone
	// by construction. An extended slot carries the tree's, when the builder
	// stores them.
	wantNs := n.Nsec

	switch {
	case !in.Extended:
		wantNs = int64(ref.Super.FixedNsec)
	case !nsec:
		wantNs = 0
	}

	if in.Mtime != n.Sec || int64(in.MtimeNs) != wantNs {
		tb.Errorf("%s: mtime %d.%09d, want %d.%09d", path, in.Mtime, in.MtimeNs, n.Sec, wantNs)
	}

	if want := Nlink(n, all); in.Nlink != want {
		tb.Errorf("%s: nlink %d, want %d", path, in.Nlink, want)
	}

	checkContent(tb, ref, e, n)
	checkXattrs(tb, in, n)

	if !in.IsDir() {
		return
	}

	entries, err := ref.Dirents(in)
	if err != nil {
		tb.Errorf("%s: dirents: %v", path, err)

		return
	}

	for _, err := range ref.CheckDir(in, e.Parent, entries) {
		tb.Errorf("%s: %v", path, err)
	}
}

func checkContent(tb testing.TB, ref *erofsspec.Image, e erofsspec.Entry, n Node) {
	tb.Helper()

	in := e.Inode

	switch n.Type {
	case tar.TypeReg:
		if in.Size != int64(len(n.Data)) {
			tb.Errorf("%s: size %d, want %d", e.Path, in.Size, len(n.Data))
		}

		got, err := ref.Read(in)
		if err != nil || !bytes.Equal(got, n.Data) {
			tb.Errorf("%s: content differs (%d bytes, %v)", e.Path, len(got), err)
		}
	case tar.TypeSymlink:
		got, err := ref.Target(in)
		if err != nil || string(got) != n.Link {
			tb.Errorf("%s: target %q (%v), want %q", e.Path, got, err, n.Link)
		}
	case tar.TypeChar, tar.TypeBlock:
		if want := Rdev(n); in.Rdev != want {
			tb.Errorf("%s: rdev %#x, want %#x", e.Path, in.Rdev, want)
		}
	case tar.TypeFifo:
		if in.Size != 0 {
			tb.Errorf("%s: fifo of size %d", e.Path, in.Size)
		}
	}
}

func checkXattrs(tb testing.TB, in *erofsspec.Inode, n Node) {
	tb.Helper()

	got := map[string]string{}

	for _, x := range in.Xattrs {
		// Index 0 is a name stored whole; any other index without a prefix
		// is one the corpus never writes.
		if x.Unresolved && x.Index != 0 {
			tb.Errorf("%s: xattr index %#x unresolved (%q)", n.Path, x.Index, x.Suffix)
		}

		if _, dup := got[x.Name()]; dup {
			tb.Errorf("%s: xattr %q twice", n.Path, x.Name())
		}

		got[x.Name()] = string(x.Value)
	}

	want := n.Xattrs
	if want == nil {
		want = map[string]string{}
	}

	if !maps.Equal(got, want) {
		tb.Errorf("%s: xattrs %v, want %v", n.Path, Keys(got), Keys(want))
	}
}

// Keys is a map's keys, sorted.
func Keys(m map[string]string) []string {
	k := slices.Collect(maps.Keys(m))
	slices.Sort(k)

	return k
}

// TypeOf maps a tar type to the mode's type bits and the dirent's file type.
func TypeOf(typ byte) (uint16, uint8) {
	switch typ {
	case tar.TypeDir:
		return erofsspec.ModeDir, erofsspec.TypeDir
	case tar.TypeSymlink:
		return erofsspec.ModeSymlink, erofsspec.TypeSymlink
	case tar.TypeChar:
		return erofsspec.ModeChar, erofsspec.TypeChar
	case tar.TypeBlock:
		return erofsspec.ModeBlock, erofsspec.TypeBlock
	case tar.TypeFifo:
		return erofsspec.ModeFifo, erofsspec.TypeFifo
	default:
		return erofsspec.ModeRegular, erofsspec.TypeRegular
	}
}
