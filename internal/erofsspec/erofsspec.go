// Package erofsspec is a reference reader of the EROFS on-disk format,
// written from the format definition (fs/erofs/erofs_fs.h in Linux) and the
// rules the kernel's reader applies to it (fs/erofs/super.c, inode.c, dir.c,
// namei.c, xattr.c and data.c), each rule cited where it is applied. It is
// the oracle the erofs package's tests hold the package to: it shares no
// code with it, is written for clarity rather than speed, and reads whole
// images held in memory.
//
// Its verdicts are the kernel's: what the kernel mounts and serves, parsed
// to the same values; what the kernel refuses, refused in the kernel's two
// classes, [ErrCorrupt] for an image whose structures break the format
// (-EFSCORRUPTED, -EINVAL, -EBADMSG) and [ErrUnsupported] for a format the
// kernel knows and does not implement (-EOPNOTSUPP). Bytes past the end of
// the image, which a block device would answer with an I/O error, are
// [ErrTruncated].
//
// Two things the kernel does and this package does not, by its own limit:
// compressed inodes are refused as [ErrUnsupported] whatever the kernel's
// configuration, and the metabox feature (compressed metadata) is refused at
// [Parse]. Neither is produced by mkfs.erofs without being asked for.
package erofsspec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Errors of the reference reader. Every refusal wraps one of the three.
var (
	// ErrCorrupt is an image whose on-disk structures break the format: the
	// kernel's -EFSCORRUPTED, -EINVAL and -EBADMSG at read time.
	ErrCorrupt = errors.New("erofsspec: corrupt image")
	// ErrUnsupported is a format the kernel defines and this reader, or the
	// kernel itself, does not implement: -EOPNOTSUPP.
	ErrUnsupported = errors.New("erofsspec: unsupported format")
	// ErrTruncated is a structure that lies past the end of the image or of
	// a device: what a block device answers with an I/O error.
	ErrTruncated = errors.New("erofsspec: structure past the end of the image")
	// ErrNotExist is a name a directory does not hold (-ENOENT).
	ErrNotExist = errors.New("erofsspec: no such entry")
	// ErrNotDir is a path component that is not a directory (-ENOTDIR).
	ErrNotDir = errors.New("erofsspec: not a directory")
	// ErrLoop is a resolution that followed more symlinks than its bound
	// (-ELOOP).
	ErrLoop = errors.New("erofsspec: too many symbolic links")
	// ErrTooLarge is a content over MaxContent, this reader's own limit.
	ErrTooLarge = errors.New("erofsspec: content too large to hold in memory")
	// ErrSkipDir, returned by a Walk callback on a directory, keeps the walk
	// out of it; it is a signal in the shape of io/fs.SkipDir, not a failure.
	ErrSkipDir = errors.New("erofsspec: skip this directory")
)

