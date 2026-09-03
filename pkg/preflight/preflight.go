package preflight

import (
	"fmt"
	"sort"
	"strings"
)

// WildcardStack is the stack id a dependency declares when it is not built
// against a particular distro. It counts as coverage, and says nothing on its
// own about whether the artifact is compiled: go-dist publishes prebuilt Go
// toolchains under stacks = ["*"] because a static binary really does run
// anywhere, while cpython publishes a source tarball under the same wildcard.
// Telling those apart is what isPrebuilt is for.
const WildcardStack = "*"

// stackIDPrefix is the namespace every Paketo stack id sits under, so that
// --stack jammy and --stack io.buildpacks.stacks.jammy mean the same thing.
const stackIDPrefix = "io.buildpacks.stacks."

// NormalizeStack expands a shorthand stack name to its full id.
//
// A value that is neither a full id nor a plausible shorthand is rejected
// here, before any cloning, so that a typo reads as an unknown stack rather
// than as a tag with no coverage.
func NormalizeStack(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if strings.HasPrefix(s, stackIDPrefix) {
		if strings.TrimPrefix(s, stackIDPrefix) == "" {
			return "", fmt.Errorf("invalid stack %q: %s must be followed by a name, for example %sjammy", s, stackIDPrefix, stackIDPrefix)
		}
		return s, nil
	}
	if strings.ContainsAny(s, "./ ") {
		return "", fmt.Errorf("invalid stack %q: want a full id such as %sjammy, or a bare name such as jammy", s, stackIDPrefix)
	}
	return stackIDPrefix + s, nil
}

// DependencyReport is one [[metadata.dependencies]] entry as evaluated.
type DependencyReport struct {
	ID      string   `json:"id"`
	Version string   `json:"version"`
	Arch    string   `json:"arch,omitempty"`
	Stacks  []string `json:"stacks"`
	// Artifact is ArtifactPrebuilt, ArtifactSource, or ArtifactUnknown when
	// the entry was read from an archive jam had already rewritten.
	Artifact string `json:"artifact"`
	// Matches is meaningful only when a stack was requested.
	Matches bool `json:"matches"`
}

const (
	// ArtifactPrebuilt is a compiled binary: uri and source name different
	// files, so the buildpack unpacks rather than builds.
	ArtifactPrebuilt = "prebuilt"
	// ArtifactSource is an entry whose uri is its own source tarball.
	// Coverage still holds offline, but by compiling during the build.
	ArtifactSource = "source"
	// ArtifactUnknown is what a vendored archive can tell us. jam rewrites
	// every uri to file:///dependencies/<sha> and drops source altogether,
	// so provenance does not survive packaging.
	ArtifactUnknown = "unknown"
)

// Component is one component buildpack as evaluated.
//
// Evaluated distinguishes "checked and passed" from "never reached", which
// matters because a failing component cancels its siblings mid-flight. A zero
// Component is an unevaluated one, and must never read as a pass.
type Component struct {
	ID           string `json:"id"`
	Version      string `json:"version,omitempty"`
	BuildpackAPI string `json:"buildpackAPI,omitempty"`
	// RequiredInGroups lists the order groups this component is a
	// non-optional member of. Empty means it is optional everywhere it
	// appears, so it never gates.
	RequiredInGroups []int `json:"requiredInGroups"`
	Evaluated        bool  `json:"evaluated"`
	Covered          bool  `json:"covered"`
	// CoveredOnlyBySource marks a component whose every matching dependency
	// is a source tarball. Coverage still holds -- the tarball is vendored
	// and the build needs no network -- but it is satisfied by compiling at
	// build time rather than by unpacking a binary, which is slower and
	// needs a toolchain in the build image.
	CoveredOnlyBySource bool               `json:"coveredOnlyBySource"`
	Dependencies        []DependencyReport `json:"dependencies"`
}

type Identity struct {
	GitURL string `json:"gitURL"`
	Tag    string `json:"tag"`
	Target string `json:"target"`
	// Stack is null when --stack was not given, in which case the report
	// records what each dependency declares without judging it.
	Stack string `json:"stack,omitempty"`
}

