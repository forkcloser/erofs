# go-erofs

A Go library for reading and creating [EROFS](https://erofs.docs.kernel.org/) filesystem images using the standard [fs.FS](https://pkg.go.dev/io/fs#FS) interface.

## About this fork

A fork of [github.com/erofs/go-erofs](https://github.com/erofs/go-erofs),
hardened against untrusted images, corrected on several on-disk paths, and
made substantially faster to read. Every fix and feature this fork has that
upstream does not, each with the change that landed it, is the table in
[`UPSTREAM.md`](./UPSTREAM.md), beside the fork point and what was taken from
upstream since; each fix ships with a regression test. A symlink target is
resolved from the image root, as it is under a kernel mount, so
[`fs.Sub`](https://pkg.go.dev/io/fs#Sub) bounds the paths a caller may write
but not where a link inside the subtree may lead.

Measured against the fork point (`44d5e74`), medians of six runs on an Apple
M5 Pro with go1.25, through the public API only:

| operation | upstream | this fork |
| --- | --- | --- |
| `fs.Stat` by path | 1638 ns, 4701 B, 9 allocs | **477 ns, 528 B, 5 allocs** |
| `Open` by path | 1467 ns, 4437 B, 6 allocs | **427 ns, 288 B, 4 allocs** |
| Read a 500-entry directory | 30.9 µs, 1015 allocs | **10.3 µs, 18 allocs** |
| Read an 8 MiB file | 346 µs, 2051 reads | **241 µs, 4 reads** |
| Write a 500-entry directory | 1.41 ms, 4348 allocs | **1.37 ms, 3319 allocs** |

The read count is `ReadAt` calls per whole-file `fs.ReadFile`, covering the
lookup and the inode as well as the data. Writing is bounded by serialization
rather than by allocation, so fewer allocations there buy little wall time.

Requires Go 1.26.

## Assurance

The reader is held to a reference reader of the on-disk format
(`internal/erofsspec`), written from the kernel's `erofs_fs.h` and the rules
its reader applies (`super.c`, `inode.c`, `dir.c`, `namei.c`, `xattr.c`,
`data.c`), sharing no code with the package. The reference is anchored first:
it parses images built by the pinned `mkfs.erofs` from a described tree back
to the tree, field for field. Committed under `internal/corpus/testdata` with
a digest manifest, so that every CI lane judges the same bytes: 18 such images
(block sizes 512, 1024, 4096 and 16384, compact and extended inodes, inline
and plain data, chunk maps as block arrays and as indexes, a blob device, long
xattr prefixes, no checksum, the dot-omitted layout) and 5 from a small
assembler for the states `mkfs.erofs` never writes from a tar (shared xattrs,
a header-only xattr body, a flat file at the null address, an xattr name under
index 0). Where `mkfs.erofs` is present, the committed images are rebuilt,
read back to the tree and passed to `fsck.erofs`, and under the build the
manifest names (`erofs-utils` 1.9.4 on Darwin) compared byte for byte; the
full tree is built under the whole option matrix, 31 sets over every block
size from 512 to 16384.

Over that corpus, and over the writer's own images at the block sizes
`mkfs.erofs` refuses, `Open` serves what the reference reads: every `Lstat`
and `Stat` field and accessor, `DataRange` against the reference's extents,
every read path, `ReadDir` and `ReadDirFile`, `ReadLink` and `Stat` through
links. Over single-byte edits and truncations of four small images (every one
of them in the full sweep, an opt-in for a local run), the reader's verdict
is the kernel's: a sound image reads to the same tree, a
corrupt or truncated one is refused as `ErrCorrupt`, an unsupported one as
`ErrNotImplemented` or `ErrCorrupt`, with the documented divergences named in
the test. The defects the differential found in the reader are listed in the
commit that added `contract_test.go`.

The reader is stricter than the kernel in the following places; all but the
last two are divergences the differential names: a dirent whose type disagrees
with its inode's mode is `ErrCorrupt`, where the kernel serves each as it is;
a dirent nid with bits 59 to 63 set is `ErrCorrupt`, where the kernel's shift
drops them and reads another inode; a symlink target longer than 4095 bytes is
`ErrCorrupt`, where the kernel serves one beside its inode whole and one in a
data block cut to a page less a byte, and one that is empty or holds a
NUL is `ErrCorrupt`, where the kernel makes a C string of it; an extended
attribute name an inode lists twice is `ErrCorrupt`, where the kernel lists
both and serves the first; a file whose map runs past its device is refused at
`Stat`, where the kernel fails at the read; an image whose superblock announces
compression is `ErrNotImplemented` even when no inode is compressed, where the
kernel mounts it; a shared extended attribute block address past the end of
the image is refused at `Open`, where the kernel fails only when an inode uses
it; a directory declaring more bytes than its image and the extra devices
given to `Open` hold is `ErrCorrupt`, its entries being their bytes, where the
kernel reads the blocks a chunk map names however often it names one; in an
image opened with its extra devices, a flat file whose blocks run over an edge
of a device's mapped range, out of it or into it, is `ErrCorrupt` by every
path, where the kernel translates the one address a read begins at and reads
on from there, so that the device answering for the far side depends on where
its reads begin (opened with none of them, the image is one device and the
file is read straight through, as the kernel reads it). Apart from those, an
image whose superblock declares no blocks is `ErrCorrupt` at `Open` when its
reader reports no length: its extent cannot be known, and nothing it declares
can be held to one.

## Features

- **Read** EROFS images through Go's `fs.FS` interface
- **Create** EROFS images from directories or any `fs.FS`
- **Merge** multiple filesystem sources with overlay whiteout support
- **Metadata-only** mode for container layer indexing (chunk-based references to original data)
- Pure Go, no CGO — uses only the standard library

### Status

- [x] Read erofs files created with default `mkfs.erofs` options
- [x] Read chunk-based erofs files with indexes
- [x] Xattr support including long xattr prefixes
- [x] Extra devices for chunked data, and an image opened without them as the kernel mounts it (flatdev: metadata and in-image data served, blob data refused where it is read)
- [x] Create erofs files from any `fs.FS`
- [x] Directory to erofs packing
- [x] Merge multiple filesystem layers with whiteout processing
- [ ] Read erofs files with compression

## Reading an EROFS image

```go
f, err := os.Open("image.erofs")
if err != nil {
    log.Fatal(err)
}
defer f.Close()

img, err := erofs.Open(f)
if err != nil {
    log.Fatal(err)
}

fs.WalkDir(img, ".", func(path string, d fs.DirEntry, err error) error {
    fmt.Println(path)
    return nil
})
```

## Merging multiple layers

Combine multiple filesystem sources into one image. The `Merge` option enables overlay semantics — AUFS-style whiteout files (`.wh.<name>`) delete entries from prior layers:

```go
outFile, _ := os.Create("merged.erofs")
w := erofs.Create(outFile)

w.CopyFrom(baseLayer)
w.CopyFrom(overlayLayer, erofs.Merge())
w.Close()
```

Merge can also be combined with `MetadataOnly` to build a merged index without copying data:

```go
w := erofs.Create(outFile)
w.CopyFrom(layer1, erofs.MetadataOnly())
w.CopyFrom(layer2, erofs.MetadataOnly(), erofs.Merge())
w.Close()
```

A metadata-only copy of an EROFS image registers the image as an extra device
of the result, followed by the image's own extra devices: every file's blocks
stay where they are and the result's chunk indexes point at them; only a
file's inline tail, shorter than a block, is copied, as one block. Open the
result with `WithExtraDevices` over the source images in copy order, each
followed by its own devices.

That tail is the file's data: `Close` reads it from the source image when it
writes the result, and the `Writer` holds none of it until then. A source
image must stay as it was, and readable, until `Close`, as a source copied
with its data must.

## Building an image programmatically

```go
outFile, _ := os.Create("image.erofs")
w := erofs.Create(outFile)

f, _ := w.Create("/hello.txt")
f.Write([]byte("hello world\n"))
f.Close()

w.Mkdir("/dir", 0o755)
w.Symlink("hello.txt", "/link")

w.Close()
outFile.Close()
```

Close each file before creating the next one. File data is appended to a single
stream, so two open writers would interleave their bytes; `Create` returns an
error rather than let that corrupt the image silently.

### The writer's surface

Beyond `Create`, `Mkdir`, `Symlink` and `CopyFrom`:

- `Link` adds a name for an existing inode; `Mknod` creates a device, FIFO or
  socket from an `fs.FileMode`.
- `Remove` and `RemoveAll` drop entries, with `unlink(2)` semantics for
  hard-linked names; together with `CopyFrom(..., Merge())` this is how
  layers are merged programmatically.
- `Chmod`, `Chown`, `Chtimes`, `Setxattr` and `SetNlink` edit metadata by
  path; `File.Chmod` and `File.Chown` do the same on an open file.
- `Stat` and `Open` read entries back before `Close`.
- `Create` options: `WithBlockSize`, `WithBuildTime`, `WithNoInlineData`
  (a regular file's data in whole blocks, none beside its inode, which a DAX
  mount needs), `WithDataFile` (an external data file for metadata-only
  images) and `WithTempDir` (where the spool lives otherwise). `CopyFrom`
  options: `MetadataOnly` and `Merge`.

`CopyFrom` reads a source's file data when the image is written, at `Close`
(at once, with a data file), one file open at a time: the source stays as it
was until then. A host directory's files are first opened then, by name: one
that cannot be opened, or has become shorter, fails `Close`; one that has
grown is copied as it was found, its first bytes; and a name that holds
another file by then is read as that file, under the metadata of the first.

A `CopyFrom` that fails once it has begun to copy has left a part of its
source in the tree, so the `Writer` has failed with it: every later call that
would change it returns that error, and `Close` returns it and writes no
image. `Stat` and `Open` still answer for what it holds. A `CopyFrom` refused
before it copies anything does not fail the `Writer`.

Paths given to the writer are cleaned: `x`, `/x` and `./x` name the same
entry. The reader is stricter and follows the `fs.FS` convention exactly.

`CopyFrom` takes ownership, times and link identity from `Sys()` when it is a
`*syscall.Stat_t` (linux, darwin and freebsd) or this
package's `*Stat`, as an image opened here hands out; a source with neither of
them still contributes mode, size and `ModTime`. Two names that share a source
inode become one inode with two names in the image when the source reports
more than one link for it; a file's link count is computed from the names the
image holds, a directory's from its subdirectories. A device keeps the major
and minor numbers it has on the host: a `*syscall.Stat_t`'s `Rdev` is read in
the host's own layout and stored in the kernel's; one whose major is over 4095
or whose minor is over 1048575 is refused, `ErrInvalid`.

## Limits

The reader accepts block sizes from 512 bytes to 64 KiB. Compressed inodes
and 48-bit chunk addressing are rejected with
`ErrNotImplemented`. The bounds that protect a caller from a hostile image
report `ErrInvalid`: paths of at most 4096 bytes, 255
symlink hops per resolution, a chunk-index map of at most 64 MiB per file,
and `fs.ReadFile` refusing files over 128 MiB (open and `io.Copy` those).
The writer refuses, `ErrInvalid`, a file name over 255 bytes, which no lookup
would reach, and one with a NUL, which would read back as another name. It
stores xattr names of at most 255 bytes after their prefix and values of at
most 65535 bytes. `Chown` refuses negative ids.

An extended attribute, name and value, that more than two files carry is
stored once, in the image's shared area, and each file names it there, as
`mkfs.erofs` stores it by default. A file names at most 255 attributes
there, the first in the order of their names, and holds the others. A file
whose inode cannot hold all of its attributes itself has its first 255
stored there, however few files carry them.

None of these bounds a copy by the size of its source. `CopyFrom` writes the
tree it walks, and an EROFS image may store once what many of its files name:
a block that many chunks map, a block of entries that several directories
list as theirs. The copy stores it for each
of them: with its data, a file at its logical size, and every directory's
names. An image of 8 KiB whose
one file is 512 chunks of one block is a copy of over two megabytes. A caller
that copies an image it does not trust bounds what it writes to.

The `fs.FS` from `Open` is safe for concurrent use; the files it opens and a
`Writer` are not. It keeps the extended attributes of the image's shared area
as it reads them, a mebibyte of them at most: the label every file of a tree
names is read once.

A `Writer` holds the tree it is given in memory until `Close`, and no file's
data: under 2 KiB, and for each name the name's path and under 512 bytes, or
under 768 for a file of an image copied with its data, so that a tree of a
million names with paths of 50 bytes is under 600 MB, and under 850 MB copied
whole from an image. On top of that come a symbolic link's target, sixteen bytes
for each run of blocks that follow each other in a file that a metadata-only
copy leaves in its source or that has holes, and the extended attributes: a name
and a value once for all the entries a `CopyFrom` found them on, and, for each
entry `Setxattr` gave them to, the strings it was given, kept as they are and
not copied, with 32 bytes for each attribute of each distinct list of them. Each
of these is held in the size the allocator rounds it up to, a quarter more at
most for one of a hundred bytes or more. An image that was copied, with its data
or without, stays open until `Close`: under 1 KiB of its own, its tables of
devices and of long attribute prefixes, and what it kept of its shared
attributes as it was read, under 1.5 MiB.

## Releasing

Versions are signed annotated tags following semantic versioning, cut from
`main`; the library has no build artefacts. Before tagging: `just lint` and
`just test` green on CI, every merged pull request's title saying what
changed for a user (the release notes are those titles, published with
`gh release create vX.Y.Z --verify-tag --generate-notes`; one that breaks a
consumer carries the `breaking` label), and the tag message naming the
headline changes.
Changes to exported names or documented behaviour after 1.0 are a major
version.
