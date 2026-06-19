package containerd

import (
	"github.com/apex/log"
	"github.com/containerd/errdefs"
)

func warnContainerdCleanupError(entry *log.Entry, err error, message string) error {
	if err == nil || errdefs.IsNotFound(err) {
		return nil
	}
	entry.WithField("error", err).Warn(message)
	return err
}
