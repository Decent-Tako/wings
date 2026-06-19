package server

import (
	"path/filepath"
	"testing"

	"github.com/goccy/go-json"

	"github.com/pelican-dev/wings/config"
	"github.com/pelican-dev/wings/environment"
	"github.com/pelican-dev/wings/environment/containerd"
	"github.com/pelican-dev/wings/remote"
)

func TestManagerInitServerSelectsContainerdRuntimeAndInstaller(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.NewAtPath(filepath.Join(dir, "wings.yml"))
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	cfg.ContainerRuntime = config.ContainerRuntimeContainerd
	cfg.System.Data = filepath.Join(dir, "servers")
	cfg.System.LogDirectory = filepath.Join(dir, "logs")
	cfg.System.TmpDirectory = filepath.Join(dir, "tmp")
	cfg.Containerd.RuntimeRoot = filepath.Join(dir, "containerd", "runtime")
	cfg.Containerd.LogDirectory = filepath.Join(dir, "containerd", "logs")
	config.Set(cfg)

	manager := NewEmptyManager(nil)
	server, err := manager.InitServer(newRuntimeSelectionServerConfig(t))
	if err != nil {
		t.Fatalf("InitServer() returned error: %v", err)
	}
	if _, ok := server.Environment.(*containerd.Environment); !ok {
		t.Fatalf("expected containerd environment, got %T", server.Environment)
	}

	install, err := NewInstallationProcess(server, &remote.InstallationScript{
		ContainerImage: "example.com/installer:latest",
		Entrypoint:     "/bin/sh",
		Script:         "echo installed",
	})
	if err != nil {
		t.Fatalf("NewInstallationProcess() returned error: %v", err)
	}
	if _, ok := install.runner.(*containerd.Installer); !ok {
		t.Fatalf("expected containerd installer runner, got %T", install.runner)
	}
}

func newRuntimeSelectionServerConfig(t *testing.T) remote.ServerConfigurationResponse {
	t.Helper()
	settings := Configuration{
		Uuid:       "runtime-selection-server",
		Invocation: "echo ${SERVER_MEMORY}",
		EnvVars: environment.Variables{
			"SERVER_JARFILE": "server.jar",
		},
		Allocations: environment.Allocations{
			DefaultMapping: &environment.DefaultAllocationMapping{Ip: "0.0.0.0", Port: 25565},
			Mappings:       map[string][]int{"0.0.0.0": []int{25565}},
		},
		Build: environment.Limits{
			MemoryLimit: 128,
			DiskSpace:   1024,
			OOMKiller:   true,
		},
		Labels: map[string]string{"test": "runtime-selection"},
	}
	settings.Container.Image = "example.com/server:latest"

	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatalf("failed to marshal server settings: %v", err)
	}
	return remote.ServerConfigurationResponse{
		Settings: raw,
		ProcessConfiguration: &remote.ProcessConfiguration{
			Stop: remote.ProcessStopConfiguration{Type: remote.ProcessStopCommand, Value: "stop"},
		},
	}
}
