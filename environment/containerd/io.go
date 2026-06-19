package containerd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"emperror.dev/errors"
	"github.com/apex/log"
	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/errdefs"

	"github.com/pelican-dev/wings/config"
	"github.com/pelican-dev/wings/environment"
	"github.com/pelican-dev/wings/system"
)

func (e *Environment) Attach(ctx context.Context) error {
	e.mu.RLock()
	if e.stdin != nil {
		e.mu.RUnlock()
		return nil
	}
	e.mu.RUnlock()

	c, err := e.container(ctx)
	if err != nil {
		return errors.Wrap(err, "environment/containerd: failed to load container for attach")
	}

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	logFile, err := os.OpenFile(e.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return errors.Wrap(err, "environment/containerd: failed to open container log")
	}

	ioOpts := []cio.Opt{
		cio.WithFIFODir(e.fifoRoot()),
		cio.WithStreams(stdinR, stdoutW, stdoutW),
		cio.WithTerminal,
	}

	task, err := c.Task(e.context(ctx), cio.NewAttach(ioOpts...))
	if err != nil {
		if !errdefs.IsNotFound(err) {
			_ = stdinR.Close()
			_ = stdinW.Close()
			_ = stdoutR.Close()
			_ = stdoutW.Close()
			_ = logFile.Close()
			return errors.Wrap(err, "environment/containerd: failed to attach to task")
		}

		task, err = c.NewTask(e.context(ctx), cio.NewCreator(ioOpts...))
		if err != nil {
			_ = stdinR.Close()
			_ = stdinW.Close()
			_ = stdoutR.Close()
			_ = stdoutW.Close()
			_ = logFile.Close()
			return errors.Wrap(err, "environment/containerd: failed to create task")
		}
	}

	exitC, err := task.Wait(e.context(ctx))
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = logFile.Close()
		return errors.Wrap(err, "environment/containerd: failed to wait on task")
	}

	pollCtx, pollStop := context.WithCancel(context.Background())
	e.mu.Lock()
	e.task = task
	e.taskIO = task.IO()
	e.stdin = stdinW
	e.stdout = stdoutW
	e.pollStop = pollStop
	e.mu.Unlock()

	go e.consumeOutput(stdoutR, logFile)
	go e.watchExit(exitC, task)
	go func() {
		if err := e.pollResources(pollCtx); err != nil && !errors.Is(err, context.Canceled) {
			e.log().WithField("error", err).Warn("error during environment resource polling")
		}
	}()

	return nil
}

func (e *Environment) SendCommand(command string) error {
	e.mu.RLock()
	stdin := e.stdin
	stop := e.meta.Stop
	e.mu.RUnlock()

	if stdin == nil {
		return errors.Wrap(ErrNotAttached, "environment/containerd: cannot send command to container")
	}
	if stop.Type == "command" && command == stop.Value {
		e.SetState(environment.ProcessStoppingState)
	}

	_, err := stdin.Write([]byte(command + "\n"))
	return errors.Wrap(err, "environment/containerd: could not write to container stream")
}

func (e *Environment) Readlog(lines int) ([]string, error) {
	b, err := os.ReadFile(e.logPath())
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, errors.WithStack(err)
	}

	trimmed := strings.TrimRight(string(b), "\n")
	if trimmed == "" {
		return []string{}, nil
	}
	all := strings.Split(trimmed, "\n")
	if lines > 0 && len(all) > lines {
		all = all[len(all)-lines:]
	}
	return all, nil
}

func (e *Environment) consumeOutput(stdout *io.PipeReader, logFile *os.File) {
	defer stdout.Close()
	defer logFile.Close()

	if err := system.ScanReader(io.TeeReader(stdout, logFile), func(v []byte) {
		e.logCallbackMx.Lock()
		defer e.logCallbackMx.Unlock()
		if e.logCallback != nil {
			e.logCallback(v)
		}
	}); err != nil && err != io.EOF {
		log.WithField("error", err).WithField("container_id", e.Id).Warn("error processing scanner line in console output")
	}
}

func (e *Environment) watchExit(exitC <-chan containerdclient.ExitStatus, task containerdclient.Task) {
	status, ok := <-exitC
	if !ok {
		return
	}

	code, exitedAt, err := status.Result()
	if err != nil {
		e.log().WithField("error", err).Warn("containerd task exited with error status")
	}

	e.mu.Lock()
	e.lastExitCode = code
	e.lastExitTime = exitedAt
	e.lastOOM = false
	e.mu.Unlock()

	_, _ = task.Delete(e.context(context.Background()))
	e.closeAttach()
	e.SetState(environment.ProcessOfflineState)
}

func (e *Environment) closeAttach() {
	e.mu.Lock()
	stdin := e.stdin
	stdout := e.stdout
	taskIO := e.taskIO
	pollStop := e.pollStop
	e.stdin = nil
	e.stdout = nil
	e.taskIO = nil
	e.task = nil
	e.pollStop = nil
	e.mu.Unlock()

	if pollStop != nil {
		pollStop()
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	if stdout != nil {
		_ = stdout.Close()
	}
	if taskIO != nil {
		taskIO.Cancel()
		_ = taskIO.Close()
	}
}

func (e *Environment) fifoRoot() string {
	return filepath.Join(config.Get().Containerd.RuntimeRoot, "fifo")
}

func (e *Environment) logPath() string {
	return filepath.Join(config.Get().Containerd.LogDirectory, e.Id+".log")
}

func truncateLog(path string) error {
	if _, err := os.Stat(path); err == nil {
		return os.Truncate(path, 0)
	} else if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		return f.Close()
	} else {
		return err
	}
}

func signalFromString(value string) syscall.Signal {
	switch strings.ToUpper(value) {
	case "SIGABRT":
		return syscall.SIGABRT
	case "SIGINT", "C":
		return syscall.SIGINT
	case "SIGTERM":
		return syscall.SIGTERM
	case "SIGKILL":
		return syscall.SIGKILL
	default:
		if n, err := strconv.Atoi(value); err == nil {
			return syscall.Signal(n)
		}
		return syscall.SIGKILL
	}
}