// The superblock (erofs_fs.h, struct erofs_super_block).
const (
	// SuperOffset is where the superblock starts, "to allow for x86 boot
	// sectors and other oddities" (EROFS_SUPER_OFFSET).
	SuperOffset = 1024
	// SuperSize is the superblock's base size; sb_extslots adds 16-byte
	// slots to it (super.c: sb_size = 128 + sb_extslots * 16).
	SuperSize = 128
	// SuperExtSlot is one extension slot (EROFS_SB_EXTSLOT_SIZE).
	SuperExtSlot = 16
	// Magic is the filesystem magic number (EROFS_SUPER_MAGIC_V1).
	Magic = 0xE0F5E1E2
	// ChecksumSeed is crc32c's seed for the superblock checksum (super.c,
	// erofs_superblock_csum_verify).
	ChecksumSeed = 0x5045B54A

	// MinBlockBits and MaxBlockBits bound blkszbits (super.c: "blkszbits %u
	// isn't supported" below 9 and above PAGE_SHIFT; 16 is the largest page
	// shift a Linux kernel mounts EROFS with, arm64 with 64 KiB pages).
	MinBlockBits = 9
	MaxBlockBits = 16
	// MinPageSize bounds sb_extslots: the kernel reads the superblock from a
	// page and refuses sb_size > PAGE_SIZE - EROFS_SUPER_OFFSET; the smallest
	// page is 4 KiB, so an image with 512-byte blocks still has that room.
	MinPageSize = 4096

	// Compatible feature bits.
	FeatureCompatSBChecksum    = 0x00000001
	FeatureCompatMtime         = 0x00000002
	FeatureCompatXattrFilter   = 0x00000004
	FeatureCompatSharedEAInMbx = 0x00000008
	FeatureCompatPlainXattrPfx = 0x00000010
	FeatureCompatIshareXattrs  = 0x00000020

	// Incompatible feature bits; a bit above FeatureIncompatMetabox is one
	// this kernel does not know (super.c: "unidentified incompatible
	// feature"; EROFS_ALL_FEATURE_INCOMPAT).
	FeatureIncompatLZ40Padding  = 0x00000001
	FeatureIncompatComprCfgs    = 0x00000002
	FeatureIncompatChunkedFile  = 0x00000004
	FeatureIncompatDeviceTable  = 0x00000008
	FeatureIncompatZTailpacking = 0x00000010
	FeatureIncompatFragments    = 0x00000020
	FeatureIncompatXattrPrefix  = 0x00000040
	FeatureIncompat48Bit        = 0x00000080
	FeatureIncompatMetabox      = 0x00000100
	FeatureIncompatAll          = FeatureIncompatMetabox<<1 - 1

	// DeviceSlotSize is one entry of the device table
	// (EROFS_DEVT_SLOT_SIZE, struct erofs_deviceslot).
	DeviceSlotSize = 128
	// DeviceTagSize is the slot's tag (sha256 digest or a path).
	DeviceTagSize = 64
)

// Super is the superblock, every field as the format stores it, with the
// values the kernel derives from them (super.c, erofs_read_superblock).
type Super struct {
	Magic               uint32
	Checksum            uint32
	FeatureCompat       uint32
	BlockBits           uint8
	ExtSlots            uint8
	Inos                uint64
	Epoch               int64
	FixedNsec           uint32
	MetaBlkAddr         uint32
	XattrBlkAddr        uint32
	UUID                [16]byte
	VolumeName          [16]byte
	FeatureIncompat     uint32
	ComprAlgs           uint16 // available_compr_algs, or lz4_max_distance before COMPR_CFGS
	ExtraDevices        uint16
	DevtSlotOff         uint16
	DirBlkBits          uint8
	XattrPrefixCount    uint8
	XattrPrefixStart    uint32
	PackedNid           uint64
	XattrFilterReserved uint8
	IshareXattrPrefixID uint8
	BuildTime           uint32

	// Derived: the block size, the root nid (rootnid_2b, or rootnid_8b when
	// the 48-bit layout sets it), the primary device's block count (blocks_lo,
	// with blocks_hi above it under the same condition), the superblock's
	// size and the device-id mask chunk indexes are read under.
	BlockSize    int
	RootNid      uint64
	Blocks       uint64
	Size         int
	DeviceIDMask uint16
}

// Device is one entry of the device table (struct erofs_deviceslot): an
// extra device holding chunk data, named by its tag, sized in blocks, and
// placed at a unified block address when the devices are laid out flat.
type Device struct {
	Tag     [DeviceTagSize]byte
	Blocks  uint64
	UniAddr uint64
}

// Prefix is one long xattr name prefix (struct erofs_xattr_long_prefix): the
// built-in prefix it extends and the infix after it, so "trusted.overlay."
// is base index 4 and infix "overlay.".
type Prefix struct {
	BaseIndex uint8
	Infix     string
}

