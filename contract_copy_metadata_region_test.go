package erofs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"testing"

	"github.com/forkcloser/go-erofs"
)

// laidAsMkfs is an empty root in an image laid out as mkfs.erofs lays its
// images out: the metadata starts at block 0, so the nids count from the
// image's first byte and the region the inodes are in runs to the image's
// end, its data included. The image declares the given count of 4 KiB
// blocks; all but the first are zeros here, where an image has its files'
// data.
func laidAsMkfs(blocks uint32) *laidImage {
	const (
		bs      = 4096
		rootNid = 36 // byte 1152, right after the superblock
	)

	head := make([]byte, bs)

	root := laidDirBlock([]laidDirent{{rootNid, laidFtDir, "."}, {rootNid, laidFtDir, ".."}})
	laidCompact(head[rootNid*32:], laidFlatInline, laidDir, 2, uint32(len(root)), 0)
	copy(head[rootNid*32+32:], root)
	laidSuper(head, 12, blocks)
	binary.LittleEndian.PutUint32(head[1024+40:], 0) // meta_blkaddr
	binary.LittleEndian.PutUint16(head[1024+14:], rootNid)

	return &laidImage{head: head, size: int64(blocks) * bs}
}

// TestContractMetadataOnlyCopyReadsWhatTheTreeNames: a metadata-only copy
// reads its source where the tree leads it. In an image laid out as
// mkfs.erofs lays it out the region the inodes are in is the whole image,
// so a copy that takes that region in first allocates and reads the image,
// data included, and has to refuse an image larger than what it will hold.
//
//nolint:paralleltest // measures the process's TotalAlloc against a budget; a parallel test's allocations would count against it
func TestContractMetadataOnlyCopyReadsWhatTheTreeNames(t *testing.T) {
	const budget = 1 << 20

	for _, tc := range []struct {
		name   string
		blocks uint32
	}{
		{"32 MiB", 32 << 20 >> 12},
		{"5 GiB", 5 << 30 >> 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := laidAsMkfs(tc.blocks)

			img, err := erofs.Open(src)
			if err != nil {
				t.Fatal(err)
			}

			src.bytes = 0
			before := allocatedSoFar()

			out := &testBuffer{}
			w := erofs.Create(out)

			err = w.CopyFrom(img, erofs.MetadataOnly())

			allocated := allocatedSoFar() - before

			if err != nil {
				t.Fatalf("CopyFrom(MetadataOnly) of an image of %d bytes holding one empty directory: %v",
					src.size, err)
			}

			if allocated > budget {
				t.Errorf("CopyFrom(MetadataOnly) of a %d byte image allocated %d bytes, over %d",
					src.size, allocated, budget)
			}

			if src.bytes > budget {
				t.Errorf("CopyFrom(MetadataOnly) read %d bytes of a %d byte image, over %d",
					src.bytes, src.size, budget)
			}

			if err = w.Close(); err != nil {
				t.Fatal(err)
			}

			copied, err := erofs.Open(bytes.NewReader(out.Bytes()))
			if err != nil {
				t.Fatal(err)
			}

			entries, err := fs.ReadDir(copied, ".")
			if err != nil || len(entries) != 0 {
				t.Errorf("the copy's root: %d entries, %v; want an empty directory", len(entries), err)
			}
		})
	}
}

// laidDirBlocks packs entries, already in name order, into directory blocks
// of bs bytes: every block but the last padded to bs, as a flat directory
// is.
func laidDirBlocks(ents []laidDirent, bs int) []byte {
	var out []byte

	for len(ents) > 0 {
		n, used := 0, 0
		for n < len(ents) && used+12+len(ents[n].name) <= bs {
			used += 12 + len(ents[n].name)
			n++
		}

		out = append(out, laidDirBlock(ents[:n])...)

		ents = ents[n:]
		if len(ents) > 0 {
			out = append(out, make([]byte, bs-used)...)
		}
	}

	return out
}

