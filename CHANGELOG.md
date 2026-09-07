# Changelog

All notable changes to this fork are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/). Changes are described against the
fork point, upstream [`erofs/go-erofs`](https://github.com/erofs/go-erofs)
commit `44d5e74`.

## [1.0.0-rc.1] - 2026-09-07

### Removed

- `Opt` and `EroFS`, deprecated upstream; use `OpenOpt` and `Open`.
- `Stat.InodeLayout`, an on-disk enum with no exported constants.

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
