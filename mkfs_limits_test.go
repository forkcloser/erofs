package erofs_test

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/builder"
	"github.com/forkcloser/erofs/internal/disk"
)

// --- Finding 6: entries under a non-directory ---

// TestParentMustBeADirectory covers attaching an entry beneath a regular
// file. planLayout only descends into directories, so the entry used to be
// accepted, stay visible through the Writer, and then be missing from the
// image with no error anywhere.
func TestParentMustBeADirectory(t *testing.T) {
	t.Parallel()

	newWriterWithFile := func(t *testing.T) *erofs.Writer {
		t.Helper()

		w := erofs.Create(&testBuffer{}, erofs.WithBuildTime(1000, 0))
		writeFile(t, w, "/a", []byte("x"))

		return w
	}

	t.Run("Create", func(t *testing.T) {
		t.Parallel()

		w := newWriterWithFile(t)
		if f, err := w.Create("/a/b"); err == nil {
			_ = f.Close()

			t.Error("Create under a regular file succeeded")
		} else if !errors.Is(err, erofs.ErrNotDirectory) {
			t.Errorf("err = %v; want ErrNotDirectory", err)
		}
	})

	t.Run("Mkdir", func(t *testing.T) {
		t.Parallel()

		w := newWriterWithFile(t)
		if err := w.Mkdir("/a/b", 0o755); !errors.Is(err, erofs.ErrNotDirectory) {
			t.Errorf("err = %v; want ErrNotDirectory", err)
		}
	})

	t.Run("Symlink", func(t *testing.T) {
		t.Parallel()

		w := newWriterWithFile(t)
		if err := w.Symlink("target", "/a/b"); !errors.Is(err, erofs.ErrNotDirectory) {
			t.Errorf("err = %v; want ErrNotDirectory", err)
		}
	})

	t.Run("Mknod", func(t *testing.T) {
		t.Parallel()

		w := newWriterWithFile(t)
		if err := w.Mknod("/a/b", fs.ModeDevice|fs.ModeCharDevice|0o666, 0); !errors.Is(err, erofs.ErrNotDirectory) {
			t.Errorf("err = %v; want ErrNotDirectory", err)
		}
	})

	t.Run("Link", func(t *testing.T) {
		t.Parallel()

		w := newWriterWithFile(t)
		if err := w.Link("/a", "/a/b"); !errors.Is(err, erofs.ErrNotDirectory) {
			t.Errorf("err = %v; want ErrNotDirectory", err)
		}
	})

	t.Run("deeper", func(t *testing.T) {
		t.Parallel()

		w := newWriterWithFile(t)
		if err := w.Mkdir("/a/b/c/d", 0o755); !errors.Is(err, erofs.ErrNotDirectory) {
			t.Errorf("err = %v; want ErrNotDirectory", err)
		}
	})

	t.Run("throughSymlink", func(t *testing.T) {
		t.Parallel()

		w := erofs.Create(&testBuffer{}, erofs.WithBuildTime(1000, 0))
		if err := w.Symlink("elsewhere", "/link"); err != nil {
			t.Fatal(err)
		}

		if err := w.Mkdir("/link/sub", 0o755); !errors.Is(err, erofs.ErrNotDirectory) {
			t.Errorf("err = %v; want ErrNotDirectory", err)
		}
	})

	// Implicit parents are still created when nothing is in the way.
	t.Run("implicitParentsStillWork", func(t *testing.T) {
		t.Parallel()

		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithBuildTime(1000, 0))
		writeFile(t, w, "/x/y/z.txt", []byte("deep"))

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		img, err := erofs.Open(bytes.NewReader(out.Bytes()))
		if err != nil {
			t.Fatal(err)
		}

		got, err := fs.ReadFile(img, "x/y/z.txt")
		if err != nil {
			t.Fatal(err)
		}

		if string(got) != "deep" {
			t.Errorf("got %q, want %q", got, "deep")
		}
	})
}

// --- Finding 7: short inline reads ---

// shortReader declares more than it delivers.
type shortReader struct {
	data []byte
	pos  int
}

func (s *shortReader) Read(p []byte) (int, error) {
	if s.pos >= len(s.data) {
		return 0, io.EOF
	}

	n := copy(p, s.data[s.pos:])
	s.pos += n

	return n, nil
}

// shortSource holds one file whose MapFile.Data only declares its size: the
// content comes from Sys, which delivers 10 bytes.
func shortSource(name string, size int) fstest.MapFS {
	return fstest.MapFS{name: &fstest.MapFile{
		Data: make([]byte, size),
		Mode: 0o644,
		Sys:  &builder.Entry{Data: &shortReader{data: bytes.Repeat([]byte("A"), 10)}},
	}}
}

