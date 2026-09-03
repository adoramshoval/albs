package builder

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/adoramshoval/albs/pkg/multiarch"
	"github.com/adoramshoval/albs/pkg/testsupport"
	"github.com/buildpacks/pack/pkg/client"
	"github.com/buildpacks/pack/pkg/dist"
)

const metaPackageTOML = `[buildpack]
  uri = "build/buildpack.tgz"

[[dependencies]]
  uri = "docker://docker.io/paketobuildpacks/cpython:1.18.42"

[[dependencies]]
  uri = "docker://docker.io/paketobuildpacks/pip:0.29.2"
`

type fakeCloner struct {
	mu sync.Mutex
	// packageTOML is written into the first clone target, standing in for the
	// meta-buildpack repository.
	packageTOML string
	// noPackageTOML simulates a repository that has no package.toml.
	noPackageTOML bool
	refs          map[string]string
	resolveErr    error
	cloneErr      error

	resolved []string
	cloned   []string
	cloneNum int
}

func (f *fakeCloner) ResolveRef(_ context.Context, repoURL, version string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, repoURL+"@"+version)
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	if ref, ok := f.refs[version]; ok {
		return ref, nil
	}
	if version == "" {
		return "", nil
	}
	if strings.HasPrefix(version, "v") {
		return "refs/tags/" + version, nil
	}
	return "refs/tags/v" + version, nil
}

func (f *fakeCloner) Clone(_ context.Context, repoURL, ref, targetDir string) error {
	f.mu.Lock()
	first := f.cloneNum == 0
	f.cloneNum++
	f.cloned = append(f.cloned, repoURL+"@"+ref)
	f.mu.Unlock()

	if f.cloneErr != nil {
		return f.cloneErr
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return err
	}
	if first {
		if f.noPackageTOML {
			return nil
		}
		return os.WriteFile(filepath.Join(targetDir, "package.toml"), []byte(f.packageTOML), 0o644)
	}
	return os.WriteFile(filepath.Join(targetDir, "buildpack.toml"), []byte("api = \"0.7\"\n"), 0o644)
}

type fakeResolver struct{ err error }

func (f fakeResolver) GetSourceURL(_ context.Context, imageURI string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	name := imageURI
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name, _, _ = strings.Cut(name, ":")
	return "https://github.com/paketo-buildpacks/" + name, nil
}

type packCall struct {
	srcDir  string
	version string
	output  string
}

type fakeJam struct {
	mu    sync.Mutex
	calls []packCall
	err   error
}

func (f *fakeJam) PackOffline(_ context.Context, srcDir, version, outputTgzPath string) error {
	f.mu.Lock()
	f.calls = append(f.calls, packCall{srcDir: srcDir, version: version, output: outputTgzPath})
	f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	return writeMultiArchArchive(outputTgzPath, version)
}

// writeMultiArchArchive mirrors what jam pack emits for a component buildpack:
// no bin/ at the root, one bin/ per platform, and detect as a same-directory
// symlink to run.
func writeMultiArchArchive(path, version string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	body := "tgz:" + version
	if err := tw.WriteHeader(&tar.Header{
		Name: "buildpack.toml", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
	}); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		return err
	}

	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		run := "ELF " + platform + " " + version
		if err := tw.WriteHeader(&tar.Header{
			Name: platform + "/bin/", Typeflag: tar.TypeDir, Mode: 0o755,
		}); err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: platform + "/bin/run", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(run)),
		}); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(run)); err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: platform + "/bin/detect", Typeflag: tar.TypeSymlink, Linkname: "run", Mode: 0o755,
		}); err != nil {
			return err
		}
	}

	if err := tw.Close(); err != nil {
		return err
	}
	return gw.Close()
}

// archiveEntries lists an archive's members, and the body of each regular file.
func archiveEntries(t *testing.T, path string) map[string]string {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip %s: %v", path, err)
	}
	defer gr.Close()

	out := map[string]string{}
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		if hdr.Typeflag == tar.TypeSymlink {
			out[name] = "-> " + hdr.Linkname
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s from %s: %v", name, path, err)
		}
		out[name] = string(body)
	}
	return out
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type fakePack struct {
	opts client.PackageBuildpackOptions
	err  error

	// BuildOffline removes its workspace on return, so anything the tests need
	// to assert about it has to be captured while the call is in flight.
	packageTOML  string
	vendoredDeps map[string]map[string]string
}

