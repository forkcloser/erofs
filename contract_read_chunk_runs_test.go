package erofs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/forkcloser/erofs"
)

// laidChunkedImage is an image of 4 KiB blocks, laid by hand, whose one
// file "f" is chunk-based, format its chunk format: a chunk for each of the
// starts, at that block of the image's data, which begins after the file's
// map, or a hole for -1. The file ends cut bytes before its last chunk
// does. Byte j of the data's block k is byte(k*37 + j), so that no two
// blocks are alike and none is zeros. data is the block the data begins at.
func laidChunkedImage(format uint32, cut int, starts []int) (img []byte, data int) {
	const bs = 4096

	unit, chunkBlocks := 4, 1<<(format&0x1F)
	if format&laidIndexes != 0 {
		unit = 8
	}

	data = (laidMapOff + len(starts)*unit + bs - 1) / bs
	last := 0

	for _, start := range starts {
		last = max(last, start+chunkBlocks)
	}

	img = make([]byte, (data+last)*bs)
	laidSuper(img, 12, uint32(data+last))

	for k := range last {
		for j := range bs {
			img[(data+k)*bs+j] = byte(k*37 + j)
		}
	}

	root := laidDirBlock([]laidDirent{{0, laidFtDir, "."}, {0, laidFtDir, ".."}, {3, laidFtReg, "f"}})
	laidCompact(img[bs:], laidFlatInline, laidDir, 2, uint32(len(root)), 0)
	copy(img[bs+32:], root)

	laidExtended(img[bs+96:], laidChunked, laidReg, uint64(len(starts)*chunkBlocks*bs-cut), format, 1)

	for i, start := range starts {
		block := ^uint32(0)
		if start >= 0 {
			block = uint32(data + start)
		}

		binary.LittleEndian.PutUint32(img[laidMapOff+i*unit+unit-4:], block)
	}

	return img, data
}

// laidChunks is laidChunkedImage's image, with what its file reads as.
func laidChunks(format uint32, cut int, starts ...int) (img, content []byte) {
	const bs = 4096

	img, data := laidChunkedImage(format, cut, starts)
	chunk := bs << (format & 0x1F)
	content = make([]byte, 0, len(starts)*chunk)

	for _, start := range starts {
		if start < 0 {
			content = append(content, make([]byte, chunk)...)

			continue
		}

		content = append(content, img[(data+start)*bs:(data+start)*bs+chunk]...)
	}

	return img, content[:len(content)-cut]
}

// repeated is chunks starts that go over the same sixteen blocks of data
// again and again: runs of sixteen chunks, in an image that stays small.
func repeated(chunks int) []int {
	starts := make([]int, 0, chunks)

	for i := range chunks {
		starts = append(starts, i%16)
	}

	return starts
}

// inRuns is chunks starts in runs of each: the chunks of a run follow each
// other in the data, and a run begins a block after where the one before
// it ended.
func inRuns(chunks, each int) []int {
	starts := make([]int, 0, chunks)

	for i := range chunks {
		starts = append(starts, i+i/each)
	}

	return starts
}

