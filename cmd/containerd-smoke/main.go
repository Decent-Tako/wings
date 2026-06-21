package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pelican-dev/wings/config"
	"github.com/pelican-dev/wings/environment"
	wingscontainerd "github.com/pelican-dev/wings/environment/containerd"
	envruntime "github.com/pelican-dev/wings/environment/runtime"
)

const readyLine = "pelican-containerd-smoke-ready"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "SMOKE result=fail error=%v\n", err)
		os.Exit(1)
	}
}

func run() error {
	socket := flag.String("socket", env("CONTAINERD_ADDRESS", "/run/containerd/containerd.sock"), "containerd socket path")
	namespace := flag.String("namespace", env("CONTAINERD_NAMESPACE", "pelican-smoke"), "containerd namespace")
	image := flag.String("image", env("SMOKE_IMAGE", "docker.io/library/busybox:1.36"), "image to run through the containerd backend")
	containerID := flag.String("container-id", env("SMOKE_CONTAINER_ID", "pelican-smoke-"+time.Now().UTC().Format("20060102T150405Z")), "containerd container ID")
	root := flag.String("root", env("SMOKE_ROOT", "/tmp/pelican-containerd-smoke"), "scratch root for runtime, logs, and server files")
	stayRunningFor := flag.Duration("stay-running-for", durationEnv("SMOKE_STAY_RUNNING_FOR", 5*time.Second), "duration to assert the task remains running")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmt.Printf("SMOKE step=configure namespace=%s socket=%s image=%s container_id=%s\n", *namespace, *socket, *image, *containerID)
	if err := configure(*socket, *namespace, *root); err != nil {
		return fmt.Errorf("step=configure: %w", err)
	}
	defer func() {
		if err := envruntime.CloseSelected(); err != nil {
			fmt.Printf("SMOKE step=close-client result=warn error=%q\n", err.Error())
		}
	}()

	if _, err := os.Stat("/var/run/docker.sock"); os.IsNotExist(err) {
		fmt.Println("SMOKE step=docker-socket result=absent")
	} else if err != nil {
		fmt.Printf("SMOKE step=docker-socket result=unknown error=%q\n", err.Error())
	} else {
		fmt.Println("SMOKE step=docker-socket result=present")
	}

	if err := envruntime.ConfigureSelected(ctx); err != nil {
		return fmt.Errorf("step=boot-without-docker/connect-containerd: %w", err)
	}
	cli, err := wingscontainerd.Client()
	if err != nil {
		return fmt.Errorf("step=boot-without-docker/client: %w", err)
	}
	version, err := cli.Version(wingscontainerd.WithNamespace(ctx))
	if err != nil {
		return fmt.Errorf("step=boot-without-docker/version: %w", err)
	}
	fmt.Printf("SMOKE step=boot-without-docker result=connected containerd_version=%s\n", version.Version)

	serverRoot := filepath.Join(*root, "server")
	if err := os.MkdirAll(serverRoot, 0o700); err != nil {
		return fmt.Errorf("step=prepare-server-root: %w", err)
	}
	procCfg := environment.NewConfiguration(environment.Settings{
		Mounts: []environment.Mount{
			{
				Default: true,
				Target:  "/home/container",
				Source:  serverRoot,
			},
		},
		Allocations: environment.Allocations{
			DefaultMapping: &environment.DefaultAllocationMapping{Ip: "0.0.0.0", Port: 25565},
			Mappings:       map[string][]int{"0.0.0.0": {25565}},
		},
		Limits: environment.Limits{
			MemoryLimit: 128,
			Swap:        0,
			CpuLimit:    50,
			OOMKiller:   true,
		},
		Labels: map[string]string{
			"pelican.dev/smoke": "containerd",
		},
	}, []string{"SERVER_MEMORY=128", "PELICAN_SMOKE=containerd"})

	proc, err := envruntime.NewProcess(*containerID, environment.ProcessMetadata{Image: *image}, procCfg)
	if err != nil {
		return fmt.Errorf("step=new-process: %w", err)
	}

	var output bytes.Buffer
	var outputMu sync.Mutex
	proc.SetLogCallback(func(line []byte) {
		outputMu.Lock()
		defer outputMu.Unlock()
		output.Write(line)
		output.WriteByte('\n')
	})

	cleaned := false
	defer func() {
		if cleaned {
			return
		}
		if err := proc.Destroy(); err != nil {
			fmt.Printf("SMOKE step=deferred-cleanup result=warn error=%q\n", err.Error())
		} else {
			fmt.Println("SMOKE step=deferred-cleanup result=ok")
		}
		cleanupRoot(*root)
	}()

	fmt.Println("SMOKE step=lifecycle action=start")
	if err := proc.Start(ctx); err != nil {
		return fmt.Errorf("step=lifecycle/start: %w", err)
	}
	fmt.Printf("SMOKE step=lifecycle action=start result=ok wings_state=%s\n", proc.State())

	if err := waitRunning(ctx, proc, 10*time.Second); err != nil {
		return fmt.Errorf("step=lifecycle/running-after-start: %w", err)
	}
	fmt.Printf("SMOKE step=lifecycle action=running-after-start result=ok wings_state=%s\n", proc.State())

	if err := proc.SendCommand("echo " + readyLine); err != nil {
		return fmt.Errorf("step=console/send-command: %w", err)
	}
	if err := waitOutput(ctx, &outputMu, &output, readyLine, 10*time.Second); err != nil {
		return fmt.Errorf("step=console/read-output: %w", err)
	}
	lines, err := proc.Readlog(10)
	if err != nil {
		return fmt.Errorf("step=console/read-log-file: %w", err)
	}
	fmt.Printf("SMOKE step=console result=ok evidence=%q\n", strings.Join(lines, " | "))

	if err := assertStaysRunning(ctx, proc, *stayRunningFor); err != nil {
		return fmt.Errorf("step=lifecycle/stays-running: %w", err)
	}
	fmt.Printf("SMOKE step=lifecycle action=stays-running duration=%s result=ok wings_state=%s\n", stayRunningFor.String(), proc.State())

	fmt.Println("SMOKE step=lifecycle action=stop")
	if err := proc.WaitForStop(ctx, 15*time.Second, true); err != nil {
		return fmt.Errorf("step=lifecycle/stop: %w", err)
	}
	running, err := proc.IsRunning(ctx)
	if err != nil {
		return fmt.Errorf("step=lifecycle/post-stop-status: %w", err)
	}
	if running {
		return fmt.Errorf("step=lifecycle/post-stop-status: task is still running")
	}
	fmt.Printf("SMOKE step=lifecycle action=stop result=ok wings_state=%s\n", proc.State())

	if err := proc.Destroy(); err != nil {
		return fmt.Errorf("step=cleanup/destroy: %w", err)
	}
	cleanupRoot(*root)
	cleaned = true
	fmt.Println("SMOKE step=cleanup result=ok")
	fmt.Println("SMOKE result=pass")
	return nil
}