func (f *fakePack) PackageBuildpack(_ context.Context, opts client.PackageBuildpackOptions) error {
	f.opts = opts

	if data, err := os.ReadFile(filepath.Join(opts.RelativeBaseDir, "package.toml")); err == nil {
		f.packageTOML = string(data)
	}
	f.vendoredDeps = map[string]map[string]string{}
	entries, err := os.ReadDir(filepath.Join(opts.RelativeBaseDir, "deps"))
	if err == nil {
		for _, e := range entries {
			f.vendoredDeps[e.Name()] = f.readDep(filepath.Join(opts.RelativeBaseDir, "deps", e.Name()))
		}
	}
	return f.err
}

// readDep is deliberately lenient: PackageBuildpack is also called in tests
// where the archives never got written.
func (f *fakePack) readDep(path string) map[string]string {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	gr, err := gzip.NewReader(file)
	if err != nil {
		return nil
	}
	defer gr.Close()

	out := map[string]string{}
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		if hdr.Typeflag == tar.TypeSymlink {
			out[name] = "-> " + hdr.Linkname
			continue
		}
		body, _ := io.ReadAll(tr)
		out[name] = string(body)
	}
	return out
}

type fakeCache struct {
	mu sync.Mutex
	// dir stands in for the cache directory on disk. Entries have to be copied
	// into it, as DiskCache does, because what gets cached lives in a component
	// workspace that BuildOffline deletes before returning.
	dir     string
	entries map[string]string
	gets    []string
	puts    []string
	putErr  error
}

func newFakeCache(t *testing.T) *fakeCache {
	t.Helper()
	return &fakeCache{dir: t.TempDir(), entries: map[string]string{}}
}

func (f *fakeCache) Get(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, key)
	path, ok := f.entries[key]
	return path, ok
}

func (f *fakeCache) Put(key, srcFilePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts = append(f.puts, key)
	if f.putErr != nil {
		return f.putErr
	}

	data, err := os.ReadFile(srcFilePath)
	if err != nil {
		return err
	}
	dest := filepath.Join(f.dir, fmt.Sprintf("entry-%d.tgz", len(f.entries)))
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return err
	}
	f.entries[key] = dest
	return nil
}

type harness struct {
	cloner *fakeCloner
	jam    *fakeJam
	pack   *fakePack
	cache  *fakeCache
	b      *Builder
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		cloner: &fakeCloner{packageTOML: metaPackageTOML},
		jam:    &fakeJam{},
		pack:   &fakePack{},
		cache:  newFakeCache(t),
	}
	h.b = NewBuilder(h.cloner, fakeResolver{}, h.jam, h.pack, h.cache, testsupport.Logger{}, 2,
		multiarch.Target{OS: "linux", Arch: "amd64"})
	return h
}

func TestBuildOfflineRewritesBuildpackURI(t *testing.T) {
	h := newHarness(t)

	out := filepath.Join(t.TempDir(), "out.cnb")
	if err := h.b.BuildOffline(context.Background(), "https://github.com/paketo-buildpacks/python", "2.9.2", out); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}

	// The checked-in build/buildpack.tgz never exists in a fresh clone.
	if got := h.pack.opts.Config.Buildpack.URI; got != "./buildpack.tgz" {
		t.Errorf("Buildpack.URI = %q, want %q", got, "./buildpack.tgz")
	}
}

func TestBuildOfflineRewritesDependenciesInOrder(t *testing.T) {
	h := newHarness(t)

	out := filepath.Join(t.TempDir(), "out.cnb")
	if err := h.b.BuildOffline(context.Background(), "https://github.com/paketo-buildpacks/python", "2.9.2", out); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}

	deps := h.pack.opts.Config.Dependencies
	if len(deps) != 2 {
		t.Fatalf("got %d dependencies, want 2", len(deps))
	}
	for i, want := range []string{"./deps/dep-0.tgz", "./deps/dep-1.tgz"} {
		if deps[i].URI != want {
			t.Errorf("dependency %d URI = %q, want %q", i, deps[i].URI, want)
		}
		if deps[i].ImageName != "" {
			t.Errorf("dependency %d still carries an image name %q", i, deps[i].ImageName)
		}
	}
}

func TestBuildOfflinePassesPackTheCloneRoot(t *testing.T) {
	h := newHarness(t)

	out := filepath.Join(t.TempDir(), "out.cnb")
	if err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", out); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}

	if h.pack.opts.Name != out {
		t.Errorf("Name = %q, want %q", h.pack.opts.Name, out)
	}
	if h.pack.opts.Format != client.FormatFile {
		t.Errorf("Format = %q, want %q", h.pack.opts.Format, client.FormatFile)
	}
	// Relative dependency URIs are resolved against this directory, which is
	// the only reason they need not be rewritten on disk.
	if _, ok := h.pack.vendoredDeps["dep-0.tgz"]; !ok {
		t.Errorf("RelativeBaseDir %q did not contain the vendored deps; saw %v",
			h.pack.opts.RelativeBaseDir, h.pack.vendoredDeps)
	}
}