// TestContractReaderReadsAChunkedFileInRuns: a chunk-based file is read a
// run of chunks at a time, the chunks that follow each other on their
// device in one read of it, and its map a block at a time: read whole, a
// file of 3000 chunks in three runs costs the reads of one of 30 chunks in
// three runs and two for each further block of its map, the block and the
// run it cuts, where it cost two reads for each chunk. Copied out through
// a buffer, a read a buffer and the same few more. With a block map and
// with indexes.
func TestContractReaderReadsAChunkedFileInRuns(t *testing.T) {
	t.Parallel()

	for _, format := range []uint32{0, laidIndexes} {
		reads := map[int]int{}

		unit := 4
		if format&laidIndexes != 0 {
			unit = 8
		}

		for _, chunks := range []int{30, 3000} {
			img, want := laidChunks(format, 100, inRuns(chunks, chunks/3)...)
			src := &countedReads{data: img}

			fsys, err := erofs.Open(src)
			if err != nil {
				t.Fatal(err)
			}

			src.calls = 0

			got, err := fs.ReadFile(fsys, "f")
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf(
					"format %#x, %d chunks: ReadFile gives %d bytes, %v; want the file's %d",
					format,
					chunks,
					len(got),
					err,
					len(want),
				)
			}

			reads[chunks] = src.calls

			f, err := fsys.Open("f")
			if err != nil {
				t.Fatal(err)
			}

			src.calls = 0

			var out slowSink

			if _, err := io.Copy(&out, f); err != nil || !bytes.Equal(out.got, want) {
				t.Fatalf(
					"format %#x, %d chunks: io.Copy gives %d bytes, %v; want the file's %d",
					format,
					chunks,
					len(out.got),
					err,
					len(want),
				)
			}

			// io.Copy's buffer is 32 KiB. On top of a read a buffer: the
			// inode, a read more for each run a buffer ends in, and the
			// map's blocks.
			if most := len(want)/(32<<10) + 1 + 4 + chunks*unit/4096 + 2; src.calls > most {
				t.Errorf(
					"format %#x, %d chunks: io.Copy read the image %d times; want %d at most",
					format,
					chunks,
					src.calls,
					most,
				)
			}
		}

		// The map of 3000 chunks is three blocks of block addresses, six of
		// indexes; that of 30 is one.
		if most := reads[30] + 2*(3000*unit/4096); reads[3000] > most {
			t.Errorf(
				"format %#x: ReadFile read the image %d times for 3000 chunks in three runs and %d for 30; want %d at most",
				format,
				reads[3000],
				reads[30],
				most,
			)
		}
	}
}

// TestContractReaderChunkedFileAtEveryStep: a chunk-based file read in
// steps of any size is its bytes, across the ends of its runs and of its
// map's windows, through holes, with chunks of one block and of two, with a
// block map and with indexes, and to an end that is not a block's.
func TestContractReaderChunkedFileAtEveryStep(t *testing.T) {
	t.Parallel()

	shapes := map[string][]int{
		"one run":                    {0, 1, 2, 3, 4, 5},
		"three runs":                 {0, 1, 4, 5, 8, 9},
		"runs out of order":          {6, 7, 0, 1, 3, 4},
		"holes between and at ends":  {-1, 0, 1, -1, -1, 4, -1},
		"nothing but holes":          {-1, -1, -1},
		"a block the file has twice": {2, 2, 3, 2},
	}
	// A map of over a block, so that a run crosses the window's end.
	shapes["a map of two windows"] = inRuns(1100, 400)

	for name, starts := range shapes {
		for _, format := range []uint32{0, 1, laidIndexes, 1 | laidIndexes} {
			if format&1 != 0 && len(starts) > 100 {
				continue // two-block chunks: the starts would overlap
			}

			laid := starts
			if format&1 != 0 {
				laid = make([]int, len(starts))
				for i, s := range starts {
					laid[i] = max(2*s, -1)
				}
			}

			img, want := laidChunks(format, 1000, laid...)

			for _, step := range []int{1, 7, 1000, 4096, 5000, 1 << 16} {
				if step < 1000 && len(want) > 1<<20 {
					continue
				}

				t.Run(fmt.Sprintf("%s, format %#x, step %d", name, format, step), func(t *testing.T) {
					t.Parallel()

					f, err := openWithDevices(t, img).Open("f")
					if err != nil {
						t.Fatal(err)
					}

					var got []byte

					buf := make([]byte, step)

					for {
						n, err := f.Read(buf)
						got = append(got, buf[:n]...)

						if errors.Is(err, io.EOF) {
							break
						}

						if err != nil {
							t.Fatalf("Read at %d: %v", len(got), err)
						}

						if n != step && len(got) != len(want) {
							t.Fatalf(
								"Read at %d gives %d bytes of %d asked, before the file's end",
								len(got)-n,
								n,
								step,
							)
						}
					}

					if !bytes.Equal(got, want) {
						t.Fatalf("read %d bytes that are not the file's %d", len(got), len(want))
					}
				})
			}
		}
	}
}

