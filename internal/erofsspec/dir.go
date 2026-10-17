package erofsspec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

// The directory block (erofs_fs.h: struct erofs_dirent; erofs.rst,
// "Directories"): dirents first, names after, the first name's offset
// doubling as the count.
const (
	// DirentSize is one on-disk dirent: a 64-bit nid, a 16-bit name offset,
	// a file type byte and a reserved byte.
	DirentSize = 12

	// The dirent file types (fs_types.h, FT_*; erofs_dirent.file_type).
	TypeUnknown = 0
	TypeRegular = 1
	TypeDir     = 2
	TypeChar    = 3
	TypeBlock   = 4
	TypeFifo    = 5
	TypeSocket  = 6
	TypeSymlink = 7
	typeMax     = 8
)

// Dirent is one directory entry: the name as stored (any bytes but NUL and
// "/" in a well-formed image), the nid it names and the type it declares.
type Dirent struct {
	Nid  uint64
	Name []byte
	Type uint8
}

// Dirents lists a directory's entries, "." and ".." included where stored,
// block by block as dir.c's erofs_readdir and erofs_fill_dentries do: in
// each block the first name offset must be a non-zero multiple of the
// dirent size below the block size; a name runs to the next entry's offset,
// or, for the last, to the first NUL within the block's used part; every
// name is 1 to 255 bytes and ends within the used part.
func (im *Image) Dirents(dir *Inode) ([]Dirent, error) {
	if !dir.IsDir() {
		return nil, fmt.Errorf("%w: inode %d", ErrNotDir, dir.Nid)
	}

	bs := int64(im.Super.BlockSize)

	var out []Dirent

	// Block by block, as erofs_readdir reads: a directory whose size field
	// is hostile fails at its first block, not at an allocation of the whole.
	for start := int64(0); start < dir.Size; start += bs {
		block, err := im.ReadAt(dir, start, min(bs, dir.Size-start))
		if err != nil {
			return nil, err
		}

		entries, err := direntsOfBlock(block, bs)
		if err != nil {
			return nil, fmt.Errorf("inode %d: %w", dir.Nid, err)
		}

		out = append(out, entries...)
	}

	return out, nil
}

func direntsOfBlock(block []byte, bs int64) ([]Dirent, error) {
	maxsize := int64(len(block))
	if maxsize < DirentSize {
		return nil, fmt.Errorf("%w: directory block of %d bytes", ErrCorrupt, maxsize)
	}

	nameoff0 := int64(binary.LittleEndian.Uint16(block[8:]))
	// "invalid de[0].nameoff %u @ nid %llu"
	if nameoff0 == 0 || nameoff0 >= bs || nameoff0%DirentSize != 0 {
		return nil, fmt.Errorf("%w: first name offset %d", ErrCorrupt, nameoff0)
	}

	// The first name starts past the block's used part: the kernel reads
	// the dirents from the rest of the block on disk and then refuses the
	// first name as a "bogus dirent" (nameoff + namelen > maxsize), so the
	// verdict is corrupt before any header past the used part is read.
	if nameoff0 > maxsize {
		return nil, fmt.Errorf("%w: first name offset %d past the %d used bytes", ErrCorrupt, nameoff0, maxsize)
	}

	count := nameoff0 / DirentSize
	out := make([]Dirent, 0, count)

	for i := range count {
		de := block[i*DirentSize:]
		nameoff := int64(binary.LittleEndian.Uint16(de[8:]))

		var namelen int64

		switch {
		case i+1 < count:
			namelen = int64(binary.LittleEndian.Uint16(block[(i+1)*DirentSize+8:])) - nameoff
		case maxsize <= nameoff:
			return nil, fmt.Errorf("%w: last name offset %d past %d", ErrCorrupt, nameoff, maxsize)
		default:
			namelen = int64(strnlen(block[nameoff:maxsize]))
		}

		// "a corrupted entry is found (including negative namelen)"
		if namelen < 1 || namelen > NameMax || nameoff+namelen > maxsize {
			return nil, fmt.Errorf(
				"%w: dirent %d name offset %d length %d in %d",
				ErrCorrupt,
				i,
				nameoff,
				namelen,
				maxsize,
			)
		}

		name := block[nameoff : nameoff+namelen]
		// A name with a NUL or a "/" inside: readdir would list it, but no
		// lookup can name it (erofs_dirnamecmp stops at the NUL and the VFS
		// never passes a slash), and fsck.erofs refuses it (lib/dir.c,
		// erofs_validate_filename, "corrupted dirent with illegal
		// filename"); it is corrupt here as there.
		if bytes.ContainsAny(name, "/\x00") {
			return nil, fmt.Errorf("%w: dirent %d name %q", ErrCorrupt, i, name)
		}

		out = append(out, Dirent{
			Nid:  binary.LittleEndian.Uint64(de[0:]),
			Name: bytes.Clone(name),
			Type: de[10],
		})
	}

	return out, nil
}

