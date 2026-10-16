// Package corpus is the described tree the erofs tests hold images to, the
// tar that carries it to mkfs.erofs, the option matrix mkfs.erofs builds it
// under, and the images so built that are committed under testdata so that
// every CI lane, with or without mkfs.erofs, judges the same bytes. It is
// test support, compiled into tests only: the reference reader's anchoring
// test parses the images back to the tree, and the package's contract tests
// hold the reader to the reference over them.
//
// The committed images are reproducible by construction: a fixed build
// time applied to the build alone, a fixed UUID, a sorted tar, no
// nanoseconds and no device major past 8 bits (the two things the Darwin
// build of erofs-utils stores differently), and a root entry in the tar,
// since mkfs.erofs otherwise gives the root the building user's ids. Where
// mkfs.erofs is present, TestCommittedIsReproducible rebuilds them, reads
// each back to its tree and passes it to fsck.erofs, and under the build
// the manifest names fails on any byte of difference.
package corpus

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Node is one entry of the described tree. Type is a tar type flag; Link
// is a symlink's target or a hardlink's target path; Major and Minor are a
// device's numbers.
type Node struct {
	Path   string
	Type   byte
	Mode   int64 // permission and setuid/setgid/sticky bits
	UID    int
	GID    int
	Sec    int64
	Nsec   int64
	Data   []byte
	Link   string
	Xattrs map[string]string
	Major  int64
	Minor  int64
}

// BuildTime is the committed images' build time (-T, applied with
// --mkfs-time to the build alone, so entries keep their own mtimes).
const BuildTime = 1_700_000_000

// RootMode is the root directory's permission bits in every image: the
// tar's root entry, the assembler's root and the writer's root carry it.
const RootMode = 0o755

// UUID is the committed images' volume UUID.
const UUID = "01234567-89ab-cdef-0123-456789abcdef"

