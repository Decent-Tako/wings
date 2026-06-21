package containerd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"emperror.dev/errors"

	"github.com/pelican-dev/wings/config"
)

const maxContainerdIdentifierLength = 128

var containerdIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func containerdRuntimeRoot() (string, error) {
	return cleanContainerdDirectory(config.Get().Containerd.RuntimeRoot, "containerd.runtime_root")
}

func containerdFIFORoot() (string, error) {
	root, err := containerdRuntimeRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "fifo"), nil
}

func containerdLogDirectory() (string, error) {
	return cleanContainerdDirectory(config.Get().Containerd.LogDirectory, "containerd.log_directory")
}

func containerdServerLogPath(id string) (string, error) {
	return containerdLogPath(id, ".log")
}

func containerdInstallerLogPath(id string) (string, error) {
	return containerdLogPath(id, "-installer.log")
}

func containerdLogPath(id, suffix string) (string, error) {
	dir, err := containerdLogDirectory()
	if err != nil {
		return "", err
	}
	name, err := cleanContainerdIdentifier(id, "containerd log identifier")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+suffix)
	if !pathWithinDirectory(dir, path) {
		return "", errors.Errorf("environment/containerd: resolved log path %q escapes %s", path, dir)
	}
	return path, nil
}

func ensureContainerdDirectory(dir string) error {
	// codeql[go/path-injection] dir is daemon-local admin configuration already
	// normalized and bounded by cleanContainerdDirectory before reaching this sink.
	return os.MkdirAll(dir, 0o700)
}

func cleanContainerdDirectory(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.Errorf("environment/containerd: %s cannot be empty", field)
	}
	if strings.ContainsRune(value, '\x00') {
		return "", errors.Errorf("environment/containerd: %s contains an invalid path byte", field)
	}
	cleaned := filepath.Clean(value)
	if !filepath.IsAbs(cleaned) {
		return "", errors.Errorf("environment/containerd: %s must be an absolute path", field)
	}
	if cleaned == string(filepath.Separator) {
		return "", errors.Errorf("environment/containerd: %s cannot be the filesystem root", field)
	}
	return cleaned, nil
}

func cleanContainerdIdentifier(value, field string) (string, error) {
	if value == "" {
		return "", errors.Errorf("environment/containerd: %s cannot be empty", field)
	}
	if len(value) > maxContainerdIdentifierLength {
		return "", errors.Errorf("environment/containerd: %s exceeds %d characters", field, maxContainerdIdentifierLength)
	}
	if value == "." || value == ".." || !containerdIdentifierPattern.MatchString(value) {
		return "", errors.Errorf("environment/containerd: %s contains unsafe path characters", field)
	}
	return value, nil
}

func pathWithinDirectory(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
