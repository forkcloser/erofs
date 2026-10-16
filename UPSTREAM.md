# Upstream

Fork of <https://github.com/erofs/go-erofs> (`main`).

- Forked from: [`44d5e74`](https://github.com/erofs/go-erofs/commit/44d5e74d4dfbc6c23985fa805cec66cc57fb8558), committed 2026-08-02
- Reviewed through: [`3772b1e`](https://github.com/erofs/go-erofs/commit/3772b1e68e1ac60347bfb379d3d81375c9f7ab02), reviewed 2026-10-16

Every upstream commit after the fork point, through the reviewed one, is in
exactly one of the tables below. A port is a commit of this repository,
adapted to the fork; nothing is merged from upstream.

## Incorporated

| Upstream commit | Here | What |
| --- | --- | --- |
| [`03d68d8`](https://github.com/erofs/go-erofs/commit/03d68d88381cdf8133a0adcc248468d33aa457f5) | [`37b9c5c`](https://github.com/forkcloser/erofs/commit/37b9c5cfad78d7a05d426600a63b902679c1bcac) | `system.posix_acl_*` xattr prefixes lose their trailing dot |
| [`3772b1e`](https://github.com/erofs/go-erofs/commit/3772b1e68e1ac60347bfb379d3d81375c9f7ab02) | [`270909d`](https://github.com/forkcloser/erofs/commit/270909d373edbddaff68b8599c19bbf04b8b0f83) | `WithNoInlineData` stores every regular file in whole blocks (`-E ^inline_data`) |

## Not incorporated

| Upstream commit | Why |
| --- | --- |
| [`ba68dad`](https://github.com/erofs/go-erofs/commit/ba68dadcd923ccaae360f13594e62959cd267911) | hardlink support exists here independently ([`a664c24`](https://github.com/forkcloser/erofs/commit/a664c24ed1eec58bbcb2865896e9262f2941d343)) |
| [`112653e`](https://github.com/erofs/go-erofs/commit/112653e5bb40eb59db4ec3c3f08fb9d68a895550) | `Remove` exists here independently ([`1dcedcd`](https://github.com/forkcloser/erofs/commit/1dcedcdfcb27bfa91b3c2be9336c7cc5e3a87080)) |
| [`52cc42c`](https://github.com/erofs/go-erofs/commit/52cc42c5291c08ce1b29d1e78088d12cea4c00df) | `RemoveAll` exists here independently ([`1dcedcd`](https://github.com/forkcloser/erofs/commit/1dcedcdfcb27bfa91b3c2be9336c7cc5e3a87080)) |

## Changes upstream does not have

| Change | Here |
| --- | --- |
| `Writer.Link` for hard links; `Remove` and `RemoveAll`, with hard-linked inodes kept alive across a removal | [`a664c24`](https://github.com/forkcloser/erofs/commit/a664c24), [`1dcedcd`](https://github.com/forkcloser/erofs/commit/1dcedcd) |
| Every size read from the image is bounded before it drives an allocation: nid and chunk addresses, chunk-index counts, directory and symlink sizes, path-resolution work; cyclic directory graphs are rejected | [`3df08d9`](https://github.com/forkcloser/erofs/commit/3df08d9), [`2781724`](https://github.com/forkcloser/erofs/commit/2781724), [`5282e37`](https://github.com/forkcloser/erofs/commit/5282e37), [`83a0a54`](https://github.com/forkcloser/erofs/commit/83a0a54), [`6b975fc`](https://github.com/forkcloser/erofs/commit/6b975fc) |
| Chunk extents are mapped at block granularity and holes are preserved | [`b87aaba`](https://github.com/forkcloser/erofs/commit/b87aaba) |
| Reader correctness: paths held to the `fs.FS` contract, a short block read at the end of the image tolerated, dirent names decoded at the maximum block size, empty symlink targets and metadata-only chunks naming the source's own device refused, one file open for writing at a time | [`a668ca0`](https://github.com/forkcloser/erofs/commit/a668ca0), [`2a2f4eb`](https://github.com/forkcloser/erofs/commit/2a2f4eb), [`c0ca854`](https://github.com/forkcloser/erofs/commit/c0ca854), [`fe740c9`](https://github.com/forkcloser/erofs/commit/fe740c9), [`32afc92`](https://github.com/forkcloser/erofs/commit/32afc92), [`5a1d5d7`](https://github.com/forkcloser/erofs/commit/5a1d5d7) |
| Short inline reads are no longer zero-filled to the declared size; an entry beneath a regular file is refused (`ErrNotDirectory`); xattr name and value lengths are checked against their on-disk field widths instead of truncated into them | [`ea2a0ed`](https://github.com/forkcloser/erofs/commit/ea2a0ed) |
| Dirent names carrying a path separator are rejected, so `fs.WalkDir` with `filepath.Join` cannot be walked out of its destination and a crafted name cannot collapse to a whiteout that erases every prior layer | [`f3e1b84`](https://github.com/forkcloser/erofs/commit/f3e1b84) |
| Superblock geometry is bounded at `Open`; an xattr with an undefined name-index prefix or a duplicate key is rejected rather than taken last-one-wins, which let `security.capability` be spoofed | https://github.com/forkcloser/erofs/releases/tag/v1.0.0 |
| Images the library wrote but could not read back: the device table stamped 128 bytes off, the last dirent of a full maximum-size block unopenable, `Symlink("", …)` resolving to the root, chunk indexes carrying address bits the format does not cover, a metadata-only copy remapping a chunk onto the destination's own data file, a negative source `Size()`, an unaligned pre-existing data file, `Mode` read from two fields | [`f3c701e`](https://github.com/forkcloser/erofs/commit/f3c701e), [`c0ca854`](https://github.com/forkcloser/erofs/commit/c0ca854), [`fe740c9`](https://github.com/forkcloser/erofs/commit/fe740c9), [`32afc92`](https://github.com/forkcloser/erofs/commit/32afc92), [`75b5a7d`](https://github.com/forkcloser/erofs/commit/75b5a7d), [`ea2a0ed`](https://github.com/forkcloser/erofs/commit/ea2a0ed) |
| Images from the reference `mkfs.erofs` with shared xattrs (SELinux labels) in the final block are readable; the reader honours the `fs.FS` path contract and `fstest.TestFS` passes; names that are not valid UTF-8 stay readable | [`2a2f4eb`](https://github.com/forkcloser/erofs/commit/2a2f4eb), [`a668ca0`](https://github.com/forkcloser/erofs/commit/a668ca0) |
| The read path is about three times faster by path and reads a 500-entry directory in 18 allocations instead of 1015 (the README's table) | [`a5e507e`](https://github.com/forkcloser/erofs/commit/a5e507e), [`dbe4342`](https://github.com/forkcloser/erofs/commit/dbe4342) |
| `CopyFrom` from a generic `fs.FS` keeps modification times; the image fast path errors where it skipped; flat-plain addresses are bounded by the image; hard links are detected across `CopyFrom`; ownership and mtime on FreeBSD | https://github.com/forkcloser/erofs/pull/8 |
| API for v1 (breaking): `Opt` and `EroFS` gone, `Writer.Mknod` takes an `fs.FileMode`, `Stat`'s `Ino`, `Nlink` and `Rdev` are `uint64`, `WithDataFile` takes an interface, `Chown` rejects ids outside 32 bits | https://github.com/forkcloser/erofs/pull/8 |
| Host stat support on linux, darwin and FreeBSD only; the other platforms copy mode, size and mtime, as Windows does | https://github.com/forkcloser/erofs/pull/24 |
| Three truncations refused with `ErrInvalid`: an image past 2³² blocks, an `Rdev` wider than 32 bits, an `i_size` at or past 2⁶³ | https://github.com/forkcloser/erofs/pull/95 |
| `mkfs` refuses a host device number wider than `i_rdev` | https://github.com/forkcloser/erofs/pull/96 |
| A malformed image matches `ErrCorrupt` | https://github.com/forkcloser/erofs/pull/108 |
| The reader follows the kernel where an image departs from `mkfs.erofs`: the superblock checksum is verified before the feature bits; a flat-plain file at the null address reads as zeros; reserved `i_format` bits, the nlink-1 bit on a compact file, a nonzero `i_nb` on an extended inode and a header-only xattr body are `ErrNotImplemented`; `Stat.Rdev` of a FIFO or socket is zero; `DataRange` covers block-array chunk maps and names the device a mapped address lands on; `Lstat` and `Stat` return a nil `FileInfo` beside their error | https://github.com/forkcloser/erofs/pull/130 |
| A structure outside a complete image is `ErrCorrupt`, not the reader's end of file | https://github.com/forkcloser/erofs/pull/131 |
| A flat file's blocks are placed through the device table, as the kernel places them | https://github.com/forkcloser/erofs/pull/132 |
| The writer refuses what its reader and the kernel refuse (symlink targets over 4095 bytes or with a NUL, names over 255 bytes, empty or bare-prefix xattr names, from the API and from `CopyFrom` alike), and `Writer.Stat` reads an entry back before `Close` with its `Stat` and accessors | https://github.com/forkcloser/erofs/pull/135 |
| The writer stamps the superblock checksum (`SB_CHKSUM`), as `mkfs.erofs` does by default, over the span the kernel verifies | https://github.com/forkcloser/erofs/pull/138 |
| The reader refuses, as the kernel refuses the mount, an ishare xattr prefix id (`EROFS_FEATURE_COMPAT_ISHARE_XATTRS`) at or past the long xattr prefix count | https://github.com/forkcloser/erofs/pull/143 |
| `cmd/erofs-cli`, upstream's debugging dump of an image's tree, is removed | https://github.com/forkcloser/erofs/pull/145 |
