package containerd

import (
	"context"
	"io"
	"os"
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
	ctx = WithNamespace(ctx)
	_, err := ensureContainerdImage(ctx, i.client, image, nil)
	return err
}

func (i *Installer) Remove(ctx context.Context, id string) error {
	logPath, err := i.logPath(id)
	if err != nil {
		return err
	}

	c, err := i.client.LoadContainer(WithNamespace(ctx), id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			_ = os.Remove(logPath)
			return nil
		}
		return err
	}

	var firstErr error
	if task, err := c.Task(WithNamespace(ctx), nil); err == nil {
		if _, err := task.Delete(WithNamespace(ctx), containerdclient.WithProcessKill); err != nil {
			firstErr = warnContainerdCleanupError(log.WithField("installer_id", id), err, "failed to delete containerd installer task during removal")
		}
	} else if !errdefs.IsNotFound(err) {
		return err
	}

	if err := c.Delete(WithNamespace(ctx), containerdclient.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	_ = os.Remove(logPath)
	return firstErr
}

func (i *Installer) Execute(ctx context.Context, spec environment.InstallationSpec, output func([]byte)) (id string, err error) {
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
		oci.WithImageConfigArgs(image, []string{spec.Entrypoint, spec.ScriptPath}),
		oci.WithEnv(spec.Env),
		oci.WithHostname("installer"),
		oci.WithTTY,
		oci.WithMounts(installerMounts(spec)),
		oci.WithAnnotations(labels),
	}
	specOpts = append(specOpts, hostNetworkSpecOpts()...)
	specOpts = append(specOpts, installerResourceSpecOpts(spec.Limits)...)

	cfg := config.Get()
	snapshotID := spec.ID + "-rootfs"
	cleanupSnapshotOnError := true
	defer func() {
		if err == nil || !cleanupSnapshotOnError {
			return
		}
		if cleanupErr := cleanupContainerdSnapshot(context.Background(), i.client, cfg.Containerd.Snapshotter, snapshotID, log.WithField("installer_id", spec.ID)); cleanupErr != nil {
			log.WithField("installer_id", spec.ID).WithField("error", cleanupErr).Warn("failed to cleanup containerd installer snapshot after create error")
		}
	}()

	container, err := i.client.NewContainer(
		ctx,
		spec.ID,
		containerdclient.WithImage(image),
		containerdclient.WithImageName(strings.TrimPrefix(spec.Image, "~")),
		containerdclient.WithSnapshotter(cfg.Containerd.Snapshotter),
		containerdclient.WithNewSnapshot(snapshotID, image),
		containerdclient.WithNewSpec(specOpts...),
		containerdclient.WithRuntime(cfg.Containerd.Runtime, nil),
		containerdclient.WithContainerLabels(labels),
	)
	if err != nil {
		if cleanupErr := i.Remove(context.Background(), spec.ID); cleanupErr != nil {
			log.WithField("installer_id", spec.ID).WithField("error", cleanupErr).Warn("failed to cleanup partially created containerd installer container")
		}
		return "", errors.Wrap(err, "environment/containerd: failed to create installer container")
	}
	id = spec.ID
	cleanupSnapshotOnError = false

	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()
	defer stdinW.Close()
	stdoutR, stdoutW := io.Pipe()
	defer stdoutR.Close()
	defer stdoutW.Close()

	logWriter, err := i.openLog(spec.ID)
	if err != nil {
		return id, err
	}
	defer logWriter.Close()

	fifoRoot, err := containerdFIFORoot()
	if err != nil {
		return id, err
	}
	ioOpts := []cio.Opt{
		cio.WithFIFODir(fifoRoot),
		cio.WithStreams(stdinR, stdoutW, stdoutW),
		cio.WithTerminal,
	}
	task, err := container.NewTask(ctx, cio.NewCreator(ioOpts...))
	if err != nil {
		return id, errors.Wrap(err, "environment/containerd: failed to create installer task")
	}

	exitC, err := task.Wait(ctx)
	if err != nil {
		return id, errors.Wrap(err, "environment/containerd: failed to wait on installer task")
	}

	taskIO := task.IO()
	defer func() {
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
	defer func() {
		_ = stdoutW.Close()
		<-done
	}()

	if err := task.Start(ctx); err != nil {
		if _, cleanupErr := task.Delete(ctx, containerdclient.WithProcessKill); cleanupErr != nil {
			warnContainerdCleanupError(log.WithField("installer_id", spec.ID), cleanupErr, "failed to delete containerd installer task after start error")
		}
		return id, errors.Wrap(err, "environment/containerd: failed to start installer task")
	}

	status := <-exitC
	code, _, err := status.Result()
	if err != nil {
		return id, errors.Wrap(err, "environment/containerd: installer task exited with an error")
	}
	if _, err := task.Delete(ctx); err != nil {
		warnContainerdCleanupError(log.WithField("installer_id", spec.ID), err, "failed to delete exited containerd installer task")
	}
	if code != 0 {
		return id, errors.Errorf("environment/containerd: installer task exited with code %d", code)
	}
	return id, nil
}

func (i *Installer) Logs(_ context.Context, id string) (io.ReadCloser, error) {
	logPath, err := i.logPath(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(logPath)
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
	logDirectory, err := containerdLogDirectory()
	if err != nil {
		return nil, err
	}
	if err := ensureContainerdDirectory(logDirectory); err != nil {
		return nil, err
	}
	logPath, err := i.logPath(id)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
}

func (i *Installer) logPath(id string) (string, error) {
	return containerdInstallerLogPath(id)
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
