package erofs

import "testing"

// TestPOSIXACLXattrPrefix pins the two ACL prefixes as the kernel spells
// them: the whole attribute name, with no trailing dot. Indexes 2 and 3 were
// written with a dot, so a stored ACL read back as "system.posix_acl_access."
// and never matched a caller asking for the real name (ported from upstream
// go-erofs 03d68d8).
func TestPOSIXACLXattrPrefix(t *testing.T) {
	for _, name := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		index, suffix := xattrSplit(name)
		if suffix != "" {
			t.Fatalf("xattrSplit(%q) suffix=%q, want empty", name, suffix)
		}
		if got := xattrIndex(index).String(); got != name {
			t.Fatalf("xattr index %d prefix=%q, want %q", index, got, name)
		}
	}
}
