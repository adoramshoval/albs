package builder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/adoramshoval/albs/pkg/interfaces"
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

type Builder struct {
	cloner      interfaces.GitCloner
	resolver    interfaces.OCIMetadataResolver
	jamPacker   interfaces.JamPacker
	packClient  interfaces.PackClient
	cache       interfaces.CacheManager
	log         interfaces.Logger
	concurrency int
}

func NewBuilder(
	cloner interfaces.GitCloner,
	res interfaces.OCIMetadataResolver,
	jamPacker interfaces.JamPacker,
	packClient interfaces.PackClient,
	cache interfaces.CacheManager,
	log interfaces.Logger,
	concurrency int,
) *Builder {
	return &Builder{
		cloner:      cloner,
		resolver:    res,
		jamPacker:   jamPacker,
		packClient:  packClient,
		cache:       cache,
		log:         log,
		concurrency: concurrency,
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

	depsDir := filepath.Join(tmpDir, depsDirName)
	if err := os.MkdirAll(depsDir, 0o755); err != nil {
		return err
	}

	updatedDeps, err := b.buildDependencies(ctx, pkgConfig.Dependencies, depsDir)
	if err != nil {
		return err
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
	b.log.Infof("Packaging composite meta-buildpack into %s...", outputPath)
	return b.packClient.PackageBuildpack(ctx, client.PackageBuildpackOptions{
		Name:            outputPath,
		Format:          client.FormatFile,
		Config:          pkgConfig,
		RelativeBaseDir: tmpDir,
	})
}

func (b *Builder) buildDependencies(ctx context.Context, deps []dist.ImageOrURI, depsDir string) ([]dist.ImageOrURI, error) {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(b.concurrency)

	updated := make([]dist.ImageOrURI, len(deps))

	for i, dep := range deps {
		i, dep := i, dep
		g.Go(func() error {
			localFileName := fmt.Sprintf("dep-%d.tgz", i)
			localFilePath := filepath.Join(depsDir, localFileName)

			if err := b.buildDependency(ctx, dep, localFilePath); err != nil {
				return err
			}

			updated[i] = dist.ImageOrURI{
				BuildpackURI: dist.BuildpackURI{URI: "./" + depsDirName + "/" + localFileName},
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return updated, nil
}

func (b *Builder) buildDependency(ctx context.Context, dep dist.ImageOrURI, localFilePath string) error {
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

	if cachedPath, found := b.cache.Get(cacheKey); found {
		b.log.Infof("Using cached archive for %s", cacheKey)
		return copyFile(cachedPath, localFilePath)
	}

	b.log.Infof("Building offline package for %s...", cacheKey)
	compDir, err := os.MkdirTemp("", "comp-bp-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(compDir)

	if err := b.cloner.Clone(ctx, repoURL, ref, compDir); err != nil {
		return fmt.Errorf("failed to clone component repo %s: %w", repoURL, err)
	}

	if err := b.jamPacker.PackOffline(ctx, compDir, parsed.Tag, localFilePath); err != nil {
		return fmt.Errorf("failed jam pack for %s: %w", repoURL, err)
	}

	if err := b.cache.Put(cacheKey, localFilePath); err != nil {
		b.log.Warnf("could not cache %s: %v", cacheKey, err)
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

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
