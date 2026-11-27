package erofs_test

import (
	"testing"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/erofsspec"
)

// TestContractWriterLeavesTheXattrNameFilterZero: the Writer computes no
// name filter and says so with a zero one, as mkfs.erofs writes when it
// computes none: in h_name_filter a set bit says a name is absent, and the
// kernel answers ENODATA on it once the image has the XATTR_FILTER feature.
// The Writer does not set that feature; an image of all ones would deny
// every attribute to whatever set it afterwards.
func TestContractWriterLeavesTheXattrNameFilterZero(t *testing.T) {
	t.Parallel()

	out := &testBuffer{}
	w := erofs.Create(out)

	writeFile(t, w, "/f", []byte("ten bytes."))

	if err := w.Setxattr("/f", "user.k", "v"); err != nil {
		t.Fatal(err)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	ref, err := erofsspec.Parse(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	if ref.Super.FeatureCompat&erofsspec.FeatureCompatXattrFilter != 0 {
		t.Errorf("the image has the XATTR_FILTER feature: compat %#x", ref.Super.FeatureCompat)
	}

	in, err := ref.Resolve("f", 0)
	if err != nil {
		t.Fatal(err)
	}

	if in.XattrSize == 0 || in.NameFilter != 0 {
		t.Errorf("f has an xattr body of %d bytes with name filter %#x; want a body, and a filter of zero",
			in.XattrSize, in.NameFilter)
	}
}
