package erofs_test

import (
	"fmt"
	"testing"
)

// TestContractWriterSharesAnAttributeOfAnyNumberOfFiles: an attribute that
// more than two files carry is shared however many they are, 255 to 258
// and 511 to 514 among them: a count of its carriers kept in a byte, and
// let run past the byte, comes back to 0, 1 and 2 at 256, 257 and 258 and
// at 512, 513 and 514, and would leave the attribute with every file.
func TestContractWriterSharesAnAttributeOfAnyNumberOfFiles(t *testing.T) {
	t.Parallel()

	for _, files := range []int{3, 255, 256, 257, 258, 259, 511, 512, 513, 514} {
		tree := map[string]map[string]string{}
		names := make([]string, 0, files)

		for i := range files {
			name := fmt.Sprintf("f%04d", i)
			names = append(names, name)
			tree[name] = map[string]string{"user.k": "the same on every file"}
		}

		_, shared := sharedOf(t, attributed(t, names, tree), names)

		named := 0

		for _, name := range names {
			named += len(shared[name])
		}

		if named != files {
			t.Errorf("%d files carry one attribute: %d of them name it in the shared area; want all", files, named)
		}
	}
}
