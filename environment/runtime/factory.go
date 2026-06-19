package runtime

import (
	"context"
	"fmt"

	"github.com/pelican-dev/wings/config"
	"github.com/pelican-dev/wings/environment"
	"github.com/pelican-dev/wings/environment/docker"
)

type Factory interface {
	Configure(ctx context.Context) error
	NewProcess(id string, meta environment.ProcessMetadata, cfg *environment.Configuration) (environment.ProcessEnvironment, error)
}

func SelectedFactory() (Factory, error) {
	return FactoryFor(config.Get().ContainerRuntime)
}

func FactoryFor(name config.ContainerRuntime) (Factory, error) {
	switch name {
	case "", config.ContainerRuntimeDocker:
		return docker.Factory{}, nil
	default:
		return nil, fmt.Errorf("environment/runtime: unsupported container runtime %q", name)
	}
}

func ConfigureSelected(ctx context.Context) error {
	factory, err := SelectedFactory()
	if err != nil {
		return err
	}
	return factory.Configure(ctx)
}

func NewProcess(id string, meta environment.ProcessMetadata, cfg *environment.Configuration) (environment.ProcessEnvironment, error) {
	factory, err := SelectedFactory()
	if err != nil {
		return nil, err
	}
	return factory.NewProcess(id, meta, cfg)
}
