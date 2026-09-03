package cmd

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/adoramshoval/albs/pkg/builder"
	"github.com/adoramshoval/albs/pkg/cache"
	"github.com/adoramshoval/albs/pkg/config"
	"github.com/adoramshoval/albs/pkg/git"
	"github.com/adoramshoval/albs/pkg/multiarch"
	"github.com/adoramshoval/albs/pkg/packer"
	"github.com/adoramshoval/albs/pkg/preflight"
	"github.com/adoramshoval/albs/pkg/resolver"
	packlogging "github.com/buildpacks/pack/pkg/logging"
	"github.com/spf13/cobra"
)

var (
	gitURL      string
	tag         string
	outputPath  string
	cacheDir    string
	repoMapPath string
	targetSpec  string
	stackSpec   string
	concurrency int
	verbose     bool
)

var rootCmd = &cobra.Command{
	Use:   "albs",
	Short: "Package Paketo meta-buildpacks into offline .cnb bundles",
	Long: "albs resolves a meta-buildpack's component buildpacks back to their source\n" +
		"repositories, vendors each one offline with jam, and assembles the result\n" +
		"into a single composite .cnb package.\n\n" +
		"Requires jam on PATH and a running Docker daemon.",
	Example: "  albs -u https://github.com/paketo-buildpacks/python -t v2.9.2 -o ./python-offline.cnb\n" +
		"  albs -u https://github.com/paketo-buildpacks/python -t v2.9.2 --repo-map ./repo-map.yaml -j 4 -v",
	SilenceUsage:  true,
	SilenceErrors: false,
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(cmd.Context())
	},
}

func run(ctx context.Context) error {
	log := newLogger()

	target, err := multiarch.ParseTarget(targetSpec)
	if err != nil {
		return err
	}

	// Normalised up front so that a mistyped stack fails as an unknown stack
	// here, rather than as a tag with no coverage an hour into the build.
	stack, err := preflight.NormalizeStack(stackSpec)
	if err != nil {
		return err
	}

	repoMap, err := config.LoadRepoMap(repoMapPath)
	if err != nil {
		return err
	}

	// Fail on a missing or outdated jam before any cloning happens, rather
	// than from inside a worker goroutine minutes later.
	jamPacker := packer.NewJamPacker(log)
	if err := jamPacker.Preflight(ctx); err != nil {
		return err
	}

	// Salting the cache with the packer fingerprint keeps archives built by
	// different jam builds from colliding on the same key.
	jamFingerprint, err := jamPacker.Fingerprint(ctx)
	if err != nil {
		return err
	}
	diskCache, err := cache.NewDiskCache(cacheDir, jamFingerprint)
	if err != nil {
		return err
	}

	packClient, err := packer.NewPackClient(log)
	if err != nil {
		return err
	}

	b := builder.NewBuilder(
		git.NewCloner(log),
		resolver.NewResolver(repoMap, log),
		jamPacker,
		packClient,
		diskCache,
		log,
		concurrency,
		target,
		stack,
	)

	return b.BuildOffline(ctx, gitURL, tag, outputPath)
}

// newLogger returns pack's own logger. It already satisfies interfaces.Logger,
// so albs and pack share one output stream and one --verbose switch.
func newLogger() *packlogging.LogWithWriters {
	if verbose {
		return packlogging.NewLogWithWriters(os.Stderr, os.Stderr, packlogging.WithVerbose())
	}
	return packlogging.NewLogWithWriters(os.Stderr, os.Stderr)
}

func Execute() error {
	return rootCmd.ExecuteContext(context.Background())
}

func init() {
	rootCmd.Flags().StringVarP(&gitURL, "git-url", "u", "", "Target Git repository URL for the meta-buildpack (required)")
	rootCmd.Flags().StringVarP(&tag, "tag", "t", "", "Released Git tag to build; also the version stamped into the package (required)")
	rootCmd.Flags().StringVarP(&outputPath, "output", "o", "./meta-buildpack-offline.cnb", "Output path for generated .cnb archive")
	rootCmd.Flags().StringVar(&cacheDir, "cache-dir", "./.cache", "Local directory path for caching component archives")
	rootCmd.Flags().StringVar(&repoMapPath, "repo-map", "", "Path to JSON/YAML file mapping image URIs to Git URLs (e.g. repo-map.json or repo-map.yaml)")
	rootCmd.Flags().StringVar(&targetSpec, "target", "linux/amd64", "Platform (<os>/<arch>) to package for; component buildpacks ship binaries for several, and only one can sit at the buildpack root")
	rootCmd.Flags().StringVar(&stackSpec, "stack", "", "Stack to assert dependency coverage against, as a full id (io.buildpacks.stacks.jammy) or a bare name (jammy); reports without failing when unset")
	rootCmd.Flags().IntVarP(&concurrency, "concurrency", "j", runtime.NumCPU(), "Maximum concurrent component builds")
	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Enable verbose logging")

	// Both are required: buildpack.toml carries no version, so the tag is the
	// only thing the package can be stamped with. Enforcing it here fails
	// before any cloning rather than partway through.
	for _, flag := range []string{"git-url", "tag"} {
		if err := rootCmd.MarkFlagRequired(flag); err != nil {
			panic(fmt.Sprintf("marking %s required: %v", flag, err))
		}
	}
}
