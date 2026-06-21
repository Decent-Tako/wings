package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pelican-dev/wings/environment"
)

func TestCleanupRootRemovesCustomTempSubdirectory(t *testing.T) {
	root, err := os.MkdirTemp(os.TempDir(), "pelican-containerd-smoke-custom-")
	if err != nil {
		t.Fatalf("failed to create temp root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("leftover"), 0o600); err != nil {
		t.Fatalf("failed to write temp artifact: %v", err)
	}

	cleanupRoot(root)

	if _, err := os.Stat(root); !os.IsNotExist(err) {
		_ = os.RemoveAll(root)
		t.Fatalf("expected cleanupRoot to remove custom temp root, stat err=%v", err)
	}
}

func TestDefaultSmokeRootUsesGoTempDirectory(t *testing.T) {
	root := defaultSmokeRoot()
	if !isTempSubdirectory(root) {
		t.Fatalf("expected default smoke root %q to be a temp subdirectory of %q", root, os.TempDir())
	}
	if filepath.Base(root) != "pelican-containerd-smoke" {
		t.Fatalf("expected stable default smoke root basename, got %q", filepath.Base(root))
	}
}

func TestCleanupRootSkipsUnsafeRoots(t *testing.T) {
	for _, root := range []string{"", ".", string(os.PathSeparator), os.TempDir(), filepath.Join(os.TempDir(), "..")} {
		t.Run(root, func(t *testing.T) {
			cleanupRoot(root)
			if _, err := os.Stat(os.TempDir()); err != nil {
				t.Fatalf("expected temp directory to remain after skipped cleanup, got %v", err)
			}
		})
	}
}

func TestAssertStaysRunningChecksProcessAtDeadline(t *testing.T) {
	proc := &fakeStatusProcess{running: false, state: environment.ProcessOfflineState}

	err := assertStaysRunning(context.Background(), proc, time.Nanosecond)
	if err == nil {
		t.Fatal("expected stopped process to fail stays-running assertion")
	}
}

func TestAssertStaysRunningPassesForRunningProcess(t *testing.T) {
	proc := &fakeStatusProcess{running: true, state: environment.ProcessRunningState}

	if err := assertStaysRunning(context.Background(), proc, time.Millisecond); err != nil {
		t.Fatalf("expected running process to pass stays-running assertion, got %v", err)
	}
}

type fakeStatusProcess struct {
	running bool
	state   string
}

func (f *fakeStatusProcess) IsRunning(context.Context) (bool, error) {
	return f.running, nil
}

func (f *fakeStatusProcess) State() string {
	return f.state
}
