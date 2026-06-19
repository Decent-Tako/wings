package docker

import (
	"context"

	"github.com/pelican-dev/wings/environment"
)

type Factory struct{}

func (Factory) Configure(ctx context.Context) error {
	return environment.ConfigureDocker(ctx)
}

func (Factory) NewProcess(id string, meta environment.ProcessMetadata, cfg *environment.Configuration) (environment.ProcessEnvironment, error) {
	return New(id, &Metadata{
		Image: meta.Image,
		Stop:  meta.Stop,
	}, cfg)
}