// Tree is the described tree the committed images carry: every type the
// format stores, ids at the compact/extended boundary, sizes one short of,
// at and one past 512 and 4096 bytes, names at the length limit, a
// directory wider than an 8 KiB block, xattrs of every built-in prefix and
// a long one. It is kept small, since it is committed under thirteen
// option sets; Full adds the bulk.
func Tree() []Node {
	t := []Node{
		{Path: "d", Type: tar.TypeDir, Mode: 0o755, UID: 1000, GID: 1000, Sec: 1_700_000_000},
		{Path: "d/empty", Type: tar.TypeReg, Mode: 0o644, Sec: 1_700_000_001},
		{Path: "d/one", Type: tar.TypeReg, Mode: 0o600, Data: []byte{'x'}, Sec: 1_700_000_002},
		{Path: "d/setuid", Type: tar.TypeReg, Mode: 0o4755, Data: []byte("suid\n"), Sec: 1_700_000_003},
		{Path: "d/sticky", Type: tar.TypeDir, Mode: 0o1777, Sec: 1_700_000_004},
		{Path: "d/sticky/setgid", Type: tar.TypeDir, Mode: 0o2775, Sec: 1_700_000_005},
		{
			Path: "d/wide",
			Type: tar.TypeReg,
			Mode: 0o644,
			Data: []byte("wide ids\n"),
			UID:  65535,
			GID:  65536,
			Sec:  1_700_000_006,
		},
		{
			Path: "d/wider",
			Type: tar.TypeReg,
			Mode: 0o644,
			Data: []byte("widest ids\n"),
			UID:  1 << 31,
			GID:  1<<32 - 1,
			Sec:  1_700_000_007,
		},
		{Path: "d/hard", Type: tar.TypeLink, Link: "d/one"},
		{Path: "d/hard2", Type: tar.TypeLink, Link: "d/one"},
		{Path: "d/sym", Type: tar.TypeSymlink, Mode: 0o777, Link: "one", Sec: 1_700_000_008},
		{Path: "d/symabs", Type: tar.TypeSymlink, Mode: 0o777, Link: "/d/one", Sec: 1_700_000_009},
		{
			Path: "d/symlong",
			Type: tar.TypeSymlink,
			Mode: 0o777,
			Link: strings.Repeat("abcdefghij/", 100) + "end",
			Sec:  1_700_000_010,
		},
		{Path: "d/symdir", Type: tar.TypeSymlink, Mode: 0o777, Link: "../sizes", Sec: 1_700_000_011},
		{Path: "d/fifo", Type: tar.TypeFifo, Mode: 0o600, Sec: 1_700_000_012},
		{Path: "d/chr", Type: tar.TypeChar, Mode: 0o620, Major: 5, Minor: 1, Sec: 1_700_000_013},
		// A minor past 255 exercises the split minor of new_encode_dev; the
		// major stays within 8 bits because mkfs.erofs builds the number
		// with the host's makedev, and macOS's dev_t holds no more.
		{Path: "d/blk", Type: tar.TypeBlock, Mode: 0o660, Major: 254, Minor: 4097, Sec: 1_700_000_014},
		{
			Path: "d/" + strings.Repeat("n", 255),
			Type: tar.TypeReg,
			Mode: 0o644,
			Data: []byte("long name\n"),
			Sec:  1_700_000_015,
		},
		{
			Path: "d/x",
			Type: tar.TypeReg,
			Mode: 0o644,
			Data: []byte("xattrs\n"),
			Sec:  1_700_000_016,
			Xattrs: map[string]string{
				"user.k":                           "v",
				"user.empty":                       "",
				"user." + strings.Repeat("n", 255): "long name",
				"user.long":                        strings.Repeat("v", 1000),
				"security.selinux":                 "system_u:object_r:etc_t:s0",
				"security.capability":              "\x01\x00\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00",
				"trusted.overlay.opaque":           "y",
				"trusted.overlay.redirect":         "/elsewhere",
				"system.posix_acl_access":          "\x02\x00\x00\x00\x01\x00\x06\x00\xff\xff\xff\xff\x04\x00\x04\x00\xff\xff\xff\xff\x20\x00\x04\x00\xff\xff\xff\xff",
				"system.posix_acl_default":         "\x02\x00\x00\x00\x01\x00\x07\x00\xff\xff\xff\xff\x04\x00\x05\x00\xff\xff\xff\xff\x20\x00\x05\x00\xff\xff\xff\xff",
			},
		},
		{
			Path: "d/x2",
			Type: tar.TypeReg,
			Mode: 0o644,
			Data: []byte("shared xattrs\n"),
			Sec:  1_700_000_017,
			Xattrs: map[string]string{
				"user.k":           "v",
				"security.selinux": "system_u:object_r:etc_t:s0",
			},
		},
		{
			Path: "d/x3",
			Type: tar.TypeReg,
			Mode: 0o644,
			Data: []byte("shared xattrs too\n"),
			Sec:  1_700_000_018,
			Xattrs: map[string]string{
				"user.k":           "v",
				"security.selinux": "system_u:object_r:etc_t:s0",
			},
		},
		{Path: "epoch", Type: tar.TypeReg, Mode: 0o644, Data: []byte("1970\n"), Sec: 0},
		{Path: "sizes", Type: tar.TypeDir, Mode: 0o755, Sec: 1_700_000_019},
		{Path: "many", Type: tar.TypeDir, Mode: 0o755, Sec: 1_700_000_020},
	}

	// Sizes one short of, at and one past 512 and 4096: the inline tail is
	// 511, 0 and 1 bytes at each, and a file spans one to nine blocks.
	for _, bs := range []int{512, 4096} {
		for _, d := range []int{-1, 0, 1} {
			n := bs + d
			t = append(
				t,
				Node{
					Path: fmt.Sprintf("sizes/%d", n),
					Type: tar.TypeReg,
					Mode: 0o644,
					Data: Pattern(n),
					Sec:  1_700_000_021,
				},
			)
		}
	}

	// A directory wider than an 8 KiB block: 300 empty files with names 1
	// to 40 bytes long, so its dirents split across blocks at every block
	// size up to 8 KiB; empty, so a plain-data image spends no block on
	// them.
	for i := range 300 {
		t = append(
			t,
			Node{Path: fmt.Sprintf("many/%0*d", 1+i%40, i), Type: tar.TypeReg, Mode: 0o644, Sec: 1_700_000_022},
		)
	}

	return t
}

