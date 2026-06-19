package containerd

import (
	"testing"

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
