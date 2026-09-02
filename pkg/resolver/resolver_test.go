package resolver

import (
	"context"
	"testing"

	"github.com/adoramshoval/albs/pkg/testsupport"
	"github.com/google/go-containerregistry/pkg/authn"
)

func TestParseReference(t *testing.T) {
	tests := []struct {
		name     string
		uri      string
		wantRepo string
		wantTag  string
		wantErr  bool
	}{
		{
			name:     "paketo dependency as written in package.toml",
			uri:      "docker://docker.io/paketobuildpacks/cpython:1.18.42",
			wantRepo: "index.docker.io/paketobuildpacks/cpython",
			wantTag:  "1.18.42",
		},
		{
			name:     "gcr dependency",
			uri:      "docker://gcr.io/paketo-buildpacks/pip:0.29.2",
			wantRepo: "gcr.io/paketo-buildpacks/pip",
			wantTag:  "0.29.2",
		},
		{
			name:     "without the docker:// scheme",
			uri:      "gcr.io/paketo-buildpacks/pip:0.29.2",
			wantRepo: "gcr.io/paketo-buildpacks/pip",
			wantTag:  "0.29.2",
		},
		{
			// Splitting on ":" would yield a tag of "5000/buildpacks/custom";
			// an untagged reference means "latest" per OCI convention.
			name:     "private registry with a port and no tag",
			uri:      "docker://registry.internal:5000/buildpacks/custom",
			wantRepo: "registry.internal:5000/buildpacks/custom",
			wantTag:  "latest",
		},
		{
			name:     "private registry with a port and a tag",
			uri:      "docker://registry.internal:5000/buildpacks/custom:2.1.0",
			wantRepo: "registry.internal:5000/buildpacks/custom",
			wantTag:  "2.1.0",
		},
		{
			// A digest pins no tag, so there is no version to resolve.
			name:     "digest reference",
			uri:      "docker://gcr.io/paketo-buildpacks/pip@sha256:" + "0000000000000000000000000000000000000000000000000000000000000000",
			wantRepo: "gcr.io/paketo-buildpacks/pip",
			wantTag:  "",
		},
		{
			// Paketo's tagged releases pin dependencies this way.
			name:     "CNB registry URN",
			uri:      "urn:cnb:registry:paketo-buildpacks/cpython@1.8.7",
			wantRepo: "paketo-buildpacks/cpython",
			wantTag:  "1.8.7",
		},
		{
			name:     "CNB registry URN with a hyphenated name",
			uri:      "urn:cnb:registry:paketo-buildpacks/pipenv-install@0.6.13",
			wantRepo: "paketo-buildpacks/pipenv-install",
			wantTag:  "0.6.13",
		},
		{
			name:    "CNB registry URN without a version",
			uri:     "urn:cnb:registry:paketo-buildpacks/cpython",
			wantErr: true,
		},
		{
			name:    "not a reference at all",
			uri:     "docker://NOT A REFERENCE",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseReference(tt.uri)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseReference(%q) = %+v, want error", tt.uri, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseReference(%q): unexpected error: %v", tt.uri, err)
			}
			if got.Repository != tt.wantRepo {
				t.Errorf("repository = %q, want %q", got.Repository, tt.wantRepo)
			}
			if got.Tag != tt.wantTag {
				t.Errorf("tag = %q, want %q", got.Tag, tt.wantTag)
			}
		})
	}
}

// newTestResolver builds a Resolver whose registry lookup is stubbed, so the
// resolution order can be exercised without network access.
func newTestResolver(repoMap map[string]string, label string) *Resolver {
	r := &Resolver{
		repoMap:  repoMap,
		log:      testsupport.Logger{},
		keychain: authn.DefaultKeychain,
	}
	r.sourceLabel = func(context.Context, string) (string, bool) {
		return label, label != ""
	}
	return r
}

func TestGetSourceURL(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		repoMap map[string]string
		label   string
		want    string
		wantErr bool
	}{
		{
			name:    "repo-map wins over the image label",
			uri:     "docker://gcr.io/paketo-buildpacks/cpython:1.18.42",
			repoMap: map[string]string{"gcr.io/paketo-buildpacks/cpython": "https://git.internal/cpython"},
			label:   "https://github.com/paketo-buildpacks/cpython",
			want:    "https://git.internal/cpython",
		},
		{
			name:    "repo-map entry written with the docker:// scheme",
			uri:     "docker://gcr.io/paketo-buildpacks/cpython:1.18.42",
			repoMap: map[string]string{"gcr.io/paketo-buildpacks/cpython:1.18.42": "https://git.internal/pinned"},
			want:    "https://git.internal/pinned",
		},
		{
			name:  "falls back to the OCI source label",
			uri:   "docker://example.com/custom/thing:1.0.0",
			label: "https://git.example.com/custom/thing",
			want:  "https://git.example.com/custom/thing",
		},
		{
			name: "paketo naming convention on docker.io",
			uri:  "docker://docker.io/paketobuildpacks/cpython:1.18.42",
			want: "https://github.com/paketo-buildpacks/cpython",
		},
		{
			name: "paketo naming convention on gcr.io",
			uri:  "docker://gcr.io/paketo-buildpacks/pip:0.29.2",
			want: "https://github.com/paketo-buildpacks/pip",
		},
		{
			// No manifest exists to inspect, so this must fall straight
			// through to the naming convention.
			name: "CNB registry URN falls back to the GitHub path",
			uri:  "urn:cnb:registry:paketo-buildpacks/pipenv-install@0.6.13",
			want: "https://github.com/paketo-buildpacks/pipenv-install",
		},
		{
			name:    "repo-map overrides a CNB registry URN",
			uri:     "urn:cnb:registry:paketo-buildpacks/cpython@1.8.7",
			repoMap: map[string]string{"paketo-buildpacks/cpython": "https://git.internal/cpython"},
			want:    "https://git.internal/cpython",
		},
		{
			name:    "unknown registry with no label and no mapping",
			uri:     "docker://registry.internal:5000/buildpacks/custom:2.1.0",
			wantErr: true,
		},
		{
			name:    "malformed uri",
			uri:     "docker://NOT A REFERENCE",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoMap := tt.repoMap
			if repoMap == nil {
				repoMap = map[string]string{}
			}
			got, err := newTestResolver(repoMap, tt.label).GetSourceURL(context.Background(), tt.uri)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("GetSourceURL(%q) = %q, want error", tt.uri, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetSourceURL(%q): unexpected error: %v", tt.uri, err)
			}
			if got != tt.want {
				t.Errorf("GetSourceURL(%q) = %q, want %q", tt.uri, got, tt.want)
			}
		})
	}
}
