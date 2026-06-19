package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelican-dev/wings/config"
	"github.com/pelican-dev/wings/environment"
	"github.com/pelican-dev/wings/events"
)

func TestManagerPersistsStartedAtInStateFile(t *testing.T) {
	newManagerStateTestConfig(t)
	startedAt := time.Date(2026, 6, 19, 10, 11, 12, 0, time.UTC)
	env := &managerStateTestEnv{state: environment.ProcessRunningState, startedAt: startedAt}
	manager := NewEmptyManager(nil)
	manager.Put([]*Server{managerStateTestServer("server-a", env)})

	if err := manager.PersistStates(); err != nil {
		t.Fatalf("PersistStates() returned error: %v", err)
	}

	data, err := os.ReadFile(config.Get().System.GetStatesPath())
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}
	if !containsAll(string(data), `"server-a"`, `"state":"running"`, `"started_at":"2026-06-19T10:11:12Z"`) {
		t.Fatalf("state file did not include persisted started_at: %s", data)
	}
}

func TestManagerReadStatesRestoresStartedAt(t *testing.T) {
	newManagerStateTestConfig(t)
	startedAt := time.Date(2026, 6, 19, 10, 11, 12, 0, time.UTC)
	env := &managerStateTestEnv{state: environment.ProcessOfflineState}
	manager := NewEmptyManager(nil)
	manager.Put([]*Server{managerStateTestServer("server-a", env)})

	data := []byte(`{"server-a":{"state":"running","started_at":"2026-06-19T10:11:12Z"}}`)
	if err := os.WriteFile(config.Get().System.GetStatesPath(), data, 0o644); err != nil {
		t.Fatalf("failed to write state file: %v", err)
	}

	states, err := manager.ReadStates()
	if err != nil {
		t.Fatalf("ReadStates() returned error: %v", err)
	}
	if states["server-a"] != environment.ProcessRunningState {
		t.Fatalf("expected restored running state, got %q", states["server-a"])
	}
	if !env.restoredStartedAt.Equal(startedAt) {
		t.Fatalf("expected started_at restore %s, got %s", startedAt, env.restoredStartedAt)
	}
}

func TestManagerReadStatesSupportsLegacyStringFormat(t *testing.T) {
	newManagerStateTestConfig(t)
	env := &managerStateTestEnv{state: environment.ProcessOfflineState}
	manager := NewEmptyManager(nil)
	manager.Put([]*Server{managerStateTestServer("server-a", env)})

	data := []byte(`{"server-a":"running"}`)
	if err := os.WriteFile(config.Get().System.GetStatesPath(), data, 0o644); err != nil {
		t.Fatalf("failed to write legacy state file: %v", err)
	}

	states, err := manager.ReadStates()
	if err != nil {
		t.Fatalf("ReadStates() returned error: %v", err)
	}
	if states["server-a"] != environment.ProcessRunningState {
		t.Fatalf("expected restored running state from legacy file, got %q", states["server-a"])
	}
}

func newManagerStateTestConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.NewAtPath(filepath.Join(dir, "wings.yml"))
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	cfg.System.RootDirectory = dir
	config.Set(cfg)
}

func managerStateTestServer(id string, env environment.ProcessEnvironment) *Server {
	return &Server{
		cfg: Configuration{
			Uuid: id,
		},
		Environment: env,
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}

type managerStateTestEnv struct {
	state             string
	startedAt         time.Time
	restoredStartedAt time.Time
}

func (e *managerStateTestEnv) Type() string { return "test" }
func (e *managerStateTestEnv) Config() *environment.Configuration {
	return environment.NewConfiguration(environment.Settings{}, nil)
}
func (e *managerStateTestEnv) Events() *events.Bus   { return events.NewBus() }
func (e *managerStateTestEnv) Exists() (bool, error) { return true, nil }
func (e *managerStateTestEnv) IsRunning(context.Context) (bool, error) {
	return e.state == environment.ProcessRunningState, nil
}
func (e *managerStateTestEnv) InSituUpdate() error                 { return nil }
func (e *managerStateTestEnv) OnBeforeStart(context.Context) error { return nil }
func (e *managerStateTestEnv) Start(context.Context) error {
	e.state = environment.ProcessRunningState
	return nil
}
func (e *managerStateTestEnv) Stop(context.Context) error {
	e.state = environment.ProcessOfflineState
	return nil
}
func (e *managerStateTestEnv) WaitForStop(context.Context, time.Duration, bool) error { return nil }
func (e *managerStateTestEnv) Terminate(context.Context, string) error {
	e.state = environment.ProcessOfflineState
	return nil
}
func (e *managerStateTestEnv) Destroy() error                        { e.state = environment.ProcessOfflineState; return nil }
func (e *managerStateTestEnv) ExitState() (uint32, bool, error)      { return 0, false, nil }
func (e *managerStateTestEnv) Create() error                         { return nil }
func (e *managerStateTestEnv) Attach(context.Context) error          { return nil }
func (e *managerStateTestEnv) SendCommand(string) error              { return nil }
func (e *managerStateTestEnv) Readlog(int) ([]string, error)         { return nil, nil }
func (e *managerStateTestEnv) State() string                         { return e.state }
func (e *managerStateTestEnv) SetState(state string)                 { e.state = state }
func (e *managerStateTestEnv) Uptime(context.Context) (int64, error) { return 0, nil }
func (e *managerStateTestEnv) SetLogCallback(func([]byte))           {}
func (e *managerStateTestEnv) StartedAt() (time.Time, bool) {
	return e.startedAt, !e.startedAt.IsZero()
}
func (e *managerStateTestEnv) RestoreStartedAt(t time.Time) { e.restoredStartedAt = t }
