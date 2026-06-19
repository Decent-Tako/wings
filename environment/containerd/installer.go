package containerd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"emperror.dev/errors"
	"github.com/apex/log"
	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/pelican-dev/wings/config"
	"github.com/pelican-dev/wings/environment"
	"github.com/pelican-dev/wings/system"
)

type Installer struct {
	client clientAPI
}

func NewInstaller() (*Installer, error) {
	cli, err := Client()
	if err != nil {
		return nil, err
	}
	return &Installer{client: cli}, nil
}

func (i *Installer) PullImage(ctx context.Context, image string) error {
	_, err := ensureContainerdImage(ctx, i.client, image, nil)
	return err
}

func (i *Installer) Remove(ctx context.Context, id string) error {
	c, err := i.client.LoadContainer(WithNamespace(ctx), id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			_ = os.Remove(i.logPath(id))
			return nil
		}
		return err
	}

	if task, err := c.Task(WithNamespace(ctx), nil); err == nil {
		_, _ = task.Delete(WithNamespace(ctx), containerdclient.WithProcessKill)
	} else if !errdefs.IsNotFound(err) {
		return err
	}

	if err := c.Delete(WithNamespace(ctx), containerdclient.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	_ = os.Remove(i.logPath(id))
	return nil
}

func (i *Installer) Execute(ctx context.Context, spec environment.InstallationSpec, output func([]byte)) (string, error) {
	ctx = WithNamespace(ctx)
	image, err := i.client.GetImage(ctx, strings.TrimPrefix(spec.Image, "~"))
	if err != nil {
		return "", errors.Wrap(err, "environment/containerd: failed to inspect installer image")
	}

	labels := map[string]string{
		"Service":       "Pelican",
		"ContainerType": "server_installer",
	}
	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(image),
		oci.WithProcessArgs(spec.Entrypoint, spec.ScriptPath),
		oci.WithEnv(spec.Env),
		oci.WithHostname("installer"),
		oci.WithTTY,
		oci.WithMounts(installerMounts(spec)),
		oci.WithHostNamespace(specs.NetworkNamespace),
		oci.WithAnnotations(labels),
	}
	specOpts = append(specOpts, installerResourceSpecOpts(spec.Limits)...)

	cfg := config.Get()
	container, err := i.client.NewContainer(
		ctx,
		spec.ID,
		containerdclient.WithImage(image),
		containerdclient.WithImageName(strings.TrimPrefix(spec.Image, "~")),
		containerdclient.WithSnapshotter(cfg.Containerd.Snapshotter),
		containerdclient.WithNewSnapshot(spec.ID+"-rootfs", image),
		containerdclient.WithNewSpec(specOpts...),
		containerdclient.WithRuntime(cfg.Containerd.Runtime, nil),
		containerdclient.WithContainerLabels(labels),
	)
	if err != nil {
		return "", errors.Wrap(err, "environment/containerd: failed to create installer container")
	}

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	logWriter, err := i.openLog(spec.ID)
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return "", err
	}

	ioOpts := []cio.Opt{
		cio.WithFIFODir(filepath.Join(config.Get().Containerd.RuntimeRoot, "fifo")),
		cio.WithStreams(stdinR, stdoutW, stdoutW),
		cio.WithTerminal,
	}
	task, err := container.NewTask(ctx, cio.NewCreator(ioOpts...))
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = logWriter.Close()
		return "", errors.Wrap(err, "environment/containerd: failed to create installer task")
	}

	exitC, err := task.Wait(ctx)
	if err != nil {
		_, _ = task.Delete(WithNamespace(context.Background()), containerdclient.WithProcessKill)
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = logWriter.Close()
		return "", errors.Wrap(err, "environment/containerd: failed to wait on installer task")
	}

	taskIO := task.IO()
	defer func() {
		_ = stdinW.Close()
		_ = stdoutW.Close()
		if taskIO != nil {
			taskIO.Cancel()
			_ = taskIO.Close()
		}
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		i.consumeOutput(stdoutR, logWriter, output)
	}()

	if err := task.Start(ctx); err != nil {
		return "", errors.Wrap(err, "environment/containerd: failed to start installer task")
	}

	status := <-exitC
	if _, _, err := status.Result(); err != nil {
		return "", errors.Wrap(err, "environment/containerd: installer task exited with an error")
	}
	_, _ = task.Delete(ctx)
	_ = stdoutW.Close()
	<-done
	return spec.ID, nil
}

func (i *Installer) Logs(_ context.Context, id string) (io.ReadCloser, error) {
	f, err := os.Open(i.logPath(id))
	if os.IsNotExist(err) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	return f, err
}

func (i *Installer) consumeOutput(stdout *io.PipeReader, logWriter io.WriteCloser, output func([]byte)) {
	defer stdout.Close()
	defer logWriter.Close()

	if err := system.ScanReader(io.TeeReader(stdout, logWriter), output); err != nil && err != io.EOF {
		log.WithField("error", err).Warn("error processing scanner line in containerd installer output")
	}
}

func (i *Installer) openLog(id string) (io.WriteCloser, error) {
	if err := os.MkdirAll(config.Get().Containerd.LogDirectory, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(i.logPath(id), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
}

func (i *Installer) logPath(id string) string {
	return filepath.Join(config.Get().Containerd.LogDirectory, id+"-installer.log")
}

func installerMounts(spec environment.InstallationSpec) []specs.Mount {
	tmpfsSize := strconv.Itoa(int(config.Get().Docker.TmpfsSize))
	return []specs.Mount{
		{
			Destination: "/mnt/server",
			Type:        "bind",
			Source:      spec.ServerPath,
			Options:     []string{"rbind", "rw"},
		},
		{
			Destination: "/mnt/install",
			Type:        "bind",
			Source:      spec.TempPath,
			Options:     []string{"rbind", "rw"},
		},
		{
			Destination: "/tmp",
			Type:        "tmpfs",
			Source:      "tmpfs",
			Options: []string{
				"rw",
				"exec",
				"nosuid",
				"size=" + tmpfsSize + "m",
			},
		},
	}
}
