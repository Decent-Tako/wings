package environment

import (
	"context"
	"io"
)

// InstallationSpec is the runtime-neutral contract for running a server egg
// installation script in a temporary container.
type InstallationSpec struct {
	ID         string
	Image      string
	Entrypoint string

	ScriptPath string
	TempPath   string
	ServerPath string

	Env         []string
	Limits      Limits
	Allocations Allocations
}

// InstallationRunner owns the runtime-specific lifecycle for installer
// containers while server.InstallationProcess owns the shared script and log
// book-keeping. Execute returns only after the installer process has exited and
// the live output callback has consumed the runtime stream to EOF.
type InstallationRunner interface {
	PullImage(ctx context.Context, image string) error
	Remove(ctx context.Context, id string) error
	Execute(ctx context.Context, spec InstallationSpec, output func([]byte)) (string, error)
	Logs(ctx context.Context, id string) (io.ReadCloser, error)
}
