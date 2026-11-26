package erofs

import (
	"encoding/binary"
	"fmt"

	"github.com/forkcloser/erofs/internal/disk"
)

// lastListedIndex is the highest name index the kernel lists (security.).
// Untyped: a typed constant would make an enum of a uint8.
const lastListedIndex = 6

// builtinPrefix is the prefix a name index stands for, and whether the
// kernel lists and serves an entry that bears it (xattr.c: the handler map
// of erofs_xattr_prefix). Every other index is stored like these and shown
// by no mount: 0, 5 (the format's header calls it lustre, and no handler
// takes it) and 7 up.
func builtinPrefix(index uint8) (string, bool) {
	switch index {
	case 1:
		return "user.", true
	case 2:
		return "system.posix_acl_access", true
	case 3:
		return "system.posix_acl_default", true
	case 4:
		return "trusted.", true
	case lastListedIndex:
		return "security.", true
	default:
		return "", false
	}
}

// listedPrefix is the name prefix an entry's e_name_index stands for, a
// long prefix's when bit 7 is set, and whether the kernel lists the entry
// (xattr.c: erofs_listxattr_foreach). One it does not list (an index with
// no handler, a long prefix past the table or built on such an index) is
// read over and left out: no mount shows it, and getxattr cannot name it.
func (img *image) listedPrefix(nameIndex uint8) (string, bool) {
	if nameIndex&disk.XattrLongPrefixFlag == 0 {
		return builtinPrefix(nameIndex)
	}

	i := int(nameIndex & disk.XattrLongPrefixMask)
	if i >= len(img.longPrefixes) || img.longPrefixes[i] == "" {
		return "", false
	}

	return img.longPrefixes[i], true
}

// setXattr records one attribute, rejecting a name already present.
//
// EROFS has no rule against an image listing the same key twice, and assigning
// into the map takes the last one silently. Anything consulting an attribute
// for a policy decision — security.capability, security.selinux — would then
// act on whichever copy the parser happened to write second.
func setXattr(stat *Stat, nid uint64, name, value string) error {
	if _, dup := stat.Xattrs[name]; dup {
		return fmt.Errorf("duplicate xattr %q for nid %d: %w", name, nid, ErrCorrupt)
	}

	stat.Xattrs[name] = value

	return nil
}

// sharedXattrFirst is the first read of a shared entry: its header and the
// name and value of most entries, a label or a capability. A block, read
// for each reference of each inode, is up to 64 KiB for those few bytes.
const sharedXattrFirst = 128

// sharedXattr reads the entry at addr in the shared xattr area: its header,
// then what follows it, the rest of the name and the value in one string,
// e_name_len bytes of the first. They run on for as long as the header
// says, across blocks: the kernel reads them a block at a time (xattr.c:
// erofs_xattr_copy_to_buffer), and mkfs.erofs lays the area out as one
// buffer with no regard for where a block ends.
func (img *image) sharedXattr(addr int64) (entry disk.XattrEntry, body string, err error) {
	blk, err := img.loadAt(addr, sharedXattrFirst)
	if err != nil {
		return entry, "", err
	}

	raw := blk.bytes()
	if len(raw) < disk.SizeXattrEntry {
		img.putBlock(blk)

		return entry, "", fmt.Errorf("the image ends inside the entry at %d: %w", addr, ErrCorrupt)
	}

	entry.Unmarshal(raw)
	raw = raw[disk.SizeXattrEntry:]

	need := int(entry.NameLen) + int(entry.ValueLen)
	if len(raw) >= need {
		body = string(raw[:need])

		img.putBlock(blk)

		return entry, body, nil
	}

	// At most a name of 255 bytes and a value of 65535.
	long := make([]byte, 0, need)
	long = append(long, raw...)

	img.putBlock(blk)

	for len(long) < need {
		blk, err = img.loadAt(addr+disk.SizeXattrEntry+int64(len(long)), int64(need-len(long)))
		if err != nil {
			return entry, "", err
		}

		long = append(long, blk.bytes()...)

		img.putBlock(blk)
	}

	return entry, string(long), nil
}

