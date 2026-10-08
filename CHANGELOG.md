# Changelog

All notable changes to this fork are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/). Changes are described against the
fork point, upstream [`erofs/go-erofs`](https://github.com/erofs/go-erofs)
commit `44d5e74`.

## [Unreleased]

### Added

- `ErrCorrupt`: every error the reader reports for a malformed image (an
  inode, dirent, chunk index or xattr whose fields disagree with the format
  or point outside the image, a bad superblock) now matches it, so a caller
  can tell a bad image from a bad argument (`ErrInvalid` alone) and from a
  failing reader (whose error is passed through untouched). `ErrCorrupt`
  also matches `ErrInvalid`, which every such error reported before.

## [1.0.1] - 2026-10-05

### Fixed

- `CopyFrom` refuses, with `ErrInvalid`, three inputs it used to truncate into
  a wrong image: data past 2^32 blocks, where the 32-bit block addresses
  wrapped and the metadata landed inside file data; a `*Stat` source whose
  `Rdev` needs more than 32 bits; and a metadata-only copy of an image whose
  `i_size` is 2^63 or more, which made `Writer.Stat` report a negative size.
- A host file's device number, read from its platform stat, is refused
  rather than narrowed when it needs more than the 32-bit `i_rdev`: on Linux a
  major past 4095 or a minor past 2^20-1 used to be stored as a different
  device.
- Errors wrap a sentinel, so `errors.Is` matches what before could only be
  told apart by its text: `ErrInvalidSuperblock` for the super block checks
  (exported but never returned until now), `ErrInvalid` for a corrupt image
  or an invalid input, `fs.ErrExist`, `fs.ErrNotExist`, `fs.ErrClosed` and
  `ErrIsDirectory` from the writer, `io.ErrUnexpectedEOF` for a short chunk
  entry. The messages are unchanged apart from the sentinel's text.
- `erofs-cli` skips the xattr listing of an entry whose `Sys()` is not an
  `*erofs.Stat` instead of panicking.

## [1.0.0] - 2026-09-12

### Removed

- `Opt` and `EroFS`, deprecated upstream; use `OpenOpt` and `Open`.
- `Stat.InodeLayout`, an on-disk enum with no exported constants.
- Host metadata from `*syscall.Stat_t` on DragonFly BSD, NetBSD, OpenBSD,
  Solaris and illumos. `CopyFrom` on those systems now takes only mode, size
  and `ModTime`, as on Windows. Supported platforms are Linux, macOS, Windows
  and FreeBSD.

### Changed

- `Stat.Ino`, `Stat.Nlink` and `Stat.Rdev` are `uint64`, matching the
  `Ino()`, `Nlink()` and `Rdev()` accessor interfaces.
- `Writer.Mknod` takes an `fs.FileMode` and rejects anything that is not a
  device, FIFO or socket.
- `WithDataFile` takes a `DataFile` interface (`io.Writer`, `io.Seeker`,
  `io.ReaderAt`); an `*os.File` still satisfies it.
- `Writer.Chown` and `File.Chown` reject uids and gids outside 32 bits.
- `CopyFrom` reproduces a source's hard links as one inode with several
  names, keyed on `(Dev, Ino)` from `syscall.Stat_t` or `builder.Entry`, or
  on the nid of a source image. A file's link count is computed from the
  names the image holds rather than copied from the source; directories keep
  the source's count. `SetNlink` still overrides.
- The metadata-only copy from an image fails where the reader fails: a
  missing or truncated shared xattr, an inline xattr running past its area,
  a dirent with a bad name offset, inline data crossing its block, and
  truncated directory or symlink data are errors rather than silently
  skipped.
- `ReadDir`, `Stat` and `DataRange()` reject a flat-plain data address that
  lies outside the image, as chunk addresses already were.
- Requires Go 1.26: the `go` directive is 1.26.0, in step with the sibling
  libraries.

### Added

- `CopyFrom` takes the modification time from `fs.FileInfo.ModTime` when
  `Sys()` carries none, so `embed.FS`, `fstest.MapFS` and `os.DirFS` on any
  platform keep their times instead of reading back as 1970.
- Ownership, times and link identity from `os.DirFS` on FreeBSD, NetBSD,
  OpenBSD, DragonFly, Solaris and illumos, alongside Linux and macOS.
- `DataFile` interface; `DataRange()` documented on `Stat` as an accessor.
- Concurrency contract and limits documented on the package, `Open` and
  `Writer`.
- Reader and writer hardening, correctness fixes and performance work since
  the fork point, described in the README.

### Fixed

- `system.posix_acl_access` and `system.posix_acl_default` xattr prefixes
  were spelled with a trailing dot (ported from upstream `03d68d8`).
