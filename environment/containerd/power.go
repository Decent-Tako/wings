package containerd

import (
	"context"
	"time"

	"emperror.dev/errors"
	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"

	"github.com/pelican-dev/wings/environment"
	"github.com/pelican-dev/wings/remote"
)

var containerdTerminateTimeout = 10 * time.Second

func (e *Environment) OnBeforeStart(ctx context.Context) error {
	if err := e.removeContainer(ctx); err != nil {
		return errors.Wrap(err, "environment/containerd: failed to remove container during pre-boot")
	}
	return e.create(ctx)
}

func (e *Environment) Start(ctx context.Context) error {
	sawError := false
	createdEnvironment := false
	defer func() {
		if sawError {
			if createdEnvironment {
				if err := e.removeContainer(context.Background()); err != nil {
					e.log().WithField("error", err).Warn("failed to cleanup containerd container after start error")
				}
			}
			e.SetState(environment.ProcessStoppingState)
			e.SetState(environment.ProcessOfflineState)
		}
	}()

	running, err := e.IsRunning(ctx)
	if err != nil {
		return errors.Wrap(err, "environment/containerd: failed to inspect task")
	}
	if running {
		e.startedAtOrRestore(ctx, time.Now())
		e.SetState(environment.ProcessRunningState)
		if err := e.Attach(ctx); err != nil {
			stillRunning, inspectErr := e.IsRunning(ctx)
			if inspectErr != nil {
				e.log().WithField("error", inspectErr).Warn("failed to inspect containerd task after reattach error")
			} else if !stillRunning {
				e.SetState(environment.ProcessStoppingState)
				e.SetState(environment.ProcessOfflineState)
			}
			return err
		}
		return nil
	}

	logPath, err := e.logPath()
	if err != nil {
		return err
	}
	if err := truncateLog(logPath); err != nil {
		return errors.Wrap(err, "environment/containerd: failed to truncate instance logs")
	}

	e.SetState(environment.ProcessStartingState)
	sawError = true

	if err := e.OnBeforeStart(ctx); err != nil {
		return errors.Wrap(err, "environment/containerd: failed to run pre-boot process")
	}
	createdEnvironment = true

	actx, cancel := context.WithTimeout(ctx, time.Second*30)
	defer cancel()
	if err := e.Attach(actx); err != nil {
		return errors.Wrap(err, "environment/containerd: failed to attach to task")
	}

	e.mu.Lock()
	task := e.task
	if task == nil {
		e.mu.Unlock()
		e.closeAttach()
		return errors.New("environment/containerd: no task available after attach")
	}

	if err := task.Start(e.context(actx)); err != nil {
		e.mu.Unlock()
		if _, cleanupErr := task.Delete(e.context(context.Background()), containerdclient.WithProcessKill); cleanupErr != nil {
			warnContainerdCleanupError(e.log(), cleanupErr, "failed to delete containerd task after start error")
		}
		e.closeAttach()
		return errors.Wrap(err, "environment/containerd: failed to start task")
	}
	e.mu.Unlock()

	e.setStartedAt(actx, time.Now())
	e.mu.Lock()
	e.lastOOM = false
	e.mu.Unlock()

	sawError = false
	return nil
}

func (e *Environment) Stop(ctx context.Context) error {
	e.mu.RLock()
	stop := e.meta.Stop
	e.mu.RUnlock()

	if stop.Type == "" || stop.Type == remote.ProcessStopSignal {
		return e.Terminate(ctx, stop.Value)
	}

	if e.st.Load() != environment.ProcessOfflineState {
		e.SetState(environment.ProcessStoppingState)
	}

	if e.IsAttached() && stop.Type == remote.ProcessStopCommand {
		return e.SendCommand(stop.Value)
	}

	return e.Terminate(ctx, "SIGTERM")
}

