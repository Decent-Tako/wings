package containerd

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"

	"github.com/pelican-dev/wings/environment"
	"github.com/pelican-dev/wings/events"
	"github.com/pelican-dev/wings/system"
)

var (
	ErrNotAttached        = errors.Sentinel("not attached to instance")
	ErrUnsupportedNetwork = errors.Sentinel("unsupported containerd networking configuration")
)

const labelStartedAt = "pelican.dev/started_at"

var _ environment.ProcessEnvironment = (*Environment)(nil)
var _ environment.ProcessMetadataUpdater = (*Environment)(nil)
var _ environment.AttachedState = (*Environment)(nil)

type Environment struct {
	mu sync.RWMutex

	Id            string
	Configuration *environment.Configuration

	meta   environment.ProcessMetadata
	client clientAPI

	emitter *events.Bus
	st      *system.AtomicString

	logCallbackMx sync.Mutex
	logCallback   func([]byte)

	task      containerdclient.Task
	taskIO    cio.IO
	stdin     *io.PipeWriter
	stdout    *io.PipeWriter
	waitStop  context.CancelFunc
	pollStop  context.CancelFunc
	oomStop   context.CancelFunc
	startedAt time.Time

	lastExitCode uint32
	lastExitTime time.Time
	lastOOM      bool
}

func New(id string, meta environment.ProcessMetadata, cfg *environment.Configuration, cli clientAPI) *Environment {
	return &Environment{
		Id:            id,
		Configuration: cfg,
		meta:          meta,
		client:        cli,
		st:            system.NewAtomicString(environment.ProcessOfflineState),
		emitter:       events.NewBus(),
	}
}

func (e *Environment) log() *log.Entry {
	return log.WithField("environment", e.Type()).WithField("container_id", e.Id)
}

func (e *Environment) Type() string {
	return "containerd"
}

func (e *Environment) Config() *environment.Configuration {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.Configuration
}

func (e *Environment) Events() *events.Bus {
	return e.emitter
}

func (e *Environment) State() string {
	return e.st.Load()
}

func (e *Environment) SetState(state string) {
	if state != environment.ProcessOfflineState &&
		state != environment.ProcessStartingState &&
		state != environment.ProcessRunningState &&
		state != environment.ProcessStoppingState {
		panic(errors.New(fmt.Sprintf("invalid server state received: %s", state)))
	}

	if e.State() != state {
		e.st.Store(state)
		e.Events().Publish(environment.StateChangeEvent, state)
	}
}

func (e *Environment) SetLogCallback(f func([]byte)) {
	e.logCallbackMx.Lock()
	defer e.logCallbackMx.Unlock()

	e.logCallback = f
}

func (e *Environment) SetProcessMetadata(m environment.ProcessMetadata) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.meta = m
}

func (e *Environment) IsAttached() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.stdin != nil
}

func (e *Environment) context(ctx context.Context) context.Context {
	return WithNamespace(ctx)
}

func (e *Environment) container(ctx context.Context) (containerdclient.Container, error) {
	return e.client.LoadContainer(e.context(ctx), e.Id)
}

func (e *Environment) currentTask(ctx context.Context) (containerdclient.Task, error) {
	c, err := e.container(ctx)
	if err != nil {
		return nil, err
	}
	return c.Task(e.context(ctx), nil)
}
