package erofs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"slices"

	"github.com/forkcloser/go-erofs/internal/builder"
	"github.com/forkcloser/go-erofs/internal/disk"
)

// maxDirBytes is the most a directory of a source image may declare as its
// entries: a copy reads them whole, into one buffer i_size sizes.
const maxDirBytes = 1 << 30 // 1 GiB

// copyWindow is the unit copyFromImage reads its source in: one read takes
// a window, and twice the read before it, up to copyAhead, when the reads
// follow each other. copyHeld is how much of the source a copy holds: what
// it reads it keeps, so nothing is read twice, and past copyHeld what the
// tree names is read piece by piece.
const (
	copyWindow = 64 << 10
	copyAhead  = 4 << 20
	copyHeld   = 1 << 30
)

// copyPage is how many windows a page of imageWindows' table finds: 32 MiB
// of the image, in 12 KiB.
const copyPage = 1 << 9

// maxQueuePrealloc caps the BFS queue's initial capacity. This is only a
// sizing hint — the queue still grows to whatever the image legitimately
// needs — so capping it costs nothing and keeps a corrupt inode count from
// requesting an enormous allocation up front.
const maxQueuePrealloc = 1 << 16

// minQueuePrealloc is the BFS queue's smallest initial capacity.
const minQueuePrealloc = 64

// imageSize reports the readable size of an image, when the io.ReaderAt can
// say. *bytes.Reader, *io.SectionReader and *os.File all can, which covers
// every way this package is realistically used. Callers must treat a false
// result as "unknown", not as "empty".
func imageSize(ra io.ReaderAt) (int64, bool) {
	switch v := ra.(type) {
	case interface{ Size() int64 }:
		return v.Size(), true
	case interface{ Stat() (fs.FileInfo, error) }:
		fi, err := v.Stat()
		if err != nil {
			return 0, false
		}

		return fi.Size(), true
	}

	return 0, false
}

// imageWindows is copyFromImage's view of its source: the image read in
// windows of copyWindow bytes, each when a byte of it is first asked for
// and never again. The inodes of a tree are in the image where its writer
// put them, among the data in one mkfs.erofs laid out, so what a copy reads
// is what the tree names and not a region; and it reads them in the order
// the tree lists them, which is not the image's, so what it read it keeps.
//
// So no byte of the image is read twice through the windows, whatever the
// order: letting a window go to hold fewer of them is reading it again
// when the walk comes back, a window for each name of an image laid out
// for it. What a copy reads beside them it reads as the reader does, at
// its size and where the tree puts it: the blocks of a directory's
// entries and of a link's target.
type imageWindows struct {
	ra io.ReaderAt
	// total is the image's length by its superblock, which the caller held
	// to the source's own.
	total int64
	// pages find the windows held, window idx in page idx/copyPage, a page
	// made when a window of it is first held: a held window is a part of
	// the read that took it, its capacity the rest of that read, and one
	// not held is empty. held is their bytes, at most room.
	pages []*[copyPage][]byte
	held  int64
	room  int64
	// The last read took run windows and ended on window last. A read of
	// the window after it takes twice as many: where the tree's metadata
	// is all there is, the image is read in few reads and not one a window.
	last, run int64
}

func newImageWindows(ra io.ReaderAt, total, room int64) *imageWindows {
	return &imageWindows{ra: ra, total: total, room: room}
}

// heldWindow is window idx when it is held, and empty when it is not.
func (w *imageWindows) heldWindow(idx int64) []byte {
	if at := idx / copyPage; at < int64(len(w.pages)) {
		if page := w.pages[at]; page != nil {
			return page[idx%copyPage]
		}
	}

	return nil
}

// hold keeps win as window idx.
func (w *imageWindows) hold(idx int64, win []byte) {
	at := idx / copyPage
	for int64(len(w.pages)) <= at {
		w.pages = append(w.pages, nil)
	}

	page := w.pages[at]
	if page == nil {
		page = new([copyPage][]byte)
		w.pages[at] = page
	}

	page[idx%copyPage] = win
}

// span is the n bytes at off: the windows' own bytes where one read took
// them all, and a copy of them where more than one did. A window the range
// runs through and that is not held is read, and those after it that the
// range needs with it. Once the windows held leave no room for one of them
// the range is read on its own, at its size, each time it is asked for.
func (w *imageWindows) span(off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || n > w.total || off > w.total-n {
		return nil, fmt.Errorf("%d bytes at offset %d lie outside the %d byte image: %w",
			n, off, w.total, ErrCorrupt)
	}

	if n == 0 {
		return []byte{}, nil
	}

	idx := off / copyWindow
	at := off - idx*copyWindow
	end := (off+n-1)/copyWindow + 1

	win, err := w.window(idx, end-idx)
	if err != nil {
		return nil, err
	}

	if len(win) == 0 {
		return w.alone(off, n)
	}

	// A window's capacity runs to the end of the read that took it.
	if at+n <= int64(cap(win)) {
		return win[at : at+n : at+n], nil
	}

	whole := make([]byte, n)
	got := int64(copy(whole, win[at:]))

	for next := idx + 1; got < n; next++ {
		win, err = w.window(next, end-next)
		if err != nil {
			return nil, err
		}

		if len(win) == 0 {
			return w.alone(off, n)
		}

		got += int64(copy(whole[got:], win))
	}

	return whole, nil
}

// window is window idx, read if it is not held, with the windows after it
// that the caller needs, need in all; empty when it is not held and no
// room is left for it.
func (w *imageWindows) window(idx, need int64) ([]byte, error) {
	if win := w.heldWindow(idx); len(win) > 0 {
		return win, nil
	}

	if w.held+min(copyWindow, w.total-idx*copyWindow) > w.room {
		return nil, nil
	}

	return w.load(idx, need)
}

// alone reads the n bytes at off into a buffer of their own.
func (w *imageWindows) alone(off, n int64) ([]byte, error) {
	buf := make([]byte, n)
	if err := w.read(buf, off); err != nil {
		return nil, err
	}

	return buf, nil
}

// load reads window idx, which is not held and which there is room for,
// and with it the windows after it that the caller needs, need in all, or
// that a run of reads has earned if they are more, short of one already
// held, of the image's end and of the room left.
func (w *imageWindows) load(idx, need int64) ([]byte, error) {
	want := need
	if w.run > 0 && idx == w.last+1 {
		want = max(need, min(2*w.run, copyAhead/copyWindow))
	}

	start := idx * copyWindow

	count := int64(1)
	for count < want && start+count*copyWindow < w.total {
		if len(w.heldWindow(idx+count)) > 0 {
			break
		}

		if w.held+min((count+1)*copyWindow, w.total-start) > w.room {
			break
		}

		count++
	}

	buf := make([]byte, min(count*copyWindow, w.total-start))
	if err := w.read(buf, start); err != nil {
		return nil, err
	}

	for i := range count {
		from := i * copyWindow
		w.hold(idx+i, buf[from:min(from+copyWindow, int64(len(buf)))])
	}

	w.held += int64(len(buf))
	w.last, w.run = idx+count-1, count

	return buf[:min(copyWindow, int64(len(buf)))], nil
}

