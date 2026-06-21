package docker

import (
	"bufio"
	"context"
	"io"
	"runtime"
	"strconv"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/docker/docker/api/types/container"
	dockerImage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"

	"github.com/pelican-dev/wings/config"
	"github.com/pelican-dev/wings/environment"
	"github.com/pelican-dev/wings/system"
)

type Installer struct {
	client *client.Client
}

func NewInstaller() (*Installer, error) {
	cli, err := environment.Docker()
	if err != nil {
		return nil, err
	}
	return &Installer{client: cli}, nil
}

func (i *Installer) PullImage(ctx context.Context, image string) error {
	var registryAuth *config.RegistryConfiguration
	for registry, c := range config.Get().Docker.Registries {
		if !strings.HasPrefix(image, registry) {
			continue
		}
		log.WithField("registry", registry).Debug("using authentication for registry")
		registryAuth = &c
		break
	}

	imagePullOptions := dockerImage.PullOptions{All: false, Platform: runtime.GOOS + "/" + runtime.GOARCH}
	if registryAuth != nil {
		b64, err := registryAuth.Base64()
		if err != nil {
			log.WithError(err).Error("failed to get registry auth credentials")
		}
		imagePullOptions.RegistryAuth = b64
	}

	pullCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	r, err := i.client.ImagePull(pullCtx, image, imagePullOptions)
	if err != nil {
		listCtx, listCancel := context.WithTimeout(ctx, 30*time.Second)
		defer listCancel()
		images, ierr := i.client.ImageList(listCtx, dockerImage.ListOptions{})
		if ierr != nil {
			return ierr
		}

		for _, img := range images {
			for _, tag := range img.RepoTags {
				if tag != image {
					continue
				}

				log.WithFields(log.Fields{
					"image": image,
					"err":   err.Error(),
				}).Warn("unable to pull requested image from remote source, however the image exists locally")
				return nil
			}
		}

		return err
	}
	defer r.Close()

	log.WithField("image", image).Debug("pulling docker image... this could take a bit of time")
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		log.Debug(scanner.Text())
	}
	return scanner.Err()
}

func (i *Installer) Remove(ctx context.Context, id string) error {
	err := i.client.ContainerRemove(ctx, id, container.RemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	})
	if err != nil && !client.IsErrNotFound(err) {
		return err
	}
	return nil
}

func (i *Installer) Execute(ctx context.Context, spec environment.InstallationSpec, output func([]byte)) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conf := &container.Config{
		Hostname:     "installer",
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  true,
		OpenStdin:    true,
		Tty:          true,
		Cmd:          []string{spec.Entrypoint, spec.ScriptPath},
		Image:        spec.Image,
		Env:          spec.Env,
		Labels: map[string]string{
			"Service":       "Pelican",
			"ContainerType": "server_installer",
		},
	}

	cfg := config.Get()
	tmpfsSize := strconv.Itoa(int(cfg.Docker.TmpfsSize))
	resources := spec.Limits.AsContainerResources()
	resources.PidsLimit = nil
	hostConf := &container.HostConfig{
		Mounts: []mount.Mount{
			{
				Target:   "/mnt/server",
				Source:   spec.ServerPath,
				Type:     mount.TypeBind,
				ReadOnly: false,
			},
			{
				Target:   "/mnt/install",
				Source:   spec.TempPath,
				Type:     mount.TypeBind,
				ReadOnly: false,
			},
		},
		Resources: resources,
		Tmpfs: map[string]string{
			"/tmp": "rw,exec,nosuid,size=" + tmpfsSize + "M",
		},
		DNS:         cfg.Docker.Network.Dns,
		LogConfig:   cfg.Docker.ContainerLogConfig(),
		NetworkMode: container.NetworkMode(cfg.Docker.Network.Mode),
		UsernsMode:  container.UsernsMode(cfg.Docker.UsernsMode),
	}

	var netConf *network.NetworkingConfig
	serverNetConfig := config.Get().Docker.Network
	if serverNetConfig.Driver == "macvlan" && spec.Allocations.DefaultMapping != nil {
		netConf = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				serverNetConfig.Name: {
					IPAMConfig: &network.EndpointIPAMConfig{
						IPv4Address: spec.Allocations.DefaultMapping.Ip,
					},
					IPAddress: spec.Allocations.DefaultMapping.Ip,
					Gateway:   serverNetConfig.Interfaces.V4.Gateway,
				},
			},
		}
	}

	r, err := i.client.ContainerCreate(ctx, conf, hostConf, netConf, nil, spec.ID)
	if err != nil {
		return "", err
	}

	if err := i.client.ContainerStart(ctx, r.ID, container.StartOptions{}); err != nil {
		return r.ID, err
	}

	streamDone := make(chan error, 1)
	go func(id string) {
		streamDone <- i.streamOutput(ctx, id, output)
	}(r.ID)

	sChan, eChan := i.client.ContainerWait(ctx, r.ID, container.WaitConditionNotRunning)
	var waitStatus container.WaitResponse
	select {
	case err := <-eChan:
		if err != nil {
			cancel()
			if streamErr := <-streamDone; streamErr != nil {
				log.WithFields(log.Fields{"container_id": r.ID, "error": streamErr}).Warn("error connecting to server install stream output")
			}
			return r.ID, err
		}
	case waitStatus = <-sChan:
	}

	if streamErr := <-streamDone; streamErr != nil {
		log.WithFields(log.Fields{"container_id": r.ID, "error": streamErr}).Warn("error connecting to server install stream output")
	}
	if err := installerExitError(waitStatus); err != nil {
		return r.ID, err
	}
	return r.ID, nil
}

func (i *Installer) Logs(ctx context.Context, id string) (io.ReadCloser, error) {
	reader, err := i.client.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     false,
	})
	if err != nil && !client.IsErrNotFound(err) {
		return nil, err
	}
	if reader == nil {
		return io.NopCloser(strings.NewReader("")), nil
	}
	return reader, nil
}

func (i *Installer) streamOutput(ctx context.Context, id string, output func([]byte)) error {
	reader, err := i.client.ContainerLogs(ctx, id, container.LogsOptions{ShowStdout: true, ShowStderr: true, Follow: true})
	if err != nil {
		return err
	}
	defer reader.Close()

	err = system.ScanReader(reader, output)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func installerExitError(status container.WaitResponse) error {
	if status.StatusCode == 0 {
		return nil
	}
	if status.Error != nil && status.Error.Message != "" {
		return errors.Errorf("environment/docker: installer container exited with code %d: %s", status.StatusCode, status.Error.Message)
	}
	return errors.Errorf("environment/docker: installer container exited with code %d", status.StatusCode)
}
