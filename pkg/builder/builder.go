package builder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adoramshoval/albs/pkg/interfaces"
	"github.com/adoramshoval/albs/pkg/multiarch"
	"github.com/adoramshoval/albs/pkg/preflight"
	"github.com/adoramshoval/albs/pkg/resolver"
	"github.com/buildpacks/pack/buildpackage"
	"github.com/buildpacks/pack/pkg/client"
	"github.com/buildpacks/pack/pkg/dist"
	"github.com/pelletier/go-toml/v2"
	"golang.org/x/sync/errgroup"
)

// depsDirName holds the vendored component archives, relative to the cloned
// meta-buildpack root.
const depsDirName = "deps"

// metaArchiveName is the packaged meta-buildpack, written alongside deps/ in
// the clone.
const metaArchiveName = "buildpack.tgz"

// packedArchiveName is jam's output for a single component, held in that
// component's own clone directory until it has been flattened into deps/.
const packedArchiveName = "packed.tgz"

type Builder struct {
	cloner      interfaces.GitCloner
	resolver    interfaces.OCIMetadataResolver
	jamPacker   interfaces.JamPacker
	packClient  interfaces.PackClient
	cache       interfaces.CacheManager
	log         interfaces.Logger
	concurrency int
	target      multiarch.Target
	// stack is the full stack id coverage is asserted against. Empty leaves
	// the preflight reporting what each dependency declares without failing
	// on it, which keeps every existing invocation working unchanged.
	stack string
}

func NewBuilder(
	cloner interfaces.GitCloner,
	res interfaces.OCIMetadataResolver,
	jamPacker interfaces.JamPacker,
	packClient interfaces.PackClient,
	cache interfaces.CacheManager,
	log interfaces.Logger,
	concurrency int,
	target multiarch.Target,
	stack string,
) *Builder {
	return &Builder{
		cloner:      cloner,
		resolver:    res,
		jamPacker:   jamPacker,
		packClient:  packClient,
		cache:       cache,
		log:         log,
		concurrency: concurrency,
		target:      target,
		stack:       stack,
	}
}

