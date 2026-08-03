package erofs

import (
	"encoding/binary"
	"fmt"

	"github.com/forkcloser/erofs/internal/disk"
)

/*
#define EROFS_XATTR_INDEX_USER              1
#define EROFS_XATTR_INDEX_POSIX_ACL_ACCESS  2
#define EROFS_XATTR_INDEX_POSIX_ACL_DEFAULT 3
#define EROFS_XATTR_INDEX_TRUSTED           4
#define EROFS_XATTR_INDEX_LUSTRE            5
#define EROFS_XATTR_INDEX_SECURITY          6
*/

type xattrIndex uint8

func (idx xattrIndex) String() string {
	switch idx {
	case 1:
		return "user."
	case 2:
		return "system.posix_acl_access."
	case 3:
		return "system.posix_acl_default."
	case 4:
		return "trusted."
	case 5:
		return "lustre."
	case 6:
		return "security."
	default:
		return ""
	}
}

// prefix returns the name prefix an index stands for.
//
// Index 0 means the name is stored whole, with no prefix. Everything from 7 up
// is undefined by the format, and mapping it to the empty prefix the way
// String does is what let a hostile image spell out "security.capability" in
// full under an unknown index and collide with the properly prefixed entry.
func (idx xattrIndex) prefix() (string, error) {
	if idx == 0 {
		return "", nil
	}
	if p := idx.String(); p != "" {
		return p, nil
	}

	return "", fmt.Errorf("unknown xattr name index %d: %w", uint8(idx), ErrInvalid)
}

// setXattr records one attribute, rejecting a name already present.
//
// EROFS has no rule against an image listing the same key twice, and assigning
// into the map takes the last one silently. Anything consulting an attribute
// for a policy decision — security.capability, security.selinux — would then
// act on whichever copy the parser happened to write second.
func setXattr(stat *Stat, nid uint64, name, value string) error {
	if _, dup := stat.Xattrs[name]; dup {
		return fmt.Errorf("duplicate xattr %q for nid %d: %w", name, nid, ErrInvalid)
	}
	stat.Xattrs[name] = value

	return nil
}

// loadXattrs reads the extended attributes for the file's inode and
// populates the given Stat's Xattrs map.
func loadXattrs(b *file, stat *Stat) (err error) {
	ino := b.info
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
		return fmt.Errorf("xattr body too small for nid %d: %w", b.nid, ErrInvalid)
	}
	var xh disk.XattrHeader
	xh.Unmarshal(xb)
	xb = xb[disk.SizeXattrBodyHeader:]

	for i := 0; i < int(xh.SharedCount); i++ {
		if len(xb) < 4 {
			pos := disk.SizeXattrBodyHeader + int64(i)*4
			b.img.putBlock(blk)
			blk, err = b.img.loadAt(addr+pos, int64(xsize)-pos)
			if err != nil {
				return fmt.Errorf("failed to read xattr body for nid %d: %w", b.nid, err)
			}
			xb = blk.bytes()
			if len(xb) < 4 {
				return fmt.Errorf("xattr shared block too small for nid %d: %w", b.nid, ErrInvalid)
			}
		}
		xattrAddr := binary.LittleEndian.Uint32(xb[:4])

		// TODO: Cache shared xattr blocks
		// xattrAddr counts 4-byte units from the shared xattr area, and is
		// widened before scaling: the multiply would otherwise wrap in uint32.
		sharedAddr := int64(b.img.sb.XattrBlkAddr)<<b.img.sb.BlkSizeBits + int64(xattrAddr)*4
		sblk, err := b.img.loadAt(sharedAddr, int64(1<<b.img.sb.BlkSizeBits))
		if err != nil {
			return fmt.Errorf("failed to read shared xattr body for nid %d: %w", b.nid, err)
		}
		sb := sblk.bytes()
		if len(sb) < disk.SizeXattrEntry {
			b.img.putBlock(sblk)
			return fmt.Errorf("shared xattr block too small for nid %d: %w", b.nid, ErrInvalid)
		}
		var xattrEntry disk.XattrEntry
		xattrEntry.Unmarshal(sb)
		sb = sb[disk.SizeXattrEntry:]
		var prefix string
		if xattrEntry.NameIndex&0x80 == 0x80 {
			// Long prefix: highest bit set
			longPrefixIndex := xattrEntry.NameIndex & 0x7F
			prefix, err = b.img.getLongPrefix(longPrefixIndex)
			if err != nil {
				b.img.putBlock(sblk)
				return fmt.Errorf("failed to get long prefix for shared xattr nid %d: %w", b.nid, err)
			}
		} else if prefix, err = xattrIndex(xattrEntry.NameIndex).prefix(); err != nil {
			b.img.putBlock(sblk)
			return fmt.Errorf("shared xattr for nid %d: %w", b.nid, err)
		}

		if len(sb) < int(xattrEntry.NameLen)+int(xattrEntry.ValueLen) {
			b.img.putBlock(sblk)
			return fmt.Errorf("shared xattr too long for nid %d: %w", b.nid, ErrInvalid)
		}
		name := prefix + string(sb[:xattrEntry.NameLen])
		sb = sb[xattrEntry.NameLen:]
		if err := setXattr(stat, b.nid, name, string(sb[:xattrEntry.ValueLen])); err != nil {
			b.img.putBlock(sblk)
			return err
		}
		b.img.putBlock(sblk)

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
				return fmt.Errorf("xattr block too small for entry at pos %d for nid %d: %w", pos, b.nid, ErrInvalid)
			}
		}

		var xattrEntry disk.XattrEntry
		xattrEntry.Unmarshal(xb)
		pos += disk.SizeXattrEntry
		xb = xb[disk.SizeXattrEntry:]
		var prefix string
		if xattrEntry.NameIndex&0x80 == 0x80 {
			// Long prefix: highest bit set
			longPrefixIndex := xattrEntry.NameIndex & 0x7F
			var err error
			prefix, err = b.img.getLongPrefix(longPrefixIndex)
			if err != nil {
				return fmt.Errorf("failed to get long prefix for inline xattr nid %d: %w", b.nid, err)
			}
		} else {
			var err error
			if prefix, err = xattrIndex(xattrEntry.NameIndex).prefix(); err != nil {
				return fmt.Errorf("inline xattr for nid %d: %w", b.nid, err)
			}
		}

		if len(xb) < int(xattrEntry.NameLen) {
			if err := reload(); err != nil {
				return err
			}
			if len(xb) < int(xattrEntry.NameLen) {
				return fmt.Errorf("xattr block too small for name of length %d for nid %d: %w", xattrEntry.NameLen, b.nid, ErrInvalid)
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
						return fmt.Errorf("empty xattr block while reading value: %w", ErrInvalid)
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
		if err := setXattr(stat, b.nid, name, value); err != nil {
			return err
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