// Full is Tree with the bulk the committed images leave out: nanoseconds,
// a 65535-byte xattr value, sizes around every block size up to 16 KiB, a
// 70000-byte file of zeros, a 200000-byte file and a 1500-entry directory,
// wider than a 16 KiB block. It is built locally where mkfs.erofs is
// present, never committed.
func Full() []Node {
	t := Tree()

	for i := range t {
		if t[i].Path == "d/empty" {
			t[i].Nsec = 123_456_789
		}

		if t[i].Path == "d/x" {
			t[i].Xattrs["user.long"] = strings.Repeat("v", 65535)
		}
	}

	for _, bs := range []int{1024, 2048, 8192, 16384} {
		for _, d := range []int{-1, 0, 1} {
			n := bs + d
			t = append(
				t,
				Node{
					Path: fmt.Sprintf("sizes/%d", n),
					Type: tar.TypeReg,
					Mode: 0o644,
					Data: Pattern(n),
					Sec:  1_700_000_021,
				},
			)
		}
	}

	t = append(
		t,
		Node{Path: "zeros", Type: tar.TypeReg, Mode: 0o644, Data: make([]byte, 70_000), Sec: 1_700_000_023},
		Node{
			Path: "big",
			Type: tar.TypeReg,
			Mode: 0o644,
			Data: Pattern(200_000),
			Sec:  1_700_000_024,
			Nsec: 999_999_999,
		},
		Node{Path: "wide", Type: tar.TypeDir, Mode: 0o755, Sec: 1_700_000_025},
	)

	for i := range 1500 {
		t = append(
			t,
			Node{Path: fmt.Sprintf("wide/%0*d", 1+i%40, i), Type: tar.TypeReg, Mode: 0o644, Sec: 1_700_000_026},
		)
	}

	return t
}

// Small is a tree of a few dozen entries, for checks that run many times
// over one image: every type, a hardlink, a multi-block file and a
// two-block directory at 512-byte blocks.
func Small() []Node {
	t := []Node{
		{Path: "d", Type: tar.TypeDir, Mode: 0o755, Sec: 1_700_000_000},
		{
			Path:   "d/f",
			Type:   tar.TypeReg,
			Mode:   0o644,
			Data:   []byte("hello\n"),
			Sec:    1_700_000_001,
			Xattrs: map[string]string{"user.k": "v", "security.selinux": "s"},
		},
		{Path: "d/h", Type: tar.TypeLink, Link: "d/f"},
		{Path: "d/s", Type: tar.TypeSymlink, Mode: 0o777, Link: "f", Sec: 1_700_000_002},
		{Path: "d/p", Type: tar.TypeFifo, Mode: 0o600, Sec: 1_700_000_003},
		{Path: "d/c", Type: tar.TypeChar, Mode: 0o620, Major: 5, Minor: 1, Sec: 1_700_000_004},
		{Path: "d/sub", Type: tar.TypeDir, Mode: 0o700, Sec: 1_700_000_005},
		{Path: "d/sub/empty", Type: tar.TypeReg, Mode: 0o644, Sec: 1_700_000_006},
		{Path: "multi", Type: tar.TypeReg, Mode: 0o644, Data: Pattern(3000), Sec: 1_700_000_007},
	}

	for i := range 40 {
		t = append(
			t,
			Node{
				Path: fmt.Sprintf("d/sub/e%02d", i),
				Type: tar.TypeReg,
				Mode: 0o644,
				Data: []byte{byte(i)},
				Sec:  1_700_000_008,
			},
		)
	}

	return t
}

// Ordered is the tree in build order: directories before their children,
// hardlinks after their targets, since both mkfs.erofs and the package's
// writer resolve them against what they have already seen.
func Ordered(nodes []Node) []Node {
	sorted := slices.Clone(nodes)
	slices.SortStableFunc(sorted, func(a, b Node) int {
		if (a.Type == tar.TypeLink) != (b.Type == tar.TypeLink) {
			if a.Type == tar.TypeLink {
				return 1
			}

			return -1
		}

		return strings.Compare(a.Path, b.Path)
	})

	return sorted
}

// Pattern is n bytes that repeat on no block boundary.
func Pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}

	return b
}

// ByPath indexes the nodes by path.
func ByPath(nodes []Node) map[string]Node {
	m := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		m[n.Path] = n
	}

	return m
}

