package containerd

import (
	"context"

	containerdclient "github.com/containerd/containerd/v2/client"
	ctrevents "github.com/containerd/containerd/v2/core/events"
)

type clientAPI interface {
	LoadContainer(context.Context, string) (containerdclient.Container, error)
	NewContainer(context.Context, string, ...containerdclient.NewContainerOpts) (containerdclient.Container, error)
	GetImage(context.Context, string) (containerdclient.Image, error)
	Pull(context.Context, string, ...containerdclient.RemoteOpt) (containerdclient.Image, error)
	Subscribe(context.Context, ...string) (<-chan *ctrevents.Envelope, <-chan error)
}
