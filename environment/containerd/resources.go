package containerd

import (
	"os"

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
	opts := []oci.SpecOpts{}
	if resources.Memory != nil {
		if resources.Memory.Limit != nil {
			opts = append(opts, oci.WithMemoryLimit(uint64(*resources.Memory.Limit)))
		}
		if resources.Memory.Swap != nil {
			opts = append(opts, oci.WithMemorySwap(*resources.Memory.Swap))
		}
	}
	if resources.Pids != nil && resources.Pids.Limit != nil && *resources.Pids.Limit > 0 {
		opts = append(opts, oci.WithPidsLimit(*resources.Pids.Limit))
	}
	if resources.CPU != nil {
		if resources.CPU.Quota != nil && resources.CPU.Period != nil {
			opts = append(opts, oci.WithCPUCFS(*resources.CPU.Quota, *resources.CPU.Period))
		}
		if resources.CPU.Shares != nil {
			opts = append(opts, oci.WithCPUShares(*resources.CPU.Shares))
		}
		if resources.CPU.Cpus != "" {
			opts = append(opts, oci.WithCPUs(resources.CPU.Cpus))
		}
	}
	if resources.BlockIO != nil {
		opts = append(opts, oci.WithBlockIO(resources.BlockIO))
	}
	return opts
}

func linuxResources(l environment.Limits) *specs.LinuxResources {
	limit := l.BoundedMemoryLimit()
	reservation := l.MemoryLimit * 1024 * 1024
	swap := l.ConvertedSwap()
	oomDisabled := !l.OOMKiller
	pids := l.ProcessLimit()

	resources := &specs.LinuxResources{
		Memory: &specs.LinuxMemory{
			Limit:            &limit,
			Reservation:      &reservation,
			Swap:             &swap,
			DisableOOMKiller: &oomDisabled,
		},
		Pids: &specs.LinuxPids{Limit: &pids},
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
