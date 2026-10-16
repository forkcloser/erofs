package corpus_test

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/forkcloser/erofs/internal/corpus"
	"github.com/forkcloser/erofs/internal/erofsspec"
)

// regenerate rewrites testdata from this host's mkfs.erofs instead of
// comparing against it: go test ./internal/corpus -regenerate.
//
//nolint:gochecknoglobals // a test flag
var regenerate = flag.Bool("regenerate", false, "rewrite the corpus from mkfs.erofs")

// builderLine prefixes the manifest line naming the mkfs.erofs build the
// committed images came from.
const builderLine = "# builder: "

// TestCommittedIsReproducible: where mkfs.erofs is present, every committed
// mkfs image is rebuilt from the tree under its option set. On every
// builder the rebuilt image is held to its tree through the reference and
// to fsck.erofs; under the builder the manifest names it is held to the
// committed bytes too, and the manifest to every file's digest. Another
// builder's bytes are logged with their first difference, not judged: a
// packing difference, should a build have one, is seen before it is held
// against the tree. A difference under the named builder is a change in
// the tree, the matrix or mkfs.erofs itself, and is settled by
// regenerating, never by skipping.
func TestCommittedIsReproducible(t *testing.T) {
	t.Parallel()

	this := builder(t)
	named := recordedBuilder(t)
	same := *regenerate || this == named

	if !same {
		t.Logf(
			"images built by %q, this mkfs.erofs is %q: holding the rebuilt images to the tree, not to the bytes",
			named,
			this,
		)
	}

	matrix := corpus.Matrix()

	var manifest strings.Builder

	fmt.Fprint(&manifest, "# Images of corpus.CommittedSets, sha256 per file.\n")
	fmt.Fprint(&manifest, "# Regenerate: go test ./internal/corpus -regenerate (mkfs.erofs on PATH).\n")
	fmt.Fprintf(&manifest, "%s%s\n", builderLine, this)

	for _, set := range corpus.CommittedSets() {
		im := rebuild(t, set, matrix)
		anchor(t, set, im)

		for _, file := range sortedKeys(files(im)) {
			b := files(im)[file]
			fmt.Fprintf(&manifest, "%s  %s  %d\n", corpus.Digest(b), file, len(b))

			if same {
				checkOrWrite(t, file, b)
			} else {
				noteDifference(t, file, b)
			}
		}
	}

	if same {
		checkOrWrite(t, "manifest.txt", []byte(manifest.String()))
	}
}

// rebuild is the set's image from this host's tools: mkfs.erofs over the
// tree's tar, or the assembler over the layout.
func rebuild(t *testing.T, set corpus.Set, matrix map[string][]string) corpus.Image {
	t.Helper()

	if set.Layout != nil {
		return corpus.Image{Name: set.Name, Img: corpus.Assemble(*set.Layout)}
	}

	return corpus.Mkfs(t, set.Name, matrix[set.Opts], corpus.Tar(t, set.Tree()))
}

// anchor holds a rebuilt image to what it must mean: fsck.erofs refuses the
// one carrying a state the kernel refuses (no tree) and accepts every
// other, and the reference reads every other back to its tree. The
// committed trees carry no nanoseconds, so whether the builder keeps them
// does not enter.
func anchor(t *testing.T, set corpus.Set, im corpus.Image) {
	t.Helper()

	if set.Tree == nil {
		corpus.FsckRefuses(t, im)

		return
	}

	corpus.Fsck(t, im)

	ref, err := erofsspec.Parse(im.Img, im.Extra())
	if err != nil {
		t.Errorf("%s: reference: %v", im.Name, err)

		return
	}

	corpus.Check(t, im, ref, set.Tree(), false)
}

// builder names this host's mkfs.erofs: its version line and the operating
// system it was built for.
func builder(t *testing.T) string {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), corpus.MkfsPath(t), "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("mkfs.erofs --version: %v\n%s", err, out)
	}

	line, _, _ := strings.Cut(string(out), "\n")

	return strings.TrimSpace(line) + " " + runtime.GOOS
}

// recordedBuilder is the builder the committed manifest names; "" while
// regenerating without one.
func recordedBuilder(t *testing.T) string {
	t.Helper()

	manifest, err := os.ReadFile("testdata/manifest.txt")
	if err != nil && !*regenerate {
		t.Fatal(err)
	}

	for line := range strings.SplitSeq(string(manifest), "\n") {
		if rest, ok := strings.CutPrefix(line, builderLine); ok {
			return rest
		}
	}

	if !*regenerate {
		t.Fatalf("the manifest names no builder (%q line); regenerate with -regenerate", builderLine)
	}

	return ""
}

// noteDifference logs how a file rebuilt by another builder differs from
// the committed one: digests, sizes, and the bytes around the first
// difference on both sides.
func noteDifference(t *testing.T, file string, built []byte) {
	t.Helper()

	committed, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(committed, built) {
		return
	}

	i := 0
	for i < len(committed) && i < len(built) && committed[i] == built[i] {
		i++
	}

	lo := max(0, i-8)

	t.Logf(
		"%s: committed %s (%d bytes), this mkfs.erofs builds %s (%d bytes); first difference at %#x: committed % x, built % x",
		file,
		corpus.Digest(committed),
		len(committed),
		corpus.Digest(built),
		len(built),
		i,
		around(committed, lo),
		around(built, lo),
	)
}

// around is up to 24 bytes of b from lo.
func around(b []byte, lo int) []byte {
	if b == nil {
		return nil
	}

	return b[lo:min(len(b), lo+24)]
}

// TestAssembledIsReproducible: on every lane, each committed assembled
// image is what the assembler writes from its layout, byte for byte.
func TestAssembledIsReproducible(t *testing.T) {
	t.Parallel()

	for _, as := range corpus.Assembled() {
		checkOrWrite(t, as.Name+".erofs", corpus.Assemble(as.Layout))
	}
}

func files(im corpus.Image) map[string][]byte {
	m := map[string][]byte{im.Name + ".erofs": im.Img}
	if im.Blob != nil {
		m[im.Name+".blob"] = im.Blob
	}

	return m
}

func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}

	return keys
}

func checkOrWrite(t *testing.T, file string, want []byte) {
	t.Helper()

	path := filepath.Join("testdata", file)

	if *regenerate {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, want, 0o600); err != nil {
			t.Fatal(err)
		}

		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v (regenerate with -regenerate)", file, err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf(
			"%s: committed %s (%d bytes), mkfs.erofs builds %s (%d bytes); regenerate with -regenerate if the change is intended",
			file,
			corpus.Digest(got),
			len(got),
			corpus.Digest(want),
			len(want),
		)
	}
}

// TestCommittedMatchesManifest: on every lane, the committed files are
// the ones the manifest names, with the digests it records.
func TestCommittedMatchesManifest(t *testing.T) {
	t.Parallel()

	manifest, err := os.ReadFile("testdata/manifest.txt")
	if err != nil {
		t.Fatal(err)
	}

	listed := map[string]string{}

	for line := range strings.SplitSeq(string(manifest), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("manifest line %q", line)
		}

		listed[fields[1]] = fields[0]
	}

	for _, im := range corpus.Committed(t) {
		files := map[string][]byte{im.Name + ".erofs": im.Img}
		if im.Blob != nil {
			files[im.Name+".blob"] = im.Blob
		}

		for file, b := range files {
			if got, want := corpus.Digest(b), listed[file]; got != want {
				t.Errorf("%s: digest %s, manifest says %q", file, got, want)
			}

			delete(listed, file)
		}
	}

	for file := range listed {
		t.Errorf("%s: in the manifest, not loaded", file)
	}
}