func strnlen(b []byte) int {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return i
	}

	return len(b)
}

// Lookup finds name in dir as namei.c's erofs_namei does: a binary search
// over the directory's blocks on each block's first name, then a binary
// search within the block, comparing with erofs_dirnamecmp, which stops at
// a NUL in the stored name and skips the prefix the bounds already matched.
// Followed literally, so that an unsorted directory, which fsck refuses
// and the kernel never checks, answers here as it answers there.
func (im *Image) Lookup(dir *Inode, name []byte) (Dirent, error) {
	if !dir.IsDir() {
		return Dirent{}, fmt.Errorf("%w: inode %d", ErrNotDir, dir.Nid)
	}

	if dir.Size == 0 {
		return Dirent{}, fmt.Errorf("%w: %q in inode %d", ErrNotExist, name, dir.Nid)
	}

	bs := int64(im.Super.BlockSize)
	head, back := int64(0), (dir.Size+bs-1)/bs-1
	startprfx, endprfx := 0, 0

	var (
		candidate  []byte
		candidates int64
	)

	// erofs_find_target_block
	for head <= back {
		mid := head + (back-head)/2

		block, ndirents, err := im.dirBlock(dir, mid)
		if err != nil {
			return Dirent{}, err
		}

		matched := min(startprfx, endprfx)

		end := bs
		if ndirents > 1 {
			end = int64(binary.LittleEndian.Uint16(block[DirentSize+8:])) & (bs - 1)
		}

		nameoff := int64(binary.LittleEndian.Uint16(block[8:])) & (bs - 1)

		diff := dirnamecmp(name, block[nameoff:end], &matched)
		if diff < 0 {
			back = mid - 1
			endprfx = matched

			continue
		}

		candidate, candidates = block, ndirents
		if diff == 0 {
			return direntAt(block, 0), nil
		}

		head = mid + 1
		startprfx = matched
	}

	// erofs_namei: the search within the block runs on the last block whose
	// first name sorted before the name, and on that block alone.
	if candidate != nil {
		if de, ok := findTargetDirent(name, candidate, bs, candidates); ok {
			return de, nil
		}
	}

	return Dirent{}, fmt.Errorf("%w: %q in inode %d", ErrNotExist, name, dir.Nid)
}

// dirBlock reads directory block i as the page cache holds it, zero past
// the directory's size, and counts its dirents from the first name offset
// masked to the block, as namei.c does; none is "corrupted dir block".
func (im *Image) dirBlock(dir *Inode, i int64) ([]byte, int64, error) {
	bs := int64(im.Super.BlockSize)

	block, err := im.ReadAt(dir, i*bs, min(bs, dir.Size-i*bs))
	if err != nil {
		return nil, 0, err
	}

	if int64(len(block)) < bs {
		block = append(block, make([]byte, bs-int64(len(block)))...)
	}

	ndirents := (int64(binary.LittleEndian.Uint16(block[8:])) & (bs - 1)) / DirentSize
	if ndirents == 0 {
		return nil, 0, fmt.Errorf("%w: directory block %d of inode %d", ErrCorrupt, i, dir.Nid)
	}

	return block, ndirents, nil
}

