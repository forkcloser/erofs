package erofs_test

import (
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
)

// TestContractWriterStoresOnceWhatAnInodeCannotName: a file that cannot
// hold its own attributes names 255 of them in the shared area and holds
// the others itself, so the others are not put in the area: with 300
// attributes of 1100 bytes the image is their bytes once and under a tenth
// more. It held the 45 the file does not name twice, once in the area
// where nothing names them.
func TestContractWriterStoresOnceWhatAnInodeCannotName(t *testing.T) {
	t.Parallel()

	const (
		count = 300
		size  = 1100
	)

	attrs := map[string]string{}
	for i := range count {
		attrs[fmt.Sprintf("user.a%03d", i)] = strings.Repeat(fmt.Sprintf("%03d ", i), size/4)
	}

	out := &testBuffer{}
	w := erofs.Create(out, erofs.WithBuildTime(corpus.BuildTime, 0))
	writeFile(t, w, "/f", nil)

	for attr, value := range attrs {
		if err := w.Setxattr("/f", attr, value); err != nil {
			t.Fatal(err)
		}
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	all, shared := sharedOf(t, out.Bytes(), []string{"f"})
	if !maps.Equal(all["f"], attrs) || len(shared["f"]) != 255 {
		t.Fatalf("f has %d attributes by the reference, %d of them in the shared area; want its %d, and 255 there",
			len(all["f"]), len(shared["f"]), count)
	}

	if most := count * size * 11 / 10; len(out.Bytes()) > most {
		t.Errorf("the image is %d bytes for %d attributes of %d bytes; want %d at most, each stored once",
			len(out.Bytes()), count, size, most)
	}

	fsckUnlessPlatformLimit(t, corpus.Image{Name: "attributes one inode cannot name", Img: out.Bytes()})
}
