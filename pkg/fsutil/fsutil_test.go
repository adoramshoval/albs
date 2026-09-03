package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, contents string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func sameInode(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		t.Fatalf("stat %s: %v", a, err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		t.Fatalf("stat %s: %v", b, err)
	}
	return os.SameFile(fa, fb)
}

// The whole point: an offline component archive runs to gigabytes, and within
// one filesystem publishing it must not move any of them.
func TestLinkOrCopyLinksWithinAFilesystem(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, filepath.Join(dir, "src.tgz"), "archive")
	dst := filepath.Join(dir, "dst.tgz")

	if err := LinkOrCopy(src, dst); err != nil {
		t.Fatalf("LinkOrCopy: %v", err)
	}
	if !sameInode(t, src, dst) {
		t.Error("LinkOrCopy copied within a single filesystem instead of linking")
	}
}

func TestCopyDuplicatesContents(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, filepath.Join(dir, "src.tgz"), "archive")
	dst := filepath.Join(dir, "dst.tgz")

	if err := Copy(src, dst); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if sameInode(t, src, dst) {
		t.Error("Copy produced a link rather than an independent file")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "archive" {
		t.Errorf("dst = %q, want %q", got, "archive")
	}
}

// Copy has to survive contents larger than its buffer, which is the only case
// that actually exercises the loop.
func TestCopyHandlesMultipleBuffers(t *testing.T) {
	dir := t.TempDir()
	contents := strings.Repeat("abcdefgh", (bufSize/8)*3+7)
	src := writeFile(t, filepath.Join(dir, "src.tgz"), contents)
	dst := filepath.Join(dir, "dst.tgz")

	if err := Copy(src, dst); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != contents {
		t.Errorf("copied %d bytes, want %d", len(got), len(contents))
	}
}

// Silently copying on any link failure would turn a real fault -- a typo in a
// path, a name already taken -- into a slow success, which is exactly the class
// of bug this package exists to avoid.
func TestLinkOrCopyReportsRealFailures(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, filepath.Join(dir, "src.tgz"), "archive")

	t.Run("destination exists", func(t *testing.T) {
		dst := writeFile(t, filepath.Join(dir, "taken.tgz"), "other")
		if err := LinkOrCopy(src, dst); err == nil {
			t.Fatal("expected an error when the destination already exists")
		}
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != "other" {
			t.Errorf("destination was overwritten: %q", got)
		}
	})

	t.Run("source missing", func(t *testing.T) {
		err := LinkOrCopy(filepath.Join(dir, "absent.tgz"), filepath.Join(dir, "out.tgz"))
		if err == nil {
			t.Fatal("expected an error when the source does not exist")
		}
	})
}

// A failed copy must not leave a truncated archive where a later run would take
// it for a complete one.
func TestCopyRemovesPartialOutput(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "dst.tgz")

	// A directory reads as a source that opens but fails partway.
	srcDir := filepath.Join(dir, "adirectory")
	if err := os.Mkdir(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := Copy(srcDir, dst); err == nil {
		t.Fatal("expected an error copying a directory")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("partial output left at %s (stat err = %v)", dst, err)
	}
}
