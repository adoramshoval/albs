package interfaces

import (
	"context"

	"github.com/buildpacks/pack/pkg/client"
)

// GitCloner resolves versions to Git refs and clones repositories to local paths.
type GitCloner interface {
	// ResolveRef maps a version (typically an OCI image tag) onto a fully
	// qualified ref in repoURL, e.g. "1.18.42" -> "refs/tags/v1.18.42".
	// An empty version resolves to the repository's default branch.
	ResolveRef(ctx context.Context, repoURL, version string) (string, error)
	Clone(ctx context.Context, repoURL, ref, targetDir string) error
}

// OCIMetadataResolver maps OCI image URIs to their source Git URLs.
type OCIMetadataResolver interface {
	GetSourceURL(ctx context.Context, imageURI string) (string, error)
}

// JamPacker encapsulates Paketo jam offline packaging operations.
type JamPacker interface {
	// version is stamped into the produced buildpack; Paketo component
	// buildpack.toml files carry no version of their own.
	PackOffline(ctx context.Context, srcDir, version, outputTgzPath string) error
}

// PackClient encapsulates Buildpacks pack client operations.
type PackClient interface {
	PackageBuildpack(ctx context.Context, opts client.PackageBuildpackOptions) error
}

// CacheManager provides caching mechanisms for generated component archives,
// and for a small metadata blob stored alongside each one.
//
// The metadata exists because packaging is lossy: jam rewrites every
// dependency uri to a local path and drops source entirely, so facts about the
// original buildpack cannot be recovered from the archive it produced. They
// are kept beside it instead.
type CacheManager interface {
	Get(key string) (string, bool)
	Put(key, srcFilePath string) error
	// GetMeta returns the metadata stored under key. Entries cached before
	// metadata was recorded simply report false.
	GetMeta(key string) ([]byte, bool)
	PutMeta(key string, data []byte) error
}

// Logger reports progress. Debug output is suppressed unless --verbose is set.
type Logger interface {
	Debugf(format string, a ...interface{})
	Infof(format string, a ...interface{})
	Warnf(format string, a ...interface{})
}
