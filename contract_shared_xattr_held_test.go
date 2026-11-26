package erofs_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"testing"
	"time"

	"github.com/forkcloser/go-erofs"
	"github.com/forkcloser/go-erofs/internal/corpus"
)

// sharing is an image of 64 KiB blocks whose shared xattr area holds the
// entries of area, and whose files "f000".. each name the entry of area
// that names gives them. It counts the reads that start at each entry.
type sharing struct {
	*laidImage

	// entry is where each entry of area starts in the image.
	entry []int64
}

// laidSharing lays the image out. The value of every entry of area is
// found nowhere else in the image.
func laidSharing(t *testing.T, area []corpus.AXattr, names ...int) *sharing {
	t.Helper()

	l := corpus.Layout{BlockBits: 16, Shared: area}

	for i, id := range names {
		l.Entries = append(l.Entries, corpus.AEntry{
			Name: fmt.Sprintf("f%03d", i), Mode: 0o100644, Nlink: 1, SharedIDs: []int{id},
		})
	}

	data := corpus.Assemble(l)
	s := &sharing{laidImage: &laidImage{head: data, size: int64(len(data)), at: map[int64]int64{}}}

	for _, x := range area {
		// The header's four bytes, the name, then the value.
		value := bytes.Index(data, x.Value)
		if value < 0 || len(x.Value) == 0 {
			t.Fatalf("the value of %q is not in the image", x.Suffix)
		}

		at := int64(value - len(x.Suffix) - 4)
		s.entry = append(s.entry, at)
		s.at[at] = 0
	}

	return s
}

// label is the security.selinux of name, and whether it has one.
func label(t *testing.T, fsys fs.FS, name string) (string, bool) {
	t.Helper()

	info, err := fs.Stat(fsys, name)
	if err != nil {
		t.Fatal(err)
	}

	st, ok := info.Sys().(*erofs.Stat)
	if !ok {
		t.Fatalf("Sys() is %T", info.Sys())
	}

	value, has := st.Xattrs["security.selinux"]

	return value, has
}

// selinux is a shared security.selinux entry of that value.
func selinux(value string) corpus.AXattr {
	return corpus.AXattr{Index: 6, Suffix: "selinux", Value: []byte(value)}
}

// TestContractSharedXattrIsReadOnceForAllThatNameIt: an entry of the shared
// area that many inodes name, as the label of a tree is, is read from the
// image once, for the first of them, and every one of them has it.
func TestContractSharedXattrIsReadOnceForAllThatNameIt(t *testing.T) {
	t.Parallel()

	const files = 60

	labels := []string{"system_u:object_r:usr_t:s0", "system_u:object_r:etc_t:s0"}

	names := make([]int, files)
	for i := range names {
		names[i] = i % len(labels)
	}

	src := laidSharing(t, []corpus.AXattr{selinux(labels[0]), selinux(labels[1])}, names...)

	fsys, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		for i := range files {
			if got, _ := label(t, fsys, fmt.Sprintf("f%03d", i)); got != labels[i%len(labels)] {
				t.Fatalf("f%03d: security.selinux is %q, want %q", i, got, labels[i%len(labels)])
			}
		}
	}

	for i, at := range src.entry {
		if src.at[at] != 1 {
			t.Errorf("the entry of %q was read %d times for %d Stats of the files that name it; want once",
				labels[i], src.at[at], files)
		}
	}
}