// TestShortInlineReadIsRejected covers a source that yields fewer bytes than
// it declared. Inline data is followed by the zero fill that aligns the next
// inode, so a short read used to be padded out with NULs and the file read
// back at its declared size with the tail silently zeroed.
func TestShortInlineReadIsRejected(t *testing.T) {
	t.Parallel()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(1000, 0))

	// Small enough to be laid out inline, inside the metadata area.
	if err := w.CopyFrom(shortSource("a.txt", 100)); err != nil {
		t.Fatal(err)
	}

	err := w.Close()
	if err == nil {
		t.Fatal("Close accepted a source that delivered 10 of 100 declared bytes")
	}

	if !strings.Contains(err.Error(), "short read") {
		t.Errorf("err = %v; want it to mention a short read", err)
	}

	t.Logf("rejected with: %v", err)
}

// TestShortFlatPlainReadIsRejected is the pre-existing counterpart, kept so
// both paths stay consistent.
func TestShortFlatPlainReadIsRejected(t *testing.T) {
	t.Parallel()

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(1000, 0))

	// Larger than a block, so it lands in the flat-plain data area.
	if err := w.CopyFrom(shortSource("big.bin", 40000)); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err == nil {
		t.Fatal("Close accepted a short flat-plain source")
	} else if !strings.Contains(err.Error(), "short read") {
		t.Errorf("err = %v; want it to mention a short read", err)
	}
}

// --- Finding 8: sub-second build time in compact inodes ---

// TestCompactInodeKeepsSubSecondMtime covers a build time with a nanosecond
// component. A compact inode stores no timestamp and the reader rebuilds it
// from the superblock, so an entry was only safe to pack compact when its
// mtime matched both superblock fields, not just the seconds.
func TestCompactInodeKeepsSubSecondMtime(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name             string
		buildSec         uint64
		buildNs          uint32
		mtimeSec         uint64
		mtimeNs          uint32
		wantSec, wantNs  uint64
		descriptionOfFix string
	}{
		{"matchingNsec", 1000, 123456789, 1000, 123456789, 1000, 123456789, "packs compact, round-trips"},
		{"zeroEntryNsec", 1000, 123456789, 1000, 0, 1000, 0, "must not inherit the build nsec"},
		{"zeroBuildNsec", 1000, 0, 1000, 0, 1000, 0, "the common case"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out := &testBuffer{}
			w := erofs.Create(out, erofs.WithBuildTime(tc.buildSec, tc.buildNs))
			writeFile(t, w, "/f.txt", []byte("hello"))

			mt := time.Unix(int64(tc.mtimeSec), int64(tc.mtimeNs))
			if err := w.Chtimes("/f.txt", mt, mt); err != nil {
				t.Fatal(err)
			}

			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			img, err := erofs.Open(bytes.NewReader(out.Bytes()))
			if err != nil {
				t.Fatal(err)
			}

			fi, err := fs.Stat(img, "f.txt")
			if err != nil {
				t.Fatal(err)
			}

			st, ok := fi.Sys().(*erofs.Stat)
			if !ok {
				t.Fatalf("Sys returned %T, want *Stat", fi.Sys())
			}

			if st.Mtime != tc.wantSec || uint64(st.MtimeNs) != tc.wantNs {
				t.Errorf("mtime = (%d,%d), want (%d,%d) — %s",
					st.Mtime, st.MtimeNs, tc.wantSec, tc.wantNs, tc.descriptionOfFix)
			}
		})
	}
}

// --- Finding 9: xattr field widths ---

