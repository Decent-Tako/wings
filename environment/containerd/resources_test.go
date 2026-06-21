package containerd

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/pelican-dev/wings/config"
	"github.com/pelican-dev/wings/environment"
)

func TestLinuxResourcesMapsBasicLimits(t *testing.T) {
	previousBlockIOProbe := canSetBlockIOWeight
	canSetBlockIOWeight = func() bool { return true }
	t.Cleanup(func() { canSetBlockIOWeight = previousBlockIOProbe })

	cfg, err := config.NewAtPath("/tmp/wings.yml")
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	cfg.AuthenticationToken = "test-token"
	config.Set(cfg)

	resources := linuxResources(environment.Limits{
		MemoryLimit: 512,
		Swap:        256,
		CpuLimit:    200,
		Threads:     "0-1",
		IoWeight:    500,
		OOMKiller:   true,
	})

	if resources.Memory == nil || resources.Memory.Limit == nil {
		t.Fatal("expected memory limit to be set")
	}
	if got := *resources.Memory.Limit; got <= 512*1024*1024 {
		t.Fatalf("expected memory limit to include overhead, got %d", got)
	}
	if resources.Memory.Reservation == nil || *resources.Memory.Reservation != 512*1024*1024 {
		t.Fatalf("expected memory reservation to match Docker units, got %+v", resources.Memory.Reservation)
	}
	if resources.CPU == nil || resources.CPU.Quota == nil || *resources.CPU.Quota != 200_000 {
		t.Fatalf("expected CPU quota to map from panel limit, got %+v", resources.CPU)
	}
	if resources.CPU.Cpus != "0-1" {
		t.Fatalf("expected cpuset to be preserved, got %q", resources.CPU.Cpus)
	}
	if resources.BlockIO == nil || resources.BlockIO.Weight == nil || *resources.BlockIO.Weight != 500 {
		t.Fatalf("expected block IO weight to be set, got %+v", resources.BlockIO)
	}
}

func TestLinuxResourcesOmitsUnsetMemoryLimits(t *testing.T) {
	cfg, err := config.NewAtPath("/tmp/wings.yml")
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	cfg.AuthenticationToken = "test-token"
	config.Set(cfg)

	resources := linuxResources(environment.Limits{
		MemoryLimit: 0,
		Swap:        0,
		OOMKiller:   true,
	})

	if resources.Memory == nil {
		t.Fatal("expected memory resource block for OOM setting")
	}
	if resources.Memory.Limit != nil {
		t.Fatalf("expected unset memory limit to be omitted, got %d", *resources.Memory.Limit)
	}
	if resources.Memory.Reservation != nil {
		t.Fatalf("expected unset memory reservation to be omitted, got %d", *resources.Memory.Reservation)
	}
	if resources.Memory.Swap != nil {
		t.Fatalf("expected unset swap to be omitted, got %d", *resources.Memory.Swap)
	}
}

func TestLinuxResourcesPreservesExplicitUnlimitedSwap(t *testing.T) {
	cfg, err := config.NewAtPath("/tmp/wings.yml")
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	cfg.AuthenticationToken = "test-token"
	config.Set(cfg)

	resources := linuxResources(environment.Limits{
		MemoryLimit: 512,
		Swap:        -1,
		OOMKiller:   true,
	})

	if resources.Memory == nil || resources.Memory.Swap == nil {
		t.Fatal("expected explicit unlimited swap to be set")
	}
	if got := *resources.Memory.Swap; got != -1 {
		t.Fatalf("expected unlimited swap sentinel -1, got %d", got)
	}
}

func TestResourceSpecOptsApplyCompleteLinuxResources(t *testing.T) {
	reservation := int64(256 * 1024 * 1024)
	limit := int64(512 * 1024 * 1024)
	swap := int64(768 * 1024 * 1024)
	disableOOMKiller := true
	resources := &specs.LinuxResources{
		Memory: &specs.LinuxMemory{
			Reservation:      &reservation,
			Limit:            &limit,
			Swap:             &swap,
			DisableOOMKiller: &disableOOMKiller,
		},
	}
	spec := oci.Spec{
		Version: specs.Version,
		Linux:   &specs.Linux{},
	}

	for _, opt := range resourceSpecOptsFrom(resources) {
		if err := opt(context.Background(), nil, nil, &spec); err != nil {
			t.Fatalf("resource spec opt returned error: %v", err)
		}
	}

	if spec.Linux.Resources == nil || spec.Linux.Resources.Memory == nil {
		t.Fatal("expected Linux memory resources to be applied")
	}
	memory := spec.Linux.Resources.Memory
	if memory.Reservation == nil || *memory.Reservation != reservation {
		t.Fatalf("expected memory reservation %d, got %+v", reservation, memory.Reservation)
	}
	if memory.DisableOOMKiller == nil || *memory.DisableOOMKiller != disableOOMKiller {
		t.Fatalf("expected DisableOOMKiller %v, got %+v", disableOOMKiller, memory.DisableOOMKiller)
	}
}
