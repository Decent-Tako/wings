package containerd

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	eventtypes "github.com/containerd/containerd/api/events"
	containerdclient "github.com/containerd/containerd/v2/client"
	ctrruntime "github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"github.com/docker/go-units"

	"github.com/pelican-dev/wings/config"
	"github.com/pelican-dev/wings/environment"
	"github.com/pelican-dev/wings/remote"
	"github.com/pelican-dev/wings/system"
)

const maxContainerdReadlogLines = 10_000

var oomSubscribeRetryDelay = time.Second

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

	logPath, err := e.logPath()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return err
	}
	logWriter, err := newRotatingLogWriter(logPath)
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return errors.Wrap(err, "environment/containerd: failed to open container log")
	}

	fifoRoot, err := e.fifoRoot()
	if err != nil {
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = logWriter.Close()
		return err
	}
	ioOpts := []cio.Opt{
		cio.WithFIFODir(fifoRoot),
		cio.WithStreams(stdinR, stdoutW, stdoutW),
		cio.WithTerminal,
	}

	createdTask := false
	task, err := c.Task(e.context(ctx), cio.NewAttach(ioOpts...))
	if err != nil {
		if !errdefs.IsNotFound(err) {
			_ = stdinR.Close()
			_ = stdinW.Close()
			_ = stdoutR.Close()
			_ = stdoutW.Close()
			_ = logWriter.Close()
			return errors.Wrap(err, "environment/containerd: failed to attach to task")
		}

		task, err = c.NewTask(e.context(ctx), cio.NewCreator(ioOpts...))
		if err != nil {
			_ = stdinR.Close()
			_ = stdinW.Close()
			_ = stdoutR.Close()
			_ = stdoutW.Close()
			_ = logWriter.Close()
			return errors.Wrap(err, "environment/containerd: failed to create task")
		}
		createdTask = true
	}

	// Register Wait before Start. Start() creates a new task through Attach(),
	// then starts that same task after Attach returns; registering the wait
	// channel here makes immediate exits observable instead of racing startup.
	waitCtx, waitStop := context.WithCancel(context.Background())
	exitC, err := task.Wait(e.context(waitCtx))
	if err != nil {
		waitStop()
		if createdTask {
			if _, cleanupErr := task.Delete(e.context(context.Background()), containerdclient.WithProcessKill); cleanupErr != nil {
				warnContainerdCleanupError(e.log(), cleanupErr, "failed to delete containerd task after attach wait error")
			}
		}
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = logWriter.Close()
		return errors.Wrap(err, "environment/containerd: failed to wait on task")
	}

	pollCtx, pollStop := context.WithCancel(context.Background())
	oomCtx, oomStop := context.WithCancel(context.Background())
	cleanupAttach := func() {
		waitStop()
		pollStop()
		oomStop()
		if createdTask {
			if _, cleanupErr := task.Delete(e.context(context.Background()), containerdclient.WithProcessKill); cleanupErr != nil {
				warnContainerdCleanupError(e.log(), cleanupErr, "failed to delete duplicate containerd task after attach race")
			}
		}
		if taskIO := task.IO(); taskIO != nil {
			taskIO.Cancel()
			_ = taskIO.Close()
		}
		_ = stdinR.Close()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = logWriter.Close()
	}

	e.mu.Lock()
	if e.stdin != nil {
		e.mu.Unlock()
		cleanupAttach()
		return nil
	}
	e.task = task
	e.taskIO = task.IO()
	e.stdin = stdinW
	e.stdout = stdoutW
	e.waitStop = waitStop
	e.pollStop = pollStop
	e.oomStop = oomStop
	e.mu.Unlock()

	go e.consumeOutput(stdoutR, logWriter)
	go e.watchExit(waitCtx, exitC, task)
	go e.watchOOM(oomCtx)
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
	if stop.Type == remote.ProcessStopCommand && command == stop.Value {
		e.SetState(environment.ProcessStoppingState)
	}

	_, err := stdin.Write([]byte(command + "\n"))
	return errors.Wrap(err, "environment/containerd: could not write to container stream")
}

func (e *Environment) Readlog(lines int) ([]string, error) {
	if lines <= 0 {
		return []string{}, nil
	}
	if lines > maxContainerdReadlogLines {
		lines = maxContainerdReadlogLines
	}

	var out []string
	paths, err := e.logPathsNewestFirst()
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		remaining := lines - len(out)
		if remaining <= 0 {
			break
		}
		chunk, err := tailFileLines(path, remaining)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, errors.WithStack(err)
		}
		out = append(chunk, out...)
	}
	if len(out) > lines {
		out = out[len(out)-lines:]
	}
	return out, nil
}

