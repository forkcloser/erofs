package erofs_test

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/forkcloser/go-erofs"
)

// TestContractChunkMapOverTheLimitIsNotCorrupt: the package documents a
// chunk map of at most 64 MiB per file as a limit, reported as ErrInvalid
// alone. A map over it that the image holds whole was reported as one lying
// past the end of the image, ErrCorrupt: a sound file called a bad image.
func TestContractChunkMapOverTheLimitIsNotCorrupt(t *testing.T) {
	t.Parallel()

	const atLimit = 64 << 20 / 4 // chunks whose block map is 64 MiB

	for _, tc := range []struct {
		name   string
		chunks int64
		format uint32
		over   bool
	}{
		{"a block map of 64 MiB", atLimit, 0, false},
		{"a block map four bytes over", atLimit + 1, 0, true},
		{"indexes of 64 MiB", atLimit / 2, laidIndexes, false},
		{"indexes eight bytes over", atLimit/2 + 1, laidIndexes, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fsys, err := erofs.Open(laidChunkedFile(tc.chunks, tc.format))
			if err != nil {
				t.Fatal(err)
			}

			_, err = fs.Stat(fsys, "f")

			switch {
			case !tc.over && err != nil:
				t.Errorf("Stat = %v; want the file, its map is at the limit", err)
			case tc.over && (!errors.Is(err, erofs.ErrInvalid) || errors.Is(err, erofs.ErrCorrupt)):
				t.Errorf("Stat = %v; want ErrInvalid alone", err)
			}
		})
	}
}

// TestContractChunkMapTheImageCannotHoldIsCorrupt: a map over the limit that
// also runs past the image's end stays ErrCorrupt: the limit does not turn
// a bad image into a sound file that is merely large.
func TestContractChunkMapTheImageCannotHoldIsCorrupt(t *testing.T) {
	t.Parallel()

	// A block map four bytes over 64 MiB, in an image of two blocks.
	src := laidChunkedFile(64<<20/4+1, 0)
	src.size = 2 * 4096
	laidSuper(src.head, 12, 2)

	fsys, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := fs.Stat(fsys, "f"); !errors.Is(err, erofs.ErrCorrupt) {
		t.Errorf("Stat = %v; want ErrCorrupt", err)
	}
}
