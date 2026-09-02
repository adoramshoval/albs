package resolver

import (
	"context"
	"fmt"
	"strings"

	"github.com/adoramshoval/albs/pkg/interfaces"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// cnbRegistryScheme prefixes dependencies published to the CNB registry rather
// than to an OCI registry. Paketo's tagged releases use this form; its main
// branch uses docker:// URIs, so both must be handled.
const cnbRegistryScheme = "urn:cnb:registry:"

// Reference is a package.toml dependency URI decomposed into the parts albs
// needs: the repository, used for source lookup, and the tag, used to pick a
// Git ref.
type Reference struct {
	Repository string
	Tag        string
	// CNBRegistry reports that this reference names a CNB registry entry
	// rather than an image. There is no manifest to inspect for such a
	// dependency, so OCI label resolution does not apply.
	CNBRegistry bool
}

// ParseReference splits a dependency URI from a package.toml.
//
// For OCI references it uses the registry's own grammar rather than splitting
// on ":", which mis-parses registries carrying a port (registry:5000/foo has
// no tag).
func ParseReference(uri string) (Reference, error) {
	if rest, ok := strings.CutPrefix(uri, cnbRegistryScheme); ok {
		id, version, found := strings.Cut(rest, "@")
		if !found || id == "" || version == "" {
			return Reference{}, fmt.Errorf("parsing CNB registry URI %q: expected %s<namespace>/<name>@<version>", uri, cnbRegistryScheme)
		}
		return Reference{Repository: id, Tag: version, CNBRegistry: true}, nil
	}

	clean := strings.TrimPrefix(uri, "docker://")

	ref, err := name.ParseReference(clean)
	if err != nil {
		return Reference{}, fmt.Errorf("parsing image URI %q: %w", uri, err)
	}

	parsed := Reference{Repository: ref.Context().Name()}
	if tagged, ok := ref.(name.Tag); ok {
		parsed.Tag = tagged.TagStr()
	}
	return parsed, nil
}

type Resolver struct {
	repoMap  map[string]string
	log      interfaces.Logger
	keychain authn.Keychain
	// sourceLabel is a field so tests can exercise the resolution order
	// without reaching a registry.
	sourceLabel func(ctx context.Context, imageURI string) (string, bool)
}

func NewResolver(repoMap map[string]string, log interfaces.Logger) interfaces.OCIMetadataResolver {
	r := &Resolver{
		repoMap:  repoMap,
		log:      log,
		keychain: authn.DefaultKeychain,
	}
	r.sourceLabel = r.fetchSourceLabel
	return r
}

func (r *Resolver) GetSourceURL(ctx context.Context, imageURI string) (string, error) {
	parsed, err := ParseReference(imageURI)
	if err != nil {
		return "", err
	}

	// 1. User-supplied override map. Match on the fully qualified repository
	// and on the URI as written, so that shorthand entries keep working.
	for _, candidate := range lookupKeys(imageURI, parsed.Repository) {
		if mapped, ok := r.repoMap[candidate]; ok {
			r.log.Debugf("resolved %s via repo-map entry %q", imageURI, candidate)
			return mapped, nil
		}
	}

	// 2. The image's own OCI source label. A CNB registry URN names no image,
	// so there is no manifest to read.
	if !parsed.CNBRegistry {
		if url, ok := r.sourceLabel(ctx, imageURI); ok {
			return url, nil
		}
		r.log.Warnf("missing org.opencontainers.image.source label on %s; attempting standard naming fallback", imageURI)
	}

	// 3. Paketo publishes every component from a like-named GitHub repository.
	if repo, ok := paketoRepo(parsed.Repository, parsed.CNBRegistry); ok {
		return repo, nil
	}

	return "", fmt.Errorf("unable to resolve Git repository source URL for image URI %q; supply one via --repo-map", imageURI)
}

func (r *Resolver) fetchSourceLabel(ctx context.Context, imageURI string) (string, bool) {
	clean := strings.TrimPrefix(imageURI, "docker://")

	ref, err := name.ParseReference(clean)
	if err != nil {
		return "", false
	}

	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(r.keychain))
	if err != nil {
		r.log.Debugf("could not fetch manifest for %s: %v", imageURI, err)
		return "", false
	}

	cfg, err := img.ConfigFile()
	if err != nil || cfg.Config.Labels == nil {
		return "", false
	}
	if url := cfg.Config.Labels["org.opencontainers.image.source"]; url != "" {
		return url, true
	}
	return "", false
}

func lookupKeys(uri, repository string) []string {
	if strings.HasPrefix(uri, cnbRegistryScheme) {
		return []string{repository, uri, strings.TrimPrefix(uri, cnbRegistryScheme)}
	}
	clean := strings.TrimPrefix(uri, "docker://")
	keys := []string{repository, clean}
	if base, _, found := strings.Cut(clean, ":"); found {
		keys = append(keys, base)
	}
	return keys
}

var paketoRegistries = []string{
	"gcr.io/paketo-buildpacks/",
	"index.docker.io/paketobuildpacks/",
	"docker.io/paketobuildpacks/",
}

func paketoRepo(repository string, cnbRegistry bool) (string, bool) {
	// A CNB registry id is already <namespace>/<name>, which is the GitHub
	// path the registry entry is published from.
	if cnbRegistry {
		namespace, id, found := strings.Cut(repository, "/")
		if !found || namespace == "" || id == "" {
			return "", false
		}
		return "https://github.com/" + repository, true
	}

	for _, prefix := range paketoRegistries {
		if !strings.HasPrefix(repository, prefix) {
			continue
		}
		id := strings.TrimPrefix(repository, prefix)
		if id == "" {
			return "", false
		}
		return "https://github.com/paketo-buildpacks/" + id, true
	}
	return "", false
}
