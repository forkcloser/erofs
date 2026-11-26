package erofs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io/fs"
	"testing"

	"github.com/forkcloser/go-erofs"
)

// symlinkAt is an image of 512-byte blocks whose metadata starts at block 3
// and whose root names one symbolic link, "l": a flat-inline inode at byte
// at, extended or compact, with the target after it.
func symlinkAt(at int, extended bool, target []byte) []byte {
	const (
		bs   = 512
		meta = 3 * bs
	)

	head := make([]byte, 7*bs)
	laidSuper(head, 9, 7)
	binary.LittleEndian.PutUint32(head[1024+40:], 3) // meta_blkaddr

	root := laidDirBlock([]laidDirent{
		{0, laidFtDir, "."}, {0, laidFtDir, ".."}, {uint64((at - meta) / 32), laidFtLink, "l"},
	})
	laidCompact(head[meta:], laidFlatInline, laidDir, 2, uint32(len(root)), 0)
	copy(head[meta+32:], root)

	if extended {
		laidExtended(head[at:], laidFlatInline, laidLink, uint64(len(target)), 0, 1)
		copy(head[at+64:], target)
	} else {
		laidCompact(head[at:], laidFlatInline, laidLink, 1, uint32(len(target)), 0)
		copy(head[at+32:], target)
	}

	return head
}

// TestContractAFastSymlinkIsJudgedWithTheInodeThatEndsInItsBlock: the kernel
// judges an inline target with its inode (inode.c, erofs_fill_symlink: a
// target that is empty or holds a NUL refuses the inode), and the block that
// makes a target inline is the one the inode ends in. An extended inode's
// last 32 bytes may be in the block after the one it starts in
// (erofs_read_inode), and its target follows them there: Lstat used to
// serve such an inode whatever its target held, and ReadLink to refuse it.
func TestContractAFastSymlinkIsJudgedWithTheInodeThatEndsInItsBlock(t *testing.T) {
	t.Parallel()

	const (
		bs       = 512
		straddle = 4*bs - 32 // the inode's last 32 bytes start block 4
		inside   = 3*bs + 256
		fills    = bs - 32 // a target from the straddling inode to block 4's end
	)

	withNUL := func(n int) []byte {
		target := bytes.Repeat([]byte("x"), n)
		target[n-1] = 0

		return target
	}

	for _, tc := range []struct {
		name     string
		at       int
		extended bool
		target   []byte
		// judged: the target is inline, and Lstat refuses the inode for it.
		judged bool
	}{
		{"an inode ending in the next block", straddle, true, []byte("ab"), false},
		{"an inode ending in the next block, a NUL in its target", straddle, true, []byte("a\x00"), true},
		{"a target to the end of the next block", straddle, true, bytes.Repeat([]byte("x"), fills), false},
		{"a target to the end of the next block, its last byte a NUL", straddle, true, withNUL(fills), true},
		{"a target one byte past the next block", straddle, true, bytes.Repeat([]byte("x"), fills+1), false},
		{"a target one byte past the next block, a NUL in it", straddle, true, withNUL(fills + 1), false},
		{"an inode inside one block, a NUL in its target", inside, true, []byte("a\x00"), true},
		{"a compact inode ending its block, the target in the next", straddle, false, []byte("ab"), false},
		{"a compact inode ending its block, a NUL in the target in the next", straddle, false, []byte("a\x00"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fsys, err := erofs.Open(bytes.NewReader(symlinkAt(tc.at, tc.extended, tc.target)))
			if err != nil {
				t.Fatal(err)
			}

			// A target that is not inline and runs over its block's end
			// is read by no one: the kernel refuses inline data across
			// blocks where it is read.
			sound := bytes.IndexByte(tc.target, 0) < 0 && len(tc.target) <= fills

			_, lerr := fs.Lstat(fsys, "l")
			if tc.judged && !errors.Is(lerr, erofs.ErrCorrupt) {
				t.Errorf("Lstat: %v; want ErrCorrupt, the target judged with the inode", lerr)
			}

			if !tc.judged && lerr != nil {
				t.Errorf("Lstat: %v; want the inode served", lerr)
			}

			got, rerr := fs.ReadLink(fsys, "l")
			if sound && (rerr != nil || got != string(tc.target)) {
				t.Errorf("ReadLink: %d bytes, %v; want the %d of the target", len(got), rerr, len(tc.target))
			}

			if !sound && !errors.Is(rerr, erofs.ErrCorrupt) {
				t.Errorf("ReadLink: %q, %v; want ErrCorrupt", got, rerr)
			}
		})
	}
}