func configure(socket, namespace, root string) error {
	cfg, err := config.NewAtPath(filepath.Join(root, "config.yml"))
	if err != nil {
		return err
	}
	cfg.AuthenticationToken = "containerd-smoke-token"
	cfg.ContainerRuntime = config.ContainerRuntimeContainerd
	cfg.Containerd.Address = socket
	cfg.Containerd.Namespace = namespace
	cfg.Containerd.RuntimeRoot = filepath.Join(root, "runtime")
	cfg.Containerd.LogDirectory = filepath.Join(root, "logs")
	cfg.Containerd.LogMaxSize = "5m"
	cfg.Containerd.LogMaxFiles = 1
	cfg.Containerd.Network.Mode = "host"
	cfg.Docker.Network.Mode = "host"
	cfg.Docker.TmpfsSize = 64
	cfg.Docker.ContainerPidLimit = 128
	cfg.System.User.Uid = 0
	cfg.System.User.Gid = 0
	config.Set(cfg)
	return nil
}

func waitRunning(ctx context.Context, proc environment.ProcessEnvironment, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		running, err := proc.IsRunning(ctx)
		if err != nil {
			return err
		}
		if running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("task did not become running within %s; wings_state=%s", timeout, proc.State())
		case <-ticker.C:
		}
	}
}

func assertStaysRunning(ctx context.Context, proc environment.ProcessEnvironment, duration time.Duration) error {
	deadline := time.NewTimer(duration)
	defer deadline.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return nil
		case <-ticker.C:
			running, err := proc.IsRunning(ctx)
			if err != nil {
				return err
			}
			if !running {
				return fmt.Errorf("task stopped before %s elapsed; wings_state=%s", duration, proc.State())
			}
			if proc.State() == environment.ProcessOfflineState {
				return fmt.Errorf("wings state became offline while task was still running")
			}
		}
	}
}

func waitOutput(ctx context.Context, mu *sync.Mutex, out *bytes.Buffer, needle string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		mu.Lock()
		found := strings.Contains(out.String(), needle)
		mu.Unlock()
		if found {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			mu.Lock()
			got := out.String()
			mu.Unlock()
			return fmt.Errorf("did not observe %q within %s; output=%q", needle, timeout, got)
		case <-ticker.C:
		}
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func cleanupRoot(root string) {
	root = filepath.Clean(root)
	tmp := filepath.Clean(os.TempDir())
	if root == "" || root == "." || root == "/" || root == tmp || !strings.HasPrefix(root, tmp+string(os.PathSeparator)) {
		fmt.Printf("SMOKE step=cleanup-root result=skip root=%q\n", root)
		return
	}
	if err := os.RemoveAll(root); err != nil {
		fmt.Printf("SMOKE step=cleanup-root result=warn root=%q error=%q\n", root, err.Error())
		return
	}
	fmt.Printf("SMOKE step=cleanup-root result=ok root=%q\n", root)
}