func (e *Environment) WaitForStop(ctx context.Context, duration time.Duration, terminate bool) error {
	tctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	onTimeout := func(err error) error {
		if terminate {
			killCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			return e.Terminate(killCtx, "SIGKILL")
		}
		return err
	}

	if err := e.Stop(tctx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return onTimeout(err)
		}
		return err
	}

	task, err := e.currentTask(tctx)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return errors.Wrap(err, "environment/containerd: error loading task for wait")
	}
	for {
		exitC, err := task.Wait(e.context(tctx))
		if err != nil {
			if errdefs.IsNotFound(err) {
				return nil
			}
			return errors.Wrap(err, "environment/containerd: error waiting on task")
		}

		select {
		case <-tctx.Done():
			return onTimeout(tctx.Err())
		case status, ok := <-exitC:
			if !ok {
				return errors.New("environment/containerd: task wait channel closed unexpectedly")
			}
			if _, _, err := status.Result(); err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					return onTimeout(err)
				}
				running, inspectErr := taskIsRunning(e.context(tctx), task)
				if inspectErr != nil {
					return errors.Wrap(inspectErr, "environment/containerd: could not inspect task after wait error")
				}
				if running {
					e.log().WithField("error", err).Warn("containerd task wait failed while task is still running; retrying wait")
					continue
				}
				return nil
			}
			return nil
		}
	}
}

func (e *Environment) Terminate(ctx context.Context, signal string) error {
	task, err := e.currentTask(ctx)
	if err != nil {
		if errdefs.IsNotFound(err) {
			if e.st.Load() != environment.ProcessOfflineState {
				e.SetState(environment.ProcessStoppingState)
				e.SetState(environment.ProcessOfflineState)
			}
			return nil
		}
		return errors.WithStack(err)
	}

	status, err := task.Status(e.context(ctx))
	if err != nil {
		return errors.WithStack(err)
	}
	if status.Status != containerdclient.Running {
		if e.st.Load() != environment.ProcessOfflineState {
			e.SetState(environment.ProcessStoppingState)
			e.SetState(environment.ProcessOfflineState)
		}
		return nil
	}

	e.SetState(environment.ProcessStoppingState)
	sig := signalFromString(signal)
	if err := task.Kill(e.context(ctx), sig); err != nil && !errdefs.IsNotFound(err) {
		return errors.WithStack(err)
	}

	waitCtx := ctx
	if _, ok := waitCtx.Deadline(); !ok {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(waitCtx, containerdTerminateTimeout)
		defer cancel()
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			status, err := task.Status(e.context(ctx))
			if err != nil {
				if errdefs.IsNotFound(err) {
					e.SetState(environment.ProcessOfflineState)
					return nil
				}
				return errors.WithStack(err)
			}
			if status.Status != containerdclient.Running {
				e.SetState(environment.ProcessOfflineState)
				return nil
			}
		case <-waitCtx.Done():
			return errors.WithStack(waitCtx.Err())
		}
	}
}

func (e *Environment) IsRunning(ctx context.Context) (bool, error) {
	task, err := e.currentTask(ctx)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return taskIsRunning(e.context(ctx), task)
}

func taskIsRunning(ctx context.Context, task containerdclient.Task) (bool, error) {
	status, err := task.Status(ctx)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return status.Status == containerdclient.Running, nil
}

func (e *Environment) ExitState() (uint32, bool, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.lastExitCode, e.lastOOM, nil
}

func (e *Environment) Uptime(ctx context.Context) (int64, error) {
	running, err := e.IsRunning(ctx)
	if err != nil {
		return 0, err
	}
	if !running {
		return 0, nil
	}

	e.mu.RLock()
	startedAt := e.startedAt
	e.mu.RUnlock()
	if startedAt.IsZero() {
		startedAt = e.startedAtOrRestore(ctx, time.Now())
		if startedAt.IsZero() {
			return 0, nil
		}
	}
	return time.Since(startedAt).Milliseconds(), nil
}