// read fills buf from off. A reader may say io.EOF with its last bytes; one
// that ends before buf is full holds less than the superblock declares.
func (w *imageWindows) read(buf []byte, off int64) error {
	switch n, err := w.ra.ReadAt(buf, off); {
	case n == len(buf):
		return nil
	case err != nil && !errors.Is(err, io.EOF):
		return err
	default:
		return fmt.Errorf("the image ends %d bytes into the %d at offset %d, inside the %d its superblock declares: %w",
			n, len(buf), off, w.total, ErrCorrupt)
	}
}

// endsWhereDeclared holds a source that reports no length to the length its
// superblock declares, by its last byte: the copy reads what the tree
// names, and a source that ends short of the image is one whatever of it
// the tree names.
func endsWhereDeclared(ra io.ReaderAt, total int64) error {
	var last [1]byte

	switch n, err := ra.ReadAt(last[:], total-1); {
	case n == len(last):
		return nil
	case err != nil && !errors.Is(err, io.EOF):
		return err
	default:
		return fmt.Errorf("superblock declares %d bytes and the image ends before them: %w", total, ErrCorrupt)
	}
}

// imgQEntry is a BFS queue entry for the image metadata walk.
type imgQEntry struct {
	nid  uint64
	path string
	// ftype is the dirent's file type; the root has none.
	ftype uint8
}