type Verdict struct {
	// Covered is true when at least one order group has every non-optional
	// member covered. It is only meaningful when a stack was requested.
	Covered         bool     `json:"covered"`
	SatisfiedGroups []int    `json:"satisfiedGroups"`
	Uncovered       []string `json:"uncovered"`
	// Complete is false when cancellation cut the audit short, so an empty
	// Uncovered cannot be read as "nothing else is wrong".
	Complete bool `json:"complete"`
}

type Report struct {
	SchemaVersion int         `json:"schemaVersion"`
	Identity      Identity    `json:"identity"`
	Verdict       Verdict     `json:"verdict"`
	Components    []Component `json:"components"`
}

const schemaVersion = 1

// Evaluate turns a component's buildpack.toml into a Component.
//
// With no stack requested it records the declared dependencies and reports
// covered, since there is nothing to fail against; the table is still worth
// writing down.
func Evaluate(bp BuildpackTOML, groups []Group, stack, arch string) Component {
	c := Component{
		ID:               bp.Buildpack.ID,
		Version:          bp.Buildpack.Version,
		BuildpackAPI:     bp.API,
		RequiredInGroups: requiredInGroups(groups, bp.Buildpack.ID),
		Evaluated:        true,
		Dependencies:     make([]DependencyReport, 0, len(bp.Metadata.Dependencies)),
	}

	// onlySource stays true only while every matching entry is known to be a
	// source tarball. A single unknown gives it up rather than guessing, so
	// the flag is never raised on evidence that is not there.
	anyMatch, onlySource := false, true
	for _, d := range bp.Metadata.Dependencies {
		matches := stack != "" && matchesStack(d.Stacks, stack) && matchesArch(d.Arch, arch)
		artifact := classify(d)
		if matches {
			anyMatch = true
			if artifact != ArtifactSource {
				onlySource = false
			}
		}
		c.Dependencies = append(c.Dependencies, DependencyReport{
			ID: d.ID, Version: d.Version, Arch: d.Arch, Stacks: d.Stacks,
			Artifact: artifact, Matches: matches,
		})
	}

	if stack == "" {
		c.Covered = true
		return c
	}

	// A component declaring no dependencies at all -- procfile, say -- has
	// nothing to be built for a stack, so it cannot fail coverage.
	if len(bp.Metadata.Dependencies) == 0 {
		c.Covered = true
		return c
	}

	c.Covered = anyMatch
	c.CoveredOnlyBySource = anyMatch && onlySource
	return c
}

// vendoredURIPrefix is what jam rewrites every dependency uri to when it packs
// a buildpack offline.
const vendoredURIPrefix = "file:///dependencies/"

// classify says whether a dependency's artifact is compiled.
//
// Paketo records a uri and a source for each entry; when they name the same
// file the artifact is the source tarball and the buildpack compiles it during
// the build. The stacks field cannot answer this -- go-dist ships a prebuilt
// toolchain under stacks = ["*"] while cpython ships source under the same
// wildcard -- so reading it there raised a false alarm on every Go build.
//
// Provenance does not survive packaging: jam rewrites uri to a local path and
// drops source, so an entry read back from a vendored archive can only be
// reported as unknown. Guessing prebuilt there would trade one wrong answer
// for another, quieter one.
func classify(d Dependency) string {
	if strings.HasPrefix(d.URI, vendoredURIPrefix) || (d.URI == "" && d.Source == "") {
		return ArtifactUnknown
	}
	if d.Source == "" {
		// A plain binary commonly omits source; nothing here is rewritten,
		// so the absence is the buildpack's own choice rather than jam's.
		return ArtifactPrebuilt
	}
	if d.URI == d.Source {
		return ArtifactSource
	}
	return ArtifactPrebuilt
}

// matchesArch treats an absent arch as matching. Older dependency entries
// carry only stacks, and failing them would be a false negative on exactly
// the old tags this check exists to reach back to.
func matchesArch(depArch, arch string) bool {
	return depArch == "" || depArch == arch
}

func matchesStack(stacks []string, stack string) bool {
	// An entry with no stacks named is as unconstrained as a wildcard.
	if len(stacks) == 0 {
		return true
	}
	return containsStack(stacks, stack) || containsStack(stacks, WildcardStack)
}

