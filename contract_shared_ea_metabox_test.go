package erofs_test

import (
	"bytes"
	"errors"
	"io/fs"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/disk"
	"github.com/forkcloser/erofs/internal/erofsspec"
)

// TestContractSharedXattrsInAMetaboxTheImageDoesNotHave: an image whose
// superblock says its shared xattr entries are read through the metabox
// (the compat feature SHARED_EA_IN_METABOX) and that has no metabox, the
// only kind Open takes. The kernel mounts it and fails the listing of every
// inode that has an xattr body, inline entries or shared ones, since the
// shared pass begins with the metabox's buffer (xattr.c:
// erofs_xattr_iter_shared); an inode with no body and every file's data are
// as before. The reader used to ignore the feature and serve the entries it
// found where the image says they are not.
func TestContractSharedXattrsInAMetaboxTheImageDoesNotHave(t *testing.T) {
	t.Parallel()

	withXattrs, bare := 0, 0

	for _, im := range corpus.Committed(t) {
		if im.Blob != nil {
			continue
		}

		sound, err := erofs.Open(bytes.NewReader(im.Img))
		if err != nil {
			continue
		}

		flagged := bytes.Clone(im.Img)
		flagged[disk.SuperBlockOffset+8] |= disk.FeatureCompatSharedEAInMetabox
		stampChecksum(flagged)

		fsys, err := erofs.Open(bytes.NewReader(flagged))
		if err != nil {
			t.Fatalf("%s: Open: %v; want the image opened, as the kernel mounts it", im.Name, err)
		}

		ref, err := erofsspec.Parse(im.Img)
		if err != nil {
			continue
		}

		bodies := 0

		err = fs.WalkDir(sound, ".", func(p string, d fs.DirEntry, err error) error {
			// What the image as committed does not serve is not this
			// test's: one of them holds an inode the reader refuses.
			if err != nil {
				return nil //nolint:nilerr // skipped, on purpose
			}

			info, err := d.Info()
			if err != nil {
				return nil //nolint:nilerr // skipped, on purpose
			}

			// The reference says which inodes have a body: one that holds
			// only entries the kernel does not list shows no xattrs and
			// has a body all the same.
			in, err := ref.Resolve(p, 0)
			if err != nil {
				return nil //nolint:nilerr // skipped, on purpose
			}

			_, lerr := fs.Lstat(fsys, p)

			if in.XattrSize > 0 {
				bodies++

				if !errors.Is(lerr, erofs.ErrCorrupt) {
					t.Errorf("%s: Lstat(%s), an inode with an xattr body: %v; want ErrCorrupt", im.Name, p, lerr)
				}
			} else {
				bare++

				if lerr != nil {
					t.Errorf("%s: Lstat(%s), an inode with no xattr body: %v; want it served", im.Name, p, lerr)
				}
			}

			if d.Type().IsRegular() && info.Size() < 1<<20 {
				want, werr := fs.ReadFile(sound, p)
				got, gerr := fs.ReadFile(fsys, p)

				if werr != nil || gerr != nil || !bytes.Equal(got, want) {
					t.Errorf("%s: ReadFile(%s): %v beside %v; want the same bytes", im.Name, p, gerr, werr)
				}
			}

			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v", im.Name, err)
		}

		withXattrs += bodies

		if bodies == 0 {
			continue
		}

		for _, opts := range [][]erofs.CopyOpt{nil, {erofs.MetadataOnly()}} {
			w := erofs.Create(&testBuffer{}, erofs.WithBuildTime(corpus.BuildTime, 0))
			if cerr := w.CopyFrom(fsys, opts...); !errors.Is(cerr, erofs.ErrCorrupt) {
				t.Errorf("%s: CopyFrom with %d options: %v; want ErrCorrupt", im.Name, len(opts), cerr)
			}
		}
	}

	if withXattrs == 0 || bare == 0 {
		t.Fatalf(
			"%d inodes with an xattr body and %d without in the committed images: the test wants both",
			withXattrs,
			bare,
		)
	}
}
