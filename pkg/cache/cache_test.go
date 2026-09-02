package cache

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeFile(t *testing.T, path, contents string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestPutThenGet(t *testing.T) {
	c, err := NewDiskCache(t.TempDir(), "jam 2.18.0")
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}

	src := writeFile(t, filepath.Join(t.TempDir(), "dep.tgz"), "archive-bytes")
	if err := c.Put("https://github.com/paketo-buildpacks/cpython@refs/tags/v1.18.42", src); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, found := c.Get("https://github.com/paketo-buildpacks/cpython@refs/tags/v1.18.42")
	if !found {
		t.Fatal("Get: entry not found after Put")
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read cached: %v", err)
	}
	if string(data) != "archive-bytes" {
		t.Errorf("cached contents = %q, want %q", data, "archive-bytes")
	}
}

// A jam upgrade must not silently reuse archives built by the previous one.
func TestSaltSeparatesEntries(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, filepath.Join(t.TempDir(), "dep.tgz"), "built-by-old-jam")

	old, err := NewDiskCache(dir, "jam 2.17.0")
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	if err := old.Put("repo@ref", src); err != nil {
		t.Fatalf("Put: %v", err)
	}

	upgraded, err := NewDiskCache(dir, "jam 2.18.0")
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	if path, found := upgraded.Get("repo@ref"); found {
		t.Errorf("Get returned %q; a different packer version must miss", path)
	}
	if _, found := old.Get("repo@ref"); !found {
		t.Error("the original entry should still be reachable under its own salt")
	}
}

// A truncated entry left by an interrupted run must not be served as valid.
func TestGetRejectsEmptyEntry(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskCache(dir, "salt")
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}

	impl := c.(*DiskCache)
	writeFile(t, impl.pathFor("repo@ref"), "")

	if path, found := c.Get("repo@ref"); found {
		t.Errorf("Get returned %q for a zero-length entry", path)
	}
}

func TestPutLeavesNoTempFilesBehind(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskCache(dir, "salt")
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}

	src := writeFile(t, filepath.Join(t.TempDir(), "dep.tgz"), "bytes")
	if err := c.Put("repo@ref", src); err != nil {
		t.Fatalf("Put: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("temporary file %q left behind", e.Name())
		}
	}
}

// Concurrent writers of one key must never yield a torn archive: with a plain
// write the readers below could observe a partial file.
func TestConcurrentPutIsAtomic(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDiskCache(dir, "salt")
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}

	const contents = "0123456789abcdef0123456789abcdef"
	srcDir := t.TempDir()
	var sources []string
	for i := 0; i < 8; i++ {
		sources = append(sources, writeFile(t, filepath.Join(srcDir, string(rune('a'+i))+".tgz"), contents))
	}

	var wg sync.WaitGroup
	for _, src := range sources {
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			if err := c.Put("repo@ref", src); err != nil {
				t.Errorf("Put: %v", err)
			}
		}(src)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, found := c.Get("repo@ref")
			if !found {
				return
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return
			}
			if string(data) != contents {
				t.Errorf("observed a torn cache entry: %q", data)
			}
		}()
	}
	wg.Wait()
}
