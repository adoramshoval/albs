package preflight

import (
	"testing"
)

// twoGroups mirrors the shape of Paketo's Go meta-buildpack: a runtime
// required by every group, and a second component required by only one. The
// difference decides whether a gap can fail the build on its own.
var twoGroups = []Group{
	{Group: []GroupEntry{
		{ID: "runtime"},
		{ID: "extras", Optional: true},
	}},
	{Group: []GroupEntry{
		{ID: "runtime"},
		{ID: "vendor"},
	}},
}

func bp(id string, deps ...Dependency) BuildpackTOML {
	return BuildpackTOML{
		API:       "0.7",
		Buildpack: Buildpack{ID: id},
		Metadata:  Metadata{Dependencies: deps},
	}
}

func TestNormalizeStack(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: ""},
		{in: "jammy", want: "io.buildpacks.stacks.jammy"},
		{in: "io.buildpacks.stacks.jammy", want: "io.buildpacks.stacks.jammy"},
		{in: "  jammy  ", want: "io.buildpacks.stacks.jammy"},
		// A dotted value that is not the Paketo namespace is a typo, not a
		// stack: reporting it as unknown beats reporting no coverage.
		{in: "io.buildpacks.stack.jammy", wantErr: true},
		{in: "io.buildpacks.stacks.", wantErr: true},
		{in: "not a stack", wantErr: true},
	} {
		got, err := NormalizeStack(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NormalizeStack(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeStack(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeStack(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEvaluateMatchesStackAndArch(t *testing.T) {
	jammy := "io.buildpacks.stacks.jammy"

	for _, tc := range []struct {
		name string
		dep  Dependency
		want bool
	}{
		{"exact", Dependency{Stacks: []string{jammy}, Arch: "amd64"}, true},
		{"wrong stack", Dependency{Stacks: []string{"io.buildpacks.stacks.noble"}, Arch: "amd64"}, false},
		// A jammy artifact built only for arm64 passes a stack-only check and
		// still fails an amd64 build, which is why arch is part of the rule.
		{"wrong arch", Dependency{Stacks: []string{jammy}, Arch: "arm64"}, false},
		{"wildcard stack", Dependency{Stacks: []string{"*"}, Arch: "amd64"}, true},
		// Older entries carry no arch at all; failing them would be a false
		// negative on exactly the old tags this check reaches back to.
		{"absent arch", Dependency{Stacks: []string{jammy}}, true},
		{"absent stacks", Dependency{Arch: "amd64"}, true},
	} {
		c := Evaluate(bp("runtime", tc.dep), twoGroups, jammy, "amd64")
		if c.Covered != tc.want {
			t.Errorf("%s: Covered = %v, want %v", tc.name, c.Covered, tc.want)
		}
	}
}

// goToolchain is go-dist's real shape: a prebuilt static binary published
// under a wildcard stack, with source recorded separately for provenance.
var goToolchain = Dependency{
	ID: "go", Version: "1.25.13", Arch: "amd64", Stacks: []string{"*"},
	URI:    "https://go.dev/dl/go1.25.13.linux-amd64.tar.gz",
	Source: "https://go.dev/dl/go1.25.13.src.tar.gz",
}

// pythonSource is cpython's fallback: uri and source name the same tarball,
// so satisfying it means compiling during the build.
var pythonSource = Dependency{
	ID: "python", Version: "3.10.20", Stacks: []string{"*"},
	URI:    "https://www.python.org/ftp/python/3.10.20/Python-3.10.20.tgz",
	Source: "https://www.python.org/ftp/python/3.10.20/Python-3.10.20.tgz",
}

// pythonJammy is cpython's prebuilt binary for a named stack.
var pythonJammy = Dependency{
	ID: "python", Version: "3.10.20", Stacks: []string{"io.buildpacks.stacks.jammy"},
	URI:    "https://artifacts.paketo.io/python/python_3.10.20_linux_amd64_jammy.tgz",
	Source: "https://www.python.org/ftp/python/3.10.20/Python-3.10.20.tgz",
}

// TestEvaluateFlagsSourceOnlyCoverage pins the distinction the stacks field
// cannot make: a wildcard entry may be a prebuilt binary or a source tarball,
// and only the latter costs a compile.
func TestEvaluateFlagsSourceOnlyCoverage(t *testing.T) {
	jammy := "io.buildpacks.stacks.jammy"

	// Go: wildcard, but prebuilt. Flagging this warned on every Go build.
	goDist := Evaluate(bp("runtime", goToolchain), twoGroups, jammy, "amd64")
	if !goDist.Covered || goDist.CoveredOnlyBySource {
		t.Errorf("prebuilt wildcard: Covered=%v CoveredOnlyBySource=%v, want true/false",
			goDist.Covered, goDist.CoveredOnlyBySource)
	}

	// Python with only the source tarball matching: covered, but by compiling.
	srcOnly := Evaluate(bp("runtime", pythonSource), twoGroups, jammy, "amd64")
	if !srcOnly.Covered || !srcOnly.CoveredOnlyBySource {
		t.Errorf("source-only: Covered=%v CoveredOnlyBySource=%v, want true/true",
			srcOnly.Covered, srcOnly.CoveredOnlyBySource)
	}

	// The ordinary case: a prebuilt binary for this stack alongside the
	// source fallback. Not flagged, because the binary is what gets used.
	both := Evaluate(bp("runtime", pythonSource, pythonJammy), twoGroups, jammy, "amd64")
	if !both.Covered || both.CoveredOnlyBySource {
		t.Errorf("mixed: Covered=%v CoveredOnlyBySource=%v, want true/false",
			both.Covered, both.CoveredOnlyBySource)
	}

	// A prebuilt binary for a stack we are not on does not rescue us: only
	// matching entries count toward the flag.
	noble := Evaluate(bp("runtime", pythonSource,
		Dependency{ID: "python", Stacks: []string{"io.buildpacks.stacks.noble"},
			URI: "https://artifacts.paketo.io/python/noble.tgz", Source: "https://python.org/src.tgz"},
	), twoGroups, jammy, "amd64")
	if !noble.Covered || !noble.CoveredOnlyBySource {
		t.Errorf("non-matching prebuilt: Covered=%v CoveredOnlyBySource=%v, want true/true",
			noble.Covered, noble.CoveredOnlyBySource)
	}
}

func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		dep  Dependency
		want string
	}{
		{"prebuilt toolchain", goToolchain, ArtifactPrebuilt},
		{"uri is its own source", pythonSource, ArtifactSource},
		// A plain binary commonly omits source, and nothing was rewritten.
		{"no source declared", Dependency{URI: "https://example.com/thing.tgz"}, ArtifactPrebuilt},
		// jam strips source and rewrites uri, so a packaged archive cannot
		// say. Reporting prebuilt here would be a quiet false negative.
		{"vendored by jam", Dependency{URI: "file:///dependencies/abc123"}, ArtifactUnknown},
		{"nothing declared", Dependency{}, ArtifactUnknown},
	} {
		if got := classify(tc.dep); got != tc.want {
			t.Errorf("%s: classify = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestEvaluateWithoutProvenanceDoesNotGuess pins the cache-hit path: coverage
// still resolves, because jam preserves stacks, but the compile-from-source
// warning must not fire on evidence that was stripped during packaging.
func TestEvaluateWithoutProvenance(t *testing.T) {
	jammy := "io.buildpacks.stacks.jammy"
	vendored := Dependency{
		ID: "python", Version: "3.10.20", Stacks: []string{"*"},
		URI: "file:///dependencies/cf2993798ae8430f3af3a00d96d9fdf3207",
	}

	c := Evaluate(bp("runtime", vendored), twoGroups, jammy, "amd64")
	if !c.Covered {
		t.Error("Covered = false; jam preserves stacks, so coverage still resolves")
	}
	if c.CoveredOnlyBySource {
		t.Error("CoveredOnlyBySource = true although provenance was stripped by jam")
	}
	if c.Dependencies[0].Artifact != ArtifactUnknown {
		t.Errorf("Artifact = %q, want %q", c.Dependencies[0].Artifact, ArtifactUnknown)
	}
}

func TestEvaluateWithoutStackAssertsNothing(t *testing.T) {
	c := Evaluate(bp("runtime", Dependency{Stacks: []string{"io.buildpacks.stacks.noble"}, Arch: "arm64"}), twoGroups, "", "amd64")
	if !c.Covered {
		t.Error("Covered = false with no stack requested; nothing should be judged")
	}
	// The inventory is still recorded -- it was computed anyway, and it is
	// what the matrix is transcribed from.
	if len(c.Dependencies) != 1 || c.Dependencies[0].Matches {
		t.Errorf("Dependencies = %+v, want one entry that matches nothing", c.Dependencies)
	}
}

func TestEvaluateComponentWithNoDependencies(t *testing.T) {
	// procfile and friends declare nothing that could have been built for the
	// wrong stack, so they cannot fail coverage.
	c := Evaluate(bp("extras"), twoGroups, "io.buildpacks.stacks.jammy", "amd64")
	if !c.Covered {
		t.Error("a component with no dependencies should never fail coverage")
	}
}

func TestUnconditionallyRequired(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		// Required by both groups: its absence makes the tag unbuildable, so
		// failing fast on it is sound.
		{"runtime", true},
		// Required by the second group only. The first may still be
		// satisfiable, so the verdict has to wait.
		{"vendor", false},
		{"extras", false},
		{"absent", false},
	} {
		if got := UnconditionallyRequired(twoGroups, tc.id); got != tc.want {
			t.Errorf("UnconditionallyRequired(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestNewReportSatisfiesGroupWhenAnotherFails(t *testing.T) {
	// vendor is uncovered, so group 1 fails -- but group 0 does not need it,
	// so the tag still builds. Gating per-component would reject this.
	r := NewReport(Identity{Stack: "io.buildpacks.stacks.jammy"}, twoGroups, []Component{
		{ID: "runtime", Evaluated: true, Covered: true},
		{ID: "vendor", Evaluated: true, Covered: false},
		{ID: "extras", Evaluated: true, Covered: false},
	})

	if !r.Verdict.Covered {
		t.Error("Covered = false; group 0 needs neither vendor nor extras")
	}
	if len(r.Verdict.SatisfiedGroups) != 1 || r.Verdict.SatisfiedGroups[0] != 0 {
		t.Errorf("SatisfiedGroups = %v, want [0]", r.Verdict.SatisfiedGroups)
	}
	if len(r.Verdict.Uncovered) != 1 || r.Verdict.Uncovered[0] != "vendor" {
		t.Errorf("Uncovered = %v, want [vendor]; extras is optional and must not appear", r.Verdict.Uncovered)
	}
}

func TestNewReportFailsWhenNoGroupSatisfiable(t *testing.T) {
	r := NewReport(Identity{Stack: "io.buildpacks.stacks.jammy"}, twoGroups, []Component{
		{ID: "runtime", Evaluated: true, Covered: false},
		{ID: "vendor", Evaluated: true, Covered: true},
	})
	if r.Verdict.Covered {
		t.Error("Covered = true although runtime is required by every group")
	}
	if !r.Verdict.Complete {
		t.Error("Complete = false although every component was evaluated")
	}
}

func TestNewReportTreatsUnevaluatedAsNotPassing(t *testing.T) {
	// Cancellation stops siblings mid-flight. Silence must not look like a
	// pass, or a partial run would report a group it never checked.
	r := NewReport(Identity{Stack: "io.buildpacks.stacks.jammy"}, twoGroups, []Component{
		{ID: "runtime", Evaluated: true, Covered: true},
		{}, // never reached
	})
	if r.Verdict.Complete {
		t.Error("Complete = true although a component was never evaluated")
	}
	if len(r.Verdict.SatisfiedGroups) != 1 || r.Verdict.SatisfiedGroups[0] != 0 {
		t.Errorf("SatisfiedGroups = %v, want [0]; group 1 needs an unevaluated component", r.Verdict.SatisfiedGroups)
	}
}

func TestNewReportWithoutStackReportsNoVerdict(t *testing.T) {
	r := NewReport(Identity{}, twoGroups, []Component{
		{ID: "runtime", Evaluated: true, Covered: true},
	})
	if !r.Verdict.Covered || len(r.Verdict.Uncovered) != 0 {
		t.Errorf("Verdict = %+v, want covered with nothing uncovered when no stack was requested", r.Verdict)
	}
}

func TestSortPutsRuntimeFirst(t *testing.T) {
	components := []Component{
		{ID: "paketo-buildpacks/pip", Dependencies: []DependencyReport{{ID: "pip"}}},
		{ID: "paketo-buildpacks/cpython", Dependencies: []DependencyReport{{ID: "python"}}},
		{ID: "paketo-buildpacks/procfile"},
	}
	Sort(components, "python")

	if components[0].ID != "paketo-buildpacks/cpython" {
		t.Errorf("first component is %q, want the one carrying the python dependency", components[0].ID)
	}
	// A miss costs ordering only, so the rest stay alphabetical.
	if components[1].ID != "paketo-buildpacks/pip" || components[2].ID != "paketo-buildpacks/procfile" {
		t.Errorf("remaining order = %q, %q; want alphabetical", components[1].ID, components[2].ID)
	}
}

func TestSidecarPath(t *testing.T) {
	for in, want := range map[string]string{
		"python-offline.cnb":      "python-offline.versions.json",
		"./out/go.cnb":            "./out/go.versions.json",
		"noextension":             "noextension.versions.json",
		"/tmp/a.b/python.tar.cnb": "/tmp/a.b/python.tar.versions.json",
	} {
		if got := SidecarPath(in); got != want {
			t.Errorf("SidecarPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCoverageErrorMentionsIncompleteness(t *testing.T) {
	e := &CoverageError{
		Stack: "io.buildpacks.stacks.jammy", Target: "linux/amd64",
		Missing: []string{"runtime"}, Partial: true,
	}
	msg := e.Error()
	for _, want := range []string{"runtime", "io.buildpacks.stacks.jammy", "--concurrency 1"} {
		if !contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