// sharedXattrHeld is how many bytes of shared entries an image keeps as it
// decoded them: the labels and capabilities of a tree, a few kilobytes that
// every inode names, are then read once. An entry that would take the image
// past it is read for each reference.
const sharedXattrHeld = 1 << 20

// sharedEntryCost is what a held entry counts for beside its strings, so
// that entries of no bytes, as those the kernel does not list are held, are
// bounded too.
const sharedEntryCost = 64

// sharedEntry is an entry of the shared xattr area as an inode's attributes
// take it: its full name and its value, and whether the kernel lists it.
type sharedEntry struct {
	name, value string
	listed      bool
}

// sharedEntryAt is the entry a reference names: ref counts 4-byte units
// from the start of the shared xattr area. A failed read is held by nothing:
// the next reference reads again.
func (img *image) sharedEntryAt(ref uint32) (sharedEntry, error) {
	img.sharedMu.RLock()
	held, ok := img.shared[ref]
	img.sharedMu.RUnlock()

	if ok {
		return held, nil
	}

	// Widened before scaling: the multiply would otherwise wrap in uint32.
	addr := int64(img.sb.XattrBlkAddr)<<img.sb.BlkSizeBits + int64(ref)*4

	entry, body, err := img.sharedXattr(addr)
	if err != nil {
		return sharedEntry{}, err
	}

	var (
		shared sharedEntry
		cost   = sharedEntryCost
	)

	if prefix, listed := img.listedPrefix(entry.NameIndex); listed {
		shared = sharedEntry{name: prefix + body[:entry.NameLen], value: body[entry.NameLen:], listed: true}
		// The value keeps all of body.
		cost += len(shared.name) + len(body)
	}

	img.sharedMu.Lock()
	defer img.sharedMu.Unlock()

	if _, raced := img.shared[ref]; !raced && img.sharedHeld+cost <= sharedXattrHeld {
		if img.shared == nil {
			img.shared = map[uint32]sharedEntry{}
		}

		img.shared[ref] = shared
		img.sharedHeld += cost
	}

	return shared, nil
}

// xattrsListable refuses the attributes of an inode in an image that says
// its shared entries are in the metabox: Open takes no image with one, and
// the kernel, with none, fails the listing of every inode that has an xattr
// body, whether or not it names a shared entry (xattr.c:
// erofs_xattr_iter_shared begins with the metabox's buffer).
func (img *image) xattrsListable(nid uint64) error {
	if img.sb.FeatureCompat&disk.FeatureCompatSharedEAInMetabox != 0 {
		return fmt.Errorf("xattrs of nid %d: shared entries in a metabox the image does not have: %w", nid, ErrCorrupt)
	}

	return nil
}