// laidSpread is an image laid out as laidAsMkfs, its inodes where at puts
// them: the root lists an empty directory for each offset of at, "d0000"..
// in at's order, then "ext", a file of five bytes whose 64-byte inode
// starts 32 bytes before the image's 64th KiB, then "zz", an empty file
// beside the root. The image is size bytes.
func laidSpread(size int, at ...int) *laidImage {
	const (
		bs      = 4096
		rootNid = 36
		zzNid   = 40
		extAt   = 64<<10 - 32
	)

	head := make([]byte, size)

	ents := make([]laidDirent, 0, len(at)+2)

	for i, off := range at {
		nid := uint64(off / 32)
		own := laidDirBlock([]laidDirent{{nid, laidFtDir, "."}, {rootNid, laidFtDir, ".."}})

		laidCompact(head[off:], laidFlatInline, laidDir, 2, uint32(len(own)), 0)
		copy(head[off+32:], own)

		ents = append(ents, laidDirent{nid, laidFtDir, fmt.Sprintf("d%04d", i)})
	}

	laidExtended(head[extAt:], laidFlatInline, laidReg, 5, 0, 1)
	copy(head[extAt+64:], "hello")
	laidCompact(head[zzNid*32:], laidFlatPlain, laidReg, 1, 0, 0)

	ents = append(ents, laidDirent{extAt / 32, laidFtReg, "ext"}, laidDirent{zzNid, laidFtReg, "zz"})

	// The root's entries, flat from block 1.
	listing := laidDirBlocks(ents, bs)
	laidCompact(head[rootNid*32:], laidFlatPlain, laidDir, 2, uint32(len(listing)), 1)
	copy(head[bs:], listing)

	laidSuper(head, 12, uint32(len(head)/bs))
	binary.LittleEndian.PutUint32(head[1024+40:], 0) // meta_blkaddr
	binary.LittleEndian.PutUint16(head[1024+14:], rootNid)

	return &laidImage{head: head, size: int64(len(head))}
}

// spreadCopy is the metadata-only copy of a laidSpread image, held to the
// tree the image describes, and what the copy read of the image.
func spreadCopy(t *testing.T, src *laidImage, dirs int) {
	t.Helper()

	img, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	src.bytes = 0
	src.at = map[int64]int64{0: 0}

	out := &testBuffer{}
	w := erofs.Create(out)

	if err = w.CopyFrom(img, erofs.MetadataOnly()); err != nil {
		t.Fatal(err)
	}

	read := src.bytes

	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	src.bytes = read

	copied := openWithDevices(t, out.Bytes(), src.whole())

	entries, err := fs.ReadDir(copied, ".")
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != dirs+2 {
		t.Fatalf("the copy's root has %d entries; want %d", len(entries), dirs+2)
	}

	for i, e := range entries[:dirs] {
		if want := fmt.Sprintf("d%04d", i); e.Name() != want || !e.IsDir() {
			t.Fatalf("entry %d is %s, a directory: %v; want the directory %s", i, e.Name(), e.IsDir(), want)
		}
	}

	got, err := fs.ReadFile(copied, "ext")
	if err != nil || string(got) != "hello" {
		t.Errorf("ext reads %q, %v; want hello", got, err)
	}

	info, err := fs.Stat(copied, "zz")
	if err != nil || info.Size() != 0 || !info.Mode().IsRegular() {
		t.Errorf("zz: %v, %v; want an empty regular file", info, err)
	}
}

// TestContractMetadataOnlyCopyOfInodesFarApart: the copy reads a window of
// its source where the tree has an inode, once, and keeps it. 300
// directories 128 KiB apart are a read each and not the 38 MiB between
// them; the two files listed after them are back where the root is, which
// is not read again for them. One of the two has its inode across the edge
// of a window, and its bytes in the next.
func TestContractMetadataOnlyCopyOfInodesFarApart(t *testing.T) {
	t.Parallel()

	const (
		dirs  = 300
		apart = 128 << 10
	)

	at := make([]int, 0, dirs)
	for i := range dirs {
		at = append(at, apart*(i+1))
	}

	src := laidSpread(apart*(dirs+1), at...)

	spreadCopy(t, src, dirs)

	if src.at[0] != 1 {
		t.Errorf("the image's first bytes were read %d times; want once", src.at[0])
	}

	if limit := int64(dirs+8) * (64 << 10); src.bytes > limit {
		t.Errorf("the copy read %d bytes of the image, over %d", src.bytes, limit)
	}
}

// TestContractMetadataOnlyCopyOfInodesInAnotherOrder: the tree lists its
// inodes in an order of its own, here 3000 directories taken in turn from
// 300 windows of the image, ten to a window: each is a visit to a window
// other than the last, and the 300 hold more than a copy that let windows
// go would keep. The copy reads the image once at most, and the root's
// entries, 51 KiB, as a directory's blocks.
func TestContractMetadataOnlyCopyOfInodesInAnotherOrder(t *testing.T) {
	t.Parallel()

	const (
		window  = 64 << 10
		windows = 300
		dirs    = 3000
	)

	at := make([]int, 0, dirs)
	for i := range dirs {
		at = append(at, (1+i%windows)*window+(i/windows)*128+4096)
	}

	src := laidSpread((windows+1)*window, at...)

	spreadCopy(t, src, dirs)

	if src.bytes > src.size+64<<10 {
		t.Errorf("the copy read %d bytes of a %d byte image", src.bytes, src.size)
	}
}

