package containerd

import (
	"context"
	"time"

	"emperror.dev/errors"
	"github.com/containerd/errdefs"

	"github.com/pelican-dev/wings/environment"
	"github.com/pelican-dev/wings/remote"
)

func (e *Environment) OnBeforeStart(ctx context.Context) error {
	if err := e.removeContainer(ctx); err != nil {
		return errors.Wrap(err, "environment/containerd: failed to remove container during pre-boot")
	}
	if err := e.Create(); err != nil {
		if cleanupErr := e.removeContainer(context.Background()); cleanupErr != nil {
			e.log().WithField("error", cleanupErr).Warn("failed to cleanup partially created containerd container after create error")
		}
		return err
	}
	return nil
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
		return e.Attach(ctx)
	}

	if err := truncateLog(e.logPath()); err != nil {
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
		return errors.New("environment/containerd: no task available after attach")
	}

	if err := task.Start(e.context(actx)); err != nil {
		e.mu.Unlock()
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
	tctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-tctx.Done():
		}
	}()

	if err := e.Stop(tctx); err != nil {
		if terminate && errors.Is(err, context.DeadlineExceeded) {
			return e.Terminate(ctx, "SIGKILL")
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
	exitC, err := task.Wait(e.context(tctx))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return errors.Wrap(err, "environment/containerd: error waiting on task")
	}

	select {
	case <-ctx.Done():
		if terminate {
			return e.Terminate(ctx, "SIGKILL")
		}
		return ctx.Err()
	case <-tctx.Done():
		if terminate {
			return e.Terminate(ctx, "SIGKILL")
		}
		return tctx.Err()
	case <-exitC:
		return nil
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
	if status.Status != "running" {
		if e.st.Load() != environment.ProcessOfflineState {
			e.SetState(environment.ProcessStoppingState)
			e.SetState(environment.ProcessOfflineState)
		}
		return nil
	}

	e.SetState(environment.ProcessStoppingState)
	if err := task.Kill(e.context(ctx), signalFromString(signal)); err != nil && !errdefs.IsNotFound(err) {
		return errors.WithStack(err)
	}

	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
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
			if status.Status != "running" {
				e.SetState(environment.ProcessOfflineState)
				return nil
			}
		case <-timeout.C:
			if err := task.Kill(e.context(ctx), signalFromString("SIGKILL")); err != nil && !errdefs.IsNotFound(err) {
				return errors.WithStack(err)
			}
			e.SetState(environment.ProcessOfflineState)
			return nil
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
	status, err := task.Status(e.context(ctx))
	if err != nil {
		return false, err
	}
	return status.Status == "running", nil
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