func (e *Environment) consumeOutput(stdout *io.PipeReader, logWriter io.WriteCloser) {
	defer stdout.Close()
	defer logWriter.Close()

	if err := system.ScanReader(io.TeeReader(stdout, logWriter), func(v []byte) {
		e.logCallbackMx.Lock()
		defer e.logCallbackMx.Unlock()
		if e.logCallback != nil {
			e.logCallback(v)
		}
	}); err != nil && err != io.EOF {
		log.WithField("error", err).WithField("container_id", e.Id).Warn("error processing scanner line in console output")
	}
}

func (e *Environment) watchExit(waitCtx context.Context, exitC <-chan containerdclient.ExitStatus, task containerdclient.Task) {
	for {
		status, ok := <-exitC
		if !ok {
			return
		}

		code, exitedAt, err := status.Result()
		if err != nil {
			if waitCtx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}

			running, inspectErr := taskIsRunning(e.context(context.Background()), task)
			if inspectErr != nil {
				e.log().WithField("error", err).WithField("inspect_error", inspectErr).Warn("containerd task wait failed; preserving attach state because task status is unknown")
				return
			}
			if running {
				e.log().WithField("error", err).Warn("containerd task wait failed while task is still running; retrying wait")
				nextExitC, waitErr := task.Wait(e.context(waitCtx))
				if waitErr != nil {
					e.log().WithField("error", waitErr).Warn("failed to re-register containerd task wait after wait error")
					return
				}
				exitC = nextExitC
				continue
			}

			e.log().WithField("error", err).Warn("containerd task wait failed after task stopped")
			if code == 0 {
				code = containerdclient.UnknownExitStatus
			}
		}

		e.mu.Lock()
		e.lastExitCode = code
		e.lastExitTime = exitedAt
		e.mu.Unlock()

		if _, err := task.Delete(e.context(context.Background())); err != nil {
			warnContainerdCleanupError(e.log(), err, "failed to delete exited containerd task")
		}
		e.closeAttach()
		e.SetState(environment.ProcessOfflineState)
		return
	}
}

