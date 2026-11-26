package erofs_test

import (
	"encoding/binary"
	"testing"

	"github.com/forkcloser/go-erofs"
)

// laidChunkedInRuns is laidChunkedFile's image with its map filled: the
// chunks of "f" go over the same sixteen blocks of the image again and
// again, so that they are runs of sixteen chunks that follow each other.
// The blocks are past the map, and read as zeros.
func laidChunkedInRuns(chunks int64, format uint32) *laidImage {
	const first = 200 // past the map of 65536 indexes, which ends in block 129

	src := laidChunkedFile(chunks, format)

	unit := int64(4)
	if format&laidIndexes != 0 {
		unit = 8
	}

	src.head = append(src.head, make([]byte, chunks*unit)...)

	for i := range chunks {
		binary.LittleEndian.PutUint32(src.head[laidMapOff+i*unit+unit-4:], uint32(first+i%16))
	}

	src.size = (first + 16) * 4096
	laidSuper(src.head, 12, first+16)

	return src
}

// TestContractCopyHoldsChunksThatFollowAsOneRun: a metadata-only copy holds
// a chunk-based file's blocks as runs, sixteen bytes for each run of chunks
// that follow each other on their device, whichever form the source's map
// has: a file of 65536 chunks in 4096 runs, 64 KiB of them, is held in
// under 72 KiB with a block map as with indexes. With a block map it was
// held a chunk at a time, in 1.2 MB, and with indexes in 83 KiB, the list
// keeping the room of its last growth.
//
//nolint:paralleltest // measures the process's live heap; a parallel test's would count in it
func TestContractCopyHoldsChunksThatFollowAsOneRun(t *testing.T) {
	const (
		chunks = 1 << 16
		budget = 72 << 10
	)

	for _, format := range []uint32{0, laidIndexes} {
		fsys, err := erofs.Open(laidChunkedInRuns(chunks, format))
		if err != nil {
			t.Fatal(err)
		}

		before := liveHeap()
		w := erofs.Create(&sink{})

		if err := w.CopyFrom(fsys, erofs.MetadataOnly()); err != nil {
			t.Fatal(err)
		}

		if held := liveHeap() - before; held > budget {
			t.Errorf("format %#x: a metadata-only copy of a file of %d chunks in %d runs holds %d bytes; want under %d",
				format, chunks, chunks/16, held, budget)
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