// TestContractReaderChunkedFileOnAnImageCutShort: an image that ends inside
// a run of a chunk-based file, behind a reader that cannot say its length,
// gives what it holds of the file and then ErrCorrupt, by io.Copy, as it
// does of a flat file, and never the file's end; ReadFile, which gives a
// file whole or not at all, gives ErrCorrupt.
func TestContractReaderChunkedFileOnAnImageCutShort(t *testing.T) {
	t.Parallel()

	for _, format := range []uint32{0, laidIndexes} {
		img, want := laidChunks(format, 0, inRuns(40, 20)...)

		// Ten blocks and 2000 bytes of the first run are there.
		const held = 10*4096 + 2000

		cut := len(img) - len(want) - 4096 + held

		fsys, err := erofs.Open(readAtOnly{bytes.NewReader(img[:cut])})
		if err != nil {
			t.Fatal(err)
		}

		f, err := fsys.Open("f")
		if err != nil {
			t.Fatal(err)
		}

		var out slowSink

		_, err = io.Copy(&out, f)
		if !errors.Is(err, erofs.ErrCorrupt) {
			t.Errorf("format %#x: io.Copy: %d bytes, %v; want ErrCorrupt", format, len(out.got), err)
		}

		if !bytes.Equal(out.got, want[:held]) {
			t.Errorf(
				"format %#x: io.Copy gave %d bytes before the error; want the %d the image holds of the file",
				format,
				len(out.got),
				held,
			)
		}

		if got, err := fs.ReadFile(fsys, "f"); !errors.Is(err, erofs.ErrCorrupt) {
			t.Errorf("format %#x: ReadFile: %d bytes, %v; want ErrCorrupt", format, len(got), err)
		}
	}
}

// severalReaders is a destination that reads what it is given to copy from
// several goroutines at once, when it is given a source it may read so: an
// io.ReaderAt that knows its size. It says whether it was.
type severalReaders struct {
	got      []byte
	parallel bool
}

func (s *severalReaders) Write(p []byte) (int, error) {
	s.got = append(s.got, p...)

	return len(p), nil
}

func (s *severalReaders) ReadFrom(r io.Reader) (int64, error) {
	src, ok := r.(interface {
		io.ReaderAt
		Size() int64
	})
	if !ok {
		got, err := io.ReadAll(r)
		s.got = append(s.got, got...)

		return int64(len(got)), err
	}

	const piece = 37 << 10

	s.parallel = true
	s.got = make([]byte, src.Size())

	var (
		wg    sync.WaitGroup
		errs  = make(chan error, 8)
		next  atomic.Int64
		total = src.Size()
	)

	for range 8 {
		wg.Go(func() {
			for {
				from := next.Add(piece) - piece
				if from >= total {
					return
				}

				to := min(from+piece, total)
				if _, err := src.ReadAt(s.got[from:to], from); err != nil && !errors.Is(err, io.EOF) {
					errs <- err

					return
				}
			}
		})
	}

	wg.Wait()
	close(errs)

	return total, <-errs
}

// TestContractReaderChunkedFileReadFromSeveralGoroutines: io.Copy hands the
// destination a source it may read at any offset, from several goroutines
// at once, as the type it is handed allows. A chunk-based file read so is
// its bytes, with a block map and with indexes.
func TestContractReaderChunkedFileReadFromSeveralGoroutines(t *testing.T) {
	t.Parallel()

	for _, format := range []uint32{0, laidIndexes} {
		img, want := laidChunks(format, 100, inRuns(3000, 100)...)

		f, err := openWithDevices(t, img).Open("f")
		if err != nil {
			t.Fatal(err)
		}

		var out severalReaders

		if _, err := io.Copy(&out, f); err != nil || !bytes.Equal(out.got, want) {
			t.Fatalf("format %#x: io.Copy gives %d bytes, %v; want the file's %d", format, len(out.got), err, len(want))
		}

		if !out.parallel {
			t.Errorf(
				"format %#x: the destination was not handed a source to read at any offset; the case was not met",
				format,
			)
		}
	}
}

