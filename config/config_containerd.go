package config

type ContainerRuntime string

const (
	ContainerRuntimeDocker     ContainerRuntime = "docker"
	ContainerRuntimeContainerd ContainerRuntime = "containerd"
)

// ContainerdConfiguration defines the settings used when Wings talks directly
// to a containerd daemon instead of the Docker Engine API.
type ContainerdConfiguration struct {
	// Address is the containerd gRPC socket path.
	Address string `default:"/run/containerd/containerd.sock" json:"address" yaml:"address"`

	// Namespace isolates Wings-managed containers from system and Kubernetes
	// workloads in the containerd metadata store.
	Namespace string `default:"pelican" json:"namespace" yaml:"namespace"`

	// Snapshotter controls the containerd snapshotter used for server rootfs
	// snapshots.
	Snapshotter string `default:"overlayfs" json:"snapshotter" yaml:"snapshotter"`

	// Runtime is the OCI runtime name used by containerd for new tasks.
	Runtime string `default:"io.containerd.runc.v2" json:"runtime" yaml:"runtime"`

	// RuntimeRoot stores FIFOs, captured logs, and other per-task runtime files.
	RuntimeRoot string `default:"/run/pelican/containerd" json:"runtime_root" yaml:"runtime_root"`

	// LogDirectory stores captured stdout/stderr so Readlog can work without a
	// Docker-style log API.
	LogDirectory string `default:"/var/log/pelican/containerd" json:"log_directory" yaml:"log_directory"`
}