// Image is a parsed image: the superblock, the device table and the long
// xattr prefix table, over the image's bytes and its extra devices' bytes.
// Inodes, directories and data are parsed on demand from it.
type Image struct {
	Super    Super
	Devices  []Device
	Prefixes []Prefix

	data  []byte
	extra [][]byte
}

// Parse reads the superblock, the device table and the long xattr prefix
// table of img. extra are the extra devices' contents, in device-id order
// (device 1 first); with none given, chunk data on an extra device is read
// from the image itself at the device's unified address, as the kernel does
// for a flat layout (data.c, erofs_map_dev with devs->flatdev).
func Parse(img []byte, extra ...[][]byte) (*Image, error) {
	im := &Image{data: img}
	if len(extra) > 0 {
		im.extra = extra[0]
	}

	if err := im.parseSuper(); err != nil {
		return nil, err
	}

	if err := im.parseDevices(); err != nil {
		return nil, err
	}

	if err := im.parsePrefixes(); err != nil {
		return nil, err
	}

	return im, nil
}

// bytesAt returns n bytes at off of the image, or ErrTruncated.
func (im *Image) bytesAt(off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off > int64(len(im.data)) || n > int64(len(im.data))-off {
		return nil, fmt.Errorf("%w: %d bytes at offset %d of a %d-byte image", ErrTruncated, n, off, len(im.data))
	}

	return im.data[off : off+n], nil
}

// parseSuper is super.c's erofs_read_superblock, rule by rule.
func (im *Image) parseSuper() error {
	raw, err := im.bytesAt(SuperOffset, SuperSize)
	if err != nil {
		return fmt.Errorf("superblock: %w", err)
	}

	s := &im.Super
	s.Magic = binary.LittleEndian.Uint32(raw[0:])
	// "cannot find valid erofs superblock"
	if s.Magic != Magic {
		return fmt.Errorf("%w: magic %#x", ErrCorrupt, s.Magic)
	}

	s.Checksum = binary.LittleEndian.Uint32(raw[4:])
	s.FeatureCompat = binary.LittleEndian.Uint32(raw[8:])
	s.BlockBits = raw[12]
	s.ExtSlots = raw[13]
	// "blkszbits %u isn't supported"
	if s.BlockBits < MinBlockBits || s.BlockBits > MaxBlockBits {
		return fmt.Errorf("%w: blkszbits %d", ErrCorrupt, s.BlockBits)
	}

	s.BlockSize = 1 << s.BlockBits
	s.DirBlkBits = raw[90]
	// "dirblkbits %u isn't supported": the field is reserved at zero.
	if s.DirBlkBits != 0 {
		return fmt.Errorf("%w: dirblkbits %d", ErrCorrupt, s.DirBlkBits)
	}

	if s.FeatureCompat&FeatureCompatSBChecksum != 0 {
		if err := im.verifyChecksum(); err != nil {
			return err
		}
	}

	s.FeatureIncompat = binary.LittleEndian.Uint32(raw[80:])
	// "unidentified incompatible feature %x, please upgrade kernel"
	if s.FeatureIncompat&^uint32(FeatureIncompatAll) != 0 {
		return fmt.Errorf("%w: incompatible features %#x", ErrCorrupt, s.FeatureIncompat)
	}

	s.Size = SuperSize + int(s.ExtSlots)*SuperExtSlot
	// "invalid sb_extslots %u (more than a fs block)"
	if s.Size > max(s.BlockSize, MinPageSize)-SuperOffset {
		return fmt.Errorf("%w: sb_extslots %d", ErrCorrupt, s.ExtSlots)
	}

	s.Inos = binary.LittleEndian.Uint64(raw[16:])
	epoch := binary.LittleEndian.Uint64(raw[24:])
	s.Epoch = int64(epoch) // #nosec G115 -- the field is signed in the kernel (s64 epoch)
	s.FixedNsec = binary.LittleEndian.Uint32(raw[32:])
	s.Blocks = uint64(binary.LittleEndian.Uint32(raw[36:]))
	s.MetaBlkAddr = binary.LittleEndian.Uint32(raw[40:])
	s.XattrBlkAddr = binary.LittleEndian.Uint32(raw[44:])
	copy(s.UUID[:], raw[48:64])
	copy(s.VolumeName[:], raw[64:80])
	s.ComprAlgs = binary.LittleEndian.Uint16(raw[84:])
	s.ExtraDevices = binary.LittleEndian.Uint16(raw[86:])
	s.DevtSlotOff = binary.LittleEndian.Uint16(raw[88:])
	s.XattrPrefixCount = raw[91]
	s.XattrPrefixStart = binary.LittleEndian.Uint32(raw[92:])
	s.PackedNid = binary.LittleEndian.Uint64(raw[96:])
	s.XattrFilterReserved = raw[104]
	s.IshareXattrPrefixID = raw[105]
	s.BuildTime = binary.LittleEndian.Uint32(raw[108:])

	return im.deriveSuper(raw)
}

