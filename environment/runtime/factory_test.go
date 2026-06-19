package runtime

import (
	"reflect"
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

func TestSelectedFactoryResolvesCurrentRuntime(t *testing.T) {
	cfg, err := config.NewAtPath("/tmp/wings.yml")
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	cfg.ContainerRuntime = config.ContainerRuntimeDocker
	config.Set(cfg)

	first, err := SelectedFactory()
	if err != nil {
		t.Fatalf("SelectedFactory() returned error: %v", err)
	}

	config.Update(func(c *config.Configuration) {
		c.ContainerRuntime = config.ContainerRuntimeContainerd
	})
	second, err := SelectedFactory()
	if err != nil {
		t.Fatalf("SelectedFactory() after config update returned error: %v", err)
	}

	if reflect.TypeOf(first) == reflect.TypeOf(second) {
		t.Fatalf("expected selected factory to resolve current runtime, got %T then %T", first, second)
	}
}
