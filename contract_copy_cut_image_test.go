package erofs_test

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"testing"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
)

// lengthless is a source that does not say how long it is: Open cannot
// hold the image to its declared size, and finds it short where it reads.
type lengthless struct{ io.ReaderAt }

// TestContractCopyOfACutImageIsCorruptAsItsReaderSays: an image shorter
// than it declares is ErrCorrupt to a copy with its data wherever it is to
// the reader, whichever way the copy reads a file: through the file, or
// straight from where its data lies, where the device's end used to pass
// for a source that shrank (io.ErrUnexpectedEOF alone). Every committed
// image, cut at four lengths.
func TestContractCopyOfACutImageIsCorruptAsItsReaderSays(t *testing.T) {
	t.Parallel()

	cut := 0

	for _, im := range corpus.Committed(t) {
		if len(im.Extra()) > 0 {
			continue
		}

		for _, keep := range []int{len(im.Img) - 1, len(im.Img) - 4096, len(im.Img) * 3 / 4, len(im.Img) / 2} {
			if keep < 2048 {
				continue
			}

			img, err := erofs.Open(lengthless{bytes.NewReader(im.Img[:keep])})
			if err != nil {
				continue
			}

			// What the reader says of the tree: the first failure of a walk
			// that reads every regular file.
			var read error

			_ = fs.WalkDir(img, ".", func(name string, d fs.DirEntry, err error) error {
				if read != nil {
					return fs.SkipAll
				}

				if err != nil {
					read = err

					return fs.SkipAll
				}

				if d.Type().IsRegular() {
					_, read = fs.ReadFile(img, name)
				}

				return nil
			})

			if !errors.Is(read, erofs.ErrCorrupt) {
				continue
			}

			cut++

			w := erofs.Create(&sink{})
			if err := errors.Join(w.CopyFrom(img), w.Close()); !errors.Is(err, erofs.ErrCorrupt) {
				t.Errorf("%s cut to %d of %d bytes: the reader says %v; the copy says %v, want ErrCorrupt",
					im.Name, keep, len(im.Img), read, err)
			}
		}
	}

	if cut < 20 {
		t.Errorf("%d cut images were corrupt to the reader; the test wants twenty at least to say anything", cut)
	}
}