// The failure this guards against is silent at packaging time: an unflattened
// component produces a .cnb whose every buildpack fails the lifecycle's detect
// phase with "fork/exec .../bin/detect: no such file or directory".
func TestBuildOfflineFlattensDependenciesForTheTarget(t *testing.T) {
	h := newHarness(t)

	out := filepath.Join(t.TempDir(), "out.cnb")
	if err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", out); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}

	for _, dep := range []string{"dep-0.tgz", "dep-1.tgz"} {
		entries := h.pack.vendoredDeps[dep]
		if got := entries["bin/detect"]; got != "-> run" {
			t.Errorf("%s bin/detect = %q, want a symlink to run; archive holds %v",
				dep, got, sortedNames(entries))
		}
		if !strings.HasPrefix(entries["bin/run"], "ELF linux/amd64") {
			t.Errorf("%s bin/run = %q, want the amd64 binary", dep, entries["bin/run"])
		}
		for name := range entries {
			if strings.HasPrefix(name, "linux/") {
				t.Errorf("%s still holds the unflattened %s", dep, name)
			}
		}
	}
}

func TestBuildOfflineHonoursTheTarget(t *testing.T) {
	h := newHarness(t)
	h.b.target = multiarch.Target{OS: "linux", Arch: "arm64"}

	out := filepath.Join(t.TempDir(), "out.cnb")
	if err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", out); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}

	if got := h.pack.vendoredDeps["dep-0.tgz"]["bin/run"]; !strings.HasPrefix(got, "ELF linux/arm64") {
		t.Errorf("bin/run = %q, want the arm64 binary", got)
	}

	// pack stamps the .cnb's image config from this; leaving it unset produces
	// a package that declares no architecture.
	want := []dist.Target{{OS: "linux", Arch: "arm64"}}
	if got := h.pack.opts.Targets; len(got) != 1 || got[0].OS != want[0].OS || got[0].Arch != want[0].Arch {
		t.Errorf("Targets = %v, want %v", got, want)
	}
}

// A target the components do not ship must fail here rather than at build time.
func TestBuildOfflineRejectsAnUnavailableTarget(t *testing.T) {
	h := newHarness(t)
	h.b.target = multiarch.Target{OS: "linux", Arch: "s390x"}

	err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2",
		filepath.Join(t.TempDir(), "out.cnb"))
	if err == nil {
		t.Fatal("expected an error for a platform the components do not ship")
	}
	if !strings.Contains(err.Error(), "linux/s390x") {
		t.Errorf("error = %q, want it to name the missing platform", err)
	}
}

// Marshalling buildpackage.Config back to TOML emits empty image/extension/
// platform keys the original never had, so the file is deliberately left alone.
func TestBuildOfflineLeavesPackageTOMLUntouched(t *testing.T) {
	h := newHarness(t)

	out := filepath.Join(t.TempDir(), "out.cnb")
	if err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", out); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}

	seen := h.pack.packageTOML
	if seen != metaPackageTOML {
		t.Errorf("package.toml was rewritten.\n got: %q\nwant: %q", seen, metaPackageTOML)
	}
	if strings.Contains(seen, "image = ") {
		t.Error("package.toml gained an empty image key")
	}
}

// The bug that broke every component build: the image tag is 1.18.42 but the
// Git tag is v1.18.42.
func TestBuildOfflineResolvesImageTagsToGitRefs(t *testing.T) {
	h := newHarness(t)

	out := filepath.Join(t.TempDir(), "out.cnb")
	if err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", out); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}

	h.cloner.mu.Lock()
	cloned := append([]string(nil), h.cloner.cloned...)
	h.cloner.mu.Unlock()

	want := map[string]bool{
		"https://github.com/paketo-buildpacks/cpython@refs/tags/v1.18.42": false,
		"https://github.com/paketo-buildpacks/pip@refs/tags/v0.29.2":      false,
	}
	for _, c := range cloned {
		if _, ok := want[c]; ok {
			want[c] = true
		}
	}
	for c, found := range want {
		if !found {
			t.Errorf("expected a clone of %q; got %v", c, cloned)
		}
	}
}

