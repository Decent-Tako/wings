package containerd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	cgroup1 "github.com/containerd/cgroups/v3/cgroup1/stats"
	cgroup2 "github.com/containerd/cgroups/v3/cgroup2/stats"
	eventtypes "github.com/containerd/containerd/api/events"
	apitypes "github.com/containerd/containerd/api/types"
	containerdclient "github.com/containerd/containerd/v2/client"
	containerdcontainers "github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/content"
	ctrevents "github.com/containerd/containerd/v2/core/events"
	containerdimages "github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/mount"
	ctrruntime "github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/pelican-dev/wings/config"
	"github.com/pelican-dev/wings/environment"
	"github.com/pelican-dev/wings/remote"
)

func TestCreatePullsImageAndCreatesContainer(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	cli.loadErr = errdefs.ErrNotFound
	cli.pullImage = fakeImage{name: "example.com/server:latest"}

	if err := env.Create(); err != nil {
		t.Fatalf("Create() returned error: %v", err)
	}

	if cli.pullRef != "example.com/server:latest" {
		t.Fatalf("expected image pull for server image, got %q", cli.pullRef)
	}
	if cli.newContainerID != env.Id {
		t.Fatalf("expected NewContainer for %q, got %q", env.Id, cli.newContainerID)
	}
	if len(cli.newContainerOpts) == 0 {
		t.Fatal("expected container creation options to be passed")
	}
}

func TestCreateUsesLocalImageWhenImageIsPrefixedWithTilde(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	env.SetProcessMetadata(environment.ProcessMetadata{Image: "~local/server:latest"})
	cli.loadErr = errdefs.ErrNotFound
	cli.getImage = fakeImage{name: "local/server:latest"}
	before := time.Now()

	if err := env.Create(); err != nil {
		t.Fatalf("Create() returned error: %v", err)
	}

	if cli.pullRef != "" {
		t.Fatalf("expected local image path to skip Pull, pulled %q", cli.pullRef)
	}
	if cli.getRef != "local/server:latest" {
		t.Fatalf("expected GetImage without tilde, got %q", cli.getRef)
	}
	assertContextDeadlineWithin(t, "local GetImage", cli.getCtx, before, 15*time.Minute+time.Second)
}

func TestCreateRejectsUnsupportedContainerdNetworking(t *testing.T) {
	t.Run("non host mode", func(t *testing.T) {
		env, cli := newContainerdTestEnvironment(t)
		cli.loadErr = errdefs.ErrNotFound
		config.Update(func(c *config.Configuration) {
			c.Containerd.Network.Mode = "bridge"
		})

		err := env.Create()
		if err == nil || !strings.Contains(err.Error(), "containerd.network.mode") {
			t.Fatalf("expected host networking validation error, got %v", err)
		}
	})

	t.Run("force outgoing ip", func(t *testing.T) {
		env, cli := newContainerdTestEnvironment(t)
		cli.loadErr = errdefs.ErrNotFound
		env.Configuration.SetSettings(environment.Settings{
			Allocations: environment.Allocations{ForceOutgoingIP: true},
			Limits:      environment.Limits{MemoryLimit: 128, OOMKiller: true},
		})

		err := env.Create()
		if err == nil || !strings.Contains(err.Error(), "force_outgoing_ip") {
			t.Fatalf("expected force_outgoing_ip validation error, got %v", err)
		}
	})

	t.Run("macvlan", func(t *testing.T) {
		env, cli := newContainerdTestEnvironment(t)
		cli.loadErr = errdefs.ErrNotFound
		config.Update(func(c *config.Configuration) {
			c.Containerd.Network.Mode = "macvlan"
		})

		err := env.Create()
		if err == nil || !strings.Contains(err.Error(), "macvlan") {
			t.Fatalf("expected macvlan validation error, got %v", err)
		}
	})
}

func TestEnsureContainerdImageFallsBackToLocalImageAfterPullFailure(t *testing.T) {
	newContainerdTestConfig(t)
	config.Update(func(c *config.Configuration) {
		c.Containerd.Namespace = "test-pelican"
	})
	cli := &fakeClient{
		pullErr:  io.ErrUnexpectedEOF,
		getImage: fakeImage{name: "example.com/server:latest"},
	}

	img, err := ensureContainerdImage(context.Background(), cli, "example.com/server:latest", nil)
	if err != nil {
		t.Fatalf("ensureContainerdImage() returned error: %v", err)
	}
	if img.Name() != "example.com/server:latest" {
		t.Fatalf("expected local fallback image, got %q", img.Name())
	}
	if cli.pullRef == "" || cli.getRef == "" {
		t.Fatalf("expected both Pull and GetImage to be attempted, pull=%q get=%q", cli.pullRef, cli.getRef)
	}
	if cli.pullNamespace != "test-pelican" || cli.getNamespace != "test-pelican" {
		t.Fatalf("expected Pull and fallback GetImage to use namespace test-pelican, pull=%q get=%q", cli.pullNamespace, cli.getNamespace)
	}
}

func TestEnsureContainerdImageUnpacksLocalImage(t *testing.T) {
	newContainerdTestConfig(t)
	unpacked := false
	unpackCalls := 0
	cli := &fakeClient{
		getImage: fakeImage{name: "local/server:latest", unpacked: &unpacked, unpackCalls: &unpackCalls},
	}

	img, err := ensureContainerdImage(context.Background(), cli, "~local/server:latest", nil)
	if err != nil {
		t.Fatalf("ensureContainerdImage() returned error: %v", err)
	}
	if img.Name() != "local/server:latest" {
		t.Fatalf("expected local image, got %q", img.Name())
	}
	if unpackCalls != 1 {
		t.Fatalf("expected local image to be unpacked once, got %d calls", unpackCalls)
	}
}

func TestEnsureImageUnpackedUsesBoundedContext(t *testing.T) {
	newContainerdTestConfig(t)
	config.Update(func(c *config.Configuration) {
		c.Containerd.ImagePullTimeout = 1
	})
	unpacked := false
	var isUnpackedCtx context.Context
	var unpackCtx context.Context
	before := time.Now()
	img := fakeImage{
		name:          "local/server:latest",
		unpacked:      &unpacked,
		isUnpackedCtx: &isUnpackedCtx,
		unpackCtx:     &unpackCtx,
	}

	if _, err := ensureImageUnpacked(context.Background(), img, "overlayfs"); err != nil {
		t.Fatalf("ensureImageUnpacked() returned error: %v", err)
	}

	for name, ctx := range map[string]context.Context{
		"IsUnpacked": isUnpackedCtx,
		"Unpack":     unpackCtx,
	} {
		if ctx == nil {
			t.Fatalf("expected %s to receive a context", name)
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatalf("expected %s context to have a deadline", name)
		}
		if deadline.Before(before) || deadline.After(before.Add(2*time.Second)) {
			t.Fatalf("expected %s deadline to use configured image timeout, got %s from start %s", name, deadline, before)
		}
	}
}

func TestEnsureContainerdImageUnpacksFallbackImageAfterPullFailure(t *testing.T) {
	newContainerdTestConfig(t)
	unpacked := false
	unpackCalls := 0
	cli := &fakeClient{
		pullErr:  io.ErrUnexpectedEOF,
		getImage: fakeImage{name: "example.com/server:latest", unpacked: &unpacked, unpackCalls: &unpackCalls},
	}

	if _, err := ensureContainerdImage(context.Background(), cli, "example.com/server:latest", nil); err != nil {
		t.Fatalf("ensureContainerdImage() returned error: %v", err)
	}
	if unpackCalls != 1 {
		t.Fatalf("expected fallback image to be unpacked once, got %d calls", unpackCalls)
	}
}

func TestEnsureContainerdImageDoesNotPublishCompletedAfterPullFailure(t *testing.T) {
	newContainerdTestConfig(t)
	cli := &fakeClient{
		pullErr: io.ErrUnexpectedEOF,
		getErr:  errdefs.ErrNotFound,
	}
	var topics []string

	_, err := ensureContainerdImage(context.Background(), cli, "example.com/server:latest", func(topic, _ string) {
		topics = append(topics, topic)
	})
	if err == nil {
		t.Fatal("expected pull failure to return an error")
	}
	for _, topic := range topics {
		if topic == environment.DockerImagePullCompleted {
			t.Fatalf("expected failed pull not to publish completed event, got topics %v", topics)
		}
	}
}

