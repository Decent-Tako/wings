package containerd

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	cgroup1 "github.com/containerd/cgroups/v3/cgroup1/stats"
	cgroup2 "github.com/containerd/cgroups/v3/cgroup2/stats"
	apitypes "github.com/containerd/containerd/api/types"
	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/pelican-dev/wings/config"
	"github.com/pelican-dev/wings/environment"
)

func (e *Environment) Exists() (bool, error) {
	_, err := e.container(context.Background())
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (e *Environment) Create() (err error) {
	ctx := e.context(context.Background())
	e.mu.RLock()
	imageName := e.meta.Image
	e.mu.RUnlock()

	if _, err := e.client.LoadContainer(ctx, e.Id); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return errors.Wrap(err, "environment/containerd: failed to inspect container")
	}

	if err := e.validateHostNetworkingOnly(); err != nil {
		return err
	}

	image, err := e.ensureImageExists(ctx, imageName)
	if err != nil {
		return errors.WithStackIf(err)
	}

	cfg := config.Get()
	labels := e.containerLabels()
	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(image),
		oci.WithEnv(e.Configuration.EnvironmentVariables()),
		oci.WithHostname(e.Id),
		oci.WithDomainname(cfg.Docker.Domainname),
		oci.WithTTY,
		oci.WithMounts(e.ociMounts()),
		oci.WithNoNewPrivileges,
		oci.WithRootFSReadonly(),
		oci.WithUIDGID(e.containerUser()),
		oci.WithDroppedCapabilities(containerdCapDrop()),
		oci.WithAnnotations(labels),
	}
	specOpts = append(specOpts, hostNetworkSpecOpts()...)
	specOpts = append(specOpts, resourceSpecOpts(e.Configuration.Limits())...)

	snapshotID := e.snapshotID()
	cleanupSnapshotOnError := true
	defer func() {
		if err == nil || !cleanupSnapshotOnError {
			return
		}
		if cleanupErr := cleanupContainerdSnapshot(context.Background(), e.client, cfg.Containerd.Snapshotter, snapshotID, e.log()); cleanupErr != nil {
			e.log().WithField("error", cleanupErr).Warn("failed to cleanup containerd snapshot after create error")
		}
	}()

	if _, err := e.client.NewContainer(
		ctx,
		e.Id,
		containerdclient.WithImage(image),
		containerdclient.WithImageName(strings.TrimPrefix(imageName, "~")),
		containerdclient.WithSnapshotter(cfg.Containerd.Snapshotter),
		containerdclient.WithNewSnapshot(snapshotID, image),
		containerdclient.WithNewSpec(specOpts...),
		containerdclient.WithRuntime(cfg.Containerd.Runtime, nil),
		containerdclient.WithContainerLabels(labels),
	); err != nil {
		if cleanupErr := e.removeContainer(context.Background()); cleanupErr != nil {
			e.log().WithField("error", cleanupErr).Warn("failed to cleanup partially created containerd container after create error")
		}
		return errors.Wrap(err, "environment/containerd: failed to create container")
	}
	cleanupSnapshotOnError = false

	return nil
}

func (e *Environment) Destroy() error {
	e.SetState(environment.ProcessStoppingState)
	if err := e.removeContainer(context.Background()); err != nil {
		return err
	}
	e.SetState(environment.ProcessOfflineState)
	return nil
}

func (e *Environment) InSituUpdate() error {
	task, err := e.currentTask(context.Background())
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return errors.Wrap(err, "environment/containerd: could not inspect task")
	}

	resources := linuxResources(e.Configuration.Limits())
	if err := task.Update(e.context(context.Background()), containerdclient.WithResources(resources)); err != nil {
		return errors.Wrap(err, "environment/containerd: could not update task resources")
	}
	return nil
}

func (e *Environment) ensureImageExists(ctx context.Context, image string) (containerdclient.Image, error) {
	return ensureContainerdImage(ctx, e.client, image, func(topic, data string) {
		e.Events().Publish(topic, data)
	})
}

