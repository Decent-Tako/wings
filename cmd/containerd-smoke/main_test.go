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
