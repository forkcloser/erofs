package disk

import "io/fs"

// The file type a directory entry carries (erofs_dirent.file_type).
const (
	FileTypeReg     = 1
	FileTypeDir     = 2
	FileTypeChrdev  = 3
	FileTypeBlkdev  = 4
	FileTypeFifo    = 5
	FileTypeSock    = 6
	FileTypeSymlink = 7
)

// The type and permission bits of an inode mode (erofs_inode.i_mode), as
// in stat(2).
const (
	StatTypeMask    = 0o170000 // Mask for the type bits
	StatTypeReg     = 0o100000 // Regular file
	StatTypeDir     = 0o040000 // Directory
	StatTypeChrdev  = 0o020000 // Character device
	StatTypeBlkdev  = 0o060000 // Block device
	StatTypeFifo    = 0o010000 // FIFO
	StatTypeSock    = 0o140000 // Socket
	StatTypeSymlink = 0o120000 // Symlink
	StatTypeIsUID   = 0o004000 // Setuid on execution
	StatTypeIsGID   = 0o002000 // Setgid on execution
	StatTypeIsVTX   = 0o001000 // Sticky bit
)

// EroFSFtypeToFileMode converts a directory entry file type to the
// corresponding fs.FileMode type bits.
func EroFSFtypeToFileMode(ftype uint8) fs.FileMode {
	switch ftype {
	case FileTypeDir:
		return fs.ModeDir
	case FileTypeChrdev:
		return fs.ModeDevice | fs.ModeCharDevice
	case FileTypeBlkdev:
		return fs.ModeDevice
	case FileTypeFifo:
		return fs.ModeNamedPipe
	case FileTypeSock:
		return fs.ModeSocket
	case FileTypeSymlink:
		return fs.ModeSymlink
	default:
		return 0
	}
}

// EroFSModeToGoFileMode converts an inode mode to an fs.FileMode, type bits
// and permission bits included. An unknown type maps to fs.ModeIrregular.
func EroFSModeToGoFileMode(mode uint16) fs.FileMode {
	var m fs.FileMode

	m |= fs.FileMode(mode) & fs.ModePerm
	switch mode & StatTypeMask {
	case StatTypeReg:
	case StatTypeDir:
		m |= fs.ModeDir
	case StatTypeChrdev:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case StatTypeBlkdev:
		m |= fs.ModeDevice
	case StatTypeFifo:
		m |= fs.ModeNamedPipe
	case StatTypeSock:
		m |= fs.ModeSocket
	case StatTypeSymlink:
		m |= fs.ModeSymlink
	default:
		m |= fs.ModeIrregular // Unknown type, treat as irregular file
	}

	if mode&StatTypeIsUID != 0 {
		m |= fs.ModeSetuid
	}

	if mode&StatTypeIsGID != 0 {
		m |= fs.ModeSetgid
	}

	if mode&StatTypeIsVTX != 0 {
		m |= fs.ModeSticky
	}

	return m
}

// RdevFromMode returns the device number an inode of the given mode stores
// in its i_u field, or 0 when the type carries none.
func RdevFromMode(mode uint16, inodeData uint32) uint32 {
	switch mode & StatTypeMask {
	case StatTypeChrdev, StatTypeBlkdev, StatTypeFifo, StatTypeSock:
		// inodeData field is device number for some file types
		return inodeData
	default:
		return 0 // Not a device type
	}
}