// TestContractSharedXattrTheKernelDoesNotListIsReadOnce: an entry whose
// name index no mount shows is left out of every inode that names it, and
// is read once to learn it, not for each of them.
func TestContractSharedXattrTheKernelDoesNotListIsReadOnce(t *testing.T) {
	t.Parallel()

	const files = 20

	// Index 5: the format's header calls it lustre, and no handler takes it.
	src := laidSharing(t, []corpus.AXattr{{Index: 5, Suffix: "selinux", Value: []byte("shown by no mount")}},
		make([]int, files)...)

	fsys, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	for i := range files {
		info, err := fs.Stat(fsys, fmt.Sprintf("f%03d", i))
		if err != nil {
			t.Fatal(err)
		}

		if st, ok := info.Sys().(*erofs.Stat); !ok || len(st.Xattrs) != 0 {
			t.Fatalf("f%03d: Sys() is %v; want no attribute", i, info.Sys())
		}
	}

	if got := src.at[src.entry[0]]; got != 1 {
		t.Errorf("the entry was read %d times for %d Stats; want once", got, files)
	}
}

// sixtyThousands is an area of entries security.selinux values of 60000
// bytes, each its own, and the files that name them, one each.
func sixtyThousands(entries int) (area []corpus.AXattr, names []int) {
	area = make([]corpus.AXattr, entries)
	names = make([]int, entries)

	for i := range area {
		area[i] = selinux(string(bytes.Repeat([]byte{byte('a' + i)}, 60000)))
		names[i] = i
	}

	return area, names
}

// labelsOf reads the label of each file of a sixtyThousands image, in order.
func labelsOf(t *testing.T, fsys fs.FS, area []corpus.AXattr) {
	t.Helper()

	for i := range area {
		if got, _ := label(t, fsys, fmt.Sprintf("f%03d", i)); got != string(area[i].Value) {
			t.Fatalf("f%03d: security.selinux is %d bytes, %.10q...; want the entry's value", i, len(got), got)
		}
	}
}

// heldOf is how many entries of a sixtyThousands image a pass over its
// files does not read: those the image kept.
func heldOf(t *testing.T, src *sharing, fsys fs.FS, area []corpus.AXattr) int {
	t.Helper()

	for _, at := range src.entry {
		src.at[at] = 0
	}

	labelsOf(t, fsys, area)

	held := 0

	for _, at := range src.entry {
		if src.at[at] == 0 {
			held++
		}
	}

	return held
}

// TestContractSharedXattrsHeldAreAMebibyte: an image keeps the shared
// entries it read up to a mebibyte of them. Twenty entries of 60000 bytes
// are more: every one reads right, a second time too, and the second pass
// reads again all but the 17 that fit a mebibyte.
func TestContractSharedXattrsHeldAreAMebibyte(t *testing.T) {
	t.Parallel()

	area, names := sixtyThousands(20)
	src := laidSharing(t, area, names...)

	fsys, err := erofs.Open(src)
	if err != nil {
		t.Fatal(err)
	}

	labelsOf(t, fsys, area)

	for i, at := range src.entry {
		if src.at[at] != 1 {
			t.Fatalf("entry %d was read %d times by the first pass; want once", i, src.at[at])
		}
	}

	if held := heldOf(t, src, fsys, area); held != 17 {
		t.Errorf("the second pass found %d of %d entries held; want 17, the most a mebibyte has of them",
			held, len(area))
	}
}

// meeting is a source whose first two reads starting at off wait for each
// other, so that two readers of one entry both find it not yet kept. It
// gives up waiting after a second, for a reader that never comes. Its reads
// of the image are one at a time: laidImage counts them.
type meeting struct {
	img *laidImage
	off int64

	mu      sync.Mutex
	arrived int
	both    chan struct{}
}

func (m *meeting) Size() int64 { return m.img.size }