// TestContractMetadataOnlyCopyReadsNoWindowItHolds: reads that follow each
// other take more of the image each time, and stop short of what is held.
// The directory at 320 KiB is met first, then four from 64 KiB on, a
// window each: the reads are one window, two, and one again, the fourth
// stopping at the first directory's. Six windows of the image are read,
// with the root's; reading on over a held one would be nine.
func TestContractMetadataOnlyCopyReadsNoWindowItHolds(t *testing.T) {
	t.Parallel()

	const window = 64 << 10

	src := laidSpread(16*window, 5*window, window+4096, 2*window, 3*window, 4*window)

	spreadCopy(t, src, 5)

	if src.bytes >= 7*window {
		t.Errorf("the copy read %d bytes of the image; want six windows of %d and a few bytes", src.bytes, window)
	}
}

// TestContractMetadataOnlyCopyEarnsItsReadAheadAgain: a read elsewhere ends
// a run. Two directories a window apart are a read of one window and one
// of two; the one met next is far from them, and the one after it follows
// it: a window, then two, as the first time, and not four. Seven windows
// are read with the root's; the run kept would be nine.
func TestContractMetadataOnlyCopyEarnsItsReadAheadAgain(t *testing.T) {
	t.Parallel()

	const window = 64 << 10

	src := laidSpread(32*window, window+4096, 2*window, 10*window, 11*window)

	spreadCopy(t, src, 4)

	if src.bytes >= 8*window {
		t.Errorf("the copy read %d bytes of the image; want seven windows of %d and a few bytes", src.bytes, window)
	}
}

// countedReads is a source that counts its reads, and says io.EOF with the
// read that ends at its last byte, as a reader may.
type countedReads struct {
	data                  []byte
	calls, bytes, largest int
}

func (c *countedReads) Size() int64 { return int64(len(c.data)) }

func (c *countedReads) ReadAt(p []byte, off int64) (int, error) {
	c.calls++

	if off >= int64(len(c.data)) {
		return 0, io.EOF
	}

	n := copy(p, c.data[off:])
	c.bytes += n
	c.largest = max(c.largest, n)

	if off+int64(n) == int64(len(c.data)) {
		return n, io.EOF
	}

	return n, nil
}

// writtenNotes is an image this package wrote that is all metadata, in the
// order a copy meets it: files empty files in the root, each with a
// user.note of a kilobyte that begins with the file's number.
func writtenNotes(t *testing.T, files int) []byte {
	t.Helper()

	made := &testBuffer{}
	mk := erofs.Create(made)
	note := bytes.Repeat([]byte{'n'}, 1000)

	for i := range files {
		name := fmt.Sprintf("f%05d", i)
		writeFile(t, mk, name, nil)

		copy(note, fmt.Sprintf("%08d", i))

		if err := mk.Setxattr(name, "user.note", string(note)); err != nil {
			t.Fatal(err)
		}
	}

	if err := mk.Close(); err != nil {
		t.Fatal(err)
	}

	return made.Bytes()
}

// writtenTree is an image this package wrote that is all metadata, in an
// order a copy does not meet it in: files empty files spread over dirs
// directories of two levels, every tenth a second name of the file before
// it.
func writtenTree(t *testing.T, files, dirs int) []byte {
	t.Helper()

	made := &testBuffer{}
	mk := erofs.Create(made)

	path := func(i int) string {
		return fmt.Sprintf("a%d/d%03d/f%05d", i%dirs%7, i%dirs, i)
	}

	for i := range files {
		if i < 7 {
			if err := mk.Mkdir(fmt.Sprintf("a%d", i), 0o755); err != nil {
				t.Fatal(err)
			}
		}

		if i < dirs {
			if err := mk.Mkdir(fmt.Sprintf("a%d/d%03d", i%7, i), 0o755); err != nil {
				t.Fatal(err)
			}
		}

		if i%10 == 9 {
			if err := mk.Link(path(i-1), path(i)); err != nil {
				t.Fatal(err)
			}

			continue
		}

		writeFile(t, mk, path(i), nil)
	}

	if err := mk.Close(); err != nil {
		t.Fatal(err)
	}

	return made.Bytes()
}