func TestOnBeforeStartPropagatesCallerCancellationToCreate(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	cli.loadErr = errdefs.ErrNotFound
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := env.OnBeforeStart(ctx)
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("expected caller cancellation from create path, got %v", err)
	}
	if cli.pullRef == "" {
		t.Fatal("expected OnBeforeStart to reach image pull with caller context")
	}
}

func TestRegistryResolverOptSkipsUnmatchedPublicImage(t *testing.T) {
	newContainerdTestConfig(t)
	config.Update(func(c *config.Configuration) {
		c.Docker.Registries = map[string]config.RegistryConfiguration{
			"registry.example.com/private": {Username: "user", Password: "pass"},
		}
	})

	opt, ok := registryResolverOpt("docker.io/library/alpine:latest")
	if ok {
		t.Fatalf("expected public image not to use configured registry resolver, got %v", opt)
	}
	if opt != nil {
		t.Fatalf("expected nil resolver opt for unmatched public image, got %v", opt)
	}
}

func TestRegistryResolverOptRequiresRegistryBoundary(t *testing.T) {
	newContainerdTestConfig(t)
	config.Update(func(c *config.Configuration) {
		c.Docker.Registries = map[string]config.RegistryConfiguration{
			"ghcr.io": {Username: "user", Password: "pass"},
		}
	})

	if _, ok := registryResolverOpt("ghcr.io.malicious.example/library/server:latest"); ok {
		t.Fatal("expected registry credentials not to match a hostname prefix")
	}
	if _, ok := registryResolverOpt("ghcr.io/decent-tako/server:latest"); !ok {
		t.Fatal("expected registry credentials to match on a path boundary")
	}
}

func TestCreateCleansSnapshotWhenNewContainerFails(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	cli.loadErr = errdefs.ErrNotFound
	cli.pullImage = fakeImage{name: "example.com/server:latest"}
	cli.newContainerErr = io.ErrUnexpectedEOF
	cli.snapshotter = &fakeSnapshotter{}
	before := time.Now()

	err := env.Create()
	if err == nil || !strings.Contains(err.Error(), "failed to create container") {
		t.Fatalf("expected create error, got %v", err)
	}
	if len(cli.snapshotter.removed) != 1 || cli.snapshotter.removed[0] != env.snapshotID() {
		t.Fatalf("expected snapshot %q to be removed, got %v", env.snapshotID(), cli.snapshotter.removed)
	}
	if len(cli.snapshotter.removeContexts) != 1 {
		t.Fatalf("expected one snapshot cleanup context, got %d", len(cli.snapshotter.removeContexts))
	}
	assertContextDeadlineWithin(t, "snapshot cleanup", cli.snapshotter.removeContexts[0], before, containerdCleanupTimeout+time.Second)
}

func TestCreatePreservesOriginalErrorWhenSnapshotCleanupFails(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	cli.loadErr = errdefs.ErrNotFound
	cli.pullImage = fakeImage{name: "example.com/server:latest"}
	cli.newContainerErr = io.ErrUnexpectedEOF
	cli.snapshotter = &fakeSnapshotter{removeErr: io.ErrClosedPipe}

	err := env.Create()
	if err == nil || !strings.Contains(err.Error(), "failed to create container") {
		t.Fatalf("expected original create error, got %v", err)
	}
	if strings.Contains(err.Error(), "closed pipe") {
		t.Fatalf("expected snapshot cleanup error not to replace original error, got %v", err)
	}
}

func TestImagePullContextPreservesCallerCancellation(t *testing.T) {
	newContainerdTestConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	pullCtx, pullCancel := imagePullContext(ctx)
	defer pullCancel()

	select {
	case <-pullCtx.Done():
		if pullCtx.Err() != context.Canceled {
			t.Fatalf("expected caller cancellation, got %v", pullCtx.Err())
		}
	default:
		t.Fatal("expected pull context to be canceled with caller context")
	}
}

func TestHostNetworkSpecOptsMountNameResolutionFiles(t *testing.T) {
	var spec oci.Spec
	for _, opt := range hostNetworkSpecOpts() {
		if err := opt(context.Background(), nil, nil, &spec); err != nil {
			t.Fatalf("hostNetworkSpecOpts() returned error: %v", err)
		}
	}

	for _, expected := range []string{"/etc/hosts", "/etc/resolv.conf"} {
		found := false
		for _, mount := range spec.Mounts {
			if mount.Destination == expected && mount.Source == expected && mount.Type == "bind" && strings.Join(mount.Options, ",") == "rbind,ro" {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected host network spec opts to bind mount %s read-only, got %#v", expected, spec.Mounts)
		}
	}
}

func TestContainerdTmpfsMountsUseStickyWorldWritableMode(t *testing.T) {
	env, _ := newContainerdTestEnvironment(t)
	serverTmp := findMount(t, env.ociMounts(), "/tmp")
	if !mountHasOption(serverTmp, "mode=1777") {
		t.Fatalf("expected server /tmp tmpfs to set sticky mode, got %#v", serverTmp.Options)
	}

	spec := newContainerdTestInstallationSpec(t)
	installerTmp := findMount(t, installerMounts(spec), "/tmp")
	if !mountHasOption(installerTmp, "mode=1777") {
		t.Fatalf("expected installer /tmp tmpfs to set sticky mode, got %#v", installerTmp.Options)
	}
}

func TestContainerdCapDropMatchesDockerBackendPolicy(t *testing.T) {
	expected := []string{
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
	got := containerdCapDrop()
	if len(got) != len(expected) {
		t.Fatalf("expected %d dropped capabilities, got %d: %v", len(expected), len(got), got)
	}
	for i := range expected {
		if got[i] != expected[i] {
			t.Fatalf("expected dropped capability %d to be %q, got %q", i, expected[i], got[i])
		}
	}
}

func findMount(t *testing.T, mounts []specs.Mount, destination string) specs.Mount {
	t.Helper()
	for _, mount := range mounts {
		if mount.Destination == destination {
			return mount
		}
	}
	t.Fatalf("expected mount for %s in %#v", destination, mounts)
	return specs.Mount{}
}

func mountHasOption(mount specs.Mount, option string) bool {
	for _, got := range mount.Options {
		if got == option {
			return true
		}
	}
	return false
}

func assertContextDeadlineWithin(t *testing.T, name string, ctx context.Context, before time.Time, timeout time.Duration) {
	t.Helper()
	if ctx == nil {
		t.Fatalf("expected %s to receive a context", name)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("expected %s context to have a deadline", name)
	}
	if deadline.Before(before) || deadline.After(before.Add(timeout)) {
		t.Fatalf("expected %s deadline within %s, got %s from start %s", name, timeout, deadline, before)
	}
}

func TestAttachDeletesNewTaskWhenWaitFails(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	task := &fakeTask{waitErr: io.ErrClosedPipe}
	cli.container = &fakeContainer{
		id:      env.Id,
		taskErr: errdefs.ErrNotFound,
		newTask: task,
		labels:  map[string]string{},
	}

	err := env.Attach(context.Background())
	if err == nil || !strings.Contains(err.Error(), "failed to wait on task") {
		t.Fatalf("expected wait error from Attach, got %v", err)
	}
	if task.deleteCalls != 1 {
		t.Fatalf("expected new task to be deleted after Wait failure, got %d deletes", task.deleteCalls)
	}
}

func TestAttachCleansNewTaskWhenAnotherAttachWinsRace(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	task := &fakeTask{
		waitHook: func() {
			attachContainerdTestStdin(t, env)
		},
	}
	cli.container = &fakeContainer{
		id:      env.Id,
		taskErr: errdefs.ErrNotFound,
		newTask: task,
		labels:  map[string]string{},
	}

	if err := env.Attach(context.Background()); err != nil {
		t.Fatalf("Attach() returned error: %v", err)
	}
	if task.deleteCalls != 1 {
		t.Fatalf("expected duplicate new task to be deleted after attach race, got %d deletes", task.deleteCalls)
	}
	if !env.IsAttached() {
		t.Fatal("expected original attach state to remain attached")
	}
}

func TestStartKeepsTaskWaitAliveAfterAttachContextCanceled(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	task := &fakeTask{waitOnContextCancel: true}
	container := &fakeContainer{
		id:      env.Id,
		taskErr: errdefs.ErrNotFound,
		newTask: task,
		labels:  map[string]string{},
	}
	cli.loadErr = errdefs.ErrNotFound
	cli.container = container

	if err := env.Start(context.Background()); err != nil {
		t.Fatalf("Start() returned error: %v", err)
	}
	defer env.closeAttach()

	if task.waitCtx == nil {
		t.Fatal("expected Attach to register task Wait")
	}
	if err := task.waitCtx.Err(); err != nil {
		t.Fatalf("expected task Wait context to survive Start attach timeout cancellation, got %v", err)
	}
	if !env.IsAttached() {
		t.Fatal("expected environment to remain attached after successful Start")
	}
}

func TestWatchExitIgnoresCanceledWaitContext(t *testing.T) {
	env, _ := newContainerdTestEnvironment(t)
	task := &fakeTask{status: containerdclient.Running}
	attachContainerdTestStdin(t, env)
	env.SetState(environment.ProcessRunningState)

	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	exitC := make(chan containerdclient.ExitStatus, 1)
	exitC <- *containerdclient.NewExitStatus(0, time.Now(), context.Canceled)
	close(exitC)

	env.watchExit(waitCtx, exitC, task)

	if task.deleteCalls != 0 {
		t.Fatalf("expected canceled wait not to delete the running task, got %d deletes", task.deleteCalls)
	}
	if !env.IsAttached() {
		t.Fatal("expected canceled wait not to close attach state")
	}
	if env.State() != environment.ProcessRunningState {
		t.Fatalf("expected canceled wait not to mark server offline, got %q", env.State())
	}
}

func TestWatchExitRecordsNonZeroCodeForErrorStatus(t *testing.T) {
	env, _ := newContainerdTestEnvironment(t)
	task := &fakeTask{status: containerdclient.Stopped}
	attachContainerdTestStdin(t, env)
	exitC := make(chan containerdclient.ExitStatus, 1)
	exitC <- *containerdclient.NewExitStatus(0, time.Now(), io.ErrClosedPipe)
	close(exitC)

	env.watchExit(context.Background(), exitC, task)

	code, _, err := env.ExitState()
	if err != nil {
		t.Fatalf("ExitState() returned error: %v", err)
	}
	if code == 0 {
		t.Fatal("expected error exit status to be recorded as non-zero")
	}
}

func TestWatchExitRetriesWaitErrorWhenTaskIsStillRunning(t *testing.T) {
	env, _ := newContainerdTestEnvironment(t)
	waitRegistered := make(chan struct{}, 1)
	task := &fakeTask{
		status: containerdclient.Running,
		waitCh: make(chan containerdclient.ExitStatus),
		waitHook: func() {
			select {
			case waitRegistered <- struct{}{}:
			default:
			}
		},
	}
	attachContainerdTestStdin(t, env)
	env.SetState(environment.ProcessRunningState)
	firstExitC := make(chan containerdclient.ExitStatus, 1)
	firstExitC <- *containerdclient.NewExitStatus(0, time.Now(), io.ErrClosedPipe)
	close(firstExitC)
	waitCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		env.watchExit(waitCtx, firstExitC, task)
	}()

	select {
	case <-waitRegistered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for watchExit to re-register task wait")
	}
	if task.deleteCalls != 0 {
		t.Fatalf("expected wait error for running task not to delete task, got %d deletes", task.deleteCalls)
	}
	if !env.IsAttached() {
		t.Fatal("expected wait error for running task to keep attach state")
	}
	if env.State() != environment.ProcessRunningState {
		t.Fatalf("expected wait error for running task to preserve running state, got %q", env.State())
	}

	task.status = containerdclient.Stopped
	close(task.waitCh)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for watchExit to stop after closing retried wait channel")
	}
}