// Nlink is the link count an image must carry for n: for a directory, two
// plus its subdirectories; for anything else, the number of names it has.
func Nlink(n Node, all map[string]Node) uint32 {
	count := uint32(0)

	for _, o := range all {
		switch {
		case n.Type == tar.TypeDir && o.Type == tar.TypeDir && path.Dir(o.Path) == n.Path:
			count++
		case n.Type != tar.TypeDir && (o.Path == n.Path || (o.Type == tar.TypeLink && o.Link == n.Path)):
			count++
		}
	}

	if n.Type == tar.TypeDir {
		return count + 2
	}

	return count
}

// Rdev is the device number mkfs.erofs stores for a node:
// new_encode_dev(makedev(major, minor)) (include/linux/kdev_t.h).
func Rdev(n Node) uint32 {
	major := uint32(n.Major) // #nosec G115 -- the tree's own small numbers
	minor := uint32(n.Minor) // #nosec G115 -- the tree's own small numbers

	return minor&0xff | major<<8 | (minor&^0xff)<<12
}

// Tar writes the tree as a PAX tar: directories before their children and
// hardlinks after their targets, since mkfs.erofs resolves both against
// what it has already read.
func Tar(tb testing.TB, nodes []Node) []byte {
	tb.Helper()

	var buf bytes.Buffer

	w := tar.NewWriter(&buf)

	// The root directory is an entry of its own: without one, mkfs.erofs
	// gives the root the building user's ids, and the image is that
	// user's.
	root := &tar.Header{
		Typeflag: tar.TypeDir,
		Name:     "./",
		Mode:     RootMode,
		ModTime:  time.Unix(BuildTime, 0),
		Format:   tar.FormatPAX,
	}
	if err := w.WriteHeader(root); err != nil {
		tb.Fatal(err)
	}

	for _, n := range Ordered(nodes) {
		h := &tar.Header{
			Typeflag: n.Type,
			Name:     n.Path,
			Mode:     n.Mode,
			Uid:      n.UID,
			Gid:      n.GID,
			Size:     int64(len(n.Data)),
			Linkname: n.Link,
			Devmajor: n.Major,
			Devminor: n.Minor,
			ModTime:  time.Unix(n.Sec, n.Nsec),
			Format:   tar.FormatPAX,
		}

		if n.Type == tar.TypeDir {
			h.Name += "/"
		}

		if len(n.Xattrs) > 0 {
			h.PAXRecords = map[string]string{}
			for k, v := range n.Xattrs {
				h.PAXRecords["SCHILY.xattr."+k] = v
			}
		}

		if err := w.WriteHeader(h); err != nil {
			tb.Fatal(err)
		}

		if _, err := w.Write(n.Data); err != nil {
			tb.Fatal(err)
		}
	}

	if err := w.Close(); err != nil {
		tb.Fatal(err)
	}

	return buf.Bytes()
}

