package cmd

import (
	"errors"
	"testing"

	"github.com/pelican-dev/wings/config"
)

func TestRunDockerStartupChecksSkipsContainerdRuntime(t *testing.T) {
	called := false
	err := runDockerStartupChecks(config.ContainerRuntimeContainerd, func() (bool, error) {
		called = true
		return false, errors.New("docker should not be checked")
	})

	if err != nil {
		t.Fatalf("runDockerStartupChecks() returned error: %v", err)
	}
	if called {
		t.Fatal("expected Docker startup checks to be skipped for containerd runtime")
	}
}

func TestRunDockerStartupChecksChecksDockerRuntime(t *testing.T) {
	called := false
	err := runDockerStartupChecks(config.ContainerRuntimeDocker, func() (bool, error) {
		called = true
		return false, nil
	})

	if err != nil {
		t.Fatalf("runDockerStartupChecks() returned error: %v", err)
	}
	if !called {
		t.Fatal("expected Docker startup checks to run for docker runtime")
	}
}