// loadXattrs reads the extended attributes for the file's inode and
// populates the given Stat's Xattrs map.
//
//nolint:gocognit // the xattr body decoded field by field, shared then inline, over one block cursor
func loadXattrs(b *file, stat *Stat) (err error) {
	if err = b.img.xattrsListable(b.nid); err != nil {
		return err
	}

	ino := b.info
	// #nosec G115 -- readInfo bounded the nid through checkNid
	addr := b.img.metaStartPos() + int64(ino.nid*disk.SizeInodeCompact) + int64(ino.icsize)
	xsize := ino.xsize

	stat.Xattrs = map[string]string{}

	blk, err := b.img.loadAt(addr, int64(xsize))
	if err != nil {
		return fmt.Errorf("failed to read xattr body for nid %d: %w", b.nid, err)
	}
	defer func() {
		if blk != nil {
			b.img.putBlock(blk)
		}
	}()

	xb := blk.bytes()
	if len(xb) < disk.SizeXattrBodyHeader {
		return fmt.Errorf("xattr body too small for nid %d: %w", b.nid, ErrCorrupt)
	}

	var xh disk.XattrHeader
	xh.Unmarshal(xb)
	xb = xb[disk.SizeXattrBodyHeader:]

	// The shared references sit between the header and the inline entries,
	// so the area i_xattr_icount sizes must hold them all. Checked here, by
	// name, rather than left to the reload below to run out of bytes.
	if disk.SizeXattrBodyHeader+int(xh.SharedCount)*4 > xsize {
		return fmt.Errorf("nid %d declares %d shared xattrs, more than its %d byte xattr area holds: %w",
			b.nid, xh.SharedCount, xsize, ErrCorrupt)
	}

	for i := range int(xh.SharedCount) {
		if len(xb) < 4 {
			pos := disk.SizeXattrBodyHeader + int64(i)*4

			b.img.putBlock(blk)

			blk, err = b.img.loadAt(addr+pos, int64(xsize)-pos)
			if err != nil {
				return fmt.Errorf("failed to read xattr body for nid %d: %w", b.nid, err)
			}

			xb = blk.bytes()
			if len(xb) < 4 {
				return fmt.Errorf("xattr shared block too small for nid %d: %w", b.nid, ErrCorrupt)
			}
		}

		var shared sharedEntry

		shared, err = b.img.sharedEntryAt(binary.LittleEndian.Uint32(xb[:4]))
		if err != nil {
			return fmt.Errorf("shared xattr for nid %d: %w", b.nid, err)
		}

		if shared.listed {
			if err = setXattr(stat, b.nid, shared.name, shared.value); err != nil {
				return err
			}
		}

		xb = xb[4:]
	}

	pos := disk.SizeXattrBodyHeader + int(xh.SharedCount)*4
	reload := func() error {
		b.img.putBlock(blk)

		blk, err = b.img.loadAt(addr+int64(pos), int64(xsize-pos))
		if err != nil {
			return fmt.Errorf("failed to read xattr body for nid %d: %w", b.nid, err)
		}

		xb = blk.bytes()

		return nil
	}

	for pos < xsize {
		if len(xb) < disk.SizeXattrEntry {
			if err := reload(); err != nil {
				return err
			}

			if len(xb) < disk.SizeXattrEntry {
				return fmt.Errorf("xattr block too small for entry at pos %d for nid %d: %w", pos, b.nid, ErrCorrupt)
			}
		}

		var xattrEntry disk.XattrEntry
		xattrEntry.Unmarshal(xb)

		pos += disk.SizeXattrEntry
		xb = xb[disk.SizeXattrEntry:]

		prefix, listed := b.img.listedPrefix(xattrEntry.NameIndex)

		if len(xb) < int(xattrEntry.NameLen) {
			if err := reload(); err != nil {
				return err
			}

			if len(xb) < int(xattrEntry.NameLen) {
				return fmt.Errorf(
					"xattr block too small for name of length %d for nid %d: %w",
					xattrEntry.NameLen,
					b.nid,
					ErrCorrupt,
				)
			}
		}

		name := prefix + string(xb[:xattrEntry.NameLen])
		pos += int(xattrEntry.NameLen)
		xb = xb[xattrEntry.NameLen:]

		var value string

		if len(xb) < int(xattrEntry.ValueLen) {
			remaining := int(xattrEntry.ValueLen)

			buf := make([]byte, 0, remaining)
			for remaining > 0 {
				copySize := len(xb)
				if copySize == 0 {
					if err := reload(); err != nil {
						return err
					}

					copySize = len(xb)
					if copySize == 0 {
						return fmt.Errorf("empty xattr block while reading value: %w", ErrCorrupt)
					}
				}

				if remaining < copySize {
					copySize = remaining
				}

				buf = append(buf, xb[:copySize]...)
				remaining -= copySize
				pos += copySize
				xb = xb[copySize:]
			}

			value = string(buf)
		} else {
			value = string(xb[:xattrEntry.ValueLen])
			pos += int(xattrEntry.ValueLen)
			xb = xb[xattrEntry.ValueLen:]
		}

		if listed {
			if err := setXattr(stat, b.nid, name, value); err != nil {
				return err
			}
		}

		// Round up to next 4 byte boundary
		if rem := pos % 4; rem != 0 {
			pad := 4 - rem

			pos += pad
			if len(xb) < pad {
				xb = nil
			} else {
				xb = xb[pad:]
			}
		}
	}

	return nil
}