// copyFromImage is a fast path for CopyFrom when the source is an *image.
// Instead of walking via the fs.FS interface (which does per-inode ReadAt
// syscalls), it reads the image in windows (imageWindows) and parses
// inodes, directory entries, xattrs, and chunk indexes directly from them.
//
//nolint:gocognit // the BFS over the source's inodes, each decoded field by field; the fast path bench_test.go's merge benchmarks measure
func (fsys *Writer) copyFromImage(img *image) error {
	metaStart := img.metaStartPos()

	// The reader opens an image declaring no blocks, its length bounding
	// it: a tree it reads, refused here rather than copied as empty.
	totalBytes := int64(img.sb.Blocks) << img.sb.BlkSizeBits
	if totalBytes <= 0 {
		return fmt.Errorf("superblock declares %d blocks, which hold no metadata: %w", img.sb.Blocks, ErrCorrupt)
	}

	// sb.Blocks, sb.MetaBlkAddr and sb.Inos are read verbatim from the image
	// and must never size an allocation on their own: a handful of tampered
	// bytes would otherwise request a terabyte-scale buffer, which fails as
	// an unrecoverable runtime OOM rather than a returned error.
	if actual, ok := imageSize(img.meta); ok {
		if totalBytes > actual {
			return fmt.Errorf("superblock declares %d bytes but image is %d bytes: %w",
				totalBytes, actual, ErrCorrupt)
		}
	} else if err := endsWhereDeclared(img.meta, totalBytes); err != nil {
		return err
	}

	if metaStart < 0 || metaStart >= totalBytes {
		return fmt.Errorf("metadata start %d outside image of %d bytes: %w",
			metaStart, totalBytes, ErrCorrupt)
	}

	blkBits := img.sb.BlkSizeBits
	buildTime := img.sb.BuildTime
	buildTimeNs := img.sb.BuildTimeNs

	blockSize := int(1 << blkBits)

	// Nothing of the image is read before the walk asks for it: sb.Blocks
	// sizes no buffer, whatever it declares and whatever the source's length.
	span := newImageWindows(img.meta, totalBytes, copyHeld).span

	// Shared xattr block address (if present): read when an inode names an
	// entry of it.
	var sharedXattrOff int64
	if img.sb.XattrBlkAddr > 0 {
		sharedXattrOff = int64(img.sb.XattrBlkAddr) << blkBits
	}

	// Pre-allocate based on the inode count from the superblock, bounded by
	// what the metadata region can physically hold (one inode per 32-byte
	// slot at minimum) and by maxQueuePrealloc.
	maxInodes := (totalBytes - metaStart) / disk.SizeInodeCompact

	// #nosec G115 -- a count past 1<<63 wraps negative, which the next line replaces
	inodeCount := int64(img.sb.Inos)
	if inodeCount <= 0 || inodeCount > maxInodes {
		inodeCount = maxInodes
	}

	if inodeCount > maxQueuePrealloc {
		inodeCount = maxQueuePrealloc
	}

	if inodeCount < minQueuePrealloc {
		inodeCount = minQueuePrealloc
	}

	queue := make([]imgQEntry, 0, inodeCount)
	queue = append(queue, imgQEntry{nid: uint64(img.sb.RootNid), path: "/"})

	// Directory nids are expanded at most once. EROFS, like POSIX, has no
	// directory hardlinks, so a directory reachable by two paths means the
	// dirent graph is not a tree. Following it would revisit the same
	// subtree forever, growing both the queue and the path strings without
	// bound. Non-directory nids are handled separately: a file nid
	// legitimately appears under several names when the source has
	// hardlinks, and each name after the first becomes an alias of it.
	expanded := make(map[int64]struct{})

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if fsys.copyMerge && fsys.marker(cur.path) {
			continue
		}

		// Bound the nid before multiplying. nid*32 wraps, so nid and
		// nid + k*2^59 name the same inode: the cycle guard below keyed on
		// the nid would see 32 distinct directories where the image has one,
		// and expand the same subtree under each.
		// #nosec G115 -- metaStart < totalBytes was checked on entry, so the difference is positive
		if cur.nid > uint64(totalBytes-metaStart)/disk.SizeInodeCompact {
			return fmt.Errorf("nid %d lies past the end of the %d byte image: %w",
				cur.nid, totalBytes, ErrCorrupt)
		}

		inodeAddr := metaStart + int64(cur.nid*disk.SizeInodeCompact) // #nosec G115 -- nid bounded just above

		buf, err := span(inodeAddr, disk.SizeInodeCompact)
		if err != nil {
			return fmt.Errorf("inode %d out of range: %w", cur.nid, err)
		}

		format := binary.LittleEndian.Uint16(buf[:2])
		layout := uint8((format & disk.InodeFormatLayoutMask) >> 1)
		compact := format&disk.InodeFormatExtended == 0

		if !compact {
			buf, err = span(inodeAddr, disk.SizeInodeExtended)
			if err != nil {
				return fmt.Errorf("extended inode %d out of range: %w", cur.nid, err)
			}
		}

		var (
			mode    uint16
			uid     uint32
			gid     uint32
			nlink   uint32
			size    uint64
			idata   uint32
			startHi uint16
			addr48  bool
			mtime   uint64
			mtimeNs uint32
			xcnt    uint16
			icSize  int
		)

		if compact {
			var ino disk.InodeCompact
			ino.Unmarshal(buf)
			mode = ino.Mode
			uid = uint32(ino.UID)
			gid = uint32(ino.GID)
			nlink = uint32(ino.NB)
			size = uint64(ino.Size)
			idata = ino.InodeData
			// The superblock's epoch plus i_mtime, as the kernel reads a
			// compact inode's time.
			mtime = buildTime + uint64(ino.Mtime)
			mtimeNs = buildTimeNs
			xcnt = ino.XattrCount
			icSize = disk.SizeInodeCompact

			// On a compact non-directory with the nlink-1 bit the link count
			// is one and i_nb is the start block's high 16 bits (inode.c:
			// erofs_read_inode); otherwise i_nb is the link count.
			if mode&disk.StatTypeMask != disk.StatTypeDir && format&disk.InodeFormatNlink1 != 0 {
				nlink = 1
				startHi = ino.NB
				addr48 = true
			}
		} else {
			var ino disk.InodeExtended
			ino.Unmarshal(buf)
			mode = ino.Mode
			uid = ino.UID
			gid = ino.GID
			nlink = ino.Nlink
			size = ino.Size
			idata = ino.InodeData
			mtime = ino.Mtime
			mtimeNs = ino.MtimeNs
			xcnt = ino.XattrCount
			icSize = disk.SizeInodeExtended
			// An extended inode's start block is 48 bits wide, i_nb its
			// high 16 (inode.c: erofs_read_inode).
			startHi = ino.NB
			addr48 = true
		}

		// The start block as the kernel composes it; judged where a block
		// is read by it, as Stat judges it.
		startBlk := uint64(idata) | uint64(startHi)<<startBlockBits32

		// What the reader refuses, the copy refuses, in the reader's class:
		// a copy of an inode Stat would not serve is no copy of the image.
		if err := checkInodeFormat(cur.nid, format); err != nil {
			return err
		}

		if err := checkChunkFormat(cur.nid, layout, idata); err != nil {
			return err
		}

		if xcnt > 0 {
			if _, err := xattrBodySize(cur.nid, xcnt); err != nil {
				return err
			}
		}

		// The kernel refuses an i_mode whose type bits name no type at iget
		// ("bogus i_mode"), and the reader refuses a dirent whose type
		// disagrees with its inode's; so does the copy, in Stat's class.
		if modeToFileType(mode) == 0 {
			return fmt.Errorf("nid %d: bogus i_mode %#o: %w", cur.nid, mode, ErrCorrupt)
		}

		// The root has no dirent to agree with: the kernel refuses a root
		// inode that is not a directory at mount (super.c, "rootino(nid %llu)
		// is not a directory").
		if cur.path == "/" && modeToFileType(mode) != disk.FileTypeDir {
			return fmt.Errorf("root inode %d has type %#o, not a directory: %w",
				cur.nid, mode&disk.StatTypeMask, ErrCorrupt)
		}

		if cur.path != "/" && cur.ftype != modeToFileType(mode) {
			return fmt.Errorf("inode %d has type %#o but its dirent says file type %d: %w",
				cur.nid, mode&disk.StatTypeMask, cur.ftype, ErrCorrupt)
		}

		// i_size is unsigned on disk and an int64 everywhere the Writer
		// reports it: its fs.FileInfo.Size, the readers its Open returns. The
		// reader refuses a size past 1<<63, and so must the copy, or the entry
		// reports a negative size until Close.
		if size > math.MaxInt64 {
			return fmt.Errorf("nid %d declares a size of %d bytes: %w", cur.nid, size, ErrCorrupt)
		}

		typ := mode & disk.StatTypeMask

		// A nid already registered under an earlier name in this copy is
		// the same inode: give it another name, as Link would, rather than
		// a second copy of its metadata and chunks. Directories are never
		// shared (BFS expands each once), and a link count of 1 says there
		// is nothing to share. Told before its attributes are read: they
		// were, for the first name, and a file of many names has them read
		// once.
		if typ != disk.StatTypeDir && nlink > 1 {
			if prior, ok := fsys.copyLinks[hardlinkKey{0, cur.nid}]; ok {
				if err := fsys.addAlias(cur.path, prior); err != nil {
					return err
				}

				continue
			}
		}

		// Parse xattr area.
		xattrSize := 0
		if xcnt > 0 {
			xattrSize = int(xcnt-1)*disk.SizeXattrEntry + disk.SizeXattrBodyHeader
		}

		var xattrs []xattr

		if xattrSize > 0 {
			if err := img.xattrsListable(cur.nid); err != nil {
				return err
			}

			xb, err := span(inodeAddr+int64(icSize), int64(xattrSize))
			if err != nil {
				return fmt.Errorf("xattrs for nid %d: %w", cur.nid, err)
			}

			fsys.xattrScratch, err = parseXattrsFromBuf(
				fsys.xattrScratch[:0], xb, span, sharedXattrOff, img.listedPrefix,
			)
			if err != nil {
				return fmt.Errorf("xattrs for nid %d: %w", cur.nid, err)
			}
			// As for any source: a name the Writer could only store hidden
			// (an ACL's with something after it is the one an image can
			// still list) is left out.
			xattrs = fsys.heldXattrs(listedXattrs(fsys.xattrScratch))
		}

		trailingAddr := inodeAddr + int64(icSize) + int64(xattrSize)

		// Build fsEntry directly, bypassing builder.Entry + add() overhead.
		fe := &fsEntry{
			path:    cur.path,
			mode:    mode,
			uid:     uid,
			gid:     gid,
			mtime:   mtime,
			mtimeNs: mtimeNs,
			size:    size,
			xattrs:  xattrs,
		}

		fe.fileClosed = true
		if fsys.copyMetadataOnly {
			fe.metadataOnly = true
		}

		// Sizes come straight off disk. For the types whose content this
		// function materializes, the declared size must fit inside the image
		// before it is used as an allocation length — or converted to int,
		// where a value past 2^63 overflows to a negative length.
		// A flat-inline inode keeps its whole blocks at i_u and its last
		// block, a whole one when the size is a multiple, beside the inode
		// (the kernel's lastblk is one short of the block count); that tail
		// lives in the inode's own block and cannot run past it. The
		// reader's loadBlock refuses that outright, and without the same
		// rule here the copy would take whatever follows the block as the
		// directory's entries or the link's target.
		// A device, a FIFO and a socket have no content: their size and
		// their layout are fields nothing reads by, which the kernel and
		// the reader serve whatever they hold, and the copy keeps.
		readWhole := typ == disk.StatTypeDir || typ == disk.StatTypeSymlink

		tail := int64(0)
		if (readWhole || typ == disk.StatTypeReg) && layout == disk.LayoutFlatInline && size > 0 {
			// #nosec G115 -- size is at most math.MaxInt64, checked above
			tail = int64(size) - (int64(size)-1)/int64(blockSize)*int64(blockSize)
		}

		if readWhole && size > uint64(totalBytes) {
			return fmt.Errorf("nid %d declares %d bytes, larger than the %d byte image: %w",
				cur.nid, size, totalBytes, ErrCorrupt)
		}

		// i_size sizes the buffer a directory or a link's target is read
		// into whole, and an image whose source reports no length is as long
		// as its superblock says. One directory gets the budget the whole
		// metadata region has. A target is bounded as the copy with its data
		// bounds it: the reader's page, then the writer's one byte less,
		// which the reader reads and Symlink refuses.
		if typ == disk.StatTypeDir && size > maxDirBytes {
			return fmt.Errorf("directory nid %d declares %d bytes, over the %d byte limit: %w",
				cur.nid, size, int64(maxDirBytes), ErrInvalid)
		}

		if typ == disk.StatTypeSymlink && size > maxSymlinkSize {
			return fmt.Errorf("nid %d: symlink target size %d out of range: %w", cur.nid, size, ErrCorrupt)
		}

		if typ == disk.StatTypeSymlink && size > maxSymlinkTarget {
			return fmt.Errorf("nid %d: symlink target is %d bytes, over the %d byte limit: %w",
				cur.nid, size, maxSymlinkTarget, ErrInvalid)
		}

		if trailingAddr%int64(blockSize)+tail > int64(blockSize) {
			return fmt.Errorf("inline data crosses block boundary for nid %d: %w", cur.nid, ErrCorrupt)
		}

		// inlineData reads a directory's entries or a symlink's target: flat
		// data is the whole blocks at i_u, then the tail beside the inode. In
		// chunks i_u is the chunk format, never a start block, and the data
		// is where the reader's map puts it.
		inlineData := func(size int64) ([]byte, error) {
			if layout == disk.LayoutChunkBased {
				ftype := fs.ModeSymlink
				if typ == disk.StatTypeDir {
					ftype = fs.ModeDir
				}

				data := make([]byte, size)
				if err := readAll(&file{img: img, name: cur.path, nid: cur.nid, ftype: ftype}, data); err != nil {
					return nil, err
				}

				return data, nil
			}

			if layout == disk.LayoutFlatPlain {
				pa, err := img.blockAddr(cur.nid, startBlk)
				if err != nil {
					return nil, err
				}

				return img.readFlat(pa, size)
			}

			if size == tail {
				return span(trailingAddr, tail)
			}

			pa, err := img.blockAddr(cur.nid, startBlk)
			if err != nil {
				return nil, err
			}

			blocks, err := img.readFlat(pa, size-tail)
			if err != nil {
				return nil, err
			}

			end, err := span(trailingAddr, tail)
			if err != nil {
				return nil, err
			}

			return slices.Concat(blocks, end), nil
		}

		switch typ {
		case disk.StatTypeDir:
			// A directory reachable twice means the dirent graph is not a
			// tree; expanding it again would loop forever. Keyed on the
			// address rather than the nid, since two nids differing only in
			// the bits nid*32 discards address the same inode.
			if _, seen := expanded[inodeAddr]; seen {
				return fmt.Errorf("directory nid %d is reachable more than once (at %s): %w",
					cur.nid, cur.path, ErrCorrupt)
			}

			expanded[inodeAddr] = struct{}{}

			dirSize := int(size) // #nosec G115 -- checked against totalBytes, an int64, above
			if dirSize > 0 {
				dirData, err := inlineData(int64(dirSize))
				if err != nil {
					return fmt.Errorf("read dir data for nid %d: %w", cur.nid, err)
				}

				listed := len(queue)

				if err := fsys.parseDirBlock(dirData, dirSize, blockSize, cur.path, &queue); err != nil {
					return fmt.Errorf("dir nid %d: %w", cur.nid, err)
				}

				// The copy with its data refuses this too; here the second
				// entry would replace the first under its path.
				if twice, ok := listedTwice(queue[listed:]); ok {
					return fmt.Errorf("dir nid %d lists %s twice: %w", cur.nid, twice, ErrCorrupt)
				}
			}

		case disk.StatTypeSymlink:
			// The reader refuses the inode (erofs_fill_symlink's rule, an
			// empty fast symlink included); so does the copy, in the same class.
			if layout == disk.LayoutFlatInline && size == 0 {
				return fmt.Errorf("nid %d: invalid fast symlink size 0: %w", cur.nid, ErrCorrupt)
			}

			if size > 0 {
				// #nosec G115 -- checked against totalBytes, an int64, above
				linkData, err := inlineData(int64(size))
				if err != nil {
					return fmt.Errorf("read symlink data for nid %d: %w", cur.nid, err)
				}

				if bytes.IndexByte(linkData, 0) >= 0 {
					return fmt.Errorf("nid %d: invalid fast symlink target: %w", cur.nid, ErrCorrupt)
				}

				fe.linkTarget = string(linkData)
			}

		case disk.StatTypeReg:
			// Flat data stays in the source image, now a device of the
			// copy: whole blocks become chunks on it; a flat-inline tail is
			// copied, since no chunk can name part of a block. A flat-plain
			// file at the null address has no data and reads as zeros, as
			// chunks without mappings do.
			if layout != disk.LayoutChunkBased && size > 0 {
				// The blocks at i_u: all of a flat-plain file's, the last one
				// partial; a flat-inline file's whole ones, its last block
				// being the tail.
				// #nosec G115 -- size is at most math.MaxInt64, checked above
				whole := uint64((int64(size) - tail + int64(blockSize) - 1) >> blkBits)
				// A flat file is copied in as many blocks as the writer
				// takes index entries: what its index costs when no chunk
				// size above a block fits its runs. And a file at the null
				// address is as large as its i_size says (checkFlatHole).
				if whole > maxChunkIndexEntries {
					return fmt.Errorf("nid %d: a flat file of %d bytes is past the %d blocks a copy takes: %w",
						cur.nid, size, int64(maxChunkIndexEntries), ErrInvalid)
				}

				switch {
				case whole == 0:
				case layout == disk.LayoutFlatPlain && nullStartBlock(startBlk, addr48):
					// The kernel maps a flat-plain file at the null address
					// as a hole (erofs_map_blocks); a flat-inline file's
					// whole blocks it reads at the address whatever it is,
					// the null address past every device. A tail after the
					// hole keeps its place only if the hole is spelled out.
					fe.chunks = deviceChunks(builder.NullPhysicalBlock, whole, 0)
				default:
					// Bounded as Stat bounds the file's blocks (blockAddr,
					// locateRange), so the copy refuses what Stat refuses
					// rather than naming blocks past every device.
					pa, err := img.blockAddr(cur.nid, startBlk)
					if err != nil {
						return fmt.Errorf("nid %d: %w", cur.nid, err)
					}

					if _, _, err := img.locateRange(pa, int64(size)-tail); err != nil {
						return fmt.Errorf("nid %d: %w", cur.nid, err)
					}

					fe.chunks = img.flatChunks(startBlk, whole)
					fe.contiguous = tail == 0 && len(fe.chunks) == 1

					if err := fsys.remapChunkDevices(cur.path, fe.chunks); err != nil {
						return err
					}
				}

				if tail > 0 {
					// Not read here: it is the file's data, and the source's
					// until Close. Held to the image all the same: after an
					// inode that ends a block, the image's last one too, the
					// tail is the block that follows.
					if trailingAddr > totalBytes-tail {
						return fmt.Errorf(
							"inline data for nid %d: %d bytes at offset %d lie outside the %d byte image: %w",
							cur.nid,
							tail,
							trailingAddr,
							totalBytes,
							ErrCorrupt,
						)
					}

					fe.tail = inlineTail{img: img, at: trailingAddr, n: int(tail)}
				}
			}

			if layout == disk.LayoutChunkBased && size > 0 {
				// #nosec G115 -- the chunk format is the low 16 bits of i_u, as the kernel's __le16 c.format reads it
				chunkFmt := uint16(idata)
				indexed := chunkFmt&disk.LayoutChunkFormatIndexes != 0
				chunkAddr := trailingAddr

				unit := int64(4)
				if indexed {
					unit = disk.SizeChunkIndex

					if chunkAddr%8 != 0 {
						chunkAddr = (chunkAddr + 7) & ^int64(7)
					}
				}

				// The declared size must be backed by a complete chunk-index
				// map inside the image. Without this an inode can claim a
				// petabyte-scale file, and the size flows through
				// calcTrailingSize into the in-memory metadata buffer, which
				// then grows without bound. Both chunk formats are checked:
				// only the indexed one yields chunks, but an unindexed inode
				// carries the same untrusted size.
				need, err := chunkMapBytes(chunkFmt, size, blkBits, unit)
				if err != nil {
					return fmt.Errorf("nid %d: %w", cur.nid, err)
				}

				index, err := span(chunkAddr, need)
				if err != nil {
					return fmt.Errorf("chunk index of a %d byte file for nid %d: %w", size, cur.nid, err)
				}

				// Parsed once per inode: a later name for this nid becomes
				// an alias above and never reaches here. A block map names
				// blocks of the image itself, device 0 before the remap.
				var chunks []builder.Chunk

				if indexed {
					chunks, err = fsys.parseChunks(index, chunkFmt, size, blkBits, img.deviceIDMask, img.locateChunk)
				} else {
					chunks, err = parseBlockMap(index, chunkFmt, size, blkBits, img.locateChunk)
				}

				if err != nil {
					return fmt.Errorf("chunk index for nid %d: %w", cur.nid, err)
				}

				if err := fsys.remapChunkDevices(cur.path, chunks); err != nil {
					return err
				}

				fe.chunks = chunks
				// Only a single non-hole extent is contiguous. Claiming
				// it for a fragmented or sparse file makes planLayout
				// pick a chunk size spanning the whole file, collapsing
				// every extent into one mapping.
				fe.contiguous = len(chunks) == 1 &&
					chunks[0].PhysicalBlock != builder.NullPhysicalBlock
			}

		case disk.StatTypeChrdev, disk.StatTypeBlkdev:
			fe.rdev = disk.RdevFromMode(mode, idata)
		}

		// Register in the tree.
		if cur.path == "/" {
			// Update root metadata.
			fsys.root.mode = fe.mode
			fsys.root.uid = fe.uid
			fsys.root.gid = fe.gid
			fsys.root.mtime = fe.mtime
			fsys.root.mtimeNs = fe.mtimeNs
			fsys.root.xattrs = fe.xattrs
		} else {
			if typ == disk.StatTypeReg && size > 0 {
				name := cur.path[1:]
				fe.reopen = func() (fs.File, error) { return img.Open(name) }
			}

			fe = fsys.placeEntry(fe)
			if typ != disk.StatTypeDir && nlink > 1 {
				fsys.copyLinks[hardlinkKey{0, cur.nid}] = fe
			}
		}
	}

	return nil
}

