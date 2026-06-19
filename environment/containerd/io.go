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

	logWriter, err := newRotatingLogWriter(e.logPath())
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

	exitC, err := task.Wait(e.context(ctx))
	if err != nil {
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
	e.mu.Lock()
	e.task = task
	e.taskIO = task.IO()
	e.stdin = stdinW
	e.stdout = stdoutW
	e.pollStop = pollStop
	e.oomStop = oomStop
	e.mu.Unlock()

	go e.consumeOutput(stdoutR, logWriter)
	go e.watchExit(exitC, task)
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
	if stop.Type == "command" && command == stop.Value {
		e.SetState(environment.ProcessStoppingState)
	}

	_, err := stdin.Write([]byte(command + "\n"))
	return errors.Wrap(err, "environment/containerd: could not write to container stream")
}

func (e *Environment) Readlog(lines int) ([]string, error) {
	if lines <= 0 {
		return []string{}, nil
	}

	out := make([]string, 0, lines)
	for _, path := range e.logPathsNewestFirst() {
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
	e.mu.Unlock()

	if _, err := task.Delete(e.context(context.Background())); err != nil {
		warnContainerdCleanupError(e.log(), err, "failed to delete exited containerd task")
	}
	e.closeAttach()
	e.SetState(environment.ProcessOfflineState)
}

func (e *Environment) closeAttach() {
	e.mu.Lock()
	stdin := e.stdin
	stdout := e.stdout
	taskIO := e.taskIO
	pollStop := e.pollStop
	oomStop := e.oomStop
	e.stdin = nil
	e.stdout = nil
	e.taskIO = nil
	e.task = nil
	e.pollStop = nil
	e.oomStop = nil
	e.mu.Unlock()

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
	events, errs := e.client.Subscribe(e.context(ctx), `topic=="/tasks/oom"`)
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-errs:
			if !ok {
				return
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				e.log().WithField("error", err).Warn("containerd OOM event subscription stopped")
			}
			return
		case event, ok := <-events:
			if !ok {
				return
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
}

func (e *Environment) fifoRoot() string {
	return filepath.Join(config.Get().Containerd.RuntimeRoot, "fifo")
}

func (e *Environment) logPath() string {
	return filepath.Join(config.Get().Containerd.LogDirectory, e.Id+".log")
}

func (e *Environment) logPathsNewestFirst() []string {
	path := e.logPath()
	paths := []string{path}
	for i := 1; i < containerdLogMaxFiles(); i++ {
		paths = append(paths, rotatedLogPath(path, i))
	}
	return paths
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
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return err
		}
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