func TestWatchExitClosesAttachWhenWaitChannelClosesAfterTaskStops(t *testing.T) {
	env, _ := newContainerdTestEnvironment(t)
	task := &fakeTask{status: containerdclient.Stopped}
	attachContainerdTestStdin(t, env)
	env.SetState(environment.ProcessRunningState)
	exitC := make(chan containerdclient.ExitStatus)
	close(exitC)

	env.watchExit(context.Background(), exitC, task)

	if env.IsAttached() {
		t.Fatal("expected closed wait channel to close attach state after task stopped")
	}
	if env.State() != environment.ProcessOfflineState {
		t.Fatalf("expected closed wait channel to mark server offline after task stopped, got %q", env.State())
	}
}

func TestStartReattachesRunningTaskAndRestoresStartedAt(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	startedAt := time.Now().Add(-2 * time.Minute).UTC()
	task := &fakeTask{
		status: containerdclient.Running,
		waitCh: make(chan containerdclient.ExitStatus),
	}
	cli.container = &fakeContainer{
		id:     env.Id,
		task:   task,
		labels: map[string]string{labelStartedAt: startedAt.Format(time.RFC3339Nano)},
	}
	defer env.closeAttach()

	if err := env.Start(context.Background()); err != nil {
		t.Fatalf("Start() returned error: %v", err)
	}
	if task.startCalls != 0 {
		t.Fatalf("expected running task reattach to skip Start, got %d calls", task.startCalls)
	}
	if env.State() != environment.ProcessRunningState {
		t.Fatalf("expected running state after reattach, got %q", env.State())
	}

	uptime, err := env.Uptime(context.Background())
	if err != nil {
		t.Fatalf("Uptime() returned error: %v", err)
	}
	if uptime <= 0 {
		t.Fatalf("expected restored uptime to be positive, got %d", uptime)
	}
}

func TestStartPreservesRunningStateWhenRunningTaskReattachFails(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	task := &fakeTask{
		status:  containerdclient.Running,
		waitErr: io.ErrClosedPipe,
	}
	cli.container = &fakeContainer{
		id:     env.Id,
		task:   task,
		labels: map[string]string{},
	}

	err := env.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "failed to wait on task") {
		t.Fatalf("expected reattach wait error, got %v", err)
	}
	if env.State() != environment.ProcessRunningState {
		t.Fatalf("expected failed reattach to preserve running state while task is live, got %q", env.State())
	}
}

func TestTerminateKillsRunningTaskAndSetsOffline(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	task := &fakeTask{status: containerdclient.Running}
	cli.container = &fakeContainer{id: env.Id, task: task, labels: map[string]string{}}
	env.SetState(environment.ProcessRunningState)

	if err := env.Terminate(context.Background(), "SIGTERM"); err != nil {
		t.Fatalf("Terminate() returned error: %v", err)
	}
	if len(task.killed) == 0 || task.killed[0] != syscall.SIGTERM {
		t.Fatalf("expected SIGTERM kill, got %v", task.killed)
	}
	if env.State() != environment.ProcessOfflineState {
		t.Fatalf("expected offline state after terminate, got %q", env.State())
	}
}

func TestTerminateRespectsCallerContextBeforeEscalating(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	task := &fakeTask{status: containerdclient.Running, stayRunningAfterKill: true}
	cli.container = &fakeContainer{id: env.Id, task: task, labels: map[string]string{}}
	env.SetState(environment.ProcessRunningState)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := env.Terminate(ctx, "SIGTERM")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded from caller context, got %v", err)
	}
	if len(task.killed) != 1 || task.killed[0] != syscall.SIGTERM {
		t.Fatalf("expected only the requested SIGTERM before caller timeout, got %v", task.killed)
	}
}

func TestStartCleansCreatedTaskAndContainerWhenStartFails(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	task := &fakeTask{startErr: io.ErrClosedPipe}
	container := &fakeContainer{
		id:      env.Id,
		taskErr: errdefs.ErrNotFound,
		newTask: task,
		labels:  map[string]string{},
	}
	cli.loadErr = errdefs.ErrNotFound
	cli.container = container

	err := env.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "failed to start task") {
		t.Fatalf("expected task start error, got %v", err)
	}
	if task.deleteCalls == 0 {
		t.Fatal("expected failed start to delete the created task")
	}
	if !container.deleted {
		t.Fatal("expected failed start to delete the container with snapshot cleanup")
	}
	if env.IsAttached() {
		t.Fatal("expected failed start to close attach state")
	}
	if env.State() != environment.ProcessOfflineState {
		t.Fatalf("expected failed start to leave environment offline, got %q", env.State())
	}
}

func TestRemoveContainerIsIdempotent(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	cli.loadErr = errdefs.ErrNotFound
	if err := env.removeContainer(context.Background()); err != nil {
		t.Fatalf("expected missing container removal to be nil, got %v", err)
	}

	cli.loadErr = nil
	cli.container = &fakeContainer{
		id:        env.Id,
		taskErr:   errdefs.ErrNotFound,
		deleteErr: errdefs.ErrNotFound,
		labels:    map[string]string{},
	}
	if err := env.removeContainer(context.Background()); err != nil {
		t.Fatalf("expected already-deleted container removal to be nil, got %v", err)
	}
}

