// Copyright 2026 Forkcloser. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package erofs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"strconv"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/disk"
)

// A metadata-only copy of an image registers the image as a device of the
// copy, after the copy's earlier devices and before the image's own extra
// devices; opened over the source images in copy order, each followed by
// its devices, the copy reads as the sources did: every entry, every byte.

// openWithDevices opens img over its extra devices.
func openWithDevices(t *testing.T, img []byte, devices ...[]byte) fs.FS {
	t.Helper()

	var opts []erofs.OpenOpt

	if len(devices) > 0 {
		readers := make([]io.ReaderAt, len(devices))
		for i, d := range devices {
			readers[i] = bytes.NewReader(d)
		}

		opts = append(opts, erofs.WithExtraDevices(readers...))
	}

	fsys, err := erofs.Open(bytes.NewReader(img), opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return fsys
}

// sameTree holds got to want: the same paths, each with the same type,
// bits, size, time and ownership, the same bytes for a file and the same
// target for a symlink.
func sameTree(t *testing.T, want, got fs.FS) {
	t.Helper()

	seen := 0

	err := fs.WalkDir(want, ".", func(name string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		seen++

		w, err := fs.Lstat(want, name)
		if err != nil {
			return err
		}

		g, err := fs.Lstat(got, name)
		if err != nil {
			t.Errorf("%s: %v in the copy", name, err)

			return nil
		}

		ws, gs := w.Sys().(*erofs.Stat), g.Sys().(*erofs.Stat)
		if w.Mode() != g.Mode() || w.Size() != g.Size() || !w.ModTime().Equal(g.ModTime()) ||
			ws.UID != gs.UID || ws.GID != gs.GID || ws.Rdev != gs.Rdev {
			t.Errorf("%s: copy %v %d %v %d:%d, source %v %d %v %d:%d",
				name, g.Mode(), g.Size(), g.ModTime(), gs.UID, gs.GID,
				w.Mode(), w.Size(), w.ModTime(), ws.UID, ws.GID)
		}

		switch {
		case w.Mode().IsRegular():
			wb, err := fs.ReadFile(want, name)
			if err != nil {
				return err
			}

			gb, err := fs.ReadFile(got, name)
			if err != nil {
				t.Errorf("%s: copy: %v", name, err)
			} else if !bytes.Equal(wb, gb) {
				t.Errorf(
					"%s: copy reads %d bytes %q, source %d %q; bytes %s",
					name,
					len(gb),
					gb[:min(8, len(gb))],
					len(wb),
					wb[:min(8, len(wb))],
					sameBytes(wb, gb),
				)
			}
		case w.Mode()&fs.ModeSymlink != 0:
			wl, err := fs.ReadLink(want, name)
			if err != nil {
				return err
			}

			if gl, err := fs.ReadLink(got, name); err != nil || gl != wl {
				t.Errorf("%s: copy links to %q (%v), source %q", name, gl, err, wl)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking the source: %v", err)
	}

	count := 0

	if err := fs.WalkDir(got, ".", func(string, fs.DirEntry, error) error { count++; return nil }); err != nil {
		t.Fatalf("walking the copy: %v", err)
	}

	if count != seen {
		t.Errorf("the copy holds %d entries, the source %d", count, seen)
	}
}

func sameBytes(a, b []byte) string {
	if bytes.Equal(a, b) {
		return "equal"
	}

	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return "differ from offset " + strconv.Itoa(i)
		}
	}

	return "differ in length"
}

// TestWriterMetadataOnlyImageIsADevice: every committed image copied
// metadata-only reads back as itself over the source and its devices.
func TestWriterMetadataOnlyImageIsADevice(t *testing.T) {
	t.Parallel()

	for _, src := range corpus.Committed(t) {
		t.Run(src.Name, func(t *testing.T) {
			t.Parallel()

			var srcDevices [][]byte
			if src.Blob != nil {
				srcDevices = append(srcDevices, src.Blob)
			}

			var srcOpts []erofs.OpenOpt
			if src.Blob != nil {
				srcOpts = append(srcOpts, erofs.WithExtraDevices(bytes.NewReader(src.Blob)))
			}

			source, err := erofs.Open(bytes.NewReader(src.Img), srcOpts...)
			if err != nil {
				t.Skipf("the package does not open %s: %v", src.Name, err)
			}

			// An image whose entries the package refuses (a header-only
			// xattr body) has no tree to compare; the reader's rules are
			// tested where they are stated.
			if err := fs.WalkDir(source, ".", func(name string, _ fs.DirEntry, err error) error {
				if err != nil {
					return err
				}

				_, err = fs.Lstat(source, name)

				return err
			}); err != nil {
				t.Skipf("the package does not read %s: %v", src.Name, err)
			}

			var buf testBuffer

			w := erofs.Create(&buf)
			if err := w.CopyFrom(source, erofs.MetadataOnly()); err != nil {
				t.Fatalf("CopyFrom: %v", err)
			}

			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			devices := append([][]byte{src.Img}, srcDevices...)
			sameTree(t, source, openWithDevices(t, buf.Bytes(), devices...))
			// fsck.erofs, where CI has it, is the verdict on the tail blocks
			// and the chunks naming the copy's own device.
			fsckWithDevice(t, buf.Bytes(), devices...)
		})
	}
}

// TestWriterMetadataOnlyMergeOfImages: two images merged metadata-only, the
// upper's files replacing the lower's, read over the two images in order.
func TestWriterMetadataOnlyMergeOfImages(t *testing.T) {
	t.Parallel()

	var lowerBuf, upperBuf, buf testBuffer

	lower := erofs.Create(&lowerBuf)
	writeFile(t, lower, "/keep", bytes.Repeat([]byte("k"), 5000))
	writeFile(t, lower, "/replaced", []byte("lower"))

	if err := lower.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}

	writeFile(t, lower, "/d/inner", bytes.Repeat([]byte("i"), 4097))

	if err := lower.Close(); err != nil {
		t.Fatal(err)
	}

	upper := erofs.Create(&upperBuf)
	writeFile(t, upper, "/replaced", bytes.Repeat([]byte("U"), 9000))
	writeFile(t, upper, "/added", []byte("tail only"))

	if err := upper.Close(); err != nil {
		t.Fatal(err)
	}

	w := erofs.Create(&buf)
	if err := w.CopyFrom(openWithDevices(t, lowerBuf.Bytes()), erofs.MetadataOnly()); err != nil {
		t.Fatal(err)
	}

	if err := w.CopyFrom(openWithDevices(t, upperBuf.Bytes()), erofs.MetadataOnly(), erofs.Merge()); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	merged := openWithDevices(t, buf.Bytes(), lowerBuf.Bytes(), upperBuf.Bytes())
	fsckWithDevice(t, buf.Bytes(), lowerBuf.Bytes(), upperBuf.Bytes())

	for name, want := range map[string][]byte{
		"keep":     bytes.Repeat([]byte("k"), 5000),
		"replaced": bytes.Repeat([]byte("U"), 9000),
		"added":    []byte("tail only"),
		"d/inner":  bytes.Repeat([]byte("i"), 4097),
	} {
		got, err := fs.ReadFile(merged, name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		} else if !bytes.Equal(got, want) {
			t.Errorf("%s: %d bytes, want %d; %s", name, len(got), len(want), sameBytes(want, got))
		}
	}
}

// TestWriterMetadataOnlyRefusesWhatStatRefuses: the fast path reads the
// source's inodes itself, and refuses what the reader's Stat refuses, in
// its class: here an i_format with reserved bits set, ErrNotImplemented,
// which the reader's Lstat gives for the same entry.
func TestWriterMetadataOnlyRefusesWhatStatRefuses(t *testing.T) {
	t.Parallel()

	for _, src := range corpus.Committed(t) {
		if src.Name != "small-chunk-512" {
			continue
		}

		edited := bytes.Clone(src.Img)
		edited[1568] = 0xff

		source := openWithDevices(t, edited)

		if _, err := fs.Lstat(source, "d/f"); !errors.Is(err, erofs.ErrNotImplemented) {
			t.Fatalf("test premise broken: Lstat of the edited inode: %v", err)
		}

		var buf testBuffer

		w := erofs.Create(&buf)
		if err := w.CopyFrom(source, erofs.MetadataOnly()); !errors.Is(err, erofs.ErrNotImplemented) {
			t.Fatalf("CopyFrom(MetadataOnly) of an image with an inode Stat refuses: %v; want ErrNotImplemented", err)
		}
	}
}

// TestWriterMetadataOnlyFlatDataOnMappedDevice: a flat file whose address
// lies in an extra device's mapped range is that device's, as the kernel's
// erofs_map_dev places it (and as the reader reads it); the copy's chunks
// name the copy's slot for that device, not a block past the image's end.
// The input is the fuzzer's: small-blobdev-512 with "multi" turned from
// chunk-based to flat-plain at block 33 of a 13-block image, which the
// blob's slot maps from block 13.
func TestWriterMetadataOnlyFlatDataOnMappedDevice(t *testing.T) {
	t.Parallel()

	for _, src := range corpus.Committed(t) {
		if src.Name != "small-blobdev-512" {
			continue
		}

		edited := bytes.Clone(src.Img)
		edited[1536] = 0x00

		source := openWithDevices(t, edited, src.Blob)

		want, err := fs.ReadFile(source, "multi")
		if err != nil || len(want) != 3000 {
			t.Fatalf("test premise broken: the source reads multi as %d bytes, %v", len(want), err)
		}

		var buf testBuffer

		w := erofs.Create(&buf)
		if err = w.CopyFrom(source, erofs.MetadataOnly()); err != nil {
			t.Fatalf("CopyFrom: %v", err)
		}

		if err = w.Close(); err != nil {
			t.Fatal(err)
		}

		copied := openWithDevices(t, buf.Bytes(), edited, src.Blob)

		got, err := fs.ReadFile(copied, "multi")
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("the copy reads multi as %d bytes, %v; %s", len(got), err, sameBytes(want, got))
		}
	}
}

