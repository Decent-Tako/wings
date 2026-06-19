package system

import "testing"

func TestGetSystemInformationSkipsDockerForContainerdRuntime(t *testing.T) {
	info, err := GetSystemInformation("containerd")
	if err != nil {
		t.Fatalf("GetSystemInformation() returned error: %v", err)
	}
	if info.Runtime == nil || info.Runtime.Name != "containerd" {
		t.Fatalf("expected containerd runtime metadata, got %+v", info.Runtime)
	}
	if info.Docker != nil {
		t.Fatalf("expected Docker metadata to be omitted for containerd runtime, got %+v", info.Docker)
	}
	if info.System.MemoryBytes <= 0 {
		t.Fatalf("expected host memory metadata, got %d", info.System.MemoryBytes)
	}
}