func TestRemoveContainerPreservesTaskDeleteError(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	task := &fakeTask{deleteErr: io.ErrClosedPipe}
	cli.container = &fakeContainer{
		id:        env.Id,
		task:      task,
		deleteErr: io.ErrUnexpectedEOF,
		labels:    map[string]string{},
	}

	err := env.removeContainer(context.Background())
	if err != io.ErrClosedPipe {
		t.Fatalf("expected task delete error to be preserved, got %v", err)
	}
}

func TestRemoveContainerUsesBoundedCleanupContext(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	task := &fakeTask{}
	container := &fakeContainer{
		id:     env.Id,
		task:   task,
		labels: map[string]string{},
	}
	cli.container = container
	before := time.Now()

	if err := env.removeContainer(context.Background()); err != nil {
		t.Fatalf("removeContainer() returned error: %v", err)
	}

	for name, ctx := range map[string]context.Context{
		"task delete":      task.deleteCtx,
		"container delete": container.deleteCtx,
	} {
		if ctx == nil {
			t.Fatalf("expected %s to receive a context", name)
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatalf("expected %s context to have a cleanup deadline", name)
		}
		if deadline.Before(before) || deadline.After(before.Add(containerdCleanupTimeout+time.Second)) {
			t.Fatalf("expected %s deadline to use cleanup timeout, got %s from start %s", name, deadline, before)
		}
	}
}

func TestInSituUpdateUsesBoundedContext(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	task := &fakeTask{status: containerdclient.Running}
	cli.container = &fakeContainer{id: env.Id, task: task, labels: map[string]string{}}
	before := time.Now()

	if err := env.InSituUpdate(); err != nil {
		t.Fatalf("InSituUpdate() returned error: %v", err)
	}

	assertContextDeadlineWithin(t, "task update", task.updateCtx, before, 11*time.Second)
	if namespace := testContainerdNamespace(task.updateCtx); namespace != "pelican" {
		t.Fatalf("expected task update to use namespace pelican, got %q", namespace)
	}
}

func TestDestroyMarksOfflineWhenContainerRemovalFails(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	cli.container = &fakeContainer{
		id:        env.Id,
		taskErr:   errdefs.ErrNotFound,
		deleteErr: io.ErrClosedPipe,
		labels:    map[string]string{},
	}
	env.SetState(environment.ProcessRunningState)

	err := env.Destroy()
	if err != io.ErrClosedPipe {
		t.Fatalf("expected container removal error to be returned, got %v", err)
	}
	if env.State() != environment.ProcessOfflineState {
		t.Fatalf("expected Destroy to mark server offline after removal error, got %q", env.State())
	}
}

func TestStartHandlesImmediateExitAfterWaitBeforeStart(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	task := &fakeTask{
		exitOnStart: true,
		waitCh:      make(chan containerdclient.ExitStatus, 1),
	}
	container := &fakeContainer{
		id:      env.Id,
		taskErr: errdefs.ErrNotFound,
		newTask: task,
		labels:  map[string]string{},
	}
	cli.loadErr = errdefs.ErrNotFound
	cli.container = container

	if err := env.Start(context.Background()); err != nil {
		t.Fatalf("Start() returned error: %v", err)
	}
	waitForContainerdState(t, env, environment.ProcessOfflineState, time.Second)
	if task.deleteCalls == 0 {
		t.Fatal("expected exited immediate task to be deleted")
	}
	if env.IsAttached() {
		t.Fatal("expected immediate exit to close attach state")
	}
}

func TestWaitForStopReturnsForImmediateExit(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	task := &fakeTask{
		status: containerdclient.Running,
		waitCh: make(chan containerdclient.ExitStatus, 1),
	}
	task.waitCh <- *containerdclient.NewExitStatus(0, time.Now(), nil)
	cli.container = &fakeContainer{id: env.Id, task: task, labels: map[string]string{}}
	attachContainerdTestStdin(t, env)
	env.SetProcessMetadata(environment.ProcessMetadata{
		Image: "example.com/server:latest",
		Stop:  remote.ProcessStopConfiguration{Type: remote.ProcessStopCommand, Value: "stop"},
	})
	env.SetState(environment.ProcessRunningState)

	if err := env.WaitForStop(context.Background(), time.Second, true); err != nil {
		t.Fatalf("WaitForStop() returned error: %v", err)
	}
	if len(task.killed) != 0 {
		t.Fatalf("expected immediate exit not to be killed, got %v", task.killed)
	}
}

func TestWaitForStopTerminatesSlowExitWhenRequested(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	task := &fakeTask{
		status: containerdclient.Running,
		waitCh: make(chan containerdclient.ExitStatus),
	}
	cli.container = &fakeContainer{id: env.Id, task: task, labels: map[string]string{}}
	attachContainerdTestStdin(t, env)
	env.SetProcessMetadata(environment.ProcessMetadata{
		Image: "example.com/server:latest",
		Stop:  remote.ProcessStopConfiguration{Type: remote.ProcessStopCommand, Value: "stop"},
	})
	env.SetState(environment.ProcessRunningState)

	if err := env.WaitForStop(context.Background(), 10*time.Millisecond, true); err != nil {
		t.Fatalf("WaitForStop() returned error: %v", err)
	}
	if len(task.killed) == 0 || task.killed[len(task.killed)-1] != syscall.SIGKILL {
		t.Fatalf("expected slow exit to receive SIGKILL, got %v", task.killed)
	}
}

func TestWaitForStopTerminatesWhenWaitStatusReportsDeadline(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	task := &fakeTask{
		status: containerdclient.Running,
		waitCh: make(chan containerdclient.ExitStatus, 1),
	}
	task.waitCh <- *containerdclient.NewExitStatus(0, time.Now(), context.DeadlineExceeded)
	cli.container = &fakeContainer{id: env.Id, task: task, labels: map[string]string{}}
	attachContainerdTestStdin(t, env)
	env.SetProcessMetadata(environment.ProcessMetadata{
		Image: "example.com/server:latest",
		Stop:  remote.ProcessStopConfiguration{Type: remote.ProcessStopCommand, Value: "stop"},
	})
	env.SetState(environment.ProcessRunningState)

	if err := env.WaitForStop(context.Background(), time.Second, true); err != nil {
		t.Fatalf("WaitForStop() returned error: %v", err)
	}
	if len(task.killed) == 0 || task.killed[len(task.killed)-1] != syscall.SIGKILL {
		t.Fatalf("expected wait deadline status to receive SIGKILL, got %v", task.killed)
	}
}

func TestInstallerExecuteCleansSnapshotWhenNewContainerFails(t *testing.T) {
	newContainerdTestConfig(t)
	config.Update(func(c *config.Configuration) {
		c.Containerd.Namespace = "test-pelican"
	})
	spec := newContainerdTestInstallationSpec(t)
	cli := &fakeClient{
		loadErr:         errdefs.ErrNotFound,
		getImage:        fakeImage{name: spec.Image},
		newContainerErr: io.ErrUnexpectedEOF,
		snapshotter:     &fakeSnapshotter{},
	}
	installer := &Installer{client: cli}
	before := time.Now()

	_, err := installer.Execute(context.Background(), spec, func([]byte) {})
	if err == nil || !strings.Contains(err.Error(), "failed to create installer container") {
		t.Fatalf("expected installer create error, got %v", err)
	}
	if len(cli.snapshotter.removed) != 1 || cli.snapshotter.removed[0] != spec.ID+"-rootfs" {
		t.Fatalf("expected installer snapshot %q to be removed, got %v", spec.ID+"-rootfs", cli.snapshotter.removed)
	}
	if len(cli.snapshotter.removeNamespaces) != 1 || cli.snapshotter.removeNamespaces[0] != "test-pelican" {
		t.Fatalf("expected installer snapshot cleanup to use namespace test-pelican, got %v", cli.snapshotter.removeNamespaces)
	}
	if len(cli.snapshotter.removeContexts) != 1 {
		t.Fatalf("expected installer snapshot cleanup context, got %d", len(cli.snapshotter.removeContexts))
	}
	assertContextDeadlineWithin(t, "installer snapshot cleanup", cli.snapshotter.removeContexts[0], before, containerdCleanupTimeout+time.Second)
}

func TestInstallerPullImageUsesConfiguredNamespace(t *testing.T) {
	newContainerdTestConfig(t)
	config.Update(func(c *config.Configuration) {
		c.Containerd.Namespace = "test-pelican"
	})
	cli := &fakeClient{
		pullImage: fakeImage{name: "example.com/installer:latest"},
	}
	installer := &Installer{client: cli}

	if err := installer.PullImage(context.Background(), "example.com/installer:latest"); err != nil {
		t.Fatalf("PullImage() returned error: %v", err)
	}
	if cli.pullNamespace != "test-pelican" {
		t.Fatalf("expected installer pull to use namespace test-pelican, got %q", cli.pullNamespace)
	}
}

