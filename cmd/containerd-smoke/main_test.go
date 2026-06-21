package main

import (
	"os"
	"path/filepath"
	"testing"
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
