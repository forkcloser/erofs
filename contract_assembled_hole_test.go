package erofs_test

import (
	"bytes"
	"io/fs"
	"testing"

	"github.com/forkcloser/erofs"
	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/erofsspec"
)

// TestAssembledHoleIsAHoleUnderEitherInode: the assembler's Hole is a flat
// file at the null address, which reads as zeros, with 32-byte inodes and
// with 64-byte ones, whose start block is sixteen bits wider.
func TestAssembledHoleIsAHoleUnderEitherInode(t *testing.T) {
	t.Parallel()

	for name, extended := range map[string]bool{"compact": false, "extended": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			img := corpus.Assemble(corpus.Layout{BlockBits: 12, Extended: extended, Entries: []corpus.AEntry{
				{Name: "hole", Mode: 0o100644, Nlink: 1, Mtime: corpus.AssembledBuildTime, Hole: 3000},
			}})

			ref, err := erofsspec.Parse(img)
			if err != nil {
				t.Fatal(err)
			}

			in, err := ref.Resolve("hole", 0)
			if err != nil {
				t.Fatal(err)
			}

			data, err := ref.Read(in)
			if err != nil || !bytes.Equal(data, make([]byte, 3000)) {
				t.Errorf("the reference reads %d bytes, %v; want 3000 zeros", len(data), err)
			}

			fsys, err := erofs.Open(bytes.NewReader(img))
			if err != nil {
				t.Fatal(err)
			}

			data, err = fs.ReadFile(fsys, "hole")
			if err != nil || !bytes.Equal(data, make([]byte, 3000)) {
				t.Errorf("the package reads %d bytes, %v; want 3000 zeros", len(data), err)
			}
		})
	}
}