// TestContractAChunkPastItsSlotOnADeviceOfNoLength: a device given without
// a length is held to the blocks its slot declares, the only extent the
// image states for it. A chunk that begins on the last of them and runs
// past it is refused by Stat, and every way of reading the file ends in
// ErrCorrupt after the blocks the slot holds, though the device has the
// bytes of the next: with a block map and with indexes, the chunk first in
// its run or after another.
func TestContractAChunkPastItsSlotOnADeviceOfNoLength(t *testing.T) {
	t.Parallel()

	for _, format := range []uint32{1, 1 | laidIndexes} {
		for _, tc := range []struct {
			starts []uint32
			want   string
		}{
			{[]uint32{3, 6}, "D0 D1 D3"},
			{[]uint32{4, 6}, "D1 D2 D3"}, // the chunk past the slot follows the one before
			{[]uint32{6}, "D3"},
		} {
			img, device := laidOverMappedRange(6, format, tc.starts...)

			fsys, err := erofs.Open(bytes.NewReader(img), erofs.WithExtraDevices(readAtOnly{bytes.NewReader(device)}))
			if err != nil {
				t.Fatal(err)
			}

			if _, err := fs.Stat(fsys, "f"); !errors.Is(err, erofs.ErrCorrupt) {
				t.Errorf("format %#x, chunks at %v: Stat: %v; want ErrCorrupt", format, tc.starts, err)
			}

			for _, r := range readEveryWay(t, fsys) {
				if !errors.Is(r.err, erofs.ErrCorrupt) {
					t.Errorf("format %#x, chunks at %v: %s: the blocks %s, %v; want ErrCorrupt",
						format, tc.starts, r.way, blocksOf(r.got), r.err)
				}

				// ReadFile gives nothing of a file it does not give whole.
				if r.way != "ReadFile" && blocksOf(r.got) != tc.want {
					t.Errorf("format %#x, chunks at %v: %s: the blocks %s before the error; want %s",
						format, tc.starts, r.way, blocksOf(r.got), tc.want)
				}
			}
		}
	}
}

// TestContractAChunkPastADeviceCutInsideABlock: a chunk that runs past a
// device given with its length, which ends inside one of the chunk's
// blocks, is read up to that block and no further, by every way of reading
// and whatever the step: the block the device does not hold whole is not
// the file's to a read of a hundred bytes that would fit in what is there.
func TestContractAChunkPastADeviceCutInsideABlock(t *testing.T) {
	t.Parallel()

	for _, format := range []uint32{1, 1 | laidIndexes} {
		// The device: its four blocks and 2048 bytes of a fifth.
		img, device := laidOverMappedRange(5, format, 3, 6)
		fsys := openWithDevices(t, img, device[:len(device)-2048])

		for _, r := range readEveryWay(t, fsys) {
			if !errors.Is(r.err, erofs.ErrCorrupt) {
				t.Errorf("format %#x: %s: the blocks %s, %v; want ErrCorrupt", format, r.way, blocksOf(r.got), r.err)
			}

			// ReadFile gives nothing of a file it does not give whole.
			if want := 3 * 4096; r.way != "ReadFile" && (len(r.got) != want || blocksOf(r.got) != "D0 D1 D3") {
				t.Errorf("format %#x: %s: %d bytes before the error, the blocks %s; want %d, D0 D1 D3",
					format, r.way, len(r.got), blocksOf(r.got), want)
			}
		}
	}
}

// TestContractReaderKeepsNoBlockOfAChunkedFileItIsDoneWith: the block of
// its map that a chunk-based file holds while it is read goes back to the
// image when the file is read to its end, and when it is closed before
// that: a thousand files read one after the other, to the end and never
// closed, then to their middle and closed, cost under 2 KiB each in all,
// where a block kept by each would be four more.
//
//nolint:paralleltest // measures the process's allocations against a budget; a parallel test's would count against it
func TestContractReaderKeepsNoBlockOfAChunkedFileItIsDoneWith(t *testing.T) {
	const (
		files  = 1000
		budget = files * 2 << 10
	)

	if raceDetector {
		t.Skip("under the race detector a pool drops a share of the blocks it is given back")
	}

	img, _ := laidChunkedImage(0, 0, repeated(64))

	fsys, err := erofs.Open(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}

	for _, whole := range []bool{true, false} {
		before := allocatedSoFar()

		for range files {
			f, err := fsys.Open("f")
			if err != nil {
				t.Fatal(err)
			}

			if whole {
				_, err = io.Copy(io.Discard, f)
			} else {
				_, err = io.CopyN(io.Discard, f, 32*4096)
				err = errors.Join(err, f.Close())
			}

			if err != nil {
				t.Fatal(err)
			}
		}

		if allocated := allocatedSoFar() - before; allocated > budget {
			t.Errorf("read to the end %v: %d files allocated %d bytes, %d each; want under %d each",
				whole, files, allocated, allocated/files, budget/files)
		}
	}
}