func TestInstallerExecuteUsesConfiguredNamespace(t *testing.T) {
	newContainerdTestConfig(t)
	config.Update(func(c *config.Configuration) {
		c.Containerd.Namespace = "test-pelican"
	})
	spec := newContainerdTestInstallationSpec(t)
	task := &fakeTask{waitCh: make(chan containerdclient.ExitStatus, 1)}
	task.waitCh <- *containerdclient.NewExitStatus(0, time.Now(), nil)
	container := &fakeContainer{
		id:      spec.ID,
		newTask: task,
		labels:  map[string]string{},
	}
	cli := &fakeClient{
		container: container,
		getImage:  fakeImage{name: spec.Image},
	}
	installer := &Installer{client: cli}

	if _, err := installer.Execute(context.Background(), spec, func([]byte) {}); err != nil {
		t.Fatalf("Execute() returned error: %v", err)
	}

	for name, namespace := range map[string]string{
		"GetImage":     cli.getNamespace,
		"NewContainer": cli.newContainerNamespace,
		"NewTask":      container.newTaskNamespace,
		"Wait":         task.waitNamespace,
		"Start":        task.startNamespace,
		"Delete":       task.deleteNamespace,
	} {
		if namespace != "test-pelican" {
			t.Fatalf("expected installer %s to use namespace test-pelican, got %q", name, namespace)
		}
	}
}

func TestInstallerExecuteCleansTaskAndContainerWhenStartFails(t *testing.T) {
	newContainerdTestConfig(t)
	spec := newContainerdTestInstallationSpec(t)
	task := &fakeTask{startErr: io.ErrClosedPipe}
	container := &fakeContainer{
		id:      spec.ID,
		newTask: task,
		labels:  map[string]string{},
	}
	cli := &fakeClient{
		container: container,
		getImage:  fakeImage{name: spec.Image},
	}
	installer := &Installer{client: cli}

	id, err := installer.Execute(context.Background(), spec, func([]byte) {})
	if err == nil || !strings.Contains(err.Error(), "failed to start installer task") {
		t.Fatalf("expected installer start error, got %v", err)
	}
	if id != spec.ID {
		t.Fatalf("expected failed installer id %q, got %q", spec.ID, id)
	}
	if task.deleteCalls == 0 {
		t.Fatal("expected failed installer start to delete the task")
	}
	if container.deleted {
		t.Fatal("expected failed installer container to remain available for log copy")
	}
}

func TestInstallerExecuteReturnsErrorForNonZeroExitCode(t *testing.T) {
	newContainerdTestConfig(t)
	spec := newContainerdTestInstallationSpec(t)
	task := &fakeTask{waitCh: make(chan containerdclient.ExitStatus, 1)}
	task.waitCh <- *containerdclient.NewExitStatus(42, time.Now(), nil)
	container := &fakeContainer{
		id:      spec.ID,
		newTask: task,
		labels:  map[string]string{},
	}
	cli := &fakeClient{
		container: container,
		getImage:  fakeImage{name: spec.Image},
	}
	installer := &Installer{client: cli}

	id, err := installer.Execute(context.Background(), spec, func([]byte) {})
	if err == nil || !strings.Contains(err.Error(), "exited with code 42") {
		t.Fatalf("expected non-zero installer exit error, got %v", err)
	}
	if id != spec.ID {
		t.Fatalf("expected failed installer id %q, got %q", spec.ID, id)
	}
	if task.deleteCalls == 0 {
		t.Fatal("expected exited installer task to be deleted")
	}
	if container.deleted {
		t.Fatal("expected failed installer container to remain available for log copy")
	}
}

func TestInstallerExecuteErrorsWhenWaitChannelCloses(t *testing.T) {
	newContainerdTestConfig(t)
	spec := newContainerdTestInstallationSpec(t)
	waitCh := make(chan containerdclient.ExitStatus)
	close(waitCh)
	task := &fakeTask{waitCh: waitCh}
	container := &fakeContainer{
		id:      spec.ID,
		newTask: task,
		labels:  map[string]string{},
	}
	cli := &fakeClient{
		container: container,
		getImage:  fakeImage{name: spec.Image},
	}
	installer := &Installer{client: cli}

	id, err := installer.Execute(context.Background(), spec, func([]byte) {})
	if err == nil || !strings.Contains(err.Error(), "wait channel closed unexpectedly") {
		t.Fatalf("expected closed wait channel error, got %v", err)
	}
	if id != spec.ID {
		t.Fatalf("expected failed installer id %q, got %q", spec.ID, id)
	}
}

func TestInstallerRemoveDeletesLogWhenContainerDeleteFails(t *testing.T) {
	newContainerdTestConfig(t)
	id := "test-installer"
	logPath, err := containerdInstallerLogPath(id)
	if err != nil {
		t.Fatalf("containerdInstallerLogPath() returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatalf("failed to create installer log directory: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("installer log"), 0o600); err != nil {
		t.Fatalf("failed to create installer log: %v", err)
	}
	task := &fakeTask{}
	container := &fakeContainer{
		id:        id,
		task:      task,
		deleteErr: io.ErrClosedPipe,
		labels:    map[string]string{},
	}
	installer := &Installer{client: &fakeClient{container: container}}
	before := time.Now()

	err = installer.Remove(context.Background(), id)
	if err != io.ErrClosedPipe {
		t.Fatalf("expected container delete error to be returned, got %v", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("expected installer log to be removed even after delete failure, stat err=%v", err)
	}
	for name, ctx := range map[string]context.Context{
		"task delete":      task.deleteCtx,
		"container delete": container.deleteCtx,
	} {
		if ctx == nil {
			t.Fatalf("expected installer %s to receive a context", name)
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatalf("expected installer %s context to have a cleanup deadline", name)
		}
		if deadline.Before(before) || deadline.After(before.Add(containerdCleanupTimeout+time.Second)) {
			t.Fatalf("expected installer %s deadline to use cleanup timeout, got %s from start %s", name, deadline, before)
		}
	}
}

func TestRestoredStartedAtTakesPrecedenceOverContainerLabel(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	stateStartedAt := time.Date(2026, 6, 19, 8, 0, 0, 0, time.UTC)
	labelTime := time.Date(2026, 6, 19, 7, 0, 0, 0, time.UTC)
	cli.container = &fakeContainer{
		id:     env.Id,
		labels: map[string]string{labelStartedAt: labelTime.Format(time.RFC3339Nano)},
	}

	env.RestoreStartedAt(stateStartedAt)
	got := env.startedAtOrRestore(context.Background(), time.Now())
	if !got.Equal(stateStartedAt) {
		t.Fatalf("expected state-file started_at %s to win over label, got %s", stateStartedAt, got)
	}
}

func TestPollResourcesPublishesStatsWithUnsupportedNetwork(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	metric := cgroup2Metric(t, 2048, 512, 250)
	task := &fakeTask{status: containerdclient.Running, metric: metric}
	cli.container = &fakeContainer{id: env.Id, task: task, labels: map[string]string{}}
	env.SetState(environment.ProcessRunningState)
	env.setStartedAt(context.Background(), time.Now().Add(-time.Minute))

	ch := make(chan []byte, 4)
	env.Events().On(ch)
	defer env.Events().Off(ch)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- env.pollResources(ctx) }()

	var event struct {
		Topic string            `json:"topic"`
		Data  environment.Stats `json:"data"`
	}
	select {
	case raw := <-ch:
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatalf("failed to decode resource event: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for resource event")
	}

	if event.Topic != environment.ResourceEvent {
		t.Fatalf("expected resource event, got %q", event.Topic)
	}
	if event.Data.Memory != 1536 {
		t.Fatalf("expected inactive file memory to be subtracted, got %d", event.Data.Memory)
	}
	if event.Data.Uptime <= 0 {
		t.Fatalf("expected positive uptime, got %d", event.Data.Uptime)
	}
	if event.Data.Network.RxBytes != 0 || event.Data.Network.TxBytes != 0 {
		t.Fatalf("expected unsupported network stats to remain zero, got %+v", event.Data.Network)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			t.Fatalf("pollResources returned unexpected error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for pollResources to stop")
	}
}

func TestWatchOOMRetriesAfterSubscriptionError(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	originalRetryDelay := oomSubscribeRetryDelay
	oomSubscribeRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { oomSubscribeRetryDelay = originalRetryDelay })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.watchOOM(ctx)
	}()

	cli.errs <- io.ErrClosedPipe
	cli.events <- containerdOOMEnvelope(t, env.Id)

	waitForCondition(t, time.Second, func() bool {
		env.mu.RLock()
		defer env.mu.RUnlock()
		return env.lastOOM
	}, "OOM event after subscription retry")

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for watchOOM to stop")
	}
}