// listedTwice reports a path that two of a directory's entries have. The
// entries of a well-formed directory ascend by name, block after block,
// and are told apart in one pass; only one out of order is searched.
func listedTwice(children []imgQEntry) (string, bool) {
	ascending := true

	for i := 1; i < len(children) && ascending; i++ {
		ascending = children[i-1].path < children[i].path
	}

	if ascending {
		return "", false
	}

	seen := make(map[string]struct{}, len(children))
	for _, c := range children {
		if _, twice := seen[c.path]; twice {
			return c.path, true
		}

		seen[c.path] = struct{}{}
	}

	return "", false
}

// parseDirBlock extracts directory entries from dirent data and enqueues
// child inodes for BFS traversal.
//
//nolint:gocognit // the dirent decode, applying the reader's ReadDir rules block by block
func (*Writer) parseDirBlock(data []byte, dirSize, blockSize int, parentPath string, queue *[]imgQEntry) error {
	pos := 0
	for pos < dirSize {
		blockEnd := min(pos+blockSize, dirSize)

		blk := data[pos:blockEnd]
		// A block shorter than one dirent holds no first name offset: the
		// kernel's readdir ends in "bogus dirent" on it and the reader's
		// ReadDir refuses it, so the copy refuses it in the same class
		// rather than copying a directory short of its entries.
		if len(blk) < disk.SizeDirent {
			return fmt.Errorf("in %s: directory block of %d bytes: %w", parentPath, len(blk), ErrCorrupt)
		}

		// The same rules as the reader's ReadDir: the first entry's name
		// offset fixes the entry count and must lie inside the block, every
		// name range must ascend and stay inside the block, and only the
		// last name in a block may be NUL-padded. A dirent that fails is an
		// error, not an entry to step over — the reader would refuse it.
		firstNameOff := int(binary.LittleEndian.Uint16(blk[8:10]))

		// Zero, past the block, or off a dirent boundary: the kernel's
		// readdir refuses each (dir.c, "invalid de[0].nameoff"), and so
		// does the reader's ReadDir.
		nEntries := firstNameOff / disk.SizeDirent
		if nEntries == 0 || firstNameOff > len(blk) || firstNameOff%disk.SizeDirent != 0 {
			return fmt.Errorf("in %s: invalid dirent name offset %d (block size %d): %w",
				parentPath, firstNameOff, len(blk), ErrCorrupt)
		}

		for i := range nEntries {
			off := i * disk.SizeDirent
			nid := binary.LittleEndian.Uint64(blk[off : off+8])
			nameOff := int(binary.LittleEndian.Uint16(blk[off+8 : off+10]))

			nameEnd := len(blk)
			if i < nEntries-1 {
				nameEnd = int(binary.LittleEndian.Uint16(blk[(i+1)*disk.SizeDirent+8 : (i+1)*disk.SizeDirent+10]))
			}

			if nameOff < firstNameOff || nameOff > nameEnd || nameEnd > len(blk) {
				return fmt.Errorf("in %s: dirent %d name range [%d:%d] out of bounds (block size %d): %w",
					parentPath, i, nameOff, nameEnd, len(blk), ErrCorrupt)
			}

			nameBytes := blk[nameOff:nameEnd]
			if i == nEntries-1 {
				if j := bytes.IndexByte(nameBytes, 0); j >= 0 {
					nameBytes = nameBytes[:j]
				}
			}

			name := string(nameBytes)
			// A name is one path element. Without this a nested dirent named
			// "y/../../../.wh..wh..opq" builds a childPath that path.Dir
			// collapses to "/", so the merge-mode whiteout handler wipes every
			// entry from every prior layer; "x/../../.wh.secret" deletes an
			// arbitrary path, and a plain "etc/passwd" overwrites a file in a
			// directory the source never named. Checked before the "."/".."
			// filter so an empty name — which the NUL-stripping above can
			// produce from an all-NUL entry — is an error here as it is in
			// the reader, not something skipped over.
			if err := checkDirentName(nameBytes); err != nil {
				return fmt.Errorf("in %s: %w", parentPath, err)
			}

			if name == "." || name == ".." {
				continue
			}

			childPath := parentPath + "/" + name
			if parentPath == "/" {
				childPath = "/" + name
			}
			// The reader's own resolve refuses to walk a path this long, and
			// add applies the same bound on the full-image route.
			if err := checkPathLen(childPath); err != nil {
				return err
			}

			*queue = append(*queue, imgQEntry{nid: nid, path: childPath, ftype: blk[off+10]})
		}

		pos = blockEnd
	}

	return nil
}

