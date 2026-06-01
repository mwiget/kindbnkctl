package cluster

import (
	"context"
	"fmt"
	"io"
)

// Backend identifies which local-Kubernetes provisioner drives the
// cluster. kindbnkctl historically only spoke kind; k3d was added as
// an alternative (faster cluster bring-up, single-binary k3s nodes)
// selectable via the argv[0] basename — invoke the binary as
// `k3dbnkctl` (a symlink to the same binary) to pick BackendK3d.
type Backend string

const (
	BackendKind Backend = "kind"
	BackendK3d  Backend = "k3d"
)

// Provisioner is the backend-agnostic surface the CLI's `cluster up`
// and `destroy` paths drive. Both Kind and K3d satisfy it. The two
// backends differ in three operator-visible ways the interface
// papers over: the config-file shape they consume (RenderConfig /
// ConfigArtifact), the k8s node name the TMM worker ends up with
// (WorkerNodeName), and the docker label their node containers carry
// (NodeContainerLabel).
type Provisioner interface {
	// Backend reports which provisioner this is.
	Backend() Backend
	// Tool is the CLI binary name on PATH ("kind" / "k3d").
	Tool() string
	// EnsurePresent verifies the backend's CLI is installed.
	EnsurePresent() error
	// RenderConfig produces the backend's cluster-config file body for
	// a two-node (1 control-plane/server + 1 worker/agent) cluster with
	// the default CNI disabled so Calico can be layered on identically
	// across backends.
	RenderConfig(name string) (string, error)
	// ConfigArtifact is the filename the rendered config is written to
	// under artifacts/ (kind.yaml / k3d.yaml).
	ConfigArtifact() string
	// ClusterExists reports whether a cluster of this name is present.
	ClusterExists(ctx context.Context, name string) (bool, error)
	// CreateCluster brings the cluster up from the rendered config.
	// nodeImage overrides the per-backend default node image when set.
	CreateCluster(ctx context.Context, name, config, nodeImage string) error
	// DeleteCluster tears the cluster down (idempotent).
	DeleteCluster(ctx context.Context, name string) error
	// WriteKubeconfig writes the cluster kubeconfig to path (mode 0600).
	WriteKubeconfig(ctx context.Context, name, path string) error
	// WorkerNodeName is the k8s node name of the non-control-plane node
	// (where TMM is pinned via the app=f5-tmm label).
	WorkerNodeName(name string) string
	// NodeContainerLabel is the `docker ps --filter label=…` selector
	// that matches this cluster's node containers.
	NodeContainerLabel(name string) string
	// DefaultNodeImage is the backend's pinned node image used when the
	// caller doesn't override it.
	DefaultNodeImage() string
}

// NewProvisioner returns the Provisioner for the chosen backend, wired
// to the given container runtime and progress writer.
func NewProvisioner(b Backend, rt Runtime, out io.Writer) (Provisioner, error) {
	switch b {
	case BackendKind:
		return &Kind{Runtime: rt, Out: out}, nil
	case BackendK3d:
		return &K3d{Runtime: rt, Out: out}, nil
	default:
		return nil, fmt.Errorf("unknown cluster backend %q (want kind or k3d)", b)
	}
}
