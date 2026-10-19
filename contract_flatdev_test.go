package erofs_test

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/disk"
)

// TestContractMetadataOnlyCopyOfAFlatdevImage: an image opened without its
// extra devices is mounted flat, each slot a window of the image from its
// mapped address; copied metadata-only, the image and those windows are
// the copy's devices, and through them the copy reads every file as the
// flat source does: the same bytes, or refused where the source is.
func TestContractMetadataOnlyCopyOfAFlatdevImage(t *testing.T) {
	t.Parallel()

	for _, im := range corpus.Committed(t) {
		if im.Tree == nil || len(im.Extra()) == 0 {
			continue
		}

		t.Run(im.Name, func(t *testing.T) {
			t.Parallel()

			src, err := erofs.Open(bytes.NewReader(im.Img))
			if err != nil {
				t.Fatalf("Open without devices: %v", err)
			}

			type view struct {
				data []byte
				err  error
			}

			want := map[string]view{}

			if werr := fs.WalkDir(src, ".", func(name string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}

				if d.Type().IsRegular() {
					data, rerr := fs.ReadFile(src, name)
					want[name] = view{data, rerr}
				}

				return nil
			}); werr != nil {
				t.Fatal(werr)
			}

			out := &testBuffer{}
			w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

			if cerr := errors.Join(w.CopyFrom(src, erofs.MetadataOnly()), w.Close()); cerr != nil {
				t.Fatalf("metadata-only copy of the flat image: %v", cerr)
			}

			// The copy's devices: the image, then each slot's window of it,
			// from the mapped address for the slot's blocks, bounded by the
			// image's end.
			var sb disk.SuperBlock

			sb.Unmarshal(im.Img[disk.SuperBlockOffset:])

			bs := 1 << sb.BlkSizeBits
			devices := [][]byte{im.Img}

			for i := range int(sb.ExtraDevices) {
				var slot disk.DeviceSlot

				slot.Unmarshal(im.Img[int(sb.DevtSlotOff)*disk.SizeDeviceSlot+i*disk.SizeDeviceSlot:])

				start := min(int(slot.MappedBlkAddr)*bs, len(im.Img))
				end := min(start+int(slot.Blocks)*bs, len(im.Img))
				devices = append(devices, im.Img[start:end])
			}

			copied := corpus.Image{Name: im.Name + "-flat-copy", Img: out.Bytes(), Devices: devices}
			parseImage(t, copied)

			if ferr := corpus.FsckVerdict(t, copied); ferr != nil {
				t.Errorf("fsck.erofs on the copy over its devices: %v", ferr)
			}

			var opts []erofs.OpenOpt
			for _, d := range devices {
				opts = append(opts, erofs.WithExtraDevices(bytes.NewReader(d)))
			}

			cp, err := erofs.Open(bytes.NewReader(copied.Img), opts...)
			if err != nil {
				t.Fatalf("Open of the copy over its devices: %v", err)
			}

			for name, w := range want {
				got, err := fs.ReadFile(cp, name)

				switch {
				case w.err == nil && err != nil:
					t.Errorf("%s: the copy refuses (%v) what the flat source reads", name, err)
				case w.err == nil && !bytes.Equal(got, w.data):
					t.Errorf("%s: the copy reads %d bytes, the flat source %d", name, len(got), len(w.data))
				case w.err != nil && err == nil:
					t.Errorf("%s: the copy reads %d bytes where the flat source refuses (%v)", name, len(got), w.err)
				case w.err != nil && !errors.Is(err, erofs.ErrCorrupt):
					t.Errorf("%s: the copy refuses as %v, the source as %v; want ErrCorrupt", name, err, w.err)
				}
			}
		})
	}
}
