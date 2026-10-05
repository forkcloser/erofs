package erofs //nolint:testpackage // white-box: forces the unexported block loop to pin it to the direct-read path

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// writeFile creates, fills and closes one file.
func writeFile(t *testing.T, w *Writer, name string, data []byte) {
	t.Helper()

	f, err := w.Create(name)
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}

	if _, err := f.Write(data); err != nil {
		t.Fatalf("Write(%q): %v", name, err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close(%q): %v", name, err)
	}
}

// readAllVia reads a file to completion using bufSize-byte Reads, optionally
// forcing the block-at-a-time path so the two can be compared.
func readAllVia(t *testing.T, img *image, name string, bufSize int, forceBlockPath bool) []byte {
	t.Helper()

	nid, ftype, base, err := img.resolve("open", name, true)
	if err != nil {
		t.Fatalf("resolve %q: %v", name, err)
	}

	f := &file{img: img, name: base, nid: nid, ftype: ftype}
	if forceBlockPath {
		// Pretend the direct path was checked and unavailable.
		f.directChecked = true
		f.direct = nil
	}

	var out []byte

	buf := make([]byte, bufSize)
	for {
		n, err := f.Read(buf)
		out = append(out, buf[:n]...)

		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("read %q: %v", name, err)
		}

		if n == 0 {
			t.Fatalf("read %q returned 0 bytes with no error", name)
		}
	}

	return out
}

// TestDirectReadMatchesBlockPath pins the fast path to the slow one. They
// must be indistinguishable for every layout and every read size, including
// sizes that do not divide the file or the block.
func TestDirectReadMatchesBlockPath(t *testing.T) {
	t.Parallel()

	const blockSize = 4096

	sizes := []int{
		1,
		100,
		blockSize - 1,
		blockSize,
		blockSize + 1,
		3*blockSize + 17,
		64 * 1024,
	}

	out := &seekBuf{}
	w := Create(out, WithBlockSize(blockSize), WithBuildTime(1000, 0))

	want := make(map[string][]byte, len(sizes))
	for _, size := range sizes {
		name := "/f" + itoa(size)

		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i*7 + size)
		}

		want[name[1:]] = data
		writeFile(t, w, name, data)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	fsys, err := Open(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatal(err)
	}

	img, ok := fsys.(*image)
	if !ok {
		t.Fatalf("Open returned %T, want *image", fsys)
	}

	for name, expect := range want {
		for _, bufSize := range []int{1, 7, 512, 4096, 5000, 1 << 20} {
			direct := readAllVia(t, img, name, bufSize, false)
			blocks := readAllVia(t, img, name, bufSize, true)

			if !bytes.Equal(direct, expect) {
				t.Errorf("%s bufSize=%d: direct path returned %d bytes, want %d",
					name, bufSize, len(direct), len(expect))
			}

			if !bytes.Equal(blocks, expect) {
				t.Errorf("%s bufSize=%d: block path returned %d bytes, want %d",
					name, bufSize, len(blocks), len(expect))
			}

			if !bytes.Equal(direct, blocks) {
				t.Errorf("%s bufSize=%d: the two read paths disagree", name, bufSize)
			}
		}
	}
}

// TestWriteToMatchesRead covers the io.WriterTo path io.Copy prefers.
func TestWriteToMatchesRead(t *testing.T) {
	t.Parallel()

	const blockSize = 4096

	out := &seekBuf{}
	w := Create(out, WithBlockSize(blockSize), WithBuildTime(1000, 0))

	data := make([]byte, 5*blockSize+123)
	for i := range data {
		data[i] = byte(i)
	}

	writeFile(t, w, "/big", data)
	writeFile(t, w, "/small", []byte("tiny"))

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	img, err := Open(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatal(err)
	}

	for name, expect := range map[string][]byte{"big": data, "small": []byte("tiny")} {
		fh, err := img.Open(name)
		if err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer

		n, err := io.Copy(&buf, fh)
		if err != nil {
			t.Fatalf("io.Copy %q: %v", name, err)
		}

		_ = fh.Close()

		if n != int64(len(expect)) || !bytes.Equal(buf.Bytes(), expect) {
			t.Errorf("%s: io.Copy produced %d bytes, want %d", name, n, len(expect))
		}
	}
}

// TestWriteToResumesFromOffset covers io.Copy after a partial Read, where
// WriteTo must pick up where the reader left off rather than restarting.
func TestWriteToResumesFromOffset(t *testing.T) {
	t.Parallel()

	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0))
	data := []byte("0123456789abcdef")
	writeFile(t, w, "/f", data)

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	img, err := Open(bytes.NewReader(out.buf))
	if err != nil {
		t.Fatal(err)
	}

	fh, err := img.Open("f")
	if err != nil {
		t.Fatal(err)
	}

	head := make([]byte, 6)
	if _, err := io.ReadFull(fh, head); err != nil {
		t.Fatal(err)
	}

	if string(head) != "012345" {
		t.Fatalf("head = %q, want %q", head, "012345")
	}

	var rest bytes.Buffer
	if _, err := io.Copy(&rest, fh); err != nil {
		t.Fatal(err)
	}

	if rest.String() != "6789abcdef" {
		t.Errorf("rest = %q, want %q", rest.String(), "6789abcdef")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}

	return string(b)
}
