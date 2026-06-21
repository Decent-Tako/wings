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
	clientMu   sync.Mutex
	clientOnce sync.Once
	client     *containerdclient.Client
	clientErr  error
)

func (Factory) Configure(ctx context.Context) error {
	cfg := config.Get().Containerd
	if cfg.Network.Mode != "host" {
		return errors.Wrapf(ErrUnsupportedNetwork, "environment/containerd: containerd currently supports host networking only; set containerd.network.mode to %q until the later CNI/portmap implementation lands", "host")
	}
	runtimeRoot, err := containerdRuntimeRoot()
	if err != nil {
		return err
	}
	logDirectory, err := containerdLogDirectory()
	if err != nil {
		return err
	}
	for _, dir := range []string{runtimeRoot, logDirectory} {
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

func (Factory) NewInstaller() (environment.InstallationRunner, error) {
	return NewInstaller()
}

func (Factory) Close() error {
	return Close()
}

func Client() (*containerdclient.Client, error) {
	clientMu.Lock()
	defer clientMu.Unlock()
	clientOnce.Do(func() {
		client, clientErr = containerdclient.New(config.Get().Containerd.Address)
	})
	return client, errors.Wrap(clientErr, "environment/containerd: could not create client")
}

func Close() error {
	clientMu.Lock()
	defer clientMu.Unlock()

	var err error
	if client != nil {
		err = client.Close()
	}
	client = nil
	clientErr = nil
	clientOnce = sync.Once{}
	return errors.Wrap(err, "environment/containerd: failed to close client")
}

func WithNamespace(ctx context.Context) context.Context {
	namespace := config.Get().Containerd.Namespace
	if namespace == "" {
		namespace = "pelican"
	}
	return namespaces.WithNamespace(ctx, namespace)
}