// chunkMapBytes returns the on-disk size of the chunk-index map an inode of
// the given size and format declares, in entries of unit bytes each.
//
// The arithmetic is done in uint64: fileSize comes straight off disk and the
// obvious int conversion overflows to a negative count for sizes past 2^63.
func chunkMapBytes(chunkFmt uint16, fileSize uint64, blkBits uint8, unit int64) (int64, error) {
	chunkBits := blkBits + uint8(chunkFmt&disk.LayoutChunkFormatBits)
	if chunkBits >= 64 { //nolint:mnd // the width of the uint64 the file size is shifted in
		return 0, fmt.Errorf("chunk size of 2^%d bytes is out of range: %w", chunkBits, ErrCorrupt)
	}

	cs := uint64(1) << chunkBits

	nchunks := fileSize / cs
	if fileSize%cs != 0 {
		nchunks++
	}

	if nchunks > uint64(maxChunkIndexBytes/unit) { // #nosec G115 -- unit is 4 or 8: a positive quotient
		return 0, fmt.Errorf("chunk index for a %d byte file exceeds the %d byte limit: %w",
			fileSize, int64(maxChunkIndexBytes), ErrInvalid)
	}

	return int64(nchunks) * unit, nil // #nosec G115 -- nchunks was bounded by maxChunkIndexBytes just above
}

