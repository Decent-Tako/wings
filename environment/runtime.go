package environment

import "github.com/pelican-dev/wings/remote"

// ProcessMetadata is runtime-neutral process metadata used to construct and
// update a server environment.
type ProcessMetadata struct {
	Image string
	Stop  remote.ProcessStopConfiguration
}

// ProcessMetadataUpdater is implemented by environments that keep mutable
// image/stop metadata outside the shared Configuration value.
type ProcessMetadataUpdater interface {
	SetProcessMetadata(ProcessMetadata)
}

// AttachedState is implemented by environments that can report whether their
// stdio stream is currently attached.
type AttachedState interface {
	IsAttached() bool
}
