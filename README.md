# go-erofs

A Go library for reading and creating [EROFS](https://erofs.docs.kernel.org/) filesystem images using the standard [fs.FS](https://pkg.go.dev/io/fs#FS) interface.

## About this fork

A fork of [github.com/erofs/go-erofs](https://github.com/erofs/go-erofs),
hardened against untrusted images, corrected on several on-disk paths, and
made substantially faster to read. Each fix below ships with a regression
test, and the security-relevant ones were confirmed to fail against the code
they replace.

**Untrusted input.** `CopyFrom` sized allocations directly from superblock and
inode fields, so a few edited bytes in an 8 KiB image could request a
terabyte-scale buffer — which Go reports as an unrecoverable fatal OOM, not a
returned error. The directory walk also had no cycle detection and never
terminated on an image whose directories reach themselves.

Path resolution bounded symlink hops but not the work done per hop, so an
8 KiB image could hold a single `Open` for 88 seconds across 33 million reads;
the total walk is now bounded. `nid` and chunk block addresses are validated
before they become file offsets, since the overflowed values were handed to
the caller's `io.ReaderAt` as negative offsets — an error for `*bytes.Reader`
and `*os.File`, a panic for a slice-backed reader. Chunk-index buffers are no
longer allocated from a declared size before the read that would show the data
is absent. Dirent names carrying a path separator are rejected, so the
standard extract pattern (`fs.WalkDir` plus `filepath.Join`) cannot be walked
out of its destination directory, and in merge mode a crafted name can no
longer collapse to a whiteout that erases every prior layer. Superblock
geometry is bounded at `Open`, and xattrs with an undefined name-index prefix
or a duplicate key are rejected rather than silently taken last-one-wins,
which is what let `security.capability` be spoofed.

**Correctness.** Chunk extents are mapped at block granularity and holes are
preserved across a re-index; previously a region declared as a hole could read
back as real bytes from the backing device, at an offset the image never
referenced. Short reads are no longer silently zero-filled, entries can no
longer be attached beneath a regular file and then vanish from the image, two
files can no longer be opened for writing at once and share one data region,
and xattr names and values are checked against their on-disk field widths
instead of being truncated into them.

Several defects were images this library wrote but could not read back. The
device table was stamped with an offset 128 bytes from where it was written.
At the maximum block size a full directory block listed every entry but could
not open the last one in each block, because the name-end offset was computed
in a `uint16` that 65536 truncates to zero. `Symlink("", …)` produced a link
that resolved to the root, so any path through it silently discarded
everything to its left. Chunk indexes carried address bits the declared format
does not cover, which the kernel reads as `phys mod 2^32`. A metadata-only
copy could remap a chunk onto the destination's own data file and serve an
unrelated layer's bytes; a source reporting a negative `Size()` produced a
plausible-looking image whose every inode sat one byte off its slot; and a
pre-existing data file that did not start on a block boundary put the first
file's chunk on an earlier block. `fs.FileInfo.Mode` and `Sys().(*Stat).Mode`
took the file type from two different on-disk fields and could disagree; both
now come from the inode.

**Compatibility.** Images produced by the reference `mkfs.erofs` are readable.
Any inode carrying a shared xattr — which is how SELinux labels are stored —
used to fail outright, because the shared xattr area sits in the image's final
block and a whole-block read there runs past the end of the file. The reader
also now honours the [`fs.FS`](https://pkg.go.dev/io/fs#FS) path contract:
rooted paths, `.` and empty elements, and `..` traversal are rejected, so
`fstest.TestFS` passes. Names that are not valid UTF-8 remain readable, since
EROFS stores names as byte strings. Note that a symlink target is resolved
from the image root, as it is under a kernel mount, so
[`fs.Sub`](https://pkg.go.dev/io/fs#Sub) bounds the paths a caller may write
but not where a link inside the subtree may lead.

**Performance.** Reading a whole file no longer costs one `ReadAt` per
filesystem block, and `io.Copy` goes through `io.WriterTo`. Inodes, dirents,
xattr headers, the superblock and device slots are all encoded and decoded
directly rather than through reflection, and a pooled block that was being
stranded on every inode read is returned. `ReadDir` converts each block's name
region once and sub-slices per entry instead of allocating a string and a heap
entry apiece, and the writer carves its entries from one slab and emits a
dirent block per `Write` rather than one per name.

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

Requires Go 1.25.

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
- [x] Extra devices for chunked data
- [x] Create erofs files from any `fs.FS`
- [x] Directory to erofs packing
- [x] AUFS whiteout to overlayfs conversion
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
