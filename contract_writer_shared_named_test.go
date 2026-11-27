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

// TestContractWriterStoresOnceTheFirstAttributeItCannotName: a file names
// 255 attributes in the shared area and no more, so of 256 it cannot hold
// the last is its own, stored beside its inode and nowhere else: the image
// grows by that one attribute from 255 to 256, where a count that ran one
// further would store it twice.
func TestContractWriterStoresOnceTheFirstAttributeItCannotName(t *testing.T) {
	t.Parallel()

	const size = 20000

	image := func(count int) ([]byte, map[string]string) {
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

		return out.Bytes(), attrs
	}

	named, _ := image(255)
	over, attrs := image(256)

	all, shared := sharedOf(t, over, []string{"f"})
	if !maps.Equal(all["f"], attrs) || len(shared["f"]) != 255 {
		t.Fatalf("f has %d attributes by the reference, %d of them in the shared area; want its 256, and 255 there",
			len(all["f"]), len(shared["f"]))
	}

	// One attribute, its entry, and what rounds the area and the image to
	// whole blocks.
	if grown, most := len(over)-len(named), size+2*4096; grown > most {
		t.Errorf("the image grows by %d bytes for the 256th attribute of %d bytes; want %d at most, the attribute once",
			grown, size, most)
	}

	fsckUnlessPlatformLimit(t, corpus.Image{Name: "256 attributes one inode cannot hold", Img: over})
}