// Matrix is every mkfs.erofs 1.9 option set the reader claims to read: the
// block sizes it accepts (512 to 16 KiB; 32 and 64 KiB are refused by
// mkfs.erofs 1.9.4 and reachable only through the package's writer), the
// inode layouts, inline and plain data, every chunk map form with and
// without a blob device, xattrs inline, shared, absent and under a long
// prefix, no superblock checksum, and the 48-bit layout dot-omitted brings.
// force-inode-compact is absent: the tree's wide ids cannot be compact and
// mkfs.erofs refuses the combination; the default layout picks compact
// inodes wherever the fields fit. Shared xattrs are absent too: mkfs.erofs
// builds them from a directory source only, never from a tar (-x0 included),
// and a directory source is not reproducible across hosts. Every set carries
// the fixed build time and UUID, so an image is a function of the tree and
// the set alone.
func Matrix() map[string][]string {
	m := map[string][]string{}

	for _, bs := range []string{"512", "1024", "2048", "4096", "8192", "16384"} {
		m["b"+bs] = []string{"-b" + bs}
		m["b"+bs+"-extended"] = []string{"-b" + bs, "-Eforce-inode-extended"}
		m["b"+bs+"-plain"] = []string{"-b" + bs, "-E^inline_data"}
	}

	m["chunk-indexes"] = []string{"-b4096", "--chunksize=4096", "-Eforce-chunk-indexes"}
	m["chunk-blockmap"] = []string{"-b4096", "--chunksize=4096", "-Eforce-inode-blockmap"}
	m["chunk-16k"] = []string{"-b4096", "--chunksize=16384"}
	m["chunk-512"] = []string{"-b512", "--chunksize=512"}
	m["blobdev"] = []string{"-b4096", "--chunksize=8192", "--blobdev=BLOB"}
	m["blobdev-512"] = []string{"-b512", "--chunksize=1024", "--blobdev=BLOB"}
	m["xattr-none"] = []string{"-b4096", "-x-1"}
	m["xattr-prefix"] = []string{"-b4096", "--xattr-prefix=trusted.overlay."}
	m["xattr-prefix-512"] = []string{"-b512", "--xattr-prefix=trusted.overlay.", "--xattr-prefix=user.lo"}
	m["nosbcrc"] = []string{"-b4096", "-Enosbcrc"}
	m["dot-omitted"] = []string{"-b4096", "-Edot-omitted"}
	m["volume"] = []string{"-b4096", "-Lvolume"}

	for name := range m {
		m[name] = append(m[name], "-T"+strconv.Itoa(BuildTime), "--mkfs-time", "-U", UUID)
	}

	return m
}

// Set is one committed image's recipe: the tree it carries and the option
// set of Matrix mkfs.erofs builds it under, or the layout the assembler
// writes.
type Set struct {
	Name   string
	Tree   func() []Node
	Opts   string
	Layout *Layout
}

// CommittedSets are the images committed under testdata: Tree under one of
// each shape the reader distinguishes, and Small, a few kilobytes, under
// four, for the checks that run thousands of variants of one image.
func CommittedSets() []Set {
	sets := []Set{}

	for _, opts := range []string{
		"b512", "b512-extended", "b1024-plain", "b4096", "b4096-extended", "b4096-plain", "b16384",
		"chunk-indexes", "chunk-blockmap", "blobdev", "xattr-prefix-512", "nosbcrc", "dot-omitted",
	} {
		sets = append(sets, Set{Name: opts, Tree: Tree, Opts: opts})
	}

	for _, opts := range []string{"b512", "b4096-extended", "chunk-512", "blobdev-512"} {
		sets = append(sets, Set{Name: "small-" + opts, Tree: Small, Opts: opts})
	}

	for _, as := range Assembled() {
		sets = append(sets, Set{Name: as.Name, Tree: as.Tree, Layout: &as.Layout})
	}

	return sets
}

// Image is one mkfs.erofs or assembler product: the image, its blob device
// when the options asked for one, the options, and the tree it was built
// from, nil for an image the kernel refuses.
type Image struct {
	Name string
	Args []string
	Img  []byte
	Blob []byte
	Tree []Node
}

// Extra is the image's extra devices as Parse and WithExtraDevices take
// them: the blob alone, or none.
func (im Image) Extra() [][]byte {
	if im.Blob == nil {
		return nil
	}

	return [][]byte{im.Blob}
}

//go:embed testdata
var testdata embed.FS

// Committed loads the committed images. Their bytes are what every lane
// judges; TestCommittedIsReproducible holds them to mkfs.erofs where it is
// present.
func Committed(tb testing.TB) []Image {
	tb.Helper()

	matrix := Matrix()

	var out []Image

	for _, set := range CommittedSets() {
		im := Image{Name: set.Name, Args: matrix[set.Opts]}
		if set.Tree != nil {
			im.Tree = set.Tree()
		}

		var err error

		im.Img, err = fs.ReadFile(testdata, "testdata/"+set.Name+".erofs")
		if err != nil {
			tb.Fatalf("committed corpus: %v (regenerate with go test ./internal/corpus -regenerate)", err)
		}

		if blob, err := fs.ReadFile(testdata, "testdata/"+set.Name+".blob"); err == nil {
			im.Blob = blob
		}

		out = append(out, im)
	}

	return out
}

