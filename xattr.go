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

// sharedXattr reads the entry at addr in the shared xattr area: its header,
// then what follows it, the rest of the name and the value in one string,
// e_name_len bytes of the first. They run on for as long as the header
// says, across blocks: the kernel reads them a block at a time (xattr.c:
// erofs_xattr_copy_to_buffer), and mkfs.erofs lays the area out as one
// buffer with no regard for where a block ends.
func (img *image) sharedXattr(addr int64) (entry disk.XattrEntry, body string, err error) {
	blk, err := img.loadAt(addr, int64(1<<img.sb.BlkSizeBits))
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

// loadXattrs reads the extended attributes for the file's inode and
// populates the given Stat's Xattrs map.
//
//nolint:gocognit // the xattr body decoded field by field, shared then inline, over one block cursor
func loadXattrs(b *file, stat *Stat) (err error) {
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

		xattrAddr := binary.LittleEndian.Uint32(xb[:4])

		// TODO: Cache shared xattr blocks
		// xattrAddr counts 4-byte units from the shared xattr area, and is
		// widened before scaling: the multiply would otherwise wrap in uint32.
		sharedAddr := int64(b.img.sb.XattrBlkAddr)<<b.img.sb.BlkSizeBits + int64(xattrAddr)*4

		var (
			xattrEntry disk.XattrEntry
			body       string
		)

		xattrEntry, body, err = b.img.sharedXattr(sharedAddr)
		if err != nil {
			return fmt.Errorf("shared xattr for nid %d: %w", b.nid, err)
		}

		if prefix, listed := b.img.listedPrefix(xattrEntry.NameIndex); listed {
			if err = setXattr(stat, b.nid, prefix+body[:xattrEntry.NameLen], body[xattrEntry.NameLen:]); err != nil {
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
