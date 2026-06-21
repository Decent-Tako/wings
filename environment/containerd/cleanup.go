package containerd

import (
	"context"

	"github.com/apex/log"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
)

type snapshotServiceProvider interface {
	SnapshotService(string) snapshots.Snapshotter
}

func warnContainerdCleanupError(entry *log.Entry, err error, message string) error {
	if err == nil || errdefs.IsNotFound(err) {
		return nil
	}
	entry.WithField("error", err).Warn(message)
	return err
}

func containerdCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(WithNamespace(ctx), containerdCleanupTimeout)
}

func cleanupContainerdSnapshot(ctx context.Context, cli clientAPI, snapshotter, snapshotID string, entry *log.Entry) error {
	provider, ok := cli.(snapshotServiceProvider)
	if !ok {
		entry.WithField("snapshot_id", snapshotID).Warn("containerd client does not expose snapshot cleanup")
		return nil
	}
	return warnContainerdCleanupError(
		entry.WithField("snapshot_id", snapshotID),
		provider.SnapshotService(snapshotter).Remove(WithNamespace(ctx), snapshotID),
		"failed to cleanup containerd snapshot",
	)
}
