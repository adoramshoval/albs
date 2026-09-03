package preflight

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

const sampleTOML = `api = "0.7"

[buildpack]
  id = "paketo-buildpacks/cpython"
  version = "1.8.7"

[[order]]
  [[order.group]]
    id = "paketo-buildpacks/cpython"
  [[order.group]]
    id = "paketo-buildpacks/pip"
    optional = true

[[metadata.dependencies]]
  id = "python"
  version = "3.10.20"
  arch = "amd64"
  stacks = ["io.buildpacks.stacks.jammy"]
  uri = "https://artifacts.paketo.io/python/python_3.10.20_linux_amd64_jammy.tgz"
  source = "https://www.python.org/ftp/python/3.10.20/Python-3.10.20.tgz"

[[metadata.dependencies]]
  id = "python"
  version = "3.10.20"
  stacks = ["*"]
  uri = "https://www.python.org/ftp/python/3.10.20/Python-3.10.20.tgz"
  source = "https://www.python.org/ftp/python/3.10.20/Python-3.10.20.tgz"
`

func check(t *testing.T, bp BuildpackTOML) {
	t.Helper()

	if bp.API != "0.7" {
		t.Errorf("API = %q, want 0.7", bp.API)
	}
	if bp.Buildpack.ID != "paketo-buildpacks/cpython" {
		t.Errorf("Buildpack.ID = %q", bp.Buildpack.ID)
	}
	if len(bp.Order) != 1 || len(bp.Order[0].Group) != 2 {
		t.Fatalf("Order = %+v, want one group of two", bp.Order)
	}
	if bp.Order[0].Group[0].Optional || !bp.Order[0].Group[1].Optional {
		t.Errorf("optional flags = %v, %v; want false, true",
			bp.Order[0].Group[0].Optional, bp.Order[0].Group[1].Optional)
	}
	if len(bp.Metadata.Dependencies) != 2 {
		t.Fatalf("got %d dependencies, want 2", len(bp.Metadata.Dependencies))
	}
	if got := bp.Metadata.Dependencies[0]; got.Arch != "amd64" || len(got.Stacks) != 1 {
		t.Errorf("first dependency = %+v, want amd64 on one stack", got)
	}
	// The source tarball carries no arch, which must survive as empty rather
	// than being invented.
	if got := bp.Metadata.Dependencies[1]; got.Arch != "" || got.Stacks[0] != "*" {
		t.Errorf("second dependency = %+v, want no arch and a wildcard stack", got)
	}

	// uri and source decide whether an artifact is compiled, so both have to
	// survive the decode: the first entry is a prebuilt binary, the second is
	// the source tarball named as its own artifact.
	if got := classify(bp.Metadata.Dependencies[0]); got != ArtifactPrebuilt {
		t.Errorf("the jammy artifact classified as %q, want %q", got, ArtifactPrebuilt)
	}
	if got := classify(bp.Metadata.Dependencies[1]); got != ArtifactSource {
		t.Errorf("the python.org tarball is its own source; classified as %q, want %q", got, ArtifactSource)
	}
}

func TestParseDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "buildpack.toml"), []byte(sampleTOML), 0o644); err != nil {
		t.Fatal(err)
	}

	bp, err := ParseDir(dir)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}
	check(t, bp)
}

func TestParseDirMissing(t *testing.T) {
	if _, err := ParseDir(t.TempDir()); err == nil {
		t.Fatal("ParseDir on a directory with no buildpack.toml returned no error")
	}
}

// TestParseArchive covers the cache-hit path, where the component is never
// cloned and the only copy of buildpack.toml is inside jam's output.
func TestParseArchive(t *testing.T) {
	for _, name := range []string{"buildpack.toml", "./buildpack.toml"} {
		path := filepath.Join(t.TempDir(), "packed.tgz")
		writeArchive(t, path, map[string]string{
			name:                  sampleTOML,
			"linux/amd64/bin/run": "ELF",
		})

		bp, err := ParseArchive(path)
		if err != nil {
			t.Fatalf("ParseArchive with entry %q: %v", name, err)
		}
		check(t, bp)
	}
}

func TestParseArchiveIgnoresNestedBuildpackTOML(t *testing.T) {
	// Only the root file describes the buildpack; a nested one belongs to
	// something the archive merely carries.
	path := filepath.Join(t.TempDir(), "packed.tgz")
	writeArchive(t, path, map[string]string{
		"deps/other/buildpack.toml": "api = \"0.1\"\n",
		"buildpack.toml":            sampleTOML,
	})

	bp, err := ParseArchive(path)
	if err != nil {
		t.Fatalf("ParseArchive: %v", err)
	}
	if bp.API != "0.7" {
		t.Errorf("API = %q, want the root buildpack.toml's 0.7", bp.API)
	}
}

func TestParseArchiveWithoutBuildpackTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packed.tgz")
	writeArchive(t, path, map[string]string{"linux/amd64/bin/run": "ELF"})

	if _, err := ParseArchive(path); err == nil {
		t.Fatal("ParseArchive on an archive with no buildpack.toml returned no error")
	}
}

func writeArchive(t *testing.T, path string, entries map[string]string) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	// Written in a fixed order so the nested-file case is deterministic:
	// the nested entry has to precede the root one to be a real test.
	names := make([]string, 0, len(entries))
	for n := range entries {
		if n != "buildpack.toml" {
			names = append(names, n)
		}
	}
	if _, ok := entries["buildpack.toml"]; ok {
		names = append(names, "buildpack.toml")
	}

	for _, n := range names {
		body := entries[n]
		if err := tw.WriteHeader(&tar.Header{
			Name: n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
}
