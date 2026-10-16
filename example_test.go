package erofs_test

import (
	"bytes"
	"fmt"
	"io/fs"
	"log" //nolint:depguard // examples end on log.Fatal, as the standard library's do
	"os"

	erofs "github.com/forkcloser/erofs"
)

func ExampleOpen() {
	// Any io.ReaderAt holding an image will do; an *os.File is the usual one.
	path := buildExampleImage()

	f, err := os.Open(path)
	if err != nil {
		log.Fatal(err)
	}

	img, err := erofs.Open(f)
	if err != nil {
		_ = f.Close()

		log.Fatal(err)
	}

	err = fs.WalkDir(img, ".", func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fmt.Println(path)

		return nil
	})
	_ = f.Close()
	_ = os.Remove(path)

	if err != nil {
		log.Fatal(err)
	}
	// Output:
	// .
	// etc
	// etc/hostname
	// usr
}

// buildExampleImage writes a small image to a temporary file and returns
// its path.
//
//revive:disable:deep-exit an example helper fails as an example does, with log.Fatal
func buildExampleImage() string {
	out, err := os.CreateTemp("", "erofs-example-*.img")
	if err != nil {
		log.Fatal(err)
	}

	path := out.Name()

	w := erofs.Create(out)
	if err = w.Mkdir("/usr", 0o755); err != nil {
		log.Fatal(err)
	}

	h, err := w.Create("/etc/hostname")
	if err != nil {
		log.Fatal(err)
	}

	if _, err := h.Write([]byte("example\n")); err != nil {
		log.Fatal(err)
	}

	if err := h.Close(); err != nil {
		log.Fatal(err)
	}

	if err := w.Close(); err != nil {
		log.Fatal(err)
	}

	if err := out.Close(); err != nil {
		log.Fatal(err)
	}

	return path
}

//revive:enable:deep-exit

func ExampleCreate() {
	var buf testBuffer

	w := erofs.Create(&buf)

	f, err := w.Create("/hello.txt")
	if err != nil {
		log.Fatal(err)
	}

	if _, err = f.Write([]byte("hello world\n")); err != nil {
		log.Fatal(err)
	}

	if err = f.Close(); err != nil {
		log.Fatal(err)
	}

	if err = w.Mkdir("/dir", 0o755); err != nil {
		log.Fatal(err)
	}

	if err = w.Close(); err != nil {
		log.Fatal(err)
	}

	// Read back
	img, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		log.Fatal(err)
	}

	data, err := fs.ReadFile(img, "hello.txt")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Print(string(data))
	// Output: hello world
}
