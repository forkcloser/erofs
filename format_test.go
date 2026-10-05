package erofs_test

import (
	"bytes"
	"encoding/binary"
	"io/fs"
	"testing"

	"github.com/forkcloser/erofs"
)

// TestPOSIXACLXattrPrefix pins the two ACL prefixes as the kernel spells
// them: the whole attribute name, with no trailing dot, so an ACL is stored
// as name index 2 or 3 with an empty suffix. Indexes 2 and 3 were written
// with a dot, so a stored ACL read back as "system.posix_acl_access." and
// never matched a caller asking for the real name (ported from upstream
// go-erofs 03d68d8).
func TestPOSIXACLXattrPrefix(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		index byte
	}{
		{"system.posix_acl_access", 2},
		{"system.posix_acl_default", 3},
	} {
		out := &testBuffer{}

		w := erofs.Create(out, erofs.WithBuildTime(1000, 0))
		if err := w.Mkdir("/d", 0o755); err != nil {
			t.Fatal(err)
		}

		value := "acl payload"
		if err := w.Setxattr("/d", tc.name, value); err != nil {
			t.Fatal(err)
		}

		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		// An xattr entry is e_name_len, e_name_index and e_value_size,
		// then the name suffix and the value: with no suffix, the value
		// follows the header directly.
		buf := out.Bytes()

		at := bytes.Index(buf, []byte(value))
		if at < 4 {
			t.Fatalf("%s: value not found in the image", tc.name)
		}

		header := make([]byte, 4)
		header[1] = tc.index
		binary.LittleEndian.PutUint16(header[2:], uint16(len(value)))

		if got := buf[at-4 : at]; !bytes.Equal(got, header) {
			t.Errorf("%s: entry header = % x, want % x (name index %d, empty suffix)", tc.name, got, header, tc.index)
		}

		img, err := erofs.Open(bytes.NewReader(buf))
		if err != nil {
			t.Fatal(err)
		}

		fi, err := fs.Stat(img, "d")
		if err != nil {
			t.Fatal(err)
		}

		if xattrs := fi.Sys().(*erofs.Stat).Xattrs; len(xattrs) != 1 || xattrs[tc.name] != value {
			t.Errorf("%s: xattrs = %q, want only that one", tc.name, xattrs)
		}
	}
}
