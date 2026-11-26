package erofs_test

import (
	"bytes"
	"io/fs"
	"testing"

	"github.com/forkcloser/go-erofs"
)

// TestContractCopyHoldsAHoledFilesRunsOnce: a copy with its data of a file
// that has holes holds the file's runs, sixteen bytes each, and nothing
// else for them: a file of 16384 chunks, data and a hole in turn, is held
// in its runs' 262144 bytes and under 16 KiB more, with a block map as with
// indexes: the list as its growth left it would be 34 KiB more. It held
// the runs of data a second time besides, as the ranges to read: 436056
// and 441360 bytes.
//
//nolint:paralleltest // measures the process's live heap; a parallel test's would count in it
func TestContractCopyHoldsAHoledFilesRunsOnce(t *testing.T) {
	const (
		chunks = 1 << 14
		budget = chunks*16 + 16<<10
	)

	// Data and a hole in turn, the data over the same sixteen blocks.
	starts := make([]int, 0, chunks)
	for i := range chunks {
		if i%2 == 0 {
			starts = append(starts, i/2%16)
		} else {
			starts = append(starts, -1)
		}
	}

	for _, format := range []uint32{0, laidIndexes} {
		img, want := laidChunks(format, 0, starts...)
		fsys := openWithDevices(t, img)

		out := &testBuffer{}

		before := liveHeap()
		w := erofs.Create(out)

		if err := w.CopyFrom(fsys); err != nil {
			t.Fatal(err)
		}

		if held := liveHeap() - before; held > budget {
			t.Errorf("format %#x: a copy of a file of %d runs, half of them holes, holds %d bytes; want under %d",
				format, chunks, held, budget)
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		// The data is read by those runs: the copy is the file.
		got, err := fs.ReadFile(openWithDevices(t, out.Bytes()), "f")
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf(
				"format %#x: the copy's file is %d bytes, %v; want the source's %d",
				format,
				len(got),
				err,
				len(want),
			)
		}
	}
}