// TestWriterMetadataOnlyRefusesBogusMode: the fast path refuses, as Stat
// does and in its class, an inode whose i_mode names no type (the kernel's
// "bogus i_mode" at iget) and a dirent whose type disagrees with its
// inode's. The first input is the fuzzer's: small-chunk-512 with the low
// byte of d/sub/e32's nid zeroed, naming nid 0, a slot of zeros.
func TestWriterMetadataOnlyRefusesBogusMode(t *testing.T) {
	t.Parallel()

	for _, src := range corpus.Committed(t) {
		if src.Name != "small-chunk-512" {
			continue
		}

		edited := bytes.Clone(src.Img)
		edited[2112] = 0x00

		source := openWithDevices(t, edited)

		if _, err := fs.Lstat(source, "d/sub/e32"); !errors.Is(err, erofs.ErrCorrupt) {
			t.Fatalf("test premise broken: Lstat of the entry naming nid 0: %v", err)
		}

		var buf testBuffer

		w := erofs.Create(&buf)
		if err := w.CopyFrom(source, erofs.MetadataOnly()); !errors.Is(err, erofs.ErrCorrupt) {
			t.Fatalf("CopyFrom(MetadataOnly) of an image with a bogus i_mode: %v; want ErrCorrupt", err)
		}
	}

	// A dirent calling a regular file a directory: the Writer packs the
	// root's dirents beside its inode, ".", ".." then "a", twelve bytes
	// each, the file type at byte ten.
	var built testBuffer

	w := erofs.Create(&built)
	writeFile(t, w, "/a", []byte("a"))

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	img := built.Bytes()
	at := inodeOffset(t, img, ".") + disk.SizeInodeExtended + 2*disk.SizeDirent + 10

	if img[at] != disk.FileTypeReg {
		t.Fatalf("test premise broken: a's dirent file type is %d, want %d", img[at], disk.FileTypeReg)
	}

	img[at] = disk.FileTypeDir

	source := openWithDevices(t, img)

	if _, err := fs.Lstat(source, "a"); !errors.Is(err, erofs.ErrCorrupt) {
		t.Fatalf("test premise broken: Lstat of a file whose dirent says directory: %v", err)
	}

	var buf testBuffer

	w = erofs.Create(&buf)
	if err := w.CopyFrom(source, erofs.MetadataOnly()); !errors.Is(err, erofs.ErrCorrupt) {
		t.Fatalf("CopyFrom(MetadataOnly) of an image whose dirent disagrees with its inode: %v; want ErrCorrupt", err)
	}

	// The case only the mode check guards: a dirent of type zero beside an
	// inode whose type bits are zero, the two in agreement about nothing.
	// i_mode is the two bytes at offset 4 of the inode.
	bogus := built.Bytes()
	bogus[at] = 0
	inode := inodeOffset(t, bogus, "a")
	binary.LittleEndian.PutUint16(bogus[inode+4:], binary.LittleEndian.Uint16(bogus[inode+4:])&^disk.StatTypeMask)

	source = openWithDevices(t, bogus)

	if _, err := fs.Lstat(source, "a"); !errors.Is(err, erofs.ErrCorrupt) {
		t.Fatalf("test premise broken: Lstat of an inode with no type: %v", err)
	}

	w = erofs.Create(&buf)
	if err := w.CopyFrom(source, erofs.MetadataOnly()); !errors.Is(err, erofs.ErrCorrupt) {
		t.Fatalf("CopyFrom(MetadataOnly) of an image with an inode of no type: %v; want ErrCorrupt", err)
	}
}