// TestContractMetadataOnlyCopyReadsMetadataInFewReads: where a tree's
// metadata follows the order the copy meets it in, as one directory's does
// in an image this package wrote, the copy reads it in reads that grow as
// they follow each other, to 4 MiB and no further: not one for every 64
// KiB, and the copy is the tree. 14000 files with a note of a kilobyte are
// 15 MiB of metadata.
func TestContractMetadataOnlyCopyReadsMetadataInFewReads(t *testing.T) {
	t.Parallel()

	const files = 14000

	made := writtenNotes(t, files)
	src := &countedReads{data: made}

	img, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	src.calls, src.bytes, src.largest = 0, 0, 0

	out := &testBuffer{}
	w := erofs.Create(out)

	if err = w.CopyFrom(img, erofs.MetadataOnly()); err != nil {
		t.Fatal(err)
	}

	// A read for every 64 KiB would be over 230.
	if src.calls > 32 {
		t.Errorf("the copy of a %d byte image took %d reads; want no more than 32", len(src.data), src.calls)
	}

	if src.largest != 4<<20 {
		t.Errorf("the copy's largest read was %d bytes; want 4 MiB", src.largest)
	}

	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	copied := openWithDevices(t, out.Bytes(), made)

	sameTree(t, openWithDevices(t, made), copied)

	for _, i := range []int{0, 1, files / 2, files - 1} {
		info, err := fs.Stat(copied, fmt.Sprintf("f%05d", i))
		if err != nil {
			t.Fatal(err)
		}

		st, ok := info.Sys().(*erofs.Stat)
		if !ok {
			t.Fatalf("Sys() is %T", info.Sys())
		}

		if note := st.Xattrs["user.note"]; len(note) != 1000 || note[:8] != fmt.Sprintf("%08d", i) {
			t.Errorf("f%05d: user.note is %d bytes, %.12q...", i, len(note), note)
		}
	}
}

// TestContractMetadataOnlyCopyReadsNothingTwice: the copy meets a tree's
// inodes level by level and the image holds them in another order, each
// directory's after it; and a file's every name is another visit to its
// inode. Whatever the order, the copy reads the image's metadata once,
// and no more than the image is long: 6000 names of empty files in 120
// directories of two levels, one in ten a second name of a file, in an
// image that is all metadata.
func TestContractMetadataOnlyCopyReadsNothingTwice(t *testing.T) {
	t.Parallel()

	made := writtenTree(t, 6000, 120)
	src := &countedReads{data: made}

	img, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	src.calls, src.bytes = 0, 0

	out := &testBuffer{}
	w := erofs.Create(out)

	if err = w.CopyFrom(img, erofs.MetadataOnly()); err != nil {
		t.Fatal(err)
	}

	if src.bytes > len(made) {
		t.Errorf("the copy read %d bytes of a %d byte image", src.bytes, len(made))
	}

	if err = w.Close(); err != nil {
		t.Fatal(err)
	}

	sameTree(t, openWithDevices(t, made), openWithDevices(t, out.Bytes(), made))
}

// TestContractMetadataOnlyCopyRefusesASourceShorterThanItDeclares: a source
// that reports no length is as long as its superblock says, and one that
// ends before that is refused, ErrCorrupt, where Open refuses the source
// that reports the shorter length: here a byte short, of an image whose
// tree names nothing near its end.
func TestContractMetadataOnlyCopyRefusesASourceShorterThanItDeclares(t *testing.T) {
	t.Parallel()

	whole := laidAsMkfs(64)

	for _, tc := range []struct {
		name string
		size int64
		want error
	}{
		{"a byte short", whole.size - 1, erofs.ErrCorrupt},
		{"whole", whole.size, nil},
	} {
		img, err := erofs.Open(readAtOnly{&laidImage{head: whole.head, size: tc.size}})
		if err != nil {
			t.Fatalf("%s: Open: %v", tc.name, err)
		}

		err = erofs.Create(&testBuffer{}).CopyFrom(img, erofs.MetadataOnly())
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: CopyFrom(MetadataOnly) from a source with no length: %v; want %v", tc.name, err, tc.want)
		}
	}
}

// errLastByte is what a failingEnd's read of its last byte fails with.
var errLastByte = errors.New("failingEnd: the last byte cannot be read")

// failingEnd is a source of no length whose last byte, read alone, fails.
type failingEnd struct{ img *laidImage }

func (f failingEnd) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 1 && off == f.img.size-1 {
		return 0, errLastByte
	}

	return f.img.ReadAt(p, off)
}

