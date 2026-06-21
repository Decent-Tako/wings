package server

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pelican-dev/wings/config"
	"github.com/pelican-dev/wings/environment"
	"github.com/pelican-dev/wings/remote"
	"github.com/pelican-dev/wings/server/filesystem"
)

func TestInstallationRunCopiesLogsAfterFailedExecuteWithContainerID(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.NewAtPath(filepath.Join(dir, "wings.yml"))
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	cfg.System.Data = filepath.Join(dir, "servers")
	cfg.System.LogDirectory = filepath.Join(dir, "logs")
	cfg.System.TmpDirectory = filepath.Join(dir, "tmp")
	cfg.AuthenticationToken = "test-token"
	config.Set(cfg)
	if err := os.MkdirAll(filepath.Join(cfg.System.LogDirectory, "install"), 0o700); err != nil {
		t.Fatalf("failed to create install log directory: %v", err)
	}

	s, err := New(nil)
	if err != nil {
		t.Fatalf("New() returned error: %v", err)
	}
	s.cfg.Uuid = "install-log-server"
	s.cfg.Invocation = "java -Xmx${SERVER_MEMORY}M -jar ${SERVER_JARFILE}"
	s.cfg.EnvVars = environment.Variables{"SERVER_JARFILE": "server.jar"}
	s.cfg.Allocations = environment.Allocations{
		DefaultMapping: &environment.DefaultAllocationMapping{Ip: "0.0.0.0", Port: 25565},
		Mappings:       map[string][]int{"0.0.0.0": []int{25565}},
	}
	s.cfg.Build = environment.Limits{MemoryLimit: 128, DiskSpace: 1024, OOMKiller: true}
	s.fs, err = filesystem.New(filepath.Join(cfg.System.Data, s.ID()), s.DiskSpace(), nil)
	if err != nil {
		t.Fatalf("filesystem.New() returned error: %v", err)
	}

	runner := &failingInstallRunner{
		id:   s.ID() + "_installer",
		err:  errors.New("installer exited with code 42"),
		logs: "installer failed before finishing\n",
	}
	ip := &InstallationProcess{
		Server: s,
		Script: &remote.InstallationScript{
			ContainerImage: "example.com/installer:latest",
			Entrypoint:     "/bin/sh",
			Script:         "echo installing",
		},
		runner: runner,
	}

	err = ip.Run()
	if err == nil || !strings.Contains(err.Error(), "exited with code 42") {
		t.Fatalf("expected original installer error, got %v", err)
	}
	if runner.logsID != runner.id {
		t.Fatalf("expected logs to be copied from %q, got %q", runner.id, runner.logsID)
	}
	if !slices.Contains(runner.removeIDs, runner.id) {
		t.Fatalf("expected installer container %q to be removed, got %v", runner.id, runner.removeIDs)
	}

	data, err := os.ReadFile(ip.GetLogPath())
	if err != nil {
		t.Fatalf("failed to read copied install log: %v", err)
	}
	if !strings.Contains(string(data), "installer failed before finishing") {
		t.Fatalf("expected copied install output, got:\n%s", data)
	}
}

type failingInstallRunner struct {
	id   string
	err  error
	logs string

	logsID    string
	removeIDs []string
}

func (f *failingInstallRunner) PullImage(context.Context, string) error { return nil }

func (f *failingInstallRunner) Remove(_ context.Context, id string) error {
	f.removeIDs = append(f.removeIDs, id)
	return nil
}

func (f *failingInstallRunner) Execute(context.Context, environment.InstallationSpec, func([]byte)) (string, error) {
	return f.id, f.err
}

func (f *failingInstallRunner) Logs(_ context.Context, id string) (io.ReadCloser, error) {
	f.logsID = id
	return io.NopCloser(strings.NewReader(f.logs)), nil
}