// TestContractReaderAllocatesNothingForAChunk: reading a chunk-based file
// allocates for the file and not for its chunks: a file of 65536 chunks is
// copied out for under 64 KiB allocated in all. It allocated eight bytes
// for each block it read, half a mebibyte here.
//
//nolint:paralleltest // measures the process's allocations against a budget; a parallel test's would count against it
func TestContractReaderAllocatesNothingForAChunk(t *testing.T) {
	const (
		chunks = 1 << 16
		budget = 64 << 10
	)

	for _, format := range []uint32{0, laidIndexes} {
		img, _ := laidChunkedImage(format, 0, repeated(chunks))

		fsys, err := erofs.Open(bytes.NewReader(img))
		if err != nil {
			t.Fatal(err)
		}

		f, err := fsys.Open("f")
		if err != nil {
			t.Fatal(err)
		}

		before := allocatedSoFar()

		n, err := io.Copy(io.Discard, f)
		if err != nil || n != chunks*4096 {
			t.Fatalf("format %#x: io.Copy: %d bytes, %v", format, n, err)
		}

		if allocated := allocatedSoFar() - before; allocated > budget {
			t.Errorf(
				"format %#x: copying out a file of %d chunks allocated %d bytes; want under %d",
				format,
				chunks,
				allocated,
				budget,
			)
		}
	}
}

// holedFiles is an image of that many files, each a block of data and a
// hole, opened over its devices: made by the Writer from sources that give
// ranges, a device each, every one the same block.
func holedFiles(t *testing.T, files int) fs.FS {
	t.Helper()

	const bs = 4096

	blob := bytes.Repeat([]byte{'d'}, bs)
	laid := &testBuffer{}
	lw := erofs.Create(laid)

	for i := range files {
		src := &dataRangerFS{deviceBlocks: 1, file: &dataRangerFileInfo{
			name: fmt.Sprintf("f%04d", i), size: 2 * bs,
			ranges: []erofs.DataRange{{Offset: 0, Size: bs}, {Offset: -1, Size: bs}},
		}}

		if err := lw.CopyFrom(src, erofs.MetadataOnly(), erofs.Merge()); err != nil {
			t.Fatal(err)
		}
	}

	if err := lw.Close(); err != nil {
		t.Fatal(err)
	}

	devices := make([]io.ReaderAt, files)
	for i := range devices {
		devices[i] = bytes.NewReader(blob)
	}

	fsys, err := erofs.Open(bytes.NewReader(laid.Bytes()), erofs.WithExtraDevices(devices...))
	if err != nil {
		t.Fatal(err)
	}

	return fsys
}

// TestContractCopyKeepsNoBlockOfAHoledFileItHasRead: a copy with its data
// of files that have holes reads each through its chunk map and gives the
// map's block back when the file's data is read, though a file that ends
// in a hole is not read to its end. Two thousand files of a block of data
// and a hole: without a data file, where Close reads them, it allocates
// under a mebibyte and 640 bytes a file; into a data file, where CopyFrom
// reads them, CopyFrom allocates under 3 KiB a file. A block kept for each
// would be 4 KiB more a file in either.
//
//nolint:paralleltest // measures the process's allocations against a budget; a parallel test's would count against it
func TestContractCopyKeepsNoBlockOfAHoledFileItHasRead(t *testing.T) {
	const files = 2000

	if raceDetector {
		t.Skip("under the race detector a pool drops a share of the blocks it is given back")
	}

	fsys := holedFiles(t, files)

	w := erofs.Create(&sink{})
	if err := w.CopyFrom(fsys); err != nil {
		t.Fatal(err)
	}

	before := allocatedSoFar()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if allocated, most := allocatedSoFar()-before, uint64(1<<20+(files+1)*640); allocated > most {
		t.Errorf(
			"Close allocated %d bytes to copy %d files of a block and a hole; want %d at most",
			allocated,
			files,
			most,
		)
	}

	dataFile, err := os.CreateTemp(t.TempDir(), "data")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dataFile.Close() })

	w = erofs.Create(&sink{}, erofs.WithDataFile(dataFile))

	before = allocatedSoFar()

	if err := w.CopyFrom(fsys); err != nil {
		t.Fatal(err)
	}

	if allocated, most := allocatedSoFar()-before, uint64(files*3<<10); allocated > most {
		t.Errorf(
			"CopyFrom allocated %d bytes to copy %d files of a block and a hole into a data file; want %d at most",
			allocated,
			files,
			most,
		)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
