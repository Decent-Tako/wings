package containerd

import (
	"context"
	"os"
	"sync"

	"emperror.dev/errors"
	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"

	"github.com/pelican-dev/wings/config"
	"github.com/pelican-dev/wings/environment"
)

type Factory struct{}

var (
	clientOnce sync.Once
	client     *containerdclient.Client
	clientErr  error
)

func (Factory) Configure(ctx context.Context) error {
	cfg := config.Get().Containerd
	for _, dir := range []string{cfg.RuntimeRoot, cfg.LogDirectory} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return errors.Wrapf(err, "environment/containerd: failed to create %s", dir)
		}
	}

	cli, err := Client()
	if err != nil {
		return err
	}
	_, err = cli.Version(WithNamespace(ctx))
	return errors.Wrap(err, "environment/containerd: failed to query containerd version")
}

func (Factory) NewProcess(id string, meta environment.ProcessMetadata, cfg *environment.Configuration) (environment.ProcessEnvironment, error) {
	cli, err := Client()
	if err != nil {
		return nil, err
	}
	return New(id, meta, cfg, cli), nil
}

func Client() (*containerdclient.Client, error) {
	clientOnce.Do(func() {
		client, clientErr = containerdclient.New(config.Get().Containerd.Address)
	})
	return client, errors.Wrap(clientErr, "environment/containerd: could not create client")
}

func WithNamespace(ctx context.Context) context.Context {
	namespace := config.Get().Containerd.Namespace
	if namespace == "" {
		namespace = "pelican"
	}
	return namespaces.WithNamespace(ctx, namespace)
}