func TestCgroupMemoryCapsInactiveFileAtUsage(t *testing.T) {
	cgroup1Metric := &cgroup1.Metrics{
		Memory: &cgroup1.MemoryStat{
			Usage:             &cgroup1.MemoryEntry{Usage: 1024},
			TotalInactiveFile: 2048,
		},
	}
	if got := cgroup1Memory(cgroup1Metric); got != 0 {
		t.Fatalf("expected cgroup1 inactive file >= usage to report 0, got %d", got)
	}

	cgroup2Metric := &cgroup2.Metrics{
		Memory: &cgroup2.MemoryStat{Usage: 1024, InactiveFile: 2048},
	}
	if got := cgroup2Memory(cgroup2Metric); got != 0 {
		t.Fatalf("expected cgroup2 inactive file >= usage to report 0, got %d", got)
	}
}

func TestDestroyRemovesServerLogs(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	config.Update(func(c *config.Configuration) {
		c.Containerd.LogMaxFiles = 2
	})
	container := &fakeContainer{id: env.Id, labels: map[string]string{}}
	cli.container = container

	logPath, err := env.logPath()
	if err != nil {
		t.Fatalf("logPath() returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatalf("failed to create log directory: %v", err)
	}
	for _, path := range []string{logPath, rotatedLogPath(logPath, 1)} {
		if err := os.WriteFile(path, []byte("old log"), 0o600); err != nil {
			t.Fatalf("failed to write test log %s: %v", path, err)
		}
	}

	if err := env.Destroy(); err != nil {
		t.Fatalf("Destroy() returned error: %v", err)
	}
	if !container.deleted {
		t.Fatal("expected Destroy to delete the container")
	}
	for _, path := range []string{logPath, rotatedLogPath(logPath, 1)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected log %s to be removed, stat err=%v", path, err)
		}
	}
}

func TestDestroyMarksOfflineWhenLogRemovalFails(t *testing.T) {
	env, cli := newContainerdTestEnvironment(t)
	container := &fakeContainer{id: env.Id, labels: map[string]string{}}
	cli.container = container
	env.SetState(environment.ProcessRunningState)

	logPath, err := env.logPath()
	if err != nil {
		t.Fatalf("logPath() returned error: %v", err)
	}
	if err := os.MkdirAll(logPath, 0o700); err != nil {
		t.Fatalf("failed to create log path directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logPath, "child"), []byte("not removable as file"), 0o600); err != nil {
		t.Fatalf("failed to write child file: %v", err)
	}

	err = env.Destroy()
	if err == nil {
		t.Fatal("expected log removal error")
	}
	if !container.deleted {
		t.Fatal("expected Destroy to delete the container")
	}
	if env.State() != environment.ProcessOfflineState {
		t.Fatalf("expected Destroy to mark offline after container removal, got %q", env.State())
	}
}

func TestReadlogTailsRotatedLogs(t *testing.T) {
	env, _ := newContainerdTestEnvironment(t)
	config.Update(func(c *config.Configuration) {
		c.Containerd.LogMaxSize = "12b"
		c.Containerd.LogMaxFiles = 3
	})

	logPath, err := env.logPath()
	if err != nil {
		t.Fatalf("logPath() returned error: %v", err)
	}
	writer, err := newRotatingLogWriter(logPath)
	if err != nil {
		t.Fatalf("newRotatingLogWriter() returned error: %v", err)
	}
	for _, line := range []string{"l01\n", "l02\n", "l03\n", "l04\n", "l05\n", "l06\n", "l07\n"} {
		if _, err := writer.Write([]byte(line)); err != nil {
			t.Fatalf("failed to write log line %q: %v", line, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close log writer: %v", err)
	}
	if _, err := os.Stat(rotatedLogPath(logPath, 1)); err != nil {
		t.Fatalf("expected rotated log file: %v", err)
	}

	lines, err := env.Readlog(5)
	if err != nil {
		t.Fatalf("Readlog() returned error: %v", err)
	}
	expected := []string{"l03", "l04", "l05", "l06", "l07"}
	if strings.Join(lines, ",") != strings.Join(expected, ",") {
		t.Fatalf("expected %v, got %v", expected, lines)
	}
}

func TestReadlogCapsRequestedLines(t *testing.T) {
	env, _ := newContainerdTestEnvironment(t)
	logPath, err := env.logPath()
	if err != nil {
		t.Fatalf("logPath() returned error: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatalf("failed to create log directory: %v", err)
	}
	if err := os.WriteFile(logPath, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatalf("failed to write log file: %v", err)
	}

	lines, err := env.Readlog(maxContainerdReadlogLines + 1)
	if err != nil {
		t.Fatalf("Readlog() returned error: %v", err)
	}
	if strings.Join(lines, ",") != "one,two" {
		t.Fatalf("expected capped log read to return available lines, got %v", lines)
	}
}

func TestContainerdLogPathRejectsUnsafeIdentifier(t *testing.T) {
	newContainerdTestConfig(t)
	if _, err := containerdServerLogPath("../escape"); err == nil {
		t.Fatal("expected unsafe log identifier to be rejected")
	}
}

func TestContainerdDirectoryRejectsRelativePath(t *testing.T) {
	if _, err := cleanContainerdDirectory("relative/path", "containerd.log_directory"); err == nil {
		t.Fatal("expected relative containerd directory to be rejected")
	}
}

func TestContainerdUserIDRejectsOutOfRangeValues(t *testing.T) {
	if _, err := containerdUserID(-1, "test.uid"); err == nil {
		t.Fatal("expected negative uid to be rejected")
	}
	if strconv.IntSize > 32 {
		if _, err := containerdUserID(int(maxContainerdUserID)+1, "test.uid"); err == nil {
			t.Fatal("expected overflowing uid to be rejected")
		}
	}
}

func TestSignalFromStringDefaultsToGracefulStop(t *testing.T) {
	if got := signalFromString(""); got != syscall.SIGTERM {
		t.Fatalf("expected empty signal to default to SIGTERM, got %v", got)
	}
	if got := signalFromString("^C"); got != syscall.SIGINT {
		t.Fatalf("expected ^C to map to SIGINT, got %v", got)
	}
	if got := signalFromString("definitely-not-a-signal"); got != syscall.SIGTERM {
		t.Fatalf("expected unknown signal to default to SIGTERM, got %v", got)
	}
}

func newContainerdTestEnvironment(t *testing.T) (*Environment, *fakeClient) {
	t.Helper()
	newContainerdTestConfig(t)

	cfg := environment.NewConfiguration(environment.Settings{
		Allocations: environment.Allocations{
			DefaultMapping: &environment.DefaultAllocationMapping{Ip: "0.0.0.0", Port: 25565},
			Mappings:       map[string][]int{"0.0.0.0": []int{25565}},
		},
		Limits: environment.Limits{
			MemoryLimit: 128,
			Swap:        0,
			CpuLimit:    100,
			OOMKiller:   true,
		},
		Labels: map[string]string{"test": "true"},
	}, []string{"SERVER_MEMORY=128"})

	cli := &fakeClient{
		pullImage: fakeImage{name: "example.com/server:latest"},
		events:    make(chan *ctrevents.Envelope),
		errs:      make(chan error),
	}
	env := New("test-server", environment.ProcessMetadata{Image: "example.com/server:latest"}, cfg, cli)
	cli.container = &fakeContainer{id: env.Id, labels: map[string]string{}}
	return env, cli
}

func newContainerdTestConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.NewAtPath(filepath.Join(dir, "wings.yml"))
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	cfg.Docker.Network.Mode = "host"
	cfg.Docker.Network.Driver = "bridge"
	cfg.Docker.TmpfsSize = 64
	cfg.Docker.ContainerPidLimit = 512
	cfg.AuthenticationToken = "test-token"
	cfg.Containerd.RuntimeRoot = filepath.Join(dir, "runtime")
	cfg.Containerd.LogDirectory = filepath.Join(dir, "logs")
	cfg.Containerd.LogMaxSize = "5m"
	cfg.Containerd.LogMaxFiles = 1
	cfg.Containerd.ImagePullTimeout = 900
	config.Set(cfg)
}

func newContainerdTestInstallationSpec(t *testing.T) environment.InstallationSpec {
	t.Helper()
	dir := t.TempDir()
	serverPath := filepath.Join(dir, "server")
	tempPath := filepath.Join(dir, "install")
	for _, path := range []string{serverPath, tempPath} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("failed to create %s: %v", path, err)
		}
	}
	return environment.InstallationSpec{
		ID:         "test-installer",
		Image:      "example.com/installer:latest",
		Entrypoint: "/bin/sh",
		ScriptPath: "/mnt/install/install.sh",
		TempPath:   tempPath,
		ServerPath: serverPath,
		Env:        []string{"SERVER_MEMORY=128"},
		Limits: environment.Limits{
			MemoryLimit: 128,
			OOMKiller:   true,
		},
		Allocations: environment.Allocations{
			DefaultMapping: &environment.DefaultAllocationMapping{Ip: "0.0.0.0", Port: 25565},
			Mappings:       map[string][]int{"0.0.0.0": []int{25565}},
		},
	}
}

func cgroup2Metric(t *testing.T, usage, inactiveFile, cpuUsec uint64) *apitypes.Metric {
	t.Helper()
	data, err := typeurl.MarshalAnyToProto(&cgroup2.Metrics{
		Memory: &cgroup2.MemoryStat{Usage: usage, InactiveFile: inactiveFile},
		CPU:    &cgroup2.CPUStat{UsageUsec: cpuUsec},
	})
	if err != nil {
		t.Fatalf("failed to marshal cgroup2 metric: %v", err)
	}
	return &apitypes.Metric{Data: data}
}

func containerdOOMEnvelope(t *testing.T, id string) *ctrevents.Envelope {
	t.Helper()
	evt, err := typeurl.MarshalAny(&eventtypes.TaskOOM{ContainerID: id})
	if err != nil {
		t.Fatalf("failed to marshal OOM event: %v", err)
	}
	return &ctrevents.Envelope{
		Topic: ctrruntime.TaskOOMEventTopic,
		Event: evt,
	}
}

func attachContainerdTestStdin(t *testing.T, env *Environment) {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, stdinR)
	}()
	t.Cleanup(func() {
		_ = stdinW.Close()
		_ = stdinR.Close()
		<-done
	})

	env.mu.Lock()
	env.stdin = stdinW
	env.mu.Unlock()
}

