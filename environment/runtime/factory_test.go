package runtime

import (
	"testing"

	"github.com/pelican-dev/wings/config"
)

func TestFactoryForDefaultsToDocker(t *testing.T) {
	factory, err := FactoryFor("")
	if err != nil {
		t.Fatalf("expected empty runtime to select docker factory: %v", err)
	}
	if factory == nil {
		t.Fatal("expected docker factory, got nil")
	}
}

func TestFactoryForContainerd(t *testing.T) {
	factory, err := FactoryFor(config.ContainerRuntimeContainerd)
	if err != nil {
		t.Fatalf("expected containerd runtime to be supported: %v", err)
	}
	if factory == nil {
		t.Fatal("expected containerd factory, got nil")
	}
}

func TestFactoryForRejectsUnsupportedRuntime(t *testing.T) {
	factory, err := FactoryFor(config.ContainerRuntime("bogus"))
	if err == nil {
		t.Fatal("expected unsupported runtime error")
	}
	if factory != nil {
		t.Fatalf("expected nil factory for unsupported runtime, got %T", factory)
	}
}