func containsStack(stacks []string, want string) bool {
	for _, s := range stacks {
		if s == want {
			return true
		}
	}
	return false
}

// requiredInGroups reports which order groups need this component to detect.
// A component absent from a group does not gate that group.
func requiredInGroups(groups []Group, id string) []int {
	out := []int{}
	for i, g := range groups {
		for _, e := range g.Group {
			if e.ID == id && !e.Optional {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

// UnconditionallyRequired reports whether every order group needs this
// component. Only then does its failure prove the whole tag unusable, which is
// what makes failing fast on it sound.
//
// A component required in some groups but not others cannot be decided alone:
// another group may still be satisfiable, so the verdict waits for the rest.
// This is what keeps an in-goroutine gate honest about a rule (P14) that is
// really a property of the whole tag.
func UnconditionallyRequired(groups []Group, id string) bool {
	if len(groups) == 0 {
		return false
	}
	for _, g := range groups {
		required := false
		for _, e := range g.Group {
			if e.ID == id && !e.Optional {
				required = true
				break
			}
		}
		if !required {
			return false
		}
	}
	return true
}

// NewReport assembles the verdict from whatever was evaluated.
//
// A group counts as satisfied only when every non-optional member was both
// evaluated and covered: an unevaluated component is never assumed to pass, so
// a cancelled run cannot report a satisfied group it never checked.
func NewReport(id Identity, groups []Group, components []Component) Report {
	byID := make(map[string]Component, len(components))
	evaluatedAll := true
	for _, c := range components {
		if !c.Evaluated {
			evaluatedAll = false
			continue
		}
		byID[c.ID] = c
	}

	satisfied := []int{}
	uncovered := map[string]bool{}
	for i, g := range groups {
		ok := true
		for _, e := range g.Group {
			if e.Optional {
				continue
			}
			c, seen := byID[e.ID]
			if !seen {
				ok = false
				continue
			}
			if !c.Covered {
				ok = false
				uncovered[e.ID] = true
			}
		}
		if ok {
			satisfied = append(satisfied, i)
		}
	}

	names := make([]string, 0, len(uncovered))
	for n := range uncovered {
		names = append(names, n)
	}
	sort.Strings(names)

	covered := len(satisfied) > 0
	// With no stack requested nothing was judged, so there is no verdict to
	// report beyond the inventory itself.
	if id.Stack == "" {
		covered = true
		names = []string{}
	}

	return Report{
		SchemaVersion: schemaVersion,
		Identity:      id,
		Verdict: Verdict{
			Covered:         covered,
			SatisfiedGroups: satisfied,
			Uncovered:       names,
			Complete:        evaluatedAll,
		},
		Components: components,
	}
}

// Sort orders components with the likely runtime first, then alphabetically.
//
// The runtime is found by matching a dependency id against the meta-buildpack
// repository name: repo "go" finds id "go" inside go-dist, repo "python" finds
// "python" inside cpython. The heuristic decides presentation order only, so a
// miss costs a moment's scanning and never hides anything.
func Sort(components []Component, repoName string) {
	primary := func(c Component) bool {
		for _, d := range c.Dependencies {
			if d.ID == repoName {
				return true
			}
		}
		return false
	}
	sort.SliceStable(components, func(i, j int) bool {
		pi, pj := primary(components[i]), primary(components[j])
		if pi != pj {
			return pi
		}
		return components[i].ID < components[j].ID
	})
}

// CoverageError reports a tag that cannot build on the requested stack. It is
// distinguished from an ordinary build failure so the caller knows the report
// it is about to write is the diagnosis rather than an aside.
type CoverageError struct {
	Stack   string
	Target  string
	Missing []string
	// Partial marks a verdict reached before every component was checked.
	Partial bool
}

func (e *CoverageError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "no order group is satisfiable on %s for %s: ", e.Stack, e.Target)
	if len(e.Missing) == 1 {
		fmt.Fprintf(&b, "%s declares no dependency built for it", e.Missing[0])
	} else {
		fmt.Fprintf(&b, "%s declare no dependencies built for it", strings.Join(e.Missing, ", "))
	}
	if e.Partial {
		b.WriteString("; other components were not checked, so this may be incomplete " +
			"(re-run with --concurrency 1 for a full report)")
	}
	return b.String()
}
