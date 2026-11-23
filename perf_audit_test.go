package erofs //nolint:testpackage // white-box: BenchmarkPerfStatInode times the unexported inode decoder alone

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"strconv"
	"testing"
)

// countingReaderAt counts ReadAt calls to quantify syscall amplification.
type countingReaderAt struct {
	ra    io.ReaderAt
	calls int
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.calls++

	return c.ra.ReadAt(p, off)
}

func buildPerfImage(tb testing.TB, size int) []byte {
	tb.Helper()

	out := &seekBuf{}
	w := Create(out, WithBuildTime(1000, 0))

	f, err := w.Create("/big")
	if err != nil {
		tb.Fatal(err)
	}

	if _, err := f.Write(bytes.Repeat([]byte("x"), size)); err != nil {
		tb.Fatal(err)
	}

	if err := f.Close(); err != nil {
		tb.Fatal(err)
	}

	if err := w.Close(); err != nil {
		tb.Fatal(err)
	}

	return out.buf
}

func BenchmarkPerfSequentialRead(b *testing.B) {
	const size = 8 << 20

	tmp, err := os.CreateTemp(b.TempDir(), "img")
	if err != nil {
		b.Fatal(err)
	}

	if _, err = tmp.Write(buildPerfImage(b, size)); err != nil {
		b.Fatal(err)
	}

	img, err := Open(tmp)
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		fh, err := img.Open("big")
		if err != nil {
			b.Fatal(err)
		}

		if _, err := io.Copy(io.Discard, fh); err != nil {
			b.Fatal(err)
		}

		_ = fh.Close()
	}
}

func buildWideImage(tb testing.TB, nfiles int) fs.FS {
	tb.Helper()

	out := &seekBuf{}

	w := Create(out, WithBuildTime(1000, 0))
	if err := w.Mkdir("/d", 0o755); err != nil {
		tb.Fatal(err)
	}

	for i := range nfiles {
		name := "/d/f" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))

		f, err := w.Create(name)
		if err != nil {
			continue
		}

		if _, err := f.Write([]byte("x")); err != nil {
			tb.Fatal(err)
		}

		if err := f.Close(); err != nil {
			tb.Fatal(err)
		}
	}

	if err := w.Close(); err != nil {
		tb.Fatal(err)
	}

	img, err := Open(bytes.NewReader(out.buf))
	if err != nil {
		tb.Fatal(err)
	}

	return img
}

// BenchmarkPerfLookup exercises path resolution: one inode decode plus a
// binary search over dirents per component.
func BenchmarkPerfLookup(b *testing.B) {
	img := buildWideImage(b, 500)
	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := fs.Stat(img, "d/faaa"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPerfReadDir exercises dirent decoding across a wide directory.
func BenchmarkPerfReadDir(b *testing.B) {
	img := buildWideImage(b, 500)
	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		if _, err := fs.ReadDir(img, "d"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPerfStatInode isolates inode decoding.
func BenchmarkPerfStatInode(b *testing.B) {
	img, ok := buildWideImage(b, 8).(*image)
	if !ok {
		b.Fatal("buildWideImage did not return an *image")
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		// ftype has to match the inode's own type: readInfo rejects a dirent
		// that disagrees, and the zero value means a regular file.
		f := &file{img: img, nid: uint64(img.sb.RootNid), ftype: fs.ModeDir}
		if _, err := f.readInfo(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPerfWriteTree covers the writer's layout and serialization work
// across many directories.
func BenchmarkPerfWriteTree(b *testing.B) {
	b.ReportAllocs()

	for range b.N {
		out := &seekBuf{}
		w := Create(out, WithBuildTime(1000, 0))

		for d := range 20 {
			dir := "/d" + string(rune('a'+d))
			if err := w.Mkdir(dir, 0o755); err != nil {
				b.Fatal(err)
			}

			for j := range 100 {
				f, err := w.Create(dir + "/f" + string(rune('a'+j%26)) + string(rune('a'+j/26)))
				if err != nil {
					continue
				}

				if _, err := f.Write([]byte("x")); err != nil {
					b.Fatal(err)
				}

				if err := f.Close(); err != nil {
					b.Fatal(err)
				}
			}
		}

		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPerfWriteWideDirs isolates the layout and serialization work that
// scales with directory width. Entries carry no data, so the spool I/O that
// dominates BenchmarkPerfWriteTree stays out of the way.
func BenchmarkPerfWriteWideDirs(b *testing.B) {
	names := make([]string, 0, 4000)

	for d := range 20 {
		for j := range 200 {
			names = append(names, "/d"+string(rune('a'+d))+
				"/f"+string(rune('a'+j%26))+string(rune('a'+(j/26)%26))+string(rune('a'+(j/676)%26)))
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		w := Create(&seekBuf{}, WithBuildTime(1000, 0))
		for _, n := range names {
			if err := w.Symlink("t", n); err != nil {
				b.Fatal(err)
			}
		}

		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPerfRemoveLinkedNames covers the removal of the first name of
// many multiply-linked files at once, as a layer's marker or RemoveAll does
// over a directory of them: each removal hands its inode to another name,
// and the whole must grow with their number, not with its square.
func BenchmarkPerfRemoveLinkedNames(b *testing.B) {
	for _, n := range []int{2000, 16000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()

			for range b.N {
				b.StopTimer()

				w := Create(&seekBuf{}, WithBuildTime(1000, 0))

				for i := range n {
					name := "/d/f" + strconv.Itoa(i)

					f, err := w.Create(name)
					if err != nil {
						b.Fatal(err)
					}

					if err = f.Close(); err != nil {
						b.Fatal(err)
					}

					if err = w.Link(name, "/e/l"+strconv.Itoa(i)); err != nil {
						b.Fatal(err)
					}
				}

				b.StartTimer()

				if err := w.RemoveAll("/d"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPerfRemoveNamesOfOneInode covers the removal of the many names
// of one file: the later names with their directory, the later names one at
// a time, and every name with the directory that holds them all. Each must
// grow with their number, not with its square.
func BenchmarkPerfRemoveNamesOfOneInode(b *testing.B) {
	for _, shape := range []struct {
		name   string
		first  string
		remove func(b *testing.B, w *Writer, n int)
	}{
		{"later names with their directory", "/first", func(b *testing.B, w *Writer, _ int) {
			b.Helper()

			if err := w.RemoveAll("/d"); err != nil {
				b.Fatal(err)
			}
		}},
		{"later names in turn", "/first", func(b *testing.B, w *Writer, n int) {
			b.Helper()

			for i := range n {
				if err := w.Remove("/d/l" + strconv.Itoa(i)); err != nil {
					b.Fatal(err)
				}
			}
		}},
		{"every name with its directory", "/d/first", func(b *testing.B, w *Writer, _ int) {
			b.Helper()

			if err := w.RemoveAll("/d"); err != nil {
				b.Fatal(err)
			}
		}},
	} {
		for _, n := range []int{20000, 80000} {
			b.Run(shape.name+"/"+strconv.Itoa(n), func(b *testing.B) {
				b.ReportAllocs()

				for range b.N {
					b.StopTimer()

					w := Create(&seekBuf{}, WithBuildTime(1000, 0))

					f, err := w.Create(shape.first)
					if err != nil {
						b.Fatal(err)
					}

					if err = f.Close(); err != nil {
						b.Fatal(err)
					}

					for i := range n {
						if err = w.Link(shape.first, "/d/l"+strconv.Itoa(i)); err != nil {
							b.Fatal(err)
						}
					}

					b.StartTimer()

					shape.remove(b, w, n)
				}
			})
		}
	}
}