// TestContractMetadataOnlyCopyPassesOnTheSourcesError: a source that fails
// where the copy asks whether it is as long as it declares has failed, and
// its error is the copy's: the image is not what is wrong.
func TestContractMetadataOnlyCopyPassesOnTheSourcesError(t *testing.T) {
	t.Parallel()

	img, err := erofs.Open(failingEnd{laidAsMkfs(64)})
	if err != nil {
		t.Fatal(err)
	}

	err = erofs.Create(&testBuffer{}).CopyFrom(img, erofs.MetadataOnly())
	if !errors.Is(err, errLastByte) || errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf("CopyFrom(MetadataOnly) = %v; want the source's error and not ErrCorrupt", err)
	}
}

// longNotes is an image this package wrote of files files, each with three
// attributes of 40000 bytes, the first under names names.
func longNotes(t *testing.T, files, names int) []byte {
	t.Helper()

	made := &testBuffer{}
	mk := erofs.Create(made)

	for i := range files {
		name := fmt.Sprintf("f%04d", i)
		writeFile(t, mk, name, nil)

		for _, attr := range []string{"user.a", "user.b", "user.c"} {
			note := bytes.Repeat([]byte{attr[5]}, 40000)
			copy(note, name)

			if err := mk.Setxattr(name, attr, string(note)); err != nil {
				t.Fatal(err)
			}
		}
	}

	for i := 1; i < names; i++ {
		if err := mk.Link("f0000", fmt.Sprintf("n%04d", i)); err != nil {
			t.Fatal(err)
		}
	}

	if err := mk.Close(); err != nil {
		t.Fatal(err)
	}

	return made.Bytes()
}

// TestContractMetadataOnlyCopyReadsLongAttributesOnce: what follows an
// inode is read through the windows whatever its length, and read for the
// file, not for each of its names. Attributes of 120 KB a file are longer
// than a window: 40 files of them are read once, 4.8 MB, and one file of
// 300 names is read as one file is, where a read for each range was twice
// the image for the first and 36 MB for the second.
func TestContractMetadataOnlyCopyReadsLongAttributesOnce(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		files, names int
	}{
		{"40 files", 40, 1},
		{"a file of 300 names", 1, 300},
	} {
		made := longNotes(t, tc.files, tc.names)
		src := &countedReads{data: made}

		img, err := erofs.Open(src)
		if err != nil {
			t.Fatal(err)
		}

		src.calls, src.bytes = 0, 0

		out := &testBuffer{}
		w := erofs.Create(out)

		if err = w.CopyFrom(img, erofs.MetadataOnly()); err != nil {
			t.Fatal(err)
		}

		// The image once, and the root's entries as a directory's blocks.
		if src.bytes > len(made)+8<<10 {
			t.Errorf("%s: the copy read %d bytes of a %d byte image", tc.name, src.bytes, len(made))
		}

		if err = w.Close(); err != nil {
			t.Fatal(err)
		}

		copied := openWithDevices(t, out.Bytes(), made)

		sameTree(t, openWithDevices(t, made), copied)

		last := fmt.Sprintf("f%04d", tc.files-1)

		info, err := fs.Stat(copied, last)
		if err != nil {
			t.Fatal(err)
		}

		st, ok := info.Sys().(*erofs.Stat)
		if !ok {
			t.Fatalf("Sys() is %T", info.Sys())
		}

		for _, attr := range []string{"user.a", "user.b", "user.c"} {
			want := bytes.Repeat([]byte{attr[5]}, 40000)
			copy(want, last)

			if got := st.Xattrs[attr]; got != string(want) {
				t.Errorf("%s: %s of %s is %d bytes, %.12q...; want 40000 of %q after the name",
					tc.name, attr, last, len(got), got, attr[5])
			}
		}
	}
}

// TestContractMetadataOnlyCopyReadsAFilesAttributesForItsFirstName: a
// further name of a file already copied is another name and nothing else.
// Its attributes are not read again and made into strings again to be
// dropped: 300 names of a file with 120 KB of attributes cost what the
// file costs, not 36 MB.
//
//nolint:paralleltest // measures the process's TotalAlloc against a budget; a parallel test's allocations would count against it
func TestContractMetadataOnlyCopyReadsAFilesAttributesForItsFirstName(t *testing.T) {
	const budget = 2 << 20

	img, err := erofs.Open(bytes.NewReader(longNotes(t, 1, 300)))
	if err != nil {
		t.Fatal(err)
	}

	before := allocatedSoFar()

	err = erofs.Create(&testBuffer{}).CopyFrom(img, erofs.MetadataOnly())

	allocated := allocatedSoFar() - before

	if err != nil {
		t.Fatal(err)
	}

	if allocated > budget {
		t.Errorf("the copy of a file of 300 names allocated %d bytes, over %d", allocated, budget)
	}
}