func (b *Builder) BuildOffline(ctx context.Context, gitURL, tag, outputPath string) error {
	tmpDir, err := os.MkdirTemp("", "meta-bp-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	ref, err := b.cloner.ResolveRef(ctx, gitURL, tag)
	if err != nil {
		return err
	}

	b.log.Infof("Cloning meta-buildpack %s (%s)...", gitURL, refLabel(ref))
	if err := b.cloner.Clone(ctx, gitURL, ref, tmpDir); err != nil {
		return err
	}

	pkgConfig, err := readPackageConfig(filepath.Join(tmpDir, "package.toml"))
	if err != nil {
		return err
	}

	// The order groups live in the meta-buildpack's own buildpack.toml, not in
	// package.toml: package.toml lists every component, while the groups say
	// which of them a build actually needs. Coverage is a property of a group,
	// so both files are required to judge it.
	metaBP, err := preflight.ParseDir(tmpDir)
	if err != nil {
		return err
	}

	depsDir := filepath.Join(tmpDir, depsDirName)
	if err := os.MkdirAll(depsDir, 0o755); err != nil {
		return err
	}

	updatedDeps, components, buildErr := b.buildDependencies(ctx, pkgConfig.Dependencies, depsDir, metaBP.Order)

	// The report is written on both paths. On the failure path it is the
	// diagnosis -- it names which stacks the tag does support -- so it has to
	// survive a run that produces no .cnb at all.
	report := b.report(gitURL, tag, metaBP.Order, components)
	sidecar := preflight.SidecarPath(outputPath)
	if err := preflight.Write(sidecar, report); err != nil {
		b.log.Warnf("could not write preflight report: %v", err)
	} else {
		b.log.Debugf("Preflight report written to %s", sidecar)
	}
	b.log.Infof("Preflight: %s", report.Summary())

	if buildErr != nil {
		return buildErr
	}
	// A component required by only some groups cannot fail the build on its
	// own, so a verdict of no satisfiable group is reached here rather than in
	// the goroutine that found the gap.
	if !report.Verdict.Covered {
		return &preflight.CoverageError{
			Stack:   b.stack,
			Target:  b.target.String(),
			Missing: report.Verdict.Uncovered,
			Partial: !report.Verdict.Complete,
		}
	}
	pkgConfig.Dependencies = updatedDeps

	// The checked-in package.toml points at build/buildpack.tgz, an artifact of
	// the upstream repository's own packaging script that a fresh clone does
	// not contain. Pointing pack at the source directory instead is not enough:
	// buildpack.toml carries no version, and pack requires one. So the
	// meta-buildpack is packaged the same way its components are, which is what
	// stamps the version.
	metaVersion, err := versionFromRef(ref)
	if err != nil {
		return err
	}
	metaArchive := filepath.Join(tmpDir, metaArchiveName)
	b.log.Infof("Packaging the meta-buildpack itself at version %s...", metaVersion)
	if err := b.jamPacker.PackOffline(ctx, tmpDir, metaVersion, metaArchive); err != nil {
		return fmt.Errorf("failed jam pack for the meta-buildpack: %w", err)
	}
	b.log.Debugf("Rewriting buildpack URI %q to ./%s", pkgConfig.Buildpack.URI, metaArchiveName)
	pkgConfig.Buildpack.URI = "./" + metaArchiveName

	// package.toml is deliberately not rewritten on disk: pack resolves every
	// relative URI in Config against RelativeBaseDir, so the file is never read
	// again. Marshalling buildpackage.Config back to TOML would emit empty
	// image/extension/platform keys the original never had.
	// Without an explicit target pack falls back to dist.Target{OS: platform.os}
	// with no architecture, which leaves the .cnb's image config claiming no
	// architecture at all. One target keeps this a single-platform package, so
	// pack still accepts the vendored deps/ URIs.
	b.log.Infof("Packaging composite meta-buildpack into %s...", outputPath)
	return b.packClient.PackageBuildpack(ctx, client.PackageBuildpackOptions{
		Name:            outputPath,
		Format:          client.FormatFile,
		Config:          pkgConfig,
		RelativeBaseDir: tmpDir,
		Targets:         []dist.Target{{OS: b.target.OS, Arch: b.target.Arch}},
	})
}

// cachedBuildpackTOML recovers a cached component's buildpack.toml.
//
// The copy stored beside the archive is the original and is preferred. Falling
// back to the archive keeps caches written before metadata was recorded
// working: coverage resolves either way, since jam preserves the stacks field,
// and only provenance is lost.
func (b *Builder) cachedBuildpackTOML(cacheKey, packedPath string) (preflight.BuildpackTOML, error) {
	if raw, ok := b.cache.GetMeta(cacheKey); ok {
		return preflight.Parse(raw, cacheKey)
	}
	b.log.Debugf("no cached buildpack.toml for %s; reading the archive, which cannot report provenance", cacheKey)
	return preflight.ParseArchive(packedPath)
}

// checkCoverage evaluates one component and decides whether to stop now.
//
// Failing fast is only sound when every order group needs this component:
// its absence then makes the whole tag unbuildable. A component required by
// some groups but not others cannot be judged alone, since another group may
// still be satisfiable, so it is recorded and left to the final verdict.
func (b *Builder) checkCoverage(bp preflight.BuildpackTOML, groups []preflight.Group, report *preflight.Component) error {
	c := preflight.Evaluate(bp, groups, b.stack, b.target.Arch)

	// Recorded before any error return, or the component that failed would be
	// missing from the report that exists to explain the failure.
	*report = c

	if b.stack == "" || c.Covered {
		return nil
	}
	if !preflight.UnconditionallyRequired(groups, c.ID) {
		b.log.Warnf("%s declares no dependency built for %s on %s; another order group may still satisfy the build",
			c.ID, b.target, b.stack)
		return nil
	}
	return &preflight.CoverageError{
		Stack:   b.stack,
		Target:  b.target.String(),
		Missing: []string{c.ID},
	}
}

// report assembles the sidecar from whatever was evaluated.
func (b *Builder) report(gitURL, tag string, groups []preflight.Group, components []preflight.Component) preflight.Report {
	preflight.Sort(components, repoName(gitURL))
	return preflight.NewReport(preflight.Identity{
		GitURL: gitURL,
		Tag:    tag,
		Target: b.target.String(),
		Stack:  b.stack,
	}, groups, components)
}

// repoName is the last path segment of a Git URL, used to guess which
// component carries the language runtime so it sorts first in the report.
func repoName(gitURL string) string {
	trimmed := strings.TrimSuffix(strings.TrimRight(gitURL, "/"), ".git")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

// buildDependencies vendors every component, gating each one on stack coverage
// before it is vendored.
//
// Results are collected into a slice indexed by component, so each goroutine
// owns its slot and no lock is needed. A slot left zero is one that was never
// evaluated -- which is not the same as one that passed, and the report keeps
// them distinct.
//
// The components slice is returned alongside any error rather than discarded
// with it, since a cancelled run still holds most of the diagnosis.
func (b *Builder) buildDependencies(
	ctx context.Context,
	deps []dist.ImageOrURI,
	depsDir string,
	groups []preflight.Group,
) ([]dist.ImageOrURI, []preflight.Component, error) {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(b.concurrency)

	updated := make([]dist.ImageOrURI, len(deps))
	components := make([]preflight.Component, len(deps))

	for i, dep := range deps {
		i, dep := i, dep
		g.Go(func() error {
			localFileName := fmt.Sprintf("dep-%d.tgz", i)
			localFilePath := filepath.Join(depsDir, localFileName)

			if err := b.buildDependency(ctx, dep, localFilePath, groups, &components[i]); err != nil {
				return err
			}

			updated[i] = dist.ImageOrURI{
				BuildpackURI: dist.BuildpackURI{URI: "./" + depsDirName + "/" + localFileName},
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, components, err
	}
	return updated, components, nil
}

func (b *Builder) buildDependency(
	ctx context.Context,
	dep dist.ImageOrURI,
	localFilePath string,
	groups []preflight.Group,
	report *preflight.Component,
) error {
	imageURI := dep.URI
	if imageURI == "" {
		imageURI = dep.ImageName
	}
	if imageURI == "" {
		return fmt.Errorf("dependency has neither a uri nor an image")
	}

	parsed, err := resolver.ParseReference(imageURI)
	if err != nil {
		return err
	}

	repoURL, err := b.resolver.GetSourceURL(ctx, imageURI)
	if err != nil {
		return err
	}

	// Image tags and Git tags routinely disagree on the "v" prefix, so the ref
	// is discovered from the remote rather than assumed.
	ref, err := b.cloner.ResolveRef(ctx, repoURL, parsed.Tag)
	if err != nil {
		return fmt.Errorf("resolving ref for %s: %w", imageURI, err)
	}

	cacheKey := fmt.Sprintf("%s@%s", repoURL, refLabel(ref))

	// What is cached is jam's own output, still carrying every platform it was
	// built for. Flattening happens afterwards so that changing --target reuses
	// the cache instead of invalidating it.
	packedPath, found := b.cache.Get(cacheKey)
	if found {
		b.log.Infof("Using cached archive for %s", cacheKey)

		// On a cache hit the component is never cloned, so buildpack.toml has
		// to come from the cache. The copy stored beside the archive is the
		// original; the one inside the archive has been through jam, which
		// rewrites every dependency uri and drops source, leaving no way to
		// tell a prebuilt binary from a source tarball.
		bp, err := b.cachedBuildpackTOML(cacheKey, packedPath)
		if err != nil {
			return err
		}
		if err := b.checkCoverage(bp, groups, report); err != nil {
			return err
		}
	} else {
		b.log.Infof("Building offline package for %s...", cacheKey)
		compDir, err := os.MkdirTemp("", "comp-bp-*")
		if err != nil {
			return err
		}
		// Deferred to the end of the call rather than the end of this block:
		// packedPath lives in here and is read by the flattening below.
		defer os.RemoveAll(compDir)

		if err := b.cloner.Clone(ctx, repoURL, ref, compDir); err != nil {
			return fmt.Errorf("failed to clone component repo %s: %w", repoURL, err)
		}

		// The gate sits here, between the clone and the vendoring: jam is the
		// expensive step, downloading every dependency the buildpack declares,
		// and this is the last moment before it starts. The raw bytes are kept
		// because this is also the last moment at which provenance exists.
		bp, raw, err := preflight.ReadDir(compDir)
		if err != nil {
			return err
		}
		if err := b.checkCoverage(bp, groups, report); err != nil {
			return err
		}

		packedPath = filepath.Join(compDir, packedArchiveName)
		if err := b.jamPacker.PackOffline(ctx, compDir, parsed.Tag, packedPath); err != nil {
			return fmt.Errorf("failed jam pack for %s: %w", repoURL, err)
		}

		if err := b.cache.Put(cacheKey, packedPath); err != nil {
			b.log.Warnf("could not cache %s: %v", cacheKey, err)
		} else if err := b.cache.PutMeta(cacheKey, raw); err != nil {
			// Losing the metadata costs a later run its provenance, not its
			// correctness: coverage still resolves from the archive.
			b.log.Warnf("could not cache buildpack.toml for %s: %v", cacheKey, err)
		}
	}

	b.log.Debugf("Flattening %s to %s", cacheKey, b.target)
	if err := multiarch.Flatten(packedPath, localFilePath, b.target); err != nil {
		return fmt.Errorf("preparing %s for %s: %w", cacheKey, b.target, err)
	}
	return nil
}

// defaultOS mirrors buildpackage's own default. pack's ConfigReader applies it
// when package.toml omits [platform], but that reader also validates
// buildpack.uri against the filesystem, which rejects the checked-in
// build/buildpack.tgz before it can be rewritten. So the file is decoded here
// and the same default applied.
const defaultOS = "linux"

func readPackageConfig(path string) (buildpackage.Config, error) {
	var cfg buildpackage.Config

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("package.toml not found in repository: %w", err)
	}
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("failed to decode package.toml: %w", err)
	}

	for i, dep := range cfg.Dependencies {
		if dep.URI != "" && dep.ImageName != "" {
			return cfg, fmt.Errorf("dependency %d is configured with both uri and image", i)
		}
	}

	// Without this, packaging fails with "provided image OS '' must be either
	// 'freebsd', 'linux' or 'windows'".
	if cfg.Platform.OS == "" {
		cfg.Platform.OS = defaultOS
	}
	return cfg, nil
}

const tagRefPrefix = "refs/tags/"

// versionFromRef derives the version to stamp the meta-buildpack with. Paketo
// buildpack.toml files carry no version of their own; upstream's packaging
// script takes one on the command line, and the released tag is that version.
func versionFromRef(ref string) (string, error) {
	if strings.HasPrefix(ref, tagRefPrefix) {
		return strings.TrimPrefix(strings.TrimPrefix(ref, tagRefPrefix), "v"), nil
	}
	return "", fmt.Errorf("cannot determine a version for the meta-buildpack from %s: "+
		"pass --tag with a released tag, since buildpack.toml carries no version of its own", refLabel(ref))
}

func refLabel(ref string) string {
	if ref == "" {
		return "default branch"
	}
	return ref
}