// findTargetDirent is namei.c's find_target_dirent: a binary search over
// the block's dirents after the first, which the block search compared.
func findTargetDirent(name, block []byte, bs, ndirents int64) (Dirent, bool) {
	head, back := int64(1), ndirents-1
	startprfx, endprfx := 0, 0

	for head <= back {
		mid := head + (back-head)/2
		nameoff := int64(binary.LittleEndian.Uint16(block[mid*DirentSize+8:])) & (bs - 1)
		matched := min(startprfx, endprfx)

		end := bs
		if mid < ndirents-1 {
			end = int64(binary.LittleEndian.Uint16(block[(mid+1)*DirentSize+8:])) & (bs - 1)
		}

		var stored []byte
		if nameoff <= end {
			stored = block[nameoff:end]
		}

		ret := dirnamecmp(name, stored, &matched)

		switch {
		case ret == 0:
			return direntAt(block, mid), true
		case ret > 0:
			head = mid + 1
			startprfx = matched
		default:
			back = mid - 1
			endprfx = matched
		}
	}

	return Dirent{}, false
}

// dirnamecmp is namei.c's erofs_dirnamecmp: the stored name has no
// terminator, so it ends at its slice or at a NUL, whichever is first; the
// comparison starts after the already matched prefix and reports where it
// stopped.
func dirnamecmp(name, stored []byte, matched *int) int {
	i := *matched

	for i < len(stored) && stored[i] != 0 {
		var c byte
		if i < len(name) {
			c = name[i]
		}

		if c != stored[i] {
			*matched = i
			if c > stored[i] {
				return 1
			}

			return -1
		}

		i++
	}

	*matched = i
	if i >= len(name) {
		return 0
	}

	return 1
}

// direntAt decodes the dirent at index i of a block, its name resolved as
// Dirents resolves it: to the next entry's offset, or the block's used
// part for the last.
func direntAt(block []byte, i int64) Dirent {
	entries, err := direntsOfBlock(block, int64(len(block)))
	if err != nil || int(i) >= len(entries) {
		de := block[i*DirentSize:]

		return Dirent{Nid: binary.LittleEndian.Uint64(de), Type: de[10]}
	}

	return entries[i]
}

// Resolve walks a slash-separated path from the root, following symlinks
// as the VFS does (fs/namei.c): a target starting with "/" restarts from
// the root, any other continues from the link's directory, and a walk that
// follows more than maxLinks links fails with ErrLoop. Linux's own bound is
// 40 (MAXSYMLINKS). A trailing symlink is followed too. The result is the
// inode the path names.
func (im *Image) Resolve(path string, maxLinks int) (*Inode, error) {
	root, err := im.Inode(im.Super.RootNid)
	if err != nil {
		return nil, err
	}

	if !root.IsDir() {
		return nil, fmt.Errorf("%w: root inode %d", ErrNotDir, root.Nid)
	}

	w := &walker{im: im, root: root, maxLinks: maxLinks}

	return w.walk(root, path)
}

type walker struct {
	im       *Image
	root     *Inode
	maxLinks int
	links    int
}

// walk resolves rest against dir: one component at a time, a symlink in
// the middle or at the end is read and its target walked in its turn.
func (w *walker) walk(dir *Inode, rest string) (*Inode, error) {
	cur := dir

	for rest != "" {
		var name string

		name, rest, _ = strings.Cut(rest, "/")
		if name == "" || name == "." {
			continue
		}

		if !cur.IsDir() {
			return nil, fmt.Errorf("%w: inode %d", ErrNotDir, cur.Nid)
		}

		de, err := w.im.Lookup(cur, []byte(name))
		if err != nil {
			return nil, err
		}

		next, err := w.im.Inode(de.Nid)
		if err != nil {
			return nil, err
		}

		if next.IsSymlink() {
			next, err = w.followLink(cur, next)
			if err != nil {
				return nil, err
			}
		}

		cur = next
	}

	return cur, nil
}

func (w *walker) followLink(dir, link *Inode) (*Inode, error) {
	w.links++
	if w.links > w.maxLinks {
		return nil, fmt.Errorf("%w: more than %d links", ErrLoop, w.maxLinks)
	}

	target, err := w.im.Target(link)
	if err != nil {
		return nil, err
	}

	base := dir
	if len(target) > 0 && target[0] == '/' {
		base = w.root
	}

	return w.walk(base, string(target))
}

// Entry is one node of a walk: the path from the root, the dirent that
// names it and its inode.
type Entry struct {
	Path   string
	Dir    Dirent
	Inode  *Inode
	Parent uint64 // the nid of the directory that holds the dirent
}