// parseChunks extracts chunk index entries from an in-memory buffer.
//
// An error is returned when the inode's declared size implies a chunk-index
// map that is larger than the data actually present, or larger than the
// reader will ever parse back (maxChunkIndexBytes). Both mean the size field
// cannot be trusted, and carrying it forward would let a corrupt source drive
// the writer's own allocations — the entry's trailing size is derived from it.
func (*Writer) parseChunks(
	data []byte,
	chunkFmt uint16,
	fileSize uint64,
	blkBits uint8,
	deviceIDMask uint16,
	place chunkPlacer,
) ([]builder.Chunk, error) {
	// The same format rules the reader's loadBlock applies. 48-bit chunk
	// addressing is unimplemented end to end, so a source declaring it must
	// not be silently reinterpreted as 32-bit.
	if chunkFmt&^(disk.LayoutChunkFormatBits|disk.LayoutChunkFormatIndexes) != 0 {
		return nil, fmt.Errorf("unsupported chunk format %#x: %w", chunkFmt, ErrNotImplemented)
	}

	chunkBits := blkBits + uint8(chunkFmt&disk.LayoutChunkFormatBits)
	// #nosec G115 -- chunkBits is at least 9, the smallest block, so the count is below 1<<55
	nchunks := int((fileSize-1)>>chunkBits) + 1
	// At most 1<<31 blocks, the format's five chunk bits: within the
	// uint32 a builder.Chunk counts blocks in.
	blocksPerChunk := uint32(1) << (chunkBits - blkBits)

	// Align to 8 bytes for index entries.
	needed := int64(nchunks) * disk.SizeChunkIndex
	if needed > maxChunkIndexBytes {
		return nil, fmt.Errorf("chunk index of %d bytes for a %d byte file exceeds the %d byte limit: %w",
			needed, fileSize, int64(maxChunkIndexBytes), ErrInvalid)
	}

	if int64(len(data)) < needed {
		return nil, fmt.Errorf("chunk index truncated: have %d bytes, need %d: %w",
			len(data), needed, ErrCorrupt)
	}

	// Grown from what survives, not from what the inode declares. Most images
	// coalesce heavily — a contiguous file collapses to a single entry — so
	// reserving nchunks up front sizes the slice from the source's say-so and
	// then retains it: a valid image with 20000 hardlinks to one 480-chunk
	// file reserved 9.6 M slots to hold 20001.
	var chunks []builder.Chunk

	for i := range nchunks {
		off := i * disk.SizeChunkIndex

		startBlkLo := binary.LittleEndian.Uint32(data[off+4 : off+8])
		if ^startBlkLo == 0 {
			// A hole. Its logical span has to be carried over: chunks
			// describe the file's layout positionally, so dropping a hole
			// slides every later extent down into the wrong logical offset
			// and the file reads back unrelated device bytes where it should
			// read zeros.
			chunks = appendRun(chunks, builder.NullPhysicalBlock, blocksPerChunk, 0)

			continue
		}
		// 32-bit addressing: the high half is the kernel's "advise" field
		// unless the 48-bit format is declared, and that format is rejected
		// above. The reader has always resolved chunks this way, so folding
		// these bits in produced an output whose chunks named blocks the
		// reader would never look at.
		deviceID := binary.LittleEndian.Uint16(data[off+2:off+4]) & deviceIDMask
		physBlock := uint64(startBlkLo)

		// Every chunk lands where erofs_map_dev lands it, judged by its own
		// start before runs are joined, its bytes bounded there as Stat
		// bounds them.
		var err error

		deviceID, physBlock, err = place(deviceID, physBlock, chunkBytes(i, chunkBits, fileSize))
		if err != nil {
			return nil, err
		}

		chunks = appendRun(chunks, physBlock, blocksPerChunk, deviceID)
	}

	// The slice outlives this call on every entry that shares the inode: a
	// copy of it at its length, since what append grew keeps the room of
	// its last step, up to a quarter more, whatever its capacity is cut to.
	return slices.Clone(chunks), nil
}

// parseXattrsFromBuf parses xattr entries from an in-memory buffer into
// dst, which it returns in the order of their names.
// at provides on-demand access to the shared xattr block at sharedOff.
// longPrefix resolves long xattr prefix indexes (NameIndex with high bit set).
//
// The same rules as the reader's loadXattrs apply, because this is the
// other door an image's attributes come in through: an entry the kernel
// does not list is left out (prefixOf says which), so the copy holds what a
// mount of the source shows, and a name listed twice is an error rather
// than last-wins (which let the parser's iteration order pick which copy a
// policy decision saw).
//
// span reads n bytes at an absolute image offset (see copyFromImage) and is
// how shared entries are reached; sharedOff is the shared xattr area's byte
// offset, which the reader computes the same way, 0 included.
//
//nolint:gocognit // the xattr body decoded field by field: shared references, then inline entries
func parseXattrsFromBuf(
	dst []xattr,
	buf []byte,
	span func(off, n int64) ([]byte, error),
	sharedOff int64,
	prefixOf func(uint8) (string, bool),
) ([]xattr, error) {
	if len(buf) < disk.SizeXattrBodyHeader {
		return nil, fmt.Errorf("xattr body of %d bytes too small: %w", len(buf), ErrCorrupt)
	}

	var xh disk.XattrHeader
	xh.Unmarshal(buf)

	pos := disk.SizeXattrBodyHeader

	// Resolve shared xattr references.
	for range int(xh.SharedCount) {
		if pos+4 > len(buf) {
			return nil, fmt.Errorf("xattr shared block too small: %w", ErrCorrupt)
		}

		idx := binary.LittleEndian.Uint32(buf[pos : pos+4])
		pos += 4

		// idx counts 4-byte units from the shared area; widened before
		// scaling so the multiply cannot wrap.
		sharedAddr := sharedOff + int64(idx)*4

		head, err := span(sharedAddr, disk.SizeXattrEntry)
		if err != nil {
			return nil, fmt.Errorf("shared xattr %d: %w", idx, err)
		}

		var xe disk.XattrEntry
		xe.Unmarshal(head)

		body, err := span(sharedAddr+disk.SizeXattrEntry, int64(xe.NameLen)+int64(xe.ValueLen))
		if err != nil {
			return nil, fmt.Errorf("shared xattr %d: %w", idx, err)
		}

		if prefix, listed := prefixOf(xe.NameIndex); listed {
			dst = append(dst, xattr{name: prefix + string(body[:xe.NameLen]), value: string(body[xe.NameLen:])})
		}
	}

	// Parse inline xattr entries.
	for pos < len(buf) {
		if pos+disk.SizeXattrEntry > len(buf) {
			return nil, fmt.Errorf("xattr block too small for entry at pos %d: %w", pos, ErrCorrupt)
		}

		var xe disk.XattrEntry
		xe.Unmarshal(buf[pos:])
		pos += disk.SizeXattrEntry

		entryLen := int(xe.NameLen) + int(xe.ValueLen)
		if pos+entryLen > len(buf) {
			return nil, fmt.Errorf("inline xattr at pos %d too long: %w", pos, ErrCorrupt)
		}

		if prefix, listed := prefixOf(xe.NameIndex); listed {
			dst = append(dst, xattr{
				name:  prefix + string(buf[pos:pos+int(xe.NameLen)]),
				value: string(buf[pos+int(xe.NameLen) : pos+entryLen]),
			})
		}

		pos += entryLen

		// Round up to 4-byte boundary.
		if rem := pos % 4; rem != 0 {
			pos += 4 - rem
		}
	}

	slices.SortFunc(dst, byXattrName)

	for i := 1; i < len(dst); i++ {
		if dst[i].name == dst[i-1].name {
			return nil, fmt.Errorf("duplicate xattr %q: %w", dst[i].name, ErrCorrupt)
		}
	}

	return dst, nil
}

