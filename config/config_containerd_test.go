package config

import (
	"path/filepath"
	"testing"
)

func TestSetAppliesContainerdIdentityFileDefaults(t *testing.T) {
	cfg, err := NewAtPath(filepath.Join(t.TempDir(), "wings.yml"))
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	setTestToken(cfg)
	cfg.ContainerRuntime = ContainerRuntimeContainerd
	cfg.System.Data = "/var/mnt/data/pelican/volumes"

	Set(cfg)
	got := Get()

	if got.System.User.Passwd.Directory != "/var/mnt/data/pelican/etc/passwd" {
		t.Fatalf("unexpected passwd directory: %s", got.System.User.Passwd.Directory)
	}
	if got.System.MachineID.Directory != "/var/mnt/data/pelican/etc/machine-id" {
		t.Fatalf("unexpected machine-id directory: %s", got.System.MachineID.Directory)
	}
}

func TestSetAppliesContainerdIdentityFileDefaultsWithoutVolumesSuffix(t *testing.T) {
	cfg, err := NewAtPath(filepath.Join(t.TempDir(), "wings.yml"))
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	setTestToken(cfg)
	cfg.ContainerRuntime = ContainerRuntimeContainerd
	cfg.System.Data = "/srv/pelican-data"

	Set(cfg)
	got := Get()

	if got.System.User.Passwd.Directory != "/srv/pelican-data/etc/passwd" {
		t.Fatalf("unexpected passwd directory: %s", got.System.User.Passwd.Directory)
	}
	if got.System.MachineID.Directory != "/srv/pelican-data/etc/machine-id" {
		t.Fatalf("unexpected machine-id directory: %s", got.System.MachineID.Directory)
	}
}

func TestSetLeavesDockerIdentityFileDefaultsUnchanged(t *testing.T) {
	cfg, err := NewAtPath(filepath.Join(t.TempDir(), "wings.yml"))
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	setTestToken(cfg)
	cfg.ContainerRuntime = ContainerRuntimeDocker
	cfg.System.Data = "/var/mnt/data/pelican/volumes"

	Set(cfg)
	got := Get()

	if got.System.User.Passwd.Directory != "/etc/pelican" {
		t.Fatalf("unexpected passwd directory for docker: %s", got.System.User.Passwd.Directory)
	}
	if got.System.MachineID.Directory != "/etc/pelican/machine-id" {
		t.Fatalf("unexpected machine-id directory for docker: %s", got.System.MachineID.Directory)
	}
}

func TestSetPreservesExplicitContainerdIdentityFileDirectories(t *testing.T) {
	cfg, err := NewAtPath(filepath.Join(t.TempDir(), "wings.yml"))
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	setTestToken(cfg)
	cfg.ContainerRuntime = ContainerRuntimeContainerd
	cfg.System.Data = "/var/mnt/data/pelican/volumes"
	cfg.System.User.Passwd.Directory = "/custom/passwd"
	cfg.System.MachineID.Directory = "/custom/machine-id"

	Set(cfg)
	got := Get()

	if got.System.User.Passwd.Directory != "/custom/passwd" {
		t.Fatalf("unexpected explicit passwd directory: %s", got.System.User.Passwd.Directory)
	}
	if got.System.MachineID.Directory != "/custom/machine-id" {
		t.Fatalf("unexpected explicit machine-id directory: %s", got.System.MachineID.Directory)
	}
}

func TestUpdateAppliesContainerdIdentityFileDefaults(t *testing.T) {
	cfg, err := NewAtPath(filepath.Join(t.TempDir(), "wings.yml"))
	if err != nil {
		t.Fatalf("failed to create config: %v", err)
	}
	setTestToken(cfg)
	cfg.System.Data = "/var/mnt/data/pelican/volumes"
	Set(cfg)

	Update(func(c *Configuration) {
		c.ContainerRuntime = ContainerRuntimeContainerd
	})
	got := Get()

	if got.System.User.Passwd.Directory != "/var/mnt/data/pelican/etc/passwd" {
		t.Fatalf("unexpected passwd directory after update: %s", got.System.User.Passwd.Directory)
	}
	if got.System.MachineID.Directory != "/var/mnt/data/pelican/etc/machine-id" {
		t.Fatalf("unexpected machine-id directory after update: %s", got.System.MachineID.Directory)
	}
}

func setTestToken(c *Configuration) {
	c.AuthenticationToken = "test-token"
}