// Walk visits every entry reachable from the root through directories,
// depth first, parents before children, in the order the dirents are
// stored, skipping "." and "..". A directory already visited is not
// descended again, so a cyclic image terminates; fn's error stops the
// walk, except ErrSkipDir, which skips the directory's children.
func (im *Image) Walk(fn func(Entry) error) error {
	root, err := im.Inode(im.Super.RootNid)
	if err != nil {
		return err
	}

	if !root.IsDir() {
		return fmt.Errorf("%w: root inode %d", ErrNotDir, root.Nid)
	}

	seen := map[uint64]bool{root.Nid: true}

	return im.walkDir(root, "", seen, fn)
}

func (im *Image) walkDir(dir *Inode, prefix string, seen map[uint64]bool, fn func(Entry) error) error {
	entries, err := im.Dirents(dir)
	if err != nil {
		return err
	}

	for _, de := range entries {
		if string(de.Name) == "." || string(de.Name) == ".." {
			continue
		}

		if err := im.walkEntry(dir, de, prefix, seen, fn); err != nil {
			return err
		}
	}

	return nil
}

// walkEntry visits one dirent and descends into it when it is a directory
// not yet seen.
func (im *Image) walkEntry(dir *Inode, de Dirent, prefix string, seen map[uint64]bool, fn func(Entry) error) error {
	in, err := im.Inode(de.Nid)
	if err != nil {
		return err
	}

	path := prefix + string(de.Name)

	err = fn(Entry{Path: path, Dir: de, Inode: in, Parent: dir.Nid})
	if errors.Is(err, ErrSkipDir) || err != nil {
		if errors.Is(err, ErrSkipDir) {
			return nil
		}

		return err
	}

	if in.IsDir() && !seen[in.Nid] {
		seen[in.Nid] = true

		return im.walkDir(in, path+"/", seen, fn)
	}

	return nil
}

// CheckDir applies fsck.erofs's directory rules on top of the kernel's
// (erofs-utils lib/dir.c, traverse_dirents with fsck set): names are packed
// back to back in entry order, strictly increasing bytewise, no name holds
// "/" or NUL, every file type is below the maximum, "." names the directory
// itself and ".." its parent (the root for the root), each present exactly
// once unless the directory omits the dot. It returns every violation.
func (im *Image) CheckDir(dir *Inode, parent uint64, entries []Dirent) []error {
	var errs []error

	if dir.Nid == im.Super.RootNid {
		parent = dir.Nid
	}

	dot, dotdot := dir.DotOmitted, false

	for i, de := range entries {
		if i > 0 && bytes.Compare(entries[i-1].Name, de.Name) >= 0 {
			errs = append(errs, fmt.Errorf("%w: name %q not after %q", ErrCorrupt, de.Name, entries[i-1].Name))
		}

		if de.Type >= typeMax {
			errs = append(errs, fmt.Errorf("%w: %q file type %d", ErrCorrupt, de.Name, de.Type))
		}

		switch string(de.Name) {
		case ".":
			errs = appendIf(errs, dot, fmt.Errorf("%w: duplicated or omitted \".\"", ErrCorrupt))
			errs = appendIf(
				errs,
				de.Nid != dir.Nid,
				fmt.Errorf("%w: \".\" names %d, not %d", ErrCorrupt, de.Nid, dir.Nid),
			)
			dot = true
		case "..":
			errs = appendIf(errs, dotdot, fmt.Errorf("%w: duplicated \"..\"", ErrCorrupt))
			errs = appendIf(
				errs,
				de.Nid != parent,
				fmt.Errorf("%w: \"..\" names %d, not %d", ErrCorrupt, de.Nid, parent),
			)
			dotdot = true
		default:
			errs = appendIf(
				errs,
				bytes.ContainsAny(de.Name, "/\x00"),
				fmt.Errorf("%w: illegal filename %q", ErrCorrupt, de.Name),
			)
		}
	}

	errs = appendIf(errs, !dot || !dotdot, fmt.Errorf("%w: \".\" or \"..\" missing", ErrCorrupt))

	return errs
}

func appendIf(errs []error, cond bool, err error) []error {
	if cond {
		return append(errs, err)
	}

	return errs
}
