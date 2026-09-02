package builder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/adoramshoval/albs/pkg/testsupport"
	"github.com/buildpacks/pack/pkg/client"
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
	return os.WriteFile(outputTgzPath, []byte("tgz:"+version), 0o644)
}

type fakePack struct {
	opts client.PackageBuildpackOptions
	err  error

	// BuildOffline removes its workspace on return, so anything the tests need
	// to assert about it has to be captured while the call is in flight.
	packageTOML  string
	vendoredDeps map[string]string
}

func (f *fakePack) PackageBuildpack(_ context.Context, opts client.PackageBuildpackOptions) error {
	f.opts = opts

	if data, err := os.ReadFile(filepath.Join(opts.RelativeBaseDir, "package.toml")); err == nil {
		f.packageTOML = string(data)
	}
	f.vendoredDeps = map[string]string{}
	entries, err := os.ReadDir(filepath.Join(opts.RelativeBaseDir, "deps"))
	if err == nil {
		for _, e := range entries {
			data, err := os.ReadFile(filepath.Join(opts.RelativeBaseDir, "deps", e.Name()))
			if err == nil {
				f.vendoredDeps[e.Name()] = string(data)
			}
		}
	}
	return f.err
}

type fakeCache struct {
	mu      sync.Mutex
	entries map[string]string
	gets    []string
	puts    []string
	putErr  error
}

func newFakeCache() *fakeCache { return &fakeCache{entries: map[string]string{}} }

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
	f.entries[key] = srcFilePath
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
		cache:  newFakeCache(),
	}
	h.b = NewBuilder(h.cloner, fakeResolver{}, h.jam, h.pack, h.cache, testsupport.Logger{}, 2)
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
	if err := os.WriteFile(cached, []byte("cached-archive"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	h.cache.entries["https://github.com/paketo-buildpacks/cpython@refs/tags/v1.18.42"] = cached

	out := filepath.Join(t.TempDir(), "out.cnb")
	if err := h.b.BuildOffline(context.Background(), "https://example.com/meta", "2.9.2", out); err != nil {
		t.Fatalf("BuildOffline: %v", err)
	}

	h.jam.mu.Lock()
	var depPacks int
	for _, c := range h.jam.calls {
		if strings.Contains(c.output, "deps") {
			depPacks++
		}
	}
	h.jam.mu.Unlock()
	if depPacks != 1 {
		t.Fatalf("jam packed %d dependencies, want 1 (cpython should have come from cache)", depPacks)
	}

	if got := h.pack.vendoredDeps["dep-0.tgz"]; got != "cached-archive" {
		t.Errorf("dep-0.tgz = %q, want the cached bytes", got)
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