// Component buildpack.toml files carry no version, so jam must be told one or
// the packaged component cannot satisfy the meta-buildpack's order groups.
func TestBuildOfflineStampsComponentVersions(t *testing.T) {
	h := newHarness(t)

	out := filepath.Join(t.TempDir(), "out.cnb")
	if err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", out); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}

	h.jam.mu.Lock()
	defer h.jam.mu.Unlock()
	got := map[string]bool{}
	for _, c := range h.jam.calls {
		if c.version == "" {
			t.Errorf("jam was called with no version for %q", c.srcDir)
		}
		got[c.version] = true
	}
	for _, want := range []string{"1.18.42", "0.29.2"} {
		if !got[want] {
			t.Errorf("expected jam to be told version %q; got %v", want, got)
		}
	}
}

func TestBuildOfflineUsesCacheInsteadOfRebuilding(t *testing.T) {
	h := newHarness(t)

	cached := filepath.Join(t.TempDir(), "cached.tgz")
	if err := writeMultiArchArchive(cached, "cached-archive"); err != nil {
		t.Fatalf("write: %v", err)
	}
	h.cache.entries["https://github.com/paketo-buildpacks/cpython@refs/tags/v1.18.42"] = cached

	out := filepath.Join(t.TempDir(), "out.cnb")
	if err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", out); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}

	// The meta-buildpack is packed too, so dependencies are counted by the
	// versions only they are stamped with.
	h.jam.mu.Lock()
	var depPacks int
	for _, c := range h.jam.calls {
		if c.version == "1.18.42" || c.version == "0.29.2" {
			depPacks++
		}
	}
	h.jam.mu.Unlock()
	if depPacks != 1 {
		t.Fatalf("jam packed %d dependencies, want 1 (cpython should have come from cache)", depPacks)
	}

	if got := h.pack.vendoredDeps["dep-0.tgz"]["buildpack.toml"]; got != "tgz:cached-archive" {
		t.Errorf("dep-0.tgz buildpack.toml = %q, want it to come from the cached archive", got)
	}
}

// Cache entries hold jam's own multi-arch output, so the same entry has to
// serve any --target rather than being invalidated by one.
func TestBuildOfflineCachesBeforeFlattening(t *testing.T) {
	h := newHarness(t)

	out := filepath.Join(t.TempDir(), "out.cnb")
	if err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", out); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}

	h.cache.mu.Lock()
	defer h.cache.mu.Unlock()
	cached, ok := h.cache.entries["https://github.com/paketo-buildpacks/cpython@refs/tags/v1.18.42"]
	if !ok {
		t.Fatalf("cpython was not cached; cache holds %v", h.cache.puts)
	}

	entries := archiveEntries(t, cached)
	if _, flattened := entries["bin/detect"]; flattened {
		t.Errorf("the cached archive was flattened; it holds %v", sortedNames(entries))
	}
	for _, want := range []string{"linux/amd64/bin/detect", "linux/arm64/bin/detect"} {
		if _, ok := entries[want]; !ok {
			t.Errorf("cached archive is missing %s; it holds %v", want, sortedNames(entries))
		}
	}
}

// A cache write failure is a performance problem, not a correctness one.
func TestBuildOfflineToleratesCacheWriteFailure(t *testing.T) {
	h := newHarness(t)
	h.cache.putErr = errors.New("disk full")

	out := filepath.Join(t.TempDir(), "out.cnb")
	if err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", out); err != nil {
		t.Fatalf("BuildOffline should tolerate cache write failures: %v", err)
	}
}

func TestBuildOfflinePropagatesErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*harness)
		wantErr string
	}{
		{
			name:    "source resolution fails",
			mutate:  func(h *harness) { h.b.resolver = fakeResolver{err: errors.New("no mapping")} },
			wantErr: "no mapping",
		},
		{
			name:    "jam fails",
			mutate:  func(h *harness) { h.jam.err = errors.New("boom") },
			wantErr: "failed jam pack",
		},
		{
			name:    "packaging fails",
			mutate:  func(h *harness) { h.pack.err = errors.New("no daemon") },
			wantErr: "no daemon",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.mutate(h)

			err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", filepath.Join(t.TempDir(), "out.cnb"))
			if err == nil {
				t.Fatalf("expected an error")
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestBuildOfflineFailsWhenPackageTOMLAbsent(t *testing.T) {
	h := newHarness(t)
	h.cloner.noPackageTOML = true

	err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", filepath.Join(t.TempDir(), "out.cnb"))
	if err == nil {
		t.Fatal("expected an error when package.toml is missing")
	}
	if !strings.Contains(err.Error(), "package.toml") {
		t.Errorf("error = %q, want it to mention package.toml", err)
	}
}

func TestBuildOfflineRespectsConcurrencyLimit(t *testing.T) {
	h := newHarness(t)

	var mu sync.Mutex
	var inFlight, peak int
	h.b.jamPacker = &countingJam{inner: h.jam, onEnter: func() {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
	}, onExit: func() {
		mu.Lock()
		inFlight--
		mu.Unlock()
	}}

	if err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", filepath.Join(t.TempDir(), "out.cnb")); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}
	if peak > 2 {
		t.Errorf("peak concurrency = %d, want <= 2", peak)
	}
}

type countingJam struct {
	inner   *fakeJam
	onEnter func()
	onExit  func()
}

func (c *countingJam) PackOffline(ctx context.Context, srcDir, version, out string) error {
	c.onEnter()
	defer c.onExit()
	return c.inner.PackOffline(ctx, srcDir, version, out)
}

// package.toml routinely omits [platform]; without a default, packaging fails
// with "provided image OS ” must be either 'freebsd', 'linux' or 'windows'".
func TestBuildOfflineDefaultsPlatformOS(t *testing.T) {
	h := newHarness(t)

	if err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", filepath.Join(t.TempDir(), "out.cnb")); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}
	if got := h.pack.opts.Config.Platform.OS; got != "linux" {
		t.Errorf("Platform.OS = %q, want %q", got, "linux")
	}
}

func TestBuildOfflineHonoursExplicitPlatformOS(t *testing.T) {
	h := newHarness(t)
	h.cloner.packageTOML = metaPackageTOML + "\n[platform]\n  os = \"windows\"\n"

	if err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", filepath.Join(t.TempDir(), "out.cnb")); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}
	if got := h.pack.opts.Config.Platform.OS; got != "windows" {
		t.Errorf("Platform.OS = %q, want %q", got, "windows")
	}
}

func TestBuildOfflineRejectsDependencyWithBothURIAndImage(t *testing.T) {
	h := newHarness(t)
	h.cloner.packageTOML = "[buildpack]\n  uri = \".\"\n\n[[dependencies]]\n  uri = \"docker://x/y:1\"\n  image = \"x/y:1\"\n"

	err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", filepath.Join(t.TempDir(), "out.cnb"))
	if err == nil || !strings.Contains(err.Error(), "both uri and image") {
		t.Fatalf("error = %v, want a complaint about uri and image", err)
	}
}

// buildpack.toml carries no version, and pack refuses to package without one,
// so the meta-buildpack must be run through jam like its components are.
func TestBuildOfflinePackagesTheMetaBuildpackItself(t *testing.T) {
	h := newHarness(t)

	if err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "v2.9.2", filepath.Join(t.TempDir(), "out.cnb")); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}

	h.jam.mu.Lock()
	defer h.jam.mu.Unlock()

	var meta *packCall
	for i := range h.jam.calls {
		if strings.HasSuffix(h.jam.calls[i].output, "buildpack.tgz") && !strings.Contains(h.jam.calls[i].output, "deps") {
			meta = &h.jam.calls[i]
		}
	}
	if meta == nil {
		t.Fatalf("jam was never asked to package the meta-buildpack; calls: %+v", h.jam.calls)
	}
	// The tag is the version upstream's own packaging script would pass.
	if meta.version != "2.9.2" {
		t.Errorf("meta-buildpack version = %q, want %q", meta.version, "2.9.2")
	}
}

// Without a tag there is no version to stamp, and the failure should say so
// rather than surfacing as pack's "buildpack.version is required".
func TestBuildOfflineRequiresATagForTheVersion(t *testing.T) {
	h := newHarness(t)

	err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "", filepath.Join(t.TempDir(), "out.cnb"))
	if err == nil {
		t.Fatal("expected an error when no tag is given")
	}
	if !strings.Contains(err.Error(), "--tag") {
		t.Errorf("error = %q, want it to point at --tag", err)
	}
}

func TestVersionFromRef(t *testing.T) {
	for _, tt := range []struct {
		ref     string
		want    string
		wantErr bool
	}{
		{ref: "refs/tags/v2.9.2", want: "2.9.2"},
		{ref: "refs/tags/2.9.2", want: "2.9.2"},
		{ref: "", wantErr: true},
		{ref: "refs/heads/main", wantErr: true},
		{ref: "39c6dad2b4e70e6defc612d4ad91e63da644f786", wantErr: true},
	} {
		got, err := versionFromRef(tt.ref)
		if tt.wantErr {
			if err == nil {
				t.Errorf("versionFromRef(%q) = %q, want error", tt.ref, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("versionFromRef(%q): %v", tt.ref, err)
		}
		if got != tt.want {
			t.Errorf("versionFromRef(%q) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}
