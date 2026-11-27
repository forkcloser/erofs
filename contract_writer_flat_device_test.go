// Copyright 2026 Forkcloser
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package erofs_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
	"github.com/forkcloser/go-erofs/internal/disk"
)

// TestContractWriterImageReadsAsOneFlatDevice: an image the Writer gives
// devices, the source image and its own devices of a metadata-only copy or
// the data file of WithDataFile, maps each after the image and the devices
// before it, as mkfs.erofs lays them (erofs_write_device_table). The image
// and its devices one after the other are then one device: mounted with no
// device given, the kernel reads an address on a device at the slot's
// mapped address plus it (erofs_map_dev under flatdev), and every file
// reads the bytes it reads with the devices given. A slot with no mapped
// address has the kernel read the image's own block at that address
// instead, and say nothing.
func TestContractWriterImageReadsAsOneFlatDevice(t *testing.T) {
	t.Parallel()

	for _, im := range corpus.Committed(t) {
		if im.Tree == nil {
			continue
		}

		t.Run("a copy of "+im.Name, func(t *testing.T) {
			t.Parallel()

			src := openImage(t, im)
			if src == nil {
				t.Skip("a 48-bit image, which the package does not open")
			}

			out := &testBuffer{}
			w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

			if err := errors.Join(w.CopyFrom(src, erofs.MetadataOnly()), w.Close()); err != nil {
				t.Fatal(err)
			}

			readsAsOneDevice(t, out.Bytes(), append([][]byte{im.Img}, im.Extra()...))
		})
	}

	t.Run("a data file", func(t *testing.T) {
		t.Parallel()

		data, err := os.Create(filepath.Join(t.TempDir(), "data.bin"))
		if err != nil {
			t.Fatal(err)
		}

		defer func() { _ = data.Close() }()

		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithDataFile(data), erofs.WithBuildTime(corpus.BuildTime, 0))
		mustCreate(t, w, "big", corpus.Pattern(200_000))
		mustCreate(t, w, "small", []byte("s"))

		if err = w.Close(); err != nil {
			t.Fatal(err)
		}

		device, err := os.ReadFile(data.Name())
		if err != nil {
			t.Fatal(err)
		}

		readsAsOneDevice(t, out.Bytes(), [][]byte{device})
	})
}

// readsAsOneDevice holds an image to its devices' mapped addresses, and
// reads it with its devices given and as one device with them after it.
func readsAsOneDevice(t *testing.T, img []byte, devices [][]byte) {
	t.Helper()

	var sb disk.SuperBlock

	sb.Unmarshal(img[disk.SuperBlockOffset:])

	if int(sb.ExtraDevices) != len(devices) {
		t.Fatalf("the image has %d devices, want %d", sb.ExtraDevices, len(devices))
	}

	bs := 1 << sb.BlkSizeBits
	flat := bytes.Clone(img)
	mapped := sb.Blocks

	for i, device := range devices {
		var slot disk.DeviceSlot

		slot.Unmarshal(img[int(sb.DevtSlotOff)*disk.SizeDeviceSlot+i*disk.SizeDeviceSlot:])

		if slot.MappedBlkAddr != mapped {
			t.Errorf("device %d is mapped at block %d, want %d: after the image and the devices before it",
				i+1, slot.MappedBlkAddr, mapped)
		}

		mapped += slot.Blocks

		// A device as long as its slot says, whatever its file's last
		// block holds.
		whole := make([]byte, int(slot.Blocks)*bs)
		copy(whole, device)
		flat = append(flat, whole...)
	}

	var opts []erofs.OpenOpt
	for _, d := range devices {
		opts = append(opts, erofs.WithExtraDevices(bytes.NewReader(d)))
	}

	given, err := erofs.Open(bytes.NewReader(img), opts...)
	if err != nil {
		t.Fatalf("Open with the devices given: %v", err)
	}

	one, err := erofs.Open(bytes.NewReader(flat))
	if err != nil {
		t.Fatalf("Open of the image and its devices as one: %v", err)
	}

	files := 0

	if err := fs.WalkDir(given, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}

		want, err := fs.ReadFile(given, name)
		if err != nil {
			return err
		}

		files++

		if got, err := fs.ReadFile(one, name); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: as one device it reads %d bytes, %v; want the %d it reads with its devices given",
				name, len(got), err, len(want))
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if files == 0 {
		t.Fatal("the image holds no regular file: nothing was read")
	}
}
