package erofs_test

import (
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	erofs "github.com/forkcloser/erofs"
)

func ExampleOpen() {
	// Any io.ReaderAt holding an image will do; an *os.File is the usual one.
	path, err := buildExampleImage()
	if err != nil {
		slog.Error("build the example image", "err", err)

		return
	}

	f, err := os.Open(path)
	if err != nil {
		slog.Error("open the image", "err", err)

		return
	}

	img, err := erofs.Open(f)
	if err != nil {
		_ = f.Close()

		slog.Error("read the image", "err", err)

		return
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
		slog.Error("walk the image", "err", err)
	}
	// Output:
	// .
	// etc
	// etc/hostname
	// usr
}

// buildExampleImage writes a small image to a temporary file and returns
// its path.
func buildExampleImage() (string, error) {
	out, err := os.CreateTemp("", "erofs-example-*.img")
	if err != nil {
		return "", err
	}

	path := out.Name()

	w := erofs.Create(out)
	if err = w.Mkdir("/usr", 0o755); err != nil {
		return "", err
	}

	h, err := w.Create("/etc/hostname")
	if err != nil {
		return "", err
	}

	if _, err := h.Write([]byte("example\n")); err != nil {
		return "", err
	}

	if err := h.Close(); err != nil {
		return "", err
	}

	if err := w.Close(); err != nil {
		return "", err
	}

	if err := out.Close(); err != nil {
		return "", err
	}

	return path, nil
}

func ExampleCreate() {
	var buf testBuffer

	w := erofs.Create(&buf)

	f, err := w.Create("/hello.txt")
	if err != nil {
		slog.Error("create the file", "err", err)

		return
	}

	if _, err = f.Write([]byte("hello world\n")); err != nil {
		slog.Error("write the file", "err", err)

		return
	}

	if err = f.Close(); err != nil {
		slog.Error("close the file", "err", err)

		return
	}

	if err = w.Mkdir("/dir", 0o755); err != nil {
		slog.Error("make the directory", "err", err)

		return
	}

	if err = w.Close(); err != nil {
		slog.Error("close the image", "err", err)

		return
	}

	// Read back
	img, err := erofs.Open(bytes.NewReader(buf.Bytes()))
	if err != nil {
		slog.Error("read the image back", "err", err)

		return
	}

	data, err := fs.ReadFile(img, "hello.txt")
	if err != nil {
		slog.Error("read hello.txt", "err", err)

		return
	}

	fmt.Print(string(data))
	// Output: hello world
}
