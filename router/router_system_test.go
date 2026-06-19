package router

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/pelican-dev/wings/config"
)

func TestDockerSystemEndpointsUnsupportedUnderContainerd(t *testing.T) {
	cfg, err := config.NewAtPath(filepath.Join(t.TempDir(), "wings.yml"))
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	cfg.ContainerRuntime = config.ContainerRuntimeContainerd
	config.Set(cfg)
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name    string
		method  string
		path    string
		handler gin.HandlerFunc
	}{
		{
			name:    "disk usage",
			method:  http.MethodGet,
			path:    "/api/system/docker/disk",
			handler: getDockerDiskUsage,
		},
		{
			name:    "image prune",
			method:  http.MethodDelete,
			path:    "/api/system/docker/image/prune",
			handler: pruneDockerImages,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(tt.method, tt.path, nil)

			tt.handler(c)

			if w.Code != http.StatusNotImplemented {
				t.Fatalf("expected status %d, got %d with body %s", http.StatusNotImplemented, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "container_runtime is containerd") {
				t.Fatalf("expected unsupported runtime message, got %s", w.Body.String())
			}
		})
	}
}
