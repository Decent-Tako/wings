package containerd

import (
	"context"
	"os"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/pelican-dev/wings/environment"
)

var canSetBlockIOWeight = blockIOWeightSupported

func resourceSpecOpts(l environment.Limits) []oci.SpecOpts {
	resources := linuxResources(l)
	return resourceSpecOptsFrom(resources)
}

func installerResourceSpecOpts(l environment.Limits) []oci.SpecOpts {
	resources := linuxResources(l)
	resources.Pids = nil
	return resourceSpecOptsFrom(resources)
}

func resourceSpecOptsFrom(resources *specs.LinuxResources) []oci.SpecOpts {
	return []oci.SpecOpts{withLinuxResources(resources)}
}

func withLinuxResources(resources *specs.LinuxResources) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		if s.Linux == nil {
			s.Linux = &specs.Linux{}
		}
		s.Linux.Resources = resources
		return nil
	}
}

func linuxResources(l environment.Limits) *specs.LinuxResources {
	limit := l.BoundedMemoryLimit()
	reservation := l.MemoryLimit * 1024 * 1024
	swap := l.ConvertedSwap()
	oomDisabled := !l.OOMKiller
	pids := l.ProcessLimit()

	memory := &specs.LinuxMemory{
		DisableOOMKiller: &oomDisabled,
	}
	if l.MemoryLimit > 0 {
		memory.Limit = &limit
		memory.Reservation = &reservation
	}
	if swap > 0 || l.Swap < 0 {
		memory.Swap = &swap
	}

	resources := &specs.LinuxResources{
		Memory: memory,
		Pids:   &specs.LinuxPids{Limit: &pids},
	}

	if l.CpuLimit > 0 {
		quota := l.CpuLimit * 1_000
		period := uint64(100_000)
		shares := uint64(1024)
		resources.CPU = &specs.LinuxCPU{
			Quota:  &quota,
			Period: &period,
			Shares: &shares,
		}
	}
	if l.Threads != "" {
		if resources.CPU == nil {
			resources.CPU = &specs.LinuxCPU{}
		}
		resources.CPU.Cpus = l.Threads
	}
	if l.IoWeight > 0 && canSetBlockIOWeight() {
		weight := l.IoWeight
		resources.BlockIO = &specs.LinuxBlockIO{Weight: &weight}
	}

	return resources
}

func blockIOWeightSupported() bool {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return true
	}
	for _, p := range []string{
		"/sys/fs/cgroup/system.slice/io.weight",
		"/sys/fs/cgroup/io.weight",
	} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}
