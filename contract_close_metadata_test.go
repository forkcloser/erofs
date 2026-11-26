package erofs_test

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/forkcloser/go-erofs"
)

// errSinkWhence is a Seek of a sink from somewhere it does not know.
var errSinkWhence = errors.New("sink: invalid whence")

// sink is an output that keeps nothing of what it is written, so that what
// a Writer allocates is the Writer's: it knows how long the image is, and
// the largest write it was handed.
type sink struct {
	pos, size int64
	largest   int
}

func (s *sink) Write(p []byte) (int, error) {
	s.pos += int64(len(p))
	s.size = max(s.size, s.pos)
	s.largest = max(s.largest, len(p))

	return len(p), nil
}

func (s *sink) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		s.pos = offset
	case io.SeekCurrent:
		s.pos += offset
	case io.SeekEnd:
		s.pos = s.size + offset
	default:
		return 0, errSinkWhence
	}

	return s.pos, nil
}

// TestContractCloseHoldsNoMoreOfTheMetadataThanABuffer: Close writes the
// image's metadata as it lays it out, and holds none of it but a buffer.
// Two thousand files with four kilobytes of attribute each are an image of
// over eight megabytes, all of it metadata: Close allocates under half of
// that in all, and hands its output no write of over a mebibyte.
//
//nolint:paralleltest // measures the process's allocations against a budget; a parallel test's would count against it
func TestContractCloseHoldsNoMoreOfTheMetadataThanABuffer(t *testing.T) {
	const (
		files = 2000
		attr  = 4000
	)

	out := &sink{}
	w := erofs.Create(out)

	for i := range files {
		name := fmt.Sprintf("/f%05d", i)
		writeFile(t, w, name, nil)

		if err := w.Setxattr(name, "user.own", strings.Repeat(fmt.Sprintf("%07d ", i), attr/8)); err != nil {
			t.Fatal(err)
		}
	}

	before := allocatedSoFar()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	allocated := allocatedSoFar() - before

	if out.size < files*attr {
		t.Fatalf("the image is %d bytes; the test wants the attributes in it, %d at least", out.size, files*attr)
	}

	if allocated > uint64(out.size)/2 {
		t.Errorf("Close allocated %d bytes to write an image of %d; want under half", allocated, out.size)
	}

	if out.largest > 1<<20 {
		t.Errorf("Close handed its output a write of %d bytes; want none over a mebibyte", out.largest)
	}
}

// TestContractCloseReturnsItsOutputsErrorWhereverItFails: an output that
// refuses a write fails Close with its error, wherever in the image the
// write falls: the first block, a file's data, the metadata, its last
// bytes.
func TestContractCloseReturnsItsOutputsErrorWhereverItFails(t *testing.T) {
	t.Parallel()

	build := func(out io.WriteSeeker) error {
		w := erofs.Create(out)

		for i := range 3 {
			name := fmt.Sprintf("/f%d", i)
			// A block of data and an inline end.
			writeFile(t, w, name, []byte(strings.Repeat(name, 2500)))

			if err := w.Setxattr(name, "user.k", strings.Repeat("v", 600)); err != nil {
				t.Fatal(err)
			}
		}

		return w.Close()
	}

	whole := &testBuffer{}
	if err := build(whole); err != nil {
		t.Fatal(err)
	}

	size := len(whole.Bytes())
	if size < 5*4096 {
		t.Fatalf("the image is %d bytes; the test wants blocks of data and of metadata, five at least", size)
	}

	for limit := 1; limit < size; limit += 251 {
		if err := build(&testBuffer{limit: limit}); !errors.Is(err, errTestBufferFull) {
			t.Errorf(
				"Close into an output of %d bytes, for an image of %d: %v; want the output's error",
				limit,
				size,
				err,
			)
		}
	}
}
