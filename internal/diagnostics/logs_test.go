package diagnostics

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelican-dev/wings/config"
)

func TestGenerateDiagnosticsReportSkipsDockerSectionsForContainerdRuntime(t *testing.T) {
	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "wings.yml"))
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	cfg.ContainerRuntime = config.ContainerRuntimeContainerd
	cfg.System.LogDirectory = t.TempDir()
	config.Set(cfg)

	report, err := GenerateDiagnosticsReport(false, false, 10)
	if err != nil {
		t.Fatalf("GenerateDiagnosticsReport() returned error: %v", err)
	}
	if !strings.Contains(report, "Container Runtime: containerd") {
		t.Fatalf("expected containerd runtime metadata in report, got:\n%s", report)
	}
	if strings.Contains(report, "Docker: Running Containers") {
		t.Fatalf("expected Docker container listing to be omitted, got:\n%s", report)
	}
}