// checkFlatHole holds a flat file at the null address to the blocks a copy
// takes of one. It is a hole as large as its i_size says, to 1<<63, with
// nothing in the image to back that: a hole a chunk index spells out has
// its index.
func checkFlatHole(nid uint64, size, blockSize int64) error {
	// #nosec G115 -- a file's size and a block size, neither negative
	if blocks := (uint64(size) + uint64(blockSize) - 1) / uint64(blockSize); blocks > maxChunkIndexEntries {
		return fmt.Errorf("nid %d: a flat file of %d bytes is past the %d blocks a copy takes: %w",
			nid, size, int64(maxChunkIndexEntries), ErrInvalid)
	}

	return nil
}

// errRangesNotTheFile is the reader's own ranges of a file failing what
// they promise: every byte of the file, once, in order, and no more.
var errRangesNotTheFile = errors.New("the file's ranges do not cover it exactly")

// coversExactly holds a file's ranges to what they promise: each of some
// bytes, and size bytes in all.
func coversExactly(ranges []DataRange, size int64) error {
	var pos int64

	for _, r := range ranges {
		if r.Size <= 0 || r.Size > size-pos {
			return fmt.Errorf("%w: one of %d bytes at byte %d of %d", errRangesNotTheFile, r.Size, pos, size)
		}

		pos += r.Size
	}

	if pos != size {
		return fmt.Errorf("%w: they end at byte %d of %d", errRangesNotTheFile, pos, size)
	}

	return nil
}

// holedData lays out, in blocks of blockSize bytes, a file of the image that
// has holes: the chunks, a hole wherever whole blocks lie inside one of the
// file's, the rest counted from zero as blocks of data; and that data, the
// file read without those holes. Nil chunks: the file has no such hole.
func holedData(f *file, ino *inode, blockSize int64) ([]builder.Chunk, io.Reader, error) {
	ranges, err := f.buildDataRanges(ino, nil)
	if err != nil {
		return nil, nil, err
	}

	// The blocks left to keep are counted below without a sign, by what
	// the ranges have covered: ranges that end past the file leave less
	// than none, which is 1<<64 of them to lay out.
	if err := coversExactly(ranges, ino.size); err != nil {
		return nil, nil, fmt.Errorf("nid %d: %w", ino.nid, err)
	}

	// #nosec G115 -- readInfo bounded the size, which is not negative
	blocks := (uint64(ino.size) + uint64(blockSize) - 1) / uint64(blockSize)
	if ino.inodeLayout == disk.LayoutFlatPlain && ino.nullAddress() {
		if err := checkFlatHole(ino.nid, ino.size, blockSize); err != nil {
			return nil, nil, err
		}
	}

	var (
		chunks []builder.Chunk
		next   uint64 // the first block no chunk covers yet
		stored uint64 // blocks of data so far
		pos    int64  // the logical byte the range starts at
	)

	// The blocks from next to end are data: a chunk, which is also what
	// rangesReader reads.
	keep := func(end uint64) {
		if end == next {
			return
		}

		chunks = append(chunks, deviceChunks(stored, end-next, 0)...)
		stored += end - next
		next = end
	}

	for _, r := range ranges {
		end := pos + r.Size
		if first, last := wholeBlocks(pos, end, ino.size, blockSize); r.Offset == holeOffset && last > first {
			keep(first)

			chunks = append(chunks, deviceChunks(builder.NullPhysicalBlock, last-first, 0)...)
			next = last
		}

		pos = end
	}

	if chunks == nil {
		return nil, nil, nil
	}

	keep(blocks)

	// The list is the entry's until Close and the reader's: one array for
	// both, at its length, and not as append grew it.
	chunks = slices.Clone(chunks)

	return chunks, &rangesReader{f: f, chunks: chunks, blockSize: blockSize, size: ino.size}, nil
}

// wholeBlocks is the blocks that lie whole between two bytes of a file of
// size bytes, from the first to before the last; the file's last block is
// whole to its end.
func wholeBlocks(from, to, size, blockSize int64) (first, last uint64) {
	// #nosec G115 -- byte offsets within the file and a block size, none negative
	first, last = uint64(from+blockSize-1)/uint64(blockSize), uint64(to)/uint64(blockSize)
	if to == size {
		// #nosec G115 -- as above
		last = (uint64(size) + uint64(blockSize) - 1) / uint64(blockSize)
	}

	return first, last
}

// rangesReader reads the data of a file of an image that has holes: the
// blocks its chunks lay out as data, one run after the other, and none of
// its holes.
//
// The chunks are the entry's own list, which Close moves to their blocks in
// the new image while this still reads by them: only what it never changes
// is read here, which chunk is a hole and how many blocks each is.
type rangesReader struct {
	f         *file
	chunks    []builder.Chunk // the runs still to read or to skip
	blockSize int64
	size      int64 // the file's, where its last block ends
	block     int64 // the file's block the next run begins at
	left      int64 // of the run being read
}

func (r *rangesReader) Read(p []byte) (int, error) {
	for r.left == 0 {
		if len(r.chunks) == 0 {
			return 0, io.EOF
		}

		run := r.chunks[0]
		r.chunks = r.chunks[1:]

		from := r.block * r.blockSize
		r.block += int64(run.Count)

		if run.PhysicalBlock != builder.NullPhysicalBlock {
			r.f.offset, r.left = from, min(r.block*r.blockSize, r.size)-from
		}
	}

	if int64(len(p)) > r.left {
		p = p[:r.left]
	}

	n, err := r.f.Read(p)
	r.left -= int64(n)

	return n, err
}