func waitForContainerdState(t *testing.T, env *Environment, state string, timeout time.Duration) {
	t.Helper()
	waitForCondition(t, timeout, func() bool {
		return env.State() == state
	}, "state %q, got %q", state, env.State())
}

func waitForCondition(t *testing.T, timeout time.Duration, ok func() bool, msg string, args ...any) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		if ok() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for "+msg, args...)
		case <-ticker.C:
		}
	}
}

type fakeClient struct {
	container     containerdclient.Container
	loadErr       error
	loadNamespace string

	newContainerID        string
	newContainerOpts      []containerdclient.NewContainerOpts
	newContainerErr       error
	newContainerNamespace string

	getRef       string
	getImage     containerdclient.Image
	getErr       error
	getCtx       context.Context
	getNamespace string

	pullRef       string
	pullImage     containerdclient.Image
	pullErr       error
	pullNamespace string

	events chan *ctrevents.Envelope
	errs   chan error

	snapshotter *fakeSnapshotter
}

func (f *fakeClient) LoadContainer(ctx context.Context, _ string) (containerdclient.Container, error) {
	f.loadNamespace = testContainerdNamespace(ctx)
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return f.container, nil
}

func (f *fakeClient) NewContainer(ctx context.Context, id string, opts ...containerdclient.NewContainerOpts) (containerdclient.Container, error) {
	f.newContainerID = id
	f.newContainerOpts = opts
	f.newContainerNamespace = testContainerdNamespace(ctx)
	if f.newContainerErr != nil {
		return nil, f.newContainerErr
	}
	if f.container == nil {
		f.container = &fakeContainer{id: id, labels: map[string]string{}}
	}
	f.loadErr = nil
	return f.container, nil
}

func (f *fakeClient) GetImage(ctx context.Context, ref string) (containerdclient.Image, error) {
	f.getRef = ref
	f.getCtx = ctx
	f.getNamespace = testContainerdNamespace(ctx)
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getImage != nil {
		return f.getImage, nil
	}
	return nil, errdefs.ErrNotFound
}

