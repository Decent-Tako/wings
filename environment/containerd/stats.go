package containerd

import (
	"context"
	"math"
	"runtime"
	"time"

	"emperror.dev/errors"

	"github.com/pelican-dev/wings/environment"
)

func (e *Environment) pollResources(ctx context.Context) error {
	if e.st.Load() == environment.ProcessOfflineState {
		return errors.New("cannot enable resource polling on a stopped server")
	}

	e.log().Info("starting resource polling for containerd task")
	defer e.log().Debug("stopped resource polling for containerd task")

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var previousCPU uint64
	var previousRead time.Time

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case now := <-ticker.C:
			if e.st.Load() == environment.ProcessOfflineState {
				return nil
			}

			task, err := e.currentTask(ctx)
			if err != nil {
				continue
			}
			metric, err := task.Metrics(e.context(ctx))
			if err != nil {
				continue
			}

			memory, cpuUsage := e.decodeMetric(metric)
			stats := environment.Stats{
				Uptime:      e.uptimeMilliseconds(now),
				Memory:      memory,
				MemoryLimit: e.configuredMemoryLimit(),
				CpuAbsolute: calculateCPUPercent(previousCPU, cpuUsage, previousRead, now),
				Network:     unsupportedNetworkStats(),
			}

			previousCPU = cpuUsage
			previousRead = now
			e.Events().Publish(environment.ResourceEvent, stats)
		}
	}
}

func unsupportedNetworkStats() environment.NetworkStats {
	// TODO(T5): collect RX/TX from the container netns once the CNI/portmap
	// implementation replaces the current host-network-only containerd backend.
	return environment.NetworkStats{}
}

func (e *Environment) uptimeMilliseconds(now time.Time) int64 {
	e.mu.RLock()
	startedAt := e.startedAt
	e.mu.RUnlock()
	if startedAt.IsZero() {
		return 0
	}
	return now.Sub(startedAt).Milliseconds()
}

func (e *Environment) configuredMemoryLimit() uint64 {
	limit := e.Configuration.Limits().BoundedMemoryLimit()
	if limit <= 0 {
		return 0
	}
	return uint64(limit)
}

func calculateCPUPercent(previousUsage, currentUsage uint64, previousRead, currentRead time.Time) float64 {
	if previousUsage == 0 || currentUsage <= previousUsage || previousRead.IsZero() {
		return 0
	}

	elapsed := currentRead.Sub(previousRead)
	if elapsed <= 0 {
		return 0
	}

	usageDelta := float64(currentUsage - previousUsage)
	percent := (usageDelta / float64(elapsed.Nanoseconds())) * 100
	if runtime.NumCPU() > 0 {
		percent = math.Min(percent, float64(runtime.NumCPU()*100))
	}
	return math.Round(percent*1000) / 1000
}