// deriveSuper is the tail of erofs_read_superblock: the ishare prefix id's
// bound, the root nid and the block count (16 bits each in the base layout,
// widened by the 48-bit layout when rootnid_8b is set), and the metabox
// refusal, this reader's own limit (package doc).
func (im *Image) deriveSuper(raw []byte) error {
	s := &im.Super

	// "invalid ishare xattr prefix id"
	if s.FeatureCompat&FeatureCompatIshareXattrs != 0 && s.IshareXattrPrefixID >= s.XattrPrefixCount {
		return fmt.Errorf("%w: ishare xattr prefix id %d of %d", ErrCorrupt, s.IshareXattrPrefixID, s.XattrPrefixCount)
	}

	rootnid8 := binary.LittleEndian.Uint64(raw[112:])
	if s.FeatureIncompat&FeatureIncompat48Bit != 0 && rootnid8 != 0 {
		s.RootNid = rootnid8
		s.Blocks |= uint64(binary.LittleEndian.Uint16(raw[14:])) << 32
	} else {
		s.RootNid = uint64(binary.LittleEndian.Uint16(raw[14:]))
	}

	if s.FeatureIncompat&FeatureIncompatMetabox != 0 {
		return fmt.Errorf("%w: metabox", ErrUnsupported)
	}

	return nil
}

// verifyChecksum is super.c's erofs_superblock_csum_verify: crc32c, seeded
// with ChecksumSeed and never inverted, over the bytes after the checksum
// field to the end of the block, less the superblock's offset once the
// block is bigger than it.
func (im *Image) verifyChecksum() error {
	n := int64(im.Super.BlockSize)
	if n > SuperOffset {
		n -= SuperOffset
	}

	n -= 8

	data, err := im.bytesAt(SuperOffset+8, n)
	if err != nil {
		return fmt.Errorf("superblock checksum: %w", err)
	}

	crc := ^crc32.Update(^uint32(ChecksumSeed), crc32.MakeTable(crc32.Castagnoli), data)
	// The kernel's "invalid checksum" refusal, -EBADMSG.
	if crc != im.Super.Checksum {
		return fmt.Errorf("%w: superblock checksum %#08x, computed %#08x", ErrCorrupt, im.Super.Checksum, crc)
	}

	return nil
}