func (f *fakeClient) Pull(ctx context.Context, ref string, _ ...containerdclient.RemoteOpt) (containerdclient.Image, error) {
	f.pullRef = ref
	f.pullNamespace = testContainerdNamespace(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	if f.pullImage != nil {
		return f.pullImage, nil
	}
	return nil, errdefs.ErrNotFound
}

func (f *fakeClient) Subscribe(context.Context, ...string) (<-chan *ctrevents.Envelope, <-chan error) {
	if f.events == nil {
		f.events = make(chan *ctrevents.Envelope)
	}
	if f.errs == nil {
		f.errs = make(chan error)
	}
	return f.events, f.errs
}

func (f *fakeClient) SnapshotService(string) snapshots.Snapshotter {
	if f.snapshotter == nil {
		f.snapshotter = &fakeSnapshotter{}
	}
	return f.snapshotter
}

type fakeContainer struct {
	id string

	task          containerdclient.Task
	taskErr       error
	taskNamespace string

	newTask          containerdclient.Task
	newTaskErr       error
	newTaskNamespace string

	labels          map[string]string
	deleteErr       error
	deleted         bool
	deleteCtx       context.Context
	deleteNamespace string
}

func (f *fakeContainer) ID() string { return f.id }

func (f *fakeContainer) Info(context.Context, ...containerdclient.InfoOpts) (containerdcontainers.Container, error) {
	return containerdcontainers.Container{ID: f.id, Labels: f.labels}, nil
}

func (f *fakeContainer) Delete(ctx context.Context, _ ...containerdclient.DeleteOpts) error {
	f.deleted = true
	f.deleteCtx = ctx
	f.deleteNamespace = testContainerdNamespace(ctx)
	return f.deleteErr
}

func (f *fakeContainer) NewTask(ctx context.Context, _ cio.Creator, _ ...containerdclient.NewTaskOpts) (containerdclient.Task, error) {
	f.newTaskNamespace = testContainerdNamespace(ctx)
	if f.newTaskErr != nil {
		return nil, f.newTaskErr
	}
	if f.newTask != nil {
		f.task = f.newTask
		return f.newTask, nil
	}
	f.task = &fakeTask{status: containerdclient.Created}
	return f.task, nil
}

func (f *fakeContainer) Spec(context.Context) (*oci.Spec, error) { return &oci.Spec{}, nil }

func (f *fakeContainer) Task(ctx context.Context, _ cio.Attach) (containerdclient.Task, error) {
	f.taskNamespace = testContainerdNamespace(ctx)
	if f.taskErr != nil {
		return nil, f.taskErr
	}
	if f.task == nil {
		return nil, errdefs.ErrNotFound
	}
	return f.task, nil
}

func (f *fakeContainer) Image(context.Context) (containerdclient.Image, error) {
	return fakeImage{name: "example.com/server:latest"}, nil
}

func (f *fakeContainer) Labels(context.Context) (map[string]string, error) {
	if f.labels == nil {
		f.labels = map[string]string{}
	}
	out := make(map[string]string, len(f.labels))
	for key, value := range f.labels {
		out[key] = value
	}
	return out, nil
}

func (f *fakeContainer) SetLabels(_ context.Context, labels map[string]string) (map[string]string, error) {
	f.labels = make(map[string]string, len(labels))
	for key, value := range labels {
		f.labels[key] = value
	}
	return f.Labels(context.Background())
}

func (f *fakeContainer) Extensions(context.Context) (map[string]typeurl.Any, error) {
	return map[string]typeurl.Any{}, nil
}

func (f *fakeContainer) Update(context.Context, ...containerdclient.UpdateContainerOpts) error {
	return nil
}

func (f *fakeContainer) Checkpoint(context.Context, string, ...containerdclient.CheckpointOpts) (containerdclient.Image, error) {
	return fakeImage{name: "checkpoint"}, nil
}

func (f *fakeContainer) Restore(context.Context, cio.Creator, string) (int, error) { return 0, nil }

type fakeTask struct {
	status containerdclient.ProcessStatus

	waitCh              chan containerdclient.ExitStatus
	waitCtx             context.Context
	waitNamespace       string
	waitErr             error
	waitOnContextCancel bool
	exitOnStart         bool
	waitHook            func()

	metric    *apitypes.Metric
	metricErr error

	startCalls           int
	startNamespace       string
	startErr             error
	deleteCalls          int
	deleteCtx            context.Context
	deleteNamespace      string
	deleteErr            error
	killed               []syscall.Signal
	killErr              error
	stayRunningAfterKill bool
	statusErr            error
	updateCtx            context.Context
}

func (f *fakeTask) ID() string { return "test-server" }

func (f *fakeTask) Pid() uint32 { return 1234 }

func (f *fakeTask) Start(ctx context.Context) error {
	f.startCalls++
	f.startNamespace = testContainerdNamespace(ctx)
	if f.startErr != nil {
		return f.startErr
	}
	if f.exitOnStart {
		if f.waitCh == nil {
			f.waitCh = make(chan containerdclient.ExitStatus, 1)
		}
		f.waitCh <- *containerdclient.NewExitStatus(0, time.Now(), nil)
		close(f.waitCh)
		f.status = containerdclient.Stopped
		return nil
	}
	f.status = containerdclient.Running
	return nil
}

func (f *fakeTask) Delete(ctx context.Context, _ ...containerdclient.ProcessDeleteOpts) (*containerdclient.ExitStatus, error) {
	f.deleteCalls++
	f.deleteCtx = ctx
	f.deleteNamespace = testContainerdNamespace(ctx)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.status = containerdclient.Stopped
	return containerdclient.NewExitStatus(0, time.Now(), nil), nil
}

func (f *fakeTask) Kill(_ context.Context, signal syscall.Signal, _ ...containerdclient.KillOpts) error {
	f.killed = append(f.killed, signal)
	if f.killErr != nil {
		return f.killErr
	}
	if !f.stayRunningAfterKill {
		f.status = containerdclient.Stopped
	}
	return nil
}

func (f *fakeTask) Wait(ctx context.Context) (<-chan containerdclient.ExitStatus, error) {
	f.waitCtx = ctx
	f.waitNamespace = testContainerdNamespace(ctx)
	if f.waitErr != nil {
		return nil, f.waitErr
	}
	if f.waitCh == nil {
		f.waitCh = make(chan containerdclient.ExitStatus)
	}
	if f.waitOnContextCancel {
		go func(ch chan containerdclient.ExitStatus) {
			<-ctx.Done()
			ch <- *containerdclient.NewExitStatus(0, time.Now(), ctx.Err())
			close(ch)
		}(f.waitCh)
	}
	if f.waitHook != nil {
		f.waitHook()
	}
	return f.waitCh, nil
}

func (f *fakeTask) CloseIO(context.Context, ...containerdclient.IOCloserOpts) error { return nil }

func (f *fakeTask) Resize(context.Context, uint32, uint32) error { return nil }

func (f *fakeTask) IO() cio.IO { return nil }

func (f *fakeTask) Status(context.Context) (containerdclient.Status, error) {
	if f.statusErr != nil {
		return containerdclient.Status{}, f.statusErr
	}
	return containerdclient.Status{Status: f.status}, nil
}

func (f *fakeTask) Pause(context.Context) error { return nil }

func (f *fakeTask) Resume(context.Context) error { return nil }

func (f *fakeTask) Exec(context.Context, string, *specs.Process, cio.Creator) (containerdclient.Process, error) {
	return nil, errdefs.ErrNotImplemented
}

func (f *fakeTask) Pids(context.Context) ([]containerdclient.ProcessInfo, error) { return nil, nil }

func (f *fakeTask) Checkpoint(context.Context, ...containerdclient.CheckpointTaskOpts) (containerdclient.Image, error) {
	return fakeImage{name: "checkpoint"}, nil
}

func (f *fakeTask) Update(ctx context.Context, _ ...containerdclient.UpdateTaskOpts) error {
	f.updateCtx = ctx
	return nil
}

func (f *fakeTask) LoadProcess(context.Context, string, cio.Attach) (containerdclient.Process, error) {
	return nil, errdefs.ErrNotImplemented
}

func (f *fakeTask) Metrics(context.Context) (*apitypes.Metric, error) {
	if f.metricErr != nil {
		return nil, f.metricErr
	}
	return f.metric, nil
}

func (f *fakeTask) Spec(context.Context) (*oci.Spec, error) { return &oci.Spec{}, nil }

type fakeImage struct {
	name          string
	unpacked      *bool
	unpackCalls   *int
	unpackErr     error
	unpackCtx     *context.Context
	isUnpackedCtx *context.Context
}

func (f fakeImage) Name() string { return f.name }

func (f fakeImage) Target() ocispec.Descriptor {
	return ocispec.Descriptor{Digest: digest.FromString(f.name)}
}

func (f fakeImage) Labels() map[string]string { return nil }

func (f fakeImage) Unpack(ctx context.Context, _ string, _ ...containerdclient.UnpackOpt) error {
	if f.unpackCtx != nil {
		*f.unpackCtx = ctx
	}
	if f.unpackCalls != nil {
		*f.unpackCalls = *f.unpackCalls + 1
	}
	if f.unpackErr != nil {
		return f.unpackErr
	}
	if f.unpacked != nil {
		*f.unpacked = true
	}
	return nil
}

func (f fakeImage) RootFS(context.Context) ([]digest.Digest, error) { return nil, nil }

func (f fakeImage) Size(context.Context) (int64, error) { return 0, nil }

func (f fakeImage) Usage(context.Context, ...containerdclient.UsageOpt) (int64, error) {
	return 0, nil
}

func (f fakeImage) Config(context.Context) (ocispec.Descriptor, error) {
	return ocispec.Descriptor{}, nil
}

func (f fakeImage) IsUnpacked(ctx context.Context, _ string) (bool, error) {
	if f.isUnpackedCtx != nil {
		*f.isUnpackedCtx = ctx
	}
	if f.unpacked == nil {
		return true, nil
	}
	return *f.unpacked, nil
}

func (f fakeImage) ContentStore() content.Store { return nil }

func (f fakeImage) Metadata() containerdimages.Image {
	return containerdimages.Image{Name: f.name}
}

func (f fakeImage) Platform() platforms.MatchComparer { return nil }

func (f fakeImage) Spec(context.Context) (ocispec.Image, error) { return ocispec.Image{}, nil }

type fakeSnapshotter struct {
	removed          []string
	removeContexts   []context.Context
	removeNamespaces []string
	removeErr        error
}

func (f *fakeSnapshotter) Stat(context.Context, string) (snapshots.Info, error) {
	return snapshots.Info{}, errdefs.ErrNotImplemented
}

func (f *fakeSnapshotter) Update(context.Context, snapshots.Info, ...string) (snapshots.Info, error) {
	return snapshots.Info{}, errdefs.ErrNotImplemented
}

func (f *fakeSnapshotter) Usage(context.Context, string) (snapshots.Usage, error) {
	return snapshots.Usage{}, errdefs.ErrNotImplemented
}

func (f *fakeSnapshotter) Mounts(context.Context, string) ([]mount.Mount, error) {
	return nil, errdefs.ErrNotImplemented
}

func (f *fakeSnapshotter) Prepare(context.Context, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, errdefs.ErrNotImplemented
}

func (f *fakeSnapshotter) View(context.Context, string, string, ...snapshots.Opt) ([]mount.Mount, error) {
	return nil, errdefs.ErrNotImplemented
}

func (f *fakeSnapshotter) Commit(context.Context, string, string, ...snapshots.Opt) error {
	return errdefs.ErrNotImplemented
}

func (f *fakeSnapshotter) Remove(ctx context.Context, key string) error {
	f.removed = append(f.removed, key)
	f.removeContexts = append(f.removeContexts, ctx)
	f.removeNamespaces = append(f.removeNamespaces, testContainerdNamespace(ctx))
	return f.removeErr
}

func (f *fakeSnapshotter) Walk(context.Context, snapshots.WalkFunc, ...string) error {
	return nil
}

func (f *fakeSnapshotter) Close() error {
	return nil
}

var _ clientAPI = (*fakeClient)(nil)
var _ snapshotServiceProvider = (*fakeClient)(nil)
var _ containerdclient.Container = (*fakeContainer)(nil)
var _ containerdclient.Task = (*fakeTask)(nil)
var _ containerdclient.Image = (*fakeImage)(nil)
var _ snapshots.Snapshotter = (*fakeSnapshotter)(nil)

func testContainerdNamespace(ctx context.Context) string {
	namespace, _ := namespaces.Namespace(ctx)
	return namespace
}