func TestXattrLimits(t *testing.T) {
	t.Parallel()

	longName := "user." + strings.Repeat("n", 256)
	bigValue := strings.Repeat("v", 65536)

	t.Run("SetxattrRejectsLongName", func(t *testing.T) {
		t.Parallel()

		w := erofs.Create(&testBuffer{}, erofs.WithBuildTime(1000, 0))
		if err := w.Mkdir("/d", 0o755); err != nil {
			t.Fatal(err)
		}

		if err := w.Setxattr("/d", longName, "v"); !errors.Is(err, erofs.ErrInvalid) {
			t.Errorf("err = %v; want ErrInvalid", err)
		}
	})

	t.Run("SetxattrRejectsBigValue", func(t *testing.T) {
		t.Parallel()

		w := erofs.Create(&testBuffer{}, erofs.WithBuildTime(1000, 0))
		if err := w.Mkdir("/d", 0o755); err != nil {
			t.Fatal(err)
		}

		if err := w.Setxattr("/d", "user.big", bigValue); !errors.Is(err, erofs.ErrInvalid) {
			t.Errorf("err = %v; want ErrInvalid", err)
		}
	})

	t.Run("BoundaryValuesAccepted", func(t *testing.T) {
		t.Parallel()

		out := &testBuffer{}

		w := erofs.Create(out, erofs.WithBuildTime(1000, 0))
		if err := w.Mkdir("/d", 0o755); err != nil {
			t.Fatal(err)
		}

		name := "user." + strings.Repeat("n", 255)

		value := strings.Repeat("v", 65535)
		if err := w.Setxattr("/d", name, value); err != nil {
			t.Fatalf("boundary xattr rejected: %v", err)
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		img, err := erofs.Open(bytes.NewReader(out.Bytes()))
		if err != nil {
			t.Fatal(err)
		}

		fi, err := fs.Stat(img, "d")
		if err != nil {
			t.Fatal(err)
		}

		got := fi.Sys().(*erofs.Stat).Xattrs[name]
		if got != value {
			t.Errorf("xattr round-trip failed: got %d bytes, want %d", len(got), len(value))
		}

		fsckImage(t, out.Bytes())
	})

	// Xattrs also arrive through CopyFrom, where the source picks lengths.
	t.Run("CopyFromRejectsOversized", func(t *testing.T) {
		t.Parallel()

		out := &testBuffer{}
		w := erofs.Create(out, erofs.WithBuildTime(1000, 0))

		err := w.CopyFrom(fstest.MapFS{"d": &fstest.MapFile{
			Mode: fs.ModeDir | 0o755,
			Sys:  &erofs.Stat{Xattrs: map[string]string{"user.big": bigValue}},
		}})
		if err == nil {
			err = w.Close()
		}

		if !errors.Is(err, erofs.ErrInvalid) {
			t.Errorf("CopyFrom/Close err = %v; want ErrInvalid", err)
		} else {
			t.Logf("rejected with: %v", err)
		}
	})
}

// --- Finding 12: the chunked-file feature flag ---

// TestChunkedFeatureFlagDeclared covers a metadata-only entry with no chunk
// mappings. It still gets LayoutChunkBased, but the flag used to be keyed off
// len(e.chunks), so the image carried chunk-based inodes without declaring
// the feature.
func TestChunkedFeatureFlagDeclared(t *testing.T) {
	t.Parallel()

	const bs = 4096

	blob := sparseBlob(bs)

	// A source that cannot describe where its data lives: the entry still
	// becomes chunk-based, but carries no chunk mappings. Keying the feature
	// flag off len(e.chunks) missed exactly this case.
	src := newSparseFS(bs, blob)
	src.noRanges = true

	out := &testBuffer{}

	w := erofs.Create(out, erofs.WithBlockSize(bs), erofs.WithBuildTime(1000, 0))
	if err := w.CopyFrom(src, erofs.MetadataOnly()); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	img, err := erofs.Open(bytes.NewReader(out.Bytes()), erofs.WithExtraDevices(bytes.NewReader(blob)))
	if err != nil {
		t.Fatal(err)
	}

	fi, err := fs.Stat(img, "f")
	if err != nil {
		t.Fatal(err)
	}

	buf := out.Bytes()

	var sb disk.SuperBlock

	sb.Unmarshal(buf[disk.SuperBlockOffset:])

	var ino disk.InodeCompact

	nid := int64(fi.Sys().(*erofs.Stat).Ino)
	ino.Unmarshal(buf[int64(sb.MetaBlkAddr)<<sb.BlkSizeBits+nid*disk.SizeInodeSlot:])

	if ino.Format&disk.InodeFormatExtended != 0 {
		t.Fatal("test premise broken: the inode is extended, decoded here as compact")
	}

	if layout := (ino.Format & disk.InodeFormatLayoutMask) >> 1; layout != disk.LayoutChunkBased {
		t.Fatalf("test premise broken: layout = %d, want chunk-based", layout)
	}

	if sb.FeatureIncompat&disk.FeatureIncompatChunkedFile == 0 {
		t.Errorf("image holds chunk-based inodes but FeatureIncompat = %#x lacks the chunked-file bit",
			sb.FeatureIncompat)
	}
}

// fsckImage validates a device-less image against the reference tool.
func fsckImage(t *testing.T, image []byte) {
	t.Helper()

	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		return
	}

	p := filepath.Join(t.TempDir(), "img.erofs")
	if err := os.WriteFile(p, image, 0o600); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.CommandContext(t.Context(), "fsck.erofs", p).CombinedOutput(); err != nil {
		t.Errorf("fsck.erofs rejected the image: %v\n%s", err, out)
	}
}

// --- Finding 11: a swallowed data-file seek error ---

// TestDataFileSeekErrorIsSticky covers WithDataFile when the initial seek
// fails. dataOff stayed at 0, so the first file's chunk indexes pointed at
// block 0 while its bytes went wherever the file position actually was.
func TestDataFileSeekErrorIsSticky(t *testing.T) {
	t.Parallel()

	df, err := os.CreateTemp(t.TempDir(), "blob-*")
	if err != nil {
		t.Fatal(err)
	}
	// Closing first makes the seek inside Create fail.
	if err := df.Close(); err != nil {
		t.Fatal(err)
	}

	w := erofs.Create(&testBuffer{}, erofs.WithDataFile(df), erofs.WithBuildTime(1000, 0))

	if f, err := w.Create("/a"); err == nil {
		_ = f.Close()

		t.Error("Create succeeded although the data file could not be seeked")
	} else {
		t.Logf("Create rejected with: %v", err)
	}

	if err := w.Close(); err == nil {
		t.Error("Close succeeded although the data file could not be seeked")
	}
}
