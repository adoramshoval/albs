package multiarch

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// entry is one archive member, described the way the tests care about it.
type entry struct {
	name     string
	typeflag byte
	linkname string
	body     string
}

func file(name, body string) entry { return entry{name: name, typeflag: tar.TypeReg, body: body} }
func dir(name string) entry        { return entry{name: name, typeflag: tar.TypeDir} }
func symlink(name, target string) entry {
	return entry{name: name, typeflag: tar.TypeSymlink, linkname: target}
}

// multiArchEntries mirrors what jam pack emits for a current Paketo component
// buildpack: no bin/ at the root, one tree per platform, and detect/build as
// same-directory symlinks to the single run binary.
func multiArchEntries() []entry {
	e := []entry{file("buildpack.toml", "api = \"0.7\"\n")}
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		e = append(e,
			dir(platform),
			dir(platform+"/bin"),
			file(platform+"/bin/run", "ELF "+platform),
			symlink(platform+"/bin/detect", "run"),
			symlink(platform+"/bin/build", "run"),
			file(platform+"/bin/env", "#!/bin/sh\n"),
		)
	}
	return append(e, file("dependencies/python.tgz", "vendored"))
}

func writeArchive(t *testing.T, path string, entries []entry) {
	t.Helper()

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		name := e.name
		if e.typeflag == tar.TypeDir {
			name += "/"
		}
		hdr := &tar.Header{
			Name:     name,
			Typeflag: e.typeflag,
			Linkname: e.linkname,
			Mode:     0o755,
			Size:     int64(len(e.body)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", e.name, err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
}

func readArchive(t *testing.T, path string) map[string]entry {
	t.Helper()

	got := map[string]entry{}
	err := walk(path, func(hdr *tar.Header) error {
		got[normalize(hdr.Name)] = entry{
			name:     normalize(hdr.Name),
			typeflag: hdr.Typeflag,
			linkname: hdr.Linkname,
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return got
}

func names(m map[string]entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// flatten writes entries to a temporary archive, flattens it and returns what
// came out.
func flatten(t *testing.T, entries []entry, target Target) (map[string]entry, error) {
	t.Helper()

	tmp := t.TempDir()
	src := filepath.Join(tmp, "src.tgz")
	dst := filepath.Join(tmp, "dst.tgz")
	writeArchive(t, src, entries)

	if err := Flatten(src, dst, target); err != nil {
		return nil, err
	}
	return readArchive(t, dst), nil
}

// The bug this package exists for: the lifecycle execs <root>/bin/detect, so a
// component packaged straight out of jam fails detection for every group.
func TestFlattenHoistsTargetBinariesToRoot(t *testing.T) {
	got, err := flatten(t, multiArchEntries(), Target{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatalf("Flatten: %v", err)
	}

	for _, want := range []string{"bin/run", "bin/detect", "bin/build", "bin/env"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s; archive holds %v", want, names(got))
		}
	}
}

// detect and build are symlinks to run in the same directory. Hoisting moves
// that directory whole, so the link target must be left exactly as it was.
func TestFlattenPreservesSymlinks(t *testing.T) {
	got, err := flatten(t, multiArchEntries(), Target{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatalf("Flatten: %v", err)
	}

	for _, name := range []string{"bin/detect", "bin/build"} {
		e := got[name]
		if e.typeflag != tar.TypeSymlink {
			t.Errorf("%s typeflag = %q, want a symlink", name, e.typeflag)
		}
		if e.linkname != "run" {
			t.Errorf("%s -> %q, want %q", name, e.linkname, "run")
		}
	}
}

func TestFlattenSelectsTheRequestedArch(t *testing.T) {
	tmp := t.TempDir()
	src, dst := filepath.Join(tmp, "src.tgz"), filepath.Join(tmp, "dst.tgz")
	writeArchive(t, src, multiArchEntries())

	if err := Flatten(src, dst, Target{OS: "linux", Arch: "arm64"}); err != nil {
		t.Fatalf("Flatten: %v", err)
	}

	got := bodies(t, dst)["bin/run"]
	if len(got) != 1 || got[0] != "ELF linux/arm64" {
		t.Errorf("bin/run = %v, want just the arm64 binary", got)
	}
}

func TestFlattenKeepsNonPlatformFiles(t *testing.T) {
	got, err := flatten(t, multiArchEntries(), Target{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatalf("Flatten: %v", err)
	}

	for _, want := range []string{"buildpack.toml", "dependencies/python.tgz"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %s; archive holds %v", want, names(got))
		}
	}
}

// The unselected platform is dead weight in the .cnb, and leaving the tree
// behind would make it ambiguous which binaries the package is meant to run.
func TestFlattenDropsOtherPlatforms(t *testing.T) {
	got, err := flatten(t, multiArchEntries(), Target{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatalf("Flatten: %v", err)
	}

	for name := range got {
		if strings.HasPrefix(name, "linux/") {
			t.Errorf("%s survived; archive holds %v", name, names(got))
		}
	}
}

// Older single-arch buildpacks already have what the lifecycle wants, so
// Flatten has to be a no-op rather than an error.
func TestFlattenPassesThroughAlreadyFlatArchives(t *testing.T) {
	entries := []entry{
		file("buildpack.toml", "api = \"0.7\"\n"),
		dir("bin"),
		file("bin/run", "ELF"),
		symlink("bin/detect", "run"),
	}

	got, err := flatten(t, entries, Target{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatalf("Flatten: %v", err)
	}

	if _, ok := got["bin/detect"]; !ok {
		t.Errorf("bin/detect was lost; archive holds %v", names(got))
	}
	if got["bin/detect"].linkname != "run" {
		t.Errorf("bin/detect -> %q, want %q", got["bin/detect"].linkname, "run")
	}
}

// Silently copying through would push the failure all the way out to a real
// build, which is exactly the failure mode this package was written to remove.
func TestFlattenRejectsAnUnavailablePlatform(t *testing.T) {
	_, err := flatten(t, multiArchEntries(), Target{OS: "linux", Arch: "s390x"})
	if err == nil {
		t.Fatal("expected an error for a platform the archive does not ship")
	}
	for _, want := range []string{"linux/s390x", "linux/amd64", "linux/arm64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// pack copies buildpack.toml into the platform folder before packaging, so an
// archive built that way carries both. The platform copy is the specific one.
func TestFlattenPrefersTheHoistedCopyOnCollision(t *testing.T) {
	entries := []entry{
		file("buildpack.toml", "root"),
		dir("linux/amd64"),
		file("linux/amd64/buildpack.toml", "platform"),
		dir("linux/amd64/bin"),
		file("linux/amd64/bin/run", "ELF"),
	}

	tmp := t.TempDir()
	src, dst := filepath.Join(tmp, "src.tgz"), filepath.Join(tmp, "dst.tgz")
	writeArchive(t, src, entries)
	if err := Flatten(src, dst, Target{OS: "linux", Arch: "amd64"}); err != nil {
		t.Fatalf("Flatten: %v", err)
	}

	bodies := bodies(t, dst)
	if len(bodies["buildpack.toml"]) != 1 {
		t.Fatalf("buildpack.toml appears %d times, want 1", len(bodies["buildpack.toml"]))
	}
	if got := bodies["buildpack.toml"][0]; got != "platform" {
		t.Errorf("buildpack.toml = %q, want the platform copy", got)
	}
}

func TestFlattenRejectsANonArchive(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src.tgz")
	if err := os.WriteFile(src, []byte("not a gzip stream"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := Flatten(src, filepath.Join(tmp, "dst.tgz"), Target{OS: "linux", Arch: "amd64"}); err == nil {
		t.Fatal("expected an error for a file that is not a gzipped tar")
	}
}

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in      string
		want    Target
		wantErr bool
	}{
		{in: "linux/amd64", want: Target{OS: "linux", Arch: "amd64"}},
		{in: "linux/arm64", want: Target{OS: "linux", Arch: "arm64"}},
		{in: "linux", wantErr: true},
		{in: "linux/", wantErr: true},
		{in: "/amd64", wantErr: true},
		{in: "linux/arm64/v8", wantErr: true},
		{in: "", wantErr: true},
	}

	for _, tt := range tests {
		got, err := ParseTarget(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseTarget(%q) = %v, want an error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseTarget(%q) = %v, want %v", tt.in, got, tt.want)
		}
		if got.String() != tt.in {
			t.Errorf("String() = %q, want %q", got.String(), tt.in)
		}
	}
}

// bodies reads every regular file in an archive, keyed by name, keeping
// duplicates so that collision handling can be asserted.
func bodies(t *testing.T, path string) map[string][]string {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gr.Close()

	out := map[string][]string{}
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		buf, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", hdr.Name, err)
		}
		name := normalize(hdr.Name)
		out[name] = append(out[name], string(buf))
	}
	return out
}

// An already-flat archive would be copied byte for byte to no effect. At the
// several gigabytes an offline component archive reaches, that is the single
// most expensive thing this package could do, so it links instead.
func TestFlattenLinksRatherThanCopyingAFlatArchive(t *testing.T) {
	entries := []entry{
		file("buildpack.toml", "api = \"0.7\"\n"),
		dir("bin"),
		file("bin/run", "ELF"),
		symlink("bin/detect", "run"),
	}

	tmp := t.TempDir()
	src, dst := filepath.Join(tmp, "src.tgz"), filepath.Join(tmp, "dst.tgz")
	writeArchive(t, src, entries)

	if err := Flatten(src, dst, Target{OS: "linux", Arch: "amd64"}); err != nil {
		t.Fatalf("Flatten: %v", err)
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if !os.SameFile(srcInfo, dstInfo) {
		t.Error("a flat archive was copied instead of linked")
	}
}

// A rewritten archive is genuinely new content and must not alias its source.
func TestFlattenRewriteDoesNotAliasTheSource(t *testing.T) {
	tmp := t.TempDir()
	src, dst := filepath.Join(tmp, "src.tgz"), filepath.Join(tmp, "dst.tgz")
	writeArchive(t, src, multiArchEntries())

	if err := Flatten(src, dst, Target{OS: "linux", Arch: "amd64"}); err != nil {
		t.Fatalf("Flatten: %v", err)
	}

	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if os.SameFile(srcInfo, dstInfo) {
		t.Error("a rewritten archive aliases its source")
	}
}
