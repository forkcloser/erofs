package erofs_test

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/disk"
)

// declaringSource is a source that declares a block size and a device, as an
// image does.
type declaringSource struct{ fs.FS }

func (declaringSource) BlockSize() uint32    { return 512 }
func (declaringSource) DeviceBlocks() uint64 { return 64 }

// TestContractCopyRefusesARootThatIsNotADirectory: the source's root becomes
// the image's root, and the kernel mounts no image whose root inode is not a
// directory (super.c, "rootino(nid %llu) is not a directory"). Such a source
// is refused, ErrNotDirectory, and leaves the Writer's root as it was: the
// image it then writes has a directory for a root, with what it held before.
func TestContractCopyRefusesARootThatIsNotADirectory(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		root *fstest.MapFile
	}{
		{"a regular file", &fstest.MapFile{Data: []byte("not a directory"), Mode: 0o644}},
		{"a FIFO", &fstest.MapFile{Mode: fs.ModeNamedPipe | 0o644}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out := &testBuffer{}
			w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
			writeFile(t, w, "/kept", []byte("kept"))

			if err := w.CopyFrom(fstest.MapFS{".": tc.root}); !errors.Is(err, erofs.ErrNotDirectory) {
				t.Errorf("CopyFrom = %v; want ErrNotDirectory", err)
			}

			if fi, err := w.Stat("/"); err != nil || !fi.IsDir() {
				t.Errorf("the Writer's root after the refusal: %v, %v; want a directory", fi, err)
			}

			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			fsys := openWithDevices(t, out.Bytes())

			if fi, err := fs.Stat(fsys, "."); err != nil || !fi.IsDir() {
				t.Errorf("the image's root: %v, %v; want a directory", fi, err)
			}

			if got, err := fs.ReadFile(fsys, "kept"); err != nil || string(got) != "kept" {
				t.Errorf("kept reads %q, %v", got, err)
			}
		})
	}
}

// TestContractARefusedRootLeavesTheWritersGeometry: the root is asked before
// the source's block size and device are taken, so a source refused for its
// root leaves neither on the Writer.
func TestContractARefusedRootLeavesTheWritersGeometry(t *testing.T) {
	t.Parallel()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))

	src := declaringSource{fstest.MapFS{".": {Data: []byte("not a directory"), Mode: 0o644}}}
	if err := w.CopyFrom(src, erofs.MetadataOnly()); !errors.Is(err, erofs.ErrNotDirectory) {
		t.Fatalf("CopyFrom = %v; want ErrNotDirectory", err)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var sb disk.SuperBlock

	sb.Unmarshal(out.Bytes()[disk.SuperBlockOffset:])

	if sb.BlkSizeBits != 12 || sb.ExtraDevices != 0 {
		t.Errorf("the image has %d-byte blocks and %d extra devices; want 4096 and 0: the refused source's",
			1<<sb.BlkSizeBits, sb.ExtraDevices)
	}
}
