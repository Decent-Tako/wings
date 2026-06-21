package docker

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
)

func TestInstallerExitError(t *testing.T) {
	if err := installerExitError(container.WaitResponse{StatusCode: 0}); err != nil {
		t.Fatalf("expected zero exit status to succeed, got %v", err)
	}

	err := installerExitError(container.WaitResponse{
		StatusCode: 2,
		Error:      &container.WaitExitError{Message: "script failed"},
	})
	if err == nil {
		t.Fatal("expected non-zero exit status to return an error")
	}
	if !strings.Contains(err.Error(), "exited with code 2") || !strings.Contains(err.Error(), "script failed") {
		t.Fatalf("expected exit code and wait error message, got %v", err)
	}
}