// Digest is the sha256 of a byte slice in hex, as the manifest records it.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// MkfsPath is mkfs.erofs's path, or "" when it is not on PATH; a test that
// needs it skips naming the capability, unless EROFS_REQUIRE_MKFS=1 says
// the leg was supposed to have it, where it fails.
func MkfsPath(tb testing.TB) string {
	tb.Helper()

	tool, err := exec.LookPath("mkfs.erofs")
	if err != nil {
		if os.Getenv("EROFS_REQUIRE_MKFS") == "1" {
			tb.Fatal("mkfs.erofs is not on PATH and EROFS_REQUIRE_MKFS=1")
		}

		tb.Skip("mkfs.erofs is not on PATH: the image build cannot run")
	}

	return tool
}

// Mkfs runs mkfs.erofs over the tar with the options, BLOB in an option
// standing for the blob device's path.
func Mkfs(tb testing.TB, name string, args []string, tarball []byte) Image {
	tb.Helper()

	tool := MkfsPath(tb)
	dir := tb.TempDir()
	src := filepath.Join(dir, "src.tar")
	out := filepath.Join(dir, "out.erofs")
	blob := filepath.Join(dir, "blob.bin")

	if err := os.WriteFile(src, tarball, 0o600); err != nil {
		tb.Fatal(err)
	}

	full := make([]string, 0, len(args)+3)
	for _, a := range args {
		full = append(full, strings.ReplaceAll(a, "BLOB", blob))
	}

	full = append(full, "--tar=f", out, src)

	// #nosec G204 -- mkfs.erofs from PATH with the matrix's options and paths under TempDir
	if outb, err := exec.CommandContext(tb.Context(), tool, full...).CombinedOutput(); err != nil {
		tb.Fatalf("%s: mkfs.erofs %s: %v\n%s", name, strings.Join(full, " "), err, outb)
	}

	im := Image{Name: name, Args: args}

	var err error

	im.Img, err = os.ReadFile(out) // #nosec G304 -- the image mkfs.erofs just wrote under TempDir
	if err != nil {
		tb.Fatal(err)
	}

	if slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--blobdev=") }) {
		im.Blob, err = os.ReadFile(blob) // #nosec G304 -- the blob mkfs.erofs just wrote under TempDir
		if err != nil {
			tb.Fatal(err)
		}
	}

	return im
}

// Fsck runs fsck.erofs on the image when the tool is on PATH and fails the
// test when it refuses the image.
func Fsck(tb testing.TB, im Image) {
	tb.Helper()

	if err := runFsck(tb, im); err != nil {
		tb.Fatalf("%s: fsck.erofs refuses the image: %v", im.Name, err)
	}
}

// FsckRefuses runs fsck.erofs on the image when the tool is on PATH and
// fails the test when it accepts it: for an image that carries a state the
// kernel refuses.
func FsckRefuses(tb testing.TB, im Image) {
	tb.Helper()

	// The Windows cross-build of fsck.erofs 1.9.4 accepts the header-only
	// xattr image the kernel and the other builds refuse (seen on CI); its
	// verdict on what it should refuse is not taken there.
	if runtime.GOOS == "windows" {
		tb.Logf("%s: fsck.erofs's refusal is not taken on windows", im.Name)

		return
	}

	if err := runFsck(tb, im); err == nil {
		tb.Fatalf("%s: fsck.erofs accepts an image the kernel refuses", im.Name)
	}
}

// runFsck is fsck.erofs's verdict on the image, nil without the tool.
func runFsck(tb testing.TB, im Image) error {
	tb.Helper()

	tool, err := exec.LookPath("fsck.erofs")
	if err != nil {
		return nil //nolint:nilerr // no tool, no verdict
	}

	dir := tb.TempDir()
	out := filepath.Join(dir, "out.erofs")
	args := []string{out}

	if err := os.WriteFile(out, im.Img, 0o600); err != nil {
		tb.Fatal(err)
	}

	if im.Blob != nil {
		blob := filepath.Join(dir, "blob.bin")
		if err := os.WriteFile(blob, im.Blob, 0o600); err != nil {
			tb.Fatal(err)
		}

		args = append(args, "--device="+blob)
	}

	// #nosec G204 -- fsck.erofs from PATH on files under TempDir
	if outb, err := exec.CommandContext(tb.Context(), tool, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, outb)
	}

	return nil
}