func ensureContainerdImage(ctx context.Context, cli clientAPI, image string, publish func(string, string)) (containerdclient.Image, error) {
	ctx = WithNamespace(ctx)
	ref := strings.TrimPrefix(image, "~")
	pullCtx, cancel := imagePullContext(ctx)
	defer cancel()

	if strings.HasPrefix(image, "~") {
		return cli.GetImage(ctx, ref)
	}

	if publish != nil {
		publish(environment.DockerImagePullStarted, "")
		defer publish(environment.DockerImagePullCompleted, "")
	}

	opts := []containerdclient.RemoteOpt{
		containerdclient.WithPullUnpack,
		containerdclient.WithPullSnapshotter(config.Get().Containerd.Snapshotter),
		containerdclient.WithPlatform(runtime.GOOS + "/" + runtime.GOARCH),
	}
	if opt, ok := registryResolverOpt(ref); ok {
		opts = append(opts, opt)
	}

	if publish != nil {
		publish(environment.DockerImagePullStatus, "pulling "+ref)
	}
	pulled, err := cli.Pull(pullCtx, ref, opts...)
	if err != nil {
		local, localErr := cli.GetImage(ctx, ref)
		if localErr == nil {
			log.WithFields(log.Fields{
				"image": ref,
				"err":   err.Error(),
			}).Warn("unable to pull requested image from remote source, however the image exists locally")
			return local, nil
		}
		return nil, errors.Wrapf(err, "environment/containerd: failed to pull %q image for server", ref)
	}
	if publish != nil {
		publish(environment.DockerImagePullStatus, "unpacked "+ref)
	}
	return pulled, nil
}

func imagePullContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := time.Duration(config.Get().Containerd.ImagePullTimeout) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	return context.WithTimeout(ctx, timeout)
}

func registryResolverOpt(ref string) (containerdclient.RemoteOpt, bool) {
	var (
		bestMatch       string
		bestCredentials config.RegistryConfiguration
	)
	for registry, credentials := range config.Get().Docker.Registries {
		if !strings.HasPrefix(ref, registry) {
			continue
		}
		if len(registry) > len(bestMatch) {
			bestMatch = registry
			bestCredentials = credentials
		}
	}
	if bestMatch != "" {
		resolver := docker.NewResolver(docker.ResolverOptions{
			Authorizer: docker.NewDockerAuthorizer(docker.WithAuthCreds(func(string) (string, string, error) {
				return bestCredentials.Username, bestCredentials.Password, nil
			})),
		})
		return containerdclient.WithResolver(resolver), true
	}
	// No configured registry credentials matched. Leaving the resolver unset
	// intentionally preserves containerd's default anonymous/public pull path.
	return nil, false
}

func hostNetworkSpecOpts() []oci.SpecOpts {
	return []oci.SpecOpts{
		oci.WithHostNamespace(specs.NetworkNamespace),
		oci.WithHostHostsFile,
		oci.WithHostResolvconf,
	}
}

func (e *Environment) validateHostNetworkingOnly() error {
	allocations := e.Configuration.Allocations()
	mode := config.Get().Containerd.Network.Mode
	// TODO(T5): replace this guard with CNI/portmap configuration reconciliation
	// when containerd networking supports Wings' Docker bridge behavior.
	if mode == "macvlan" {
		return errors.Wrap(ErrUnsupportedNetwork, "environment/containerd: macvlan requires the later CNI implementation")
	}
	if mode != "host" {
		return errors.Wrapf(ErrUnsupportedNetwork, "environment/containerd: containerd currently supports host networking only; set containerd.network.mode to %q until the later CNI/portmap implementation lands", "host")
	}
	if allocations.ForceOutgoingIP {
		return errors.Wrap(ErrUnsupportedNetwork, "environment/containerd: force_outgoing_ip requires the later CNI/SNAT implementation")
	}
	return nil
}

func (e *Environment) ociMounts() []specs.Mount {
	mounts := e.Configuration.Mounts()
	out := make([]specs.Mount, 0, len(mounts)+1)
	for _, m := range mounts {
		options := []string{"rbind"}
		if m.ReadOnly {
			options = append(options, "ro")
		} else {
			options = append(options, "rw")
		}
		out = append(out, specs.Mount{
			Destination: m.Target,
			Type:        "bind",
			Source:      m.Source,
			Options:     options,
		})
	}

	out = append(out, specs.Mount{
		Destination: "/tmp",
		Type:        "tmpfs",
		Source:      "tmpfs",
		Options: []string{
			"rw",
			"exec",
			"nosuid",
			"size=" + strconv.Itoa(int(config.Get().Docker.TmpfsSize)) + "m",
		},
	})
	return out
}

