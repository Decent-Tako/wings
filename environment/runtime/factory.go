package runtime

import (
	"context"
	"fmt"
	"sync"

	"github.com/pelican-dev/wings/config"
	"github.com/pelican-dev/wings/environment"
	"github.com/pelican-dev/wings/environment/containerd"
	"github.com/pelican-dev/wings/environment/docker"
)

var (
	selectedFactoryMu sync.RWMutex
	selectedFactory   Factory
	selectedRuntime   config.ContainerRuntime
)

type Factory interface {
	Configure(ctx context.Context) error
	NewProcess(id string, meta environment.ProcessMetadata, cfg *environment.Configuration) (environment.ProcessEnvironment, error)
	NewInstaller() (environment.InstallationRunner, error)
}

func SelectedFactory() (Factory, error) {
	selectedFactoryMu.RLock()
	if selectedFactory != nil {
		factory := selectedFactory
		selectedFactoryMu.RUnlock()
		return factory, nil
	}
	selectedFactoryMu.RUnlock()

	return snapshotSelectedFactory(config.Get().ContainerRuntime)
}

func FactoryFor(name config.ContainerRuntime) (Factory, error) {
	switch name {
	case "", config.ContainerRuntimeDocker:
		return docker.Factory{}, nil
	case config.ContainerRuntimeContainerd:
		return containerd.Factory{}, nil
	default:
		return nil, fmt.Errorf("environment/runtime: unsupported container runtime %q", name)
	}
}

func snapshotSelectedFactory(name config.ContainerRuntime) (Factory, error) {
	factory, err := FactoryFor(name)
	if err != nil {
		return nil, err
	}

	selectedFactoryMu.Lock()
	defer selectedFactoryMu.Unlock()
	if selectedFactory != nil {
		return selectedFactory, nil
	}
	selectedFactory = factory
	if name == "" {
		name = config.ContainerRuntimeDocker
	}
	selectedRuntime = name
	return selectedFactory, nil
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

func NewInstaller() (environment.InstallationRunner, error) {
	factory, err := SelectedFactory()
	if err != nil {
		return nil, err
	}
	return factory.NewInstaller()
}

func resetSelectedFactoryForTest() {
	selectedFactoryMu.Lock()
	defer selectedFactoryMu.Unlock()
	selectedFactory = nil
	selectedRuntime = ""
}