// parseDevices is super.c's erofs_scan_devices and erofs_init_device: the
// table exists only under the DEVICE_TABLE feature, holds extra_devices
// slots from devt_slotoff * 128, and sets the device-id mask to the next
// power of two above the device count, less one.
func (im *Image) parseDevices() error {
	s := &im.Super
	if s.FeatureIncompat&FeatureIncompatDeviceTable == 0 || s.ExtraDevices == 0 {
		return nil
	}

	s.DeviceIDMask = uint16(roundupPow2(uint64(s.ExtraDevices)+1) - 1) // #nosec G115 -- bounded by 2^17 - 1

	pos := int64(s.DevtSlotOff) * DeviceSlotSize
	wide := s.FeatureIncompat&FeatureIncompat48Bit != 0

	for range s.ExtraDevices {
		raw, err := im.bytesAt(pos, DeviceSlotSize)
		if err != nil {
			return fmt.Errorf("device table: %w", err)
		}

		var d Device

		copy(d.Tag[:], raw[:DeviceTagSize])
		d.Blocks = uint64(binary.LittleEndian.Uint32(raw[64:]))
		d.UniAddr = uint64(binary.LittleEndian.Uint32(raw[68:]))

		if wide {
			d.Blocks |= uint64(binary.LittleEndian.Uint16(raw[72:])) << 32
			d.UniAddr |= uint64(binary.LittleEndian.Uint16(raw[74:])) << 32
		}

		im.Devices = append(im.Devices, d)
		pos += DeviceSlotSize
	}

	return nil
}

// parsePrefixes is xattr.c's erofs_xattr_prefixes_init: xattr_prefix_count
// items from byte offset xattr_prefix_start * 4, each a 4-aligned
// erofs_read_metadata item (a 16-bit length, zero meaning 65536, then the
// bytes), holding a base index and an infix of at most EROFS_NAME_LEN
// bytes. The items live in the image itself when the PLAIN_XATTR_PFX
// feature says so or no packed inode exists, and in the packed inode's data
// otherwise.
func (im *Image) parsePrefixes() error {
	s := &im.Super
	if s.XattrPrefixCount == 0 {
		return nil
	}

	src := im.bytesAt
	if s.FeatureCompat&FeatureCompatPlainXattrPfx == 0 && s.PackedNid != 0 {
		packed, err := im.Inode(s.PackedNid)
		if err != nil {
			return fmt.Errorf("packed inode: %w", err)
		}

		content, err := im.Read(packed)
		if err != nil {
			return fmt.Errorf("packed inode: %w", err)
		}

		src = func(off, n int64) ([]byte, error) { return sliceAt(content, off, n) }
	}

	pos := int64(s.XattrPrefixStart) * 4

	for range s.XattrPrefixCount {
		item, next, err := readMetadata(src, pos)
		if err != nil {
			return fmt.Errorf("xattr prefixes: %w", err)
		}
		// A prefix item is its base index and an infix of at most
		// EROFS_NAME_LEN bytes: shorter than the index alone or longer than
		// that is corrupt.
		if len(item) < 1 || len(item) > NameMax+1 {
			return fmt.Errorf("%w: xattr prefix of %d bytes", ErrCorrupt, len(item))
		}

		im.Prefixes = append(im.Prefixes, Prefix{BaseIndex: item[0], Infix: string(item[1:])})
		pos = next
	}

	return nil
}

// readMetadata is super.c's erofs_read_metadata: at the next 4-byte
// boundary, a 16-bit length (zero standing for 65536) and that many bytes;
// it returns the bytes and the position after them.
func readMetadata(src func(off, n int64) ([]byte, error), pos int64) ([]byte, int64, error) {
	pos = roundUp(pos, 4)

	raw, err := src(pos, 2)
	if err != nil {
		return nil, 0, err
	}

	n := int64(binary.LittleEndian.Uint16(raw))
	if n == 0 {
		n = 1 << 16
	}

	pos += 2

	item, err := src(pos, n)
	if err != nil {
		return nil, 0, err
	}

	return item, pos + n, nil
}

func sliceAt(b []byte, off, n int64) ([]byte, error) {
	if off < 0 || n < 0 || off > int64(len(b)) || n > int64(len(b))-off {
		return nil, fmt.Errorf("%w: %d bytes at offset %d of %d", ErrTruncated, n, off, len(b))
	}

	return b[off : off+n], nil
}

func roundUp(v, to int64) int64 { return (v + to - 1) / to * to }

func roundupPow2(v uint64) uint64 {
	p := uint64(1)
	for p < v {
		p <<= 1
	}

	return p
}