func (e *Environment) closeAttach() {
	e.mu.Lock()
	stdin := e.stdin
	stdout := e.stdout
	taskIO := e.taskIO
	waitStop := e.waitStop
	pollStop := e.pollStop
	oomStop := e.oomStop
	e.stdin = nil
	e.stdout = nil
	e.taskIO = nil
	e.task = nil
	e.waitStop = nil
	e.pollStop = nil
	e.oomStop = nil
	e.mu.Unlock()

	if waitStop != nil {
		waitStop()
	}
	if pollStop != nil {
		pollStop()
	}
	if oomStop != nil {
		oomStop()
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

func (e *Environment) watchOOM(ctx context.Context) {
	for {
		events, errs := e.client.Subscribe(e.context(ctx), `topic=="/tasks/oom"`)
		retry := false
		for !retry {
			select {
			case <-ctx.Done():
				return
			case err, ok := <-errs:
				if ok && (err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
					return
				}
				if ok {
					e.log().WithField("error", err).Warn("containerd OOM event subscription stopped; retrying")
				} else {
					e.log().Warn("containerd OOM event subscription closed; retrying")
				}
				retry = true
			case event, ok := <-events:
				if !ok {
					e.log().Warn("containerd OOM event stream closed; retrying")
					retry = true
					continue
				}
				if event == nil || event.Event == nil || event.Topic != ctrruntime.TaskOOMEventTopic {
					continue
				}
				var oom eventtypes.TaskOOM
				if err := typeurl.UnmarshalTo(event.Event, &oom); err != nil {
					e.log().WithField("error", err).Warn("could not decode containerd OOM event")
					continue
				}
				if oom.ContainerID != e.Id {
					continue
				}
				e.mu.Lock()
				e.lastOOM = true
				e.mu.Unlock()
			}
		}

		timer := time.NewTimer(oomSubscribeRetryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
	}
}

func (e *Environment) fifoRoot() (string, error) {
	return containerdFIFORoot()
}

func (e *Environment) logPath() (string, error) {
	return containerdServerLogPath(e.Id)
}

func (e *Environment) logPathsNewestFirst() ([]string, error) {
	path, err := e.logPath()
	if err != nil {
		return nil, err
	}
	paths := []string{path}
	for i := 1; i < containerdLogMaxFiles(); i++ {
		paths = append(paths, rotatedLogPath(path, i))
	}
	return paths, nil
}

func (e *Environment) removeLogs() error {
	paths, err := e.logPathsNewestFirst()
	if err != nil {
		return err
	}

	var firstErr error
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func truncateLog(path string) error {
	for i := 1; i < containerdLogMaxFiles(); i++ {
		if err := os.Remove(rotatedLogPath(path, i)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

type rotatingLogWriter struct {
	path     string
	file     *os.File
	size     int64
	maxSize  int64
	maxFiles int
}

func newRotatingLogWriter(path string) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	size := int64(0)
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}
	return &rotatingLogWriter{
		path:     path,
		file:     f,
		size:     size,
		maxSize:  containerdLogMaxSize(),
		maxFiles: containerdLogMaxFiles(),
	}, nil
}

func (w *rotatingLogWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		if w.file == nil {
			return total - len(p), os.ErrClosed
		}
		if w.maxSize > 0 && w.size >= w.maxSize {
			if err := w.rotate(); err != nil {
				return total - len(p), err
			}
		}

		chunk := p
		if w.maxSize > 0 {
			remaining := w.maxSize - w.size
			if remaining < int64(len(chunk)) {
				chunk = p[:int(remaining)]
			}
		}

		n, err := w.file.Write(chunk)
		w.size += int64(n)
		p = p[n:]
		if err != nil {
			return total - len(p), err
		}
		if n == 0 {
			return total - len(p), io.ErrShortWrite
		}
	}
	return total, nil
}

func (w *rotatingLogWriter) Close() error {
	if w.file == nil {
		return nil
	}
	return w.file.Close()
}

func (w *rotatingLogWriter) rotate() error {
	// TODO(T5): make rotation crash-safe with fsync/generation metadata before
	// increasing guarantees around concurrent Readlog during rotation.
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
		w.file = nil
	}
	for i := w.maxFiles - 2; i >= 1; i-- {
		src := rotatedLogPath(w.path, i)
		dst := rotatedLogPath(w.path, i+1)
		if err := os.Rename(src, dst); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if w.maxFiles > 1 {
		_ = os.Remove(rotatedLogPath(w.path, 1))
		if err := os.Rename(w.path, rotatedLogPath(w.path, 1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
		return err
	}

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	w.file = f
	w.size = 0
	return nil
}

func tailFileLines(path string, max int) ([]string, error) {
	if max <= 0 {
		return []string{}, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() == 0 {
		return []string{}, nil
	}

	const chunkSize int64 = 32 * 1024
	pos := st.Size()
	newlines := 0
	var buf []byte
	for pos > 0 && newlines <= max {
		readSize := chunkSize
		if pos < readSize {
			readSize = pos
		}
		pos -= readSize
		chunk := make([]byte, readSize)
		if _, err := f.ReadAt(chunk, pos); err != nil && err != io.EOF {
			return nil, err
		}
		newlines += bytes.Count(chunk, []byte{'\n'})
		buf = append(chunk, buf...)
	}

	buf = bytes.TrimRight(buf, "\n")
	if len(buf) == 0 {
		return []string{}, nil
	}
	out := strings.Split(string(buf), "\n")
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return out, nil
}

func rotatedLogPath(path string, generation int) string {
	return path + "." + strconv.Itoa(generation)
}

func containerdLogMaxSize() int64 {
	size, err := units.RAMInBytes(config.Get().Containerd.LogMaxSize)
	if err != nil || size <= 0 {
		return 5 * 1024 * 1024
	}
	return size
}

func containerdLogMaxFiles() int {
	if maxFiles := config.Get().Containerd.LogMaxFiles; maxFiles > 0 {
		return maxFiles
	}
	return 1
}

func signalFromString(value string) syscall.Signal {
	switch strings.ToUpper(value) {
	case "SIGABRT":
		return syscall.SIGABRT
	case "SIGINT", "C", "^C":
		return syscall.SIGINT
	case "SIGTERM":
		return syscall.SIGTERM
	case "SIGKILL":
		return syscall.SIGKILL
	default:
		if n, err := strconv.Atoi(value); err == nil {
			return syscall.Signal(n)
		}
		return syscall.SIGTERM
	}
}