func (e *Environment) containerLabels() map[string]string {
	confLabels := e.Configuration.Labels()
	labels := make(map[string]string, 2+len(confLabels))
	for key, value := range confLabels {
		labels[key] = value
	}
	labels["Service"] = "Pelican"
	labels["ContainerType"] = "server_process"
	return labels
}

func (e *Environment) containerUser() (uint32, uint32) {
	cfg := config.Get()
	if cfg.System.User.Rootless.Enabled {
		return uint32(cfg.System.User.Rootless.ContainerUID), uint32(cfg.System.User.Rootless.ContainerGID)
	}
	return uint32(cfg.System.User.Uid), uint32(cfg.System.User.Gid)
}

func (e *Environment) snapshotID() string {
	return e.Id + "-rootfs"
}

func (e *Environment) removeContainer(ctx context.Context) error {
	ctx = e.context(ctx)
	c, err := e.client.LoadContainer(ctx, e.Id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}

	task, err := c.Task(ctx, nil)
	var firstErr error
	if err == nil {
		if _, err := task.Delete(ctx, containerdclient.WithProcessKill); err != nil {
			firstErr = warnContainerdCleanupError(e.log(), err, "failed to delete containerd task during container removal")
		}
	} else if !errdefs.IsNotFound(err) {
		return err
	}

	e.closeAttach()
	if err := c.Delete(ctx, containerdclient.WithSnapshotCleanup); err != nil {
		return warnContainerdCleanupError(e.log(), err, "failed to delete containerd container during container removal")
	}
	return firstErr
}

func (e *Environment) decodeMetric(metric *apitypes.Metric) (uint64, uint64) {
	if metric == nil || metric.Data == nil {
		return 0, 0
	}

	switch {
	case typeurl.Is(metric.Data, (*cgroup1.Metrics)(nil)):
		var data cgroup1.Metrics
		if err := typeurl.UnmarshalTo(metric.Data, &data); err != nil {
			return 0, 0
		}
		return cgroup1Memory(&data), cgroup1CPU(&data)
	case typeurl.Is(metric.Data, (*cgroup2.Metrics)(nil)):
		var data cgroup2.Metrics
		if err := typeurl.UnmarshalTo(metric.Data, &data); err != nil {
			return 0, 0
		}
		return cgroup2Memory(&data), cgroup2CPU(&data)
	default:
		return 0, 0
	}
}

func cgroup1Memory(data *cgroup1.Metrics) uint64 {
	memory := data.GetMemory()
	if memory == nil || memory.GetUsage() == nil {
		return 0
	}
	usage := memory.GetUsage().GetUsage()
	if inactive := memory.GetTotalInactiveFile(); inactive > 0 && inactive < usage {
		return usage - inactive
	}
	if inactive := memory.GetInactiveFile(); inactive < usage {
		return usage - inactive
	}
	return usage
}

func cgroup1CPU(data *cgroup1.Metrics) uint64 {
	if data.GetCPU() == nil || data.GetCPU().GetUsage() == nil {
		return 0
	}
	return data.GetCPU().GetUsage().GetTotal()
}

func cgroup2Memory(data *cgroup2.Metrics) uint64 {
	memory := data.GetMemory()
	if memory == nil {
		return 0
	}
	usage := memory.GetUsage()
	if inactive := memory.GetInactiveFile(); inactive < usage {
		return usage - inactive
	}
	return usage
}

func cgroup2CPU(data *cgroup2.Metrics) uint64 {
	if data.GetCPU() == nil {
		return 0
	}
	return data.GetCPU().GetUsageUsec() * uint64(time.Microsecond)
}

func containerdCapDrop() []string {
	return []string{
		"CAP_SETPCAP",
		"CAP_MKNOD",
		"CAP_AUDIT_WRITE",
		"CAP_NET_RAW",
		"CAP_DAC_OVERRIDE",
		"CAP_FOWNER",
		"CAP_FSETID",
		"CAP_NET_BIND_SERVICE",
		"CAP_SYS_CHROOT",
		"CAP_SETFCAP",
		"CAP_SYS_PTRACE",
	}
}
