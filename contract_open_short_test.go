package erofs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
	"github.com/forkcloser/go-erofs/internal/disk"
)

var errReaderBroke = errors.New("the reader broke")

// brokenReader fails every read with an error of its own.
type brokenReader struct{}

func (brokenReader) ReadAt([]byte, int64) (int, error) { return 0, errReaderBroke }

// TestContractOpenOfAnImageTooShortIsCorrupt: an image that ends before or
// inside its superblock is ErrInvalidSuperblock, and so ErrCorrupt. Open
// used to hand up the reader's bare io.EOF, the one error a caller takes
// for a clean end; a reader's own failure still passes through as it is.
func TestContractOpenOfAnImageTooShortIsCorrupt(t *testing.T) {
	t.Parallel()

	// The superblock is the 128 bytes from byte 1024.
	for _, n := range []int{0, 100, 1024, 1100, 1151} {
		t.Run(fmt.Sprintf("%d bytes", n), func(t *testing.T) {
			t.Parallel()

			_, err := erofs.Open(bytes.NewReader(make([]byte, n)))
			if !errors.Is(err, erofs.ErrInvalidSuperblock) || !errors.Is(err, erofs.ErrCorrupt) ||
				errors.Is(err, io.EOF) {
				t.Errorf("Open = %v; want ErrInvalidSuperblock, and no io.EOF", err)
			}
		})
	}

	t.Run("a reader that fails", func(t *testing.T) {
		t.Parallel()

		_, err := erofs.Open(brokenReader{})
		if !errors.Is(err, errReaderBroke) || errors.Is(err, erofs.ErrCorrupt) {
			t.Errorf("Open = %v; want the reader's error, and not ErrCorrupt", err)
		}
	})
}

// TestContractDeviceTablePastTheImageIsCorrupt: a device table the
// superblock places past the image's end is ErrCorrupt, not the io.EOF the
// reader answered the slot's read with.
func TestContractDeviceTablePastTheImageIsCorrupt(t *testing.T) {
	t.Parallel()

	var im corpus.Image

	for _, c := range corpus.Committed(t) {
		if c.Name == "small-blobdev-512" {
			im = c
		}
	}

	if im.Img == nil {
		t.Fatal("the committed small-blobdev-512 image is missing")
	}

	edited := bytes.Clone(im.Img)
	// No checksum to redo, and devt_slotoff at its highest: slot 0xFFFF,
	// byte 8388480, in an image far shorter.
	edited[disk.SuperBlockOffset+8] &^= disk.FeatureCompatSBChksum
	binary.LittleEndian.PutUint16(edited[disk.SuperBlockOffset+88:], 0xFFFF)

	if len(edited) >= 0xFFFF*disk.SizeDeviceSlot {
		t.Fatalf("the image is %d bytes: the table is inside it and the test tests nothing", len(edited))
	}

	_, err := erofs.Open(bytes.NewReader(edited), erofs.WithExtraDevices(bytes.NewReader(im.Blob)))
	if !errors.Is(err, erofs.ErrCorrupt) || errors.Is(err, io.EOF) {
		t.Errorf("Open = %v; want ErrCorrupt, and no io.EOF", err)
	}
}