func (m *meeting) ReadAt(p []byte, off int64) (int, error) {
	m.mu.Lock()

	wait := false

	if off == m.off && m.arrived < 2 {
		m.arrived++
		wait = true

		if m.arrived == 2 {
			close(m.both)
		}
	}

	m.mu.Unlock()

	if wait {
		select {
		case <-m.both:
		case <-time.After(time.Second):
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.img.ReadAt(p, off)
}

// TestContractSharedXattrReadByTwoAtOnceCountsOnce: two Stats that read
// one entry at the same time, neither finding it kept, leave it kept once:
// the mebibyte still has room for the 17 entries it has room for.
func TestContractSharedXattrReadByTwoAtOnceCountsOnce(t *testing.T) {
	t.Parallel()

	area, names := sixtyThousands(20)
	src := laidSharing(t, area, names...)
	met := &meeting{img: src.laidImage, off: src.entry[0], both: make(chan struct{})}

	fsys, err := erofs.Open(met)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	for range 2 {
		wg.Go(func() {
			if _, err := fs.Stat(fsys, "f000"); err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()

	if got := src.at[src.entry[0]]; got != 2 {
		t.Fatalf("the two Stats read the entry %d times; the test wants them to meet there, 2", got)
	}

	labelsOf(t, fsys, area)

	if held := heldOf(t, src, fsys, area); held != 17 {
		t.Errorf("%d of %d entries are held after two Stats read the first at once; want 17", held, len(area))
	}
}

// errOnce is the read a onceFailing source fails.
var errOnce = errors.New("onceFailing: the read it fails")

// onceFailing is a source whose first read starting at off fails.
type onceFailing struct {
	io.ReaderAt

	off    int64
	failed bool
}

func (o *onceFailing) ReadAt(p []byte, off int64) (int, error) {
	if off == o.off && !o.failed {
		o.failed = true

		return 0, errOnce
	}

	return o.ReaderAt.ReadAt(p, off)
}

// TestContractSharedXattrThatFailedToReadIsReadAgain: a read of a shared
// entry that fails is the failure of that Stat, with the source's error,
// and of no other: the next one reads the entry.
func TestContractSharedXattrThatFailedToReadIsReadAgain(t *testing.T) {
	t.Parallel()

	const want = "system_u:object_r:usr_t:s0"

	src := laidSharing(t, []corpus.AXattr{selinux(want)}, 0, 0)

	fsys, err := erofs.Open(&onceFailing{ReaderAt: src, off: src.entry[0]})
	if err != nil {
		t.Fatal(err)
	}

	if _, err = fs.Stat(fsys, "f000"); !errors.Is(err, errOnce) {
		t.Fatalf("Stat over a failing read of the entry: %v; want the source's error", err)
	}

	for _, name := range []string{"f000", "f001"} {
		if got, _ := label(t, fsys, name); got != want {
			t.Errorf("%s after the failed read: security.selinux is %q, want %q", name, got, want)
		}
	}
}

// TestContractSharedXattrsAreReadByManyAtOnce: the entries an image keeps
// are there for every goroutine that reads it: each Stat, of many at once,
// has its file's own label.
func TestContractSharedXattrsAreReadByManyAtOnce(t *testing.T) {
	t.Parallel()

	const (
		files   = 64
		readers = 8
	)

	area := make([]corpus.AXattr, 8)
	for i := range area {
		area[i] = selinux(fmt.Sprintf("system_u:object_r:type%d_t:s0", i))
	}

	names := make([]int, files)
	for i := range names {
		names[i] = i % len(area)
	}

	// The image's bytes alone: laidImage counts its reads, one reader at a time.
	fsys, err := erofs.Open(bytes.NewReader(laidSharing(t, area, names...).whole()))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	for r := range readers {
		wg.Go(func() {
			for i := range files {
				// Each reader starts elsewhere, so that the first reads of an entry meet.
				n := (i + r*files/readers) % files

				info, err := fs.Stat(fsys, fmt.Sprintf("f%03d", n))
				if err != nil {
					t.Errorf("f%03d: %v", n, err)

					return
				}

				st, ok := info.Sys().(*erofs.Stat)
				if !ok || st.Xattrs["security.selinux"] != string(area[n%len(area)].Value) {
					t.Errorf("f%03d: Sys() is %v; want the label %q", n, info.Sys(), area[n%len(area)].Value)

					return
				}
			}
		})
	}

	wg.Wait()
}
