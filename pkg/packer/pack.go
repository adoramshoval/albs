package packer

import (
	"context"
	"fmt"

	"github.com/adoramshoval/albs/pkg/interfaces"
	"github.com/buildpacks/pack/pkg/client"
	packlogging "github.com/buildpacks/pack/pkg/logging"
	"github.com/google/go-containerregistry/pkg/authn"
)

type PackClientImpl struct {
	client *client.Client
}

// NewPackClient builds a pack client sharing albs' logger and the ambient
// registry credentials, so `docker login` carries over to private registries.
//
// Note that pack always constructs a Docker client from the environment, so a
// reachable Docker daemon remains a hard requirement.
func NewPackClient(logger packlogging.Logger) (interfaces.PackClient, error) {
	c, err := client.NewClient(
		client.WithLogger(logger),
		client.WithKeychain(authn.DefaultKeychain),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create pack client (is the Docker daemon running?): %w", err)
	}
	return &PackClientImpl{client: c}, nil
}

func (p *PackClientImpl) PackageBuildpack(ctx context.Context, opts client.PackageBuildpackOptions) error {
	if err := p.client.PackageBuildpack(ctx, opts); err != nil {
		return fmt.Errorf("pack buildpack package failed: %w", err)
	}
	return nil
}