// Close gives back what the file holds of its map. Whoever reads the data
// closes it when it is done: the last run may end before the file does, so
// that no read of it ends the file.
func (r *rangesReader) Close() error {
	return r.f.Close()
}

// deviceChunks names a run of blocks from start on device as chunk runs, each
// within a chunk's 32-bit count; from the null block, the runs are holes.
func deviceChunks(start, blocks uint64, device uint16) []builder.Chunk {
	var chunks []builder.Chunk

	for blocks > 0 {
		count := min(blocks, math.MaxUint32)
		chunks = append(chunks, builder.Chunk{
			PhysicalBlock: start,
			Count:         uint32(count), // #nosec G115 -- bounded by math.MaxUint32 just above
			DeviceID:      device,
		})

		if start != builder.NullPhysicalBlock {
			start += count
		}

		blocks -= count
	}

	return chunks
}

// chunkPlacer places a chunk's first block, on device deviceID, where
// erofs_map_dev lands it, with its n logical bytes bounded there as Stat
// bounds them: the device and the block on it, or the refusal.
type chunkPlacer func(deviceID uint16, block uint64, n int64) (uint16, uint64, error)

// chunkBytes is the logical size of chunk i of a fileSize-byte file.
func chunkBytes(i int, chunkBits uint8, fileSize uint64) int64 {
	size := uint64(1) << chunkBits
	// #nosec G115 -- i is below the chunk count, so the start is below fileSize
	start := uint64(i) << chunkBits

	return int64(min(size, fileSize-start)) // #nosec G115 -- at most one chunk, far below MaxInt64
}

// parseBlockMap reads a chunk-based inode's block map, the unindexed chunk
// format: one 32-bit block address per chunk, the null address a hole, each
// addressed on the primary device and placed where erofs_map_dev lands it.
func parseBlockMap(
	data []byte,
	chunkFmt uint16,
	fileSize uint64,
	blkBits uint8,
	place chunkPlacer,
) ([]builder.Chunk, error) {
	if chunkFmt&^disk.LayoutChunkFormatBits != 0 {
		return nil, fmt.Errorf("unsupported chunk format %#x: %w", chunkFmt, ErrNotImplemented)
	}

	chunkBits := blkBits + uint8(chunkFmt&disk.LayoutChunkFormatBits)
	// #nosec G115 -- chunkBits is at least 9, the smallest block, so the count is below 1<<55
	nchunks := int((fileSize-1)>>chunkBits) + 1
	// At most 1<<31 blocks, the format's five chunk bits: within the
	// uint32 a builder.Chunk counts blocks in.
	blocksPerChunk := uint32(1) << (chunkBits - blkBits)

	if len(data) < nchunks*4 {
		return nil, fmt.Errorf("block map of %d entries truncated at %d bytes: %w", nchunks, len(data), ErrCorrupt)
	}

	var chunks []builder.Chunk

	for i := range nchunks {
		addr := binary.LittleEndian.Uint32(data[i*4:])

		if addr == disk.NullAddr {
			chunks = appendRun(chunks, builder.NullPhysicalBlock, blocksPerChunk, 0)

			continue
		}

		dev, start, err := place(0, uint64(addr), chunkBytes(i, chunkBits, fileSize))
		if err != nil {
			return nil, err
		}

		chunks = appendRun(chunks, start, blocksPerChunk, dev)
	}

	// The slice outlives this call on every entry that shares the inode: a
	// copy of it at its length, since what append grew keeps the room of
	// its last step, up to a quarter more, whatever its capacity is cut to.
	return slices.Clone(chunks), nil
}

// appendRun adds a chunk of that many blocks at start on device, a hole
// from the null block, to the chunks of a file: as more of the last run
// when it follows it and the run's count holds it, as a run of its own
// otherwise. The two forms of a chunk map are read into the same runs by it.
func appendRun(chunks []builder.Chunk, start uint64, blocks uint32, device uint16) []builder.Chunk {
	chunk := builder.Chunk{PhysicalBlock: start, Count: blocks, DeviceID: device}

	if len(chunks) > 0 {
		last := &chunks[len(chunks)-1]
		if follows(last, &chunk) && uint64(last.Count)+uint64(blocks) <= math.MaxUint32 {
			last.Count += blocks

			return chunks
		}
	}

	return append(chunks, chunk)
}

// flatChunks names the blocks from start on the devices the kernel's
// erofs_map_dev lands them on: a device-0 address inside an extra device's
// mapped range is that device's, the rest the image's. The ids are the
// source's own, 0 the image, for remapChunkDevices.
func (img *image) flatChunks(start, blocks uint64) []builder.Chunk {
	var chunks []builder.Chunk

	for blocks > 0 {
		run, dev, phys := img.flatRun(start, blocks)
		chunks = append(chunks, deviceChunks(phys, run, dev)...)
		start += run
		blocks -= run
	}

	return chunks
}

// flatRun is the longest run from start, at most blocks long, that lies on
// one device: its length, the device, and its first block on that device.
func (img *image) flatRun(start, blocks uint64) (uint64, uint16, uint64) {
	next := blocks // the run on the image ends where a mapped range begins

	for i, dev := range img.devices {
		if dev.mappedBlkAddr == 0 {
			continue
		}

		lo, hi := uint64(dev.mappedBlkAddr), uint64(dev.mappedBlkAddr)+uint64(dev.blocks)
		if start >= lo && start < hi {
			return min(
					blocks,
					hi-start,
				), uint16(
					i + 1,
				), start - lo // #nosec G115 -- the device count is a uint16 on disk
		}

		if lo > start {
			next = min(next, lo-start)
		}
	}

	return next, 0, start
}

// locateChunk is the chunkPlacer of an image, the reader's own placement
// and bound: an explicit device id names its device, a device-0 block
// inside an extra device's mapped range is that device's and the rest the
// image's (erofs_map_dev, as locate reads it), and the chunk's n logical
// bytes are bounded on that device as boundOnDevice bounds them at Stat, so
// the copy refuses what Stat refuses and places what the kernel reads.
func (img *image) locateChunk(deviceID uint16, block uint64, n int64) (uint16, uint64, error) {
	if block > uint64(math.MaxInt64)>>img.sb.BlkSizeBits {
		return 0, 0, fmt.Errorf("chunk block %d is out of range: %w", block, ErrCorrupt)
	}

	// #nosec G115 -- bounded just above
	dev, off, err := img.locate(deviceID, int64(block)<<img.sb.BlkSizeBits)
	if err != nil {
		return 0, 0, err
	}

	if err := img.boundOnDevice(dev, off, n); err != nil {
		return 0, 0, err
	}

	// #nosec G115 -- locate returns a non-negative offset
	return dev, uint64(off) >> img.sb.BlkSizeBits, nil
}

// readFlat reads n bytes of flat data at device-0 address pa from the
// device the kernel's erofs_map_dev lands it on, bounded as the reader's
// own reads are.
func (img *image) readFlat(pa, n int64) ([]byte, error) {
	dev, off, err := img.locateRange(pa, n)
	if err != nil {
		return nil, err
	}

	buf := make([]byte, n)
	if _, err := img.deviceReader(dev).ReadAt(buf, off); err != nil {
		return nil, err
	}

	return buf, nil
}
