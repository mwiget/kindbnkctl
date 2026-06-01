package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/template"

	"github.com/mwiget/kindbnkctl/internal/embedded"
	"github.com/mwiget/kindbnkctl/internal/version"
)

// K3dConfig is the input shape for the embedded k3d.yaml.tmpl.
type K3dConfig struct {
	Name string
}

// RenderK3dConfig substitutes the embedded k3d Simple-config template.
func RenderK3dConfig(in K3dConfig) (string, error) {
	raw, err := embedded.Templates.ReadFile("templates/k3d.yaml.tmpl")
	if err != nil {
		return "", err
	}
	t, err := template.New("k3d").Parse(string(raw))
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, in); err != nil {
		return "", err
	}
	return b.String(), nil
}

// K3d wraps the `k3d` CLI. It implements Provisioner. k3d always talks
// to docker (it has no podman provider), so Runtime is carried only for
// symmetry with Kind and the docker label helpers.
type K3d struct {
	Runtime Runtime
	Out     io.Writer
}

func (k *K3d) cmd(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "k3d", args...)
}

// Backend reports this provisioner as k3d.
func (k *K3d) Backend() Backend { return BackendK3d }

// Tool is the CLI binary k3d drives.
func (k *K3d) Tool() string { return "k3d" }

// RenderConfig renders the embedded k3d.yaml.tmpl.
func (k *K3d) RenderConfig(name string) (string, error) {
	return RenderK3dConfig(K3dConfig{Name: name})
}

// ConfigArtifact is the filename the rendered k3d config lands at.
func (k *K3d) ConfigArtifact() string { return "k3d.yaml" }

// DefaultNodeImage is the pinned k3s image (k3d ignores poc.yaml's
// kind_node_image; that's a kindest/node ref).
func (k *K3d) DefaultNodeImage() string { return version.K3sNodeImage }

// WorkerNodeName is the k8s node name of k3d's agent node. k3d names
// nodes "k3d-<cluster>-agent-0" (and the matching container the same).
func (k *K3d) WorkerNodeName(name string) string {
	return "k3d-" + name + "-agent-0"
}

// NodeContainerLabel selects k3d's node containers for this cluster.
func (k *K3d) NodeContainerLabel(name string) string {
	return "k3d.cluster=" + name
}

// EnsurePresent verifies the `k3d` binary is on PATH.
func (k *K3d) EnsurePresent() error {
	if _, err := exec.LookPath("k3d"); err != nil {
		return fmt.Errorf("k3d not found on PATH — install k3d (https://k3d.io/stable/#installation) and retry")
	}
	return nil
}

// ClusterExists reports whether `k3d cluster list -o json` includes name.
func (k *K3d) ClusterExists(ctx context.Context, name string) (bool, error) {
	c := k.cmd(ctx, "cluster", "list", "-o", "json")
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = io.Discard
	if err := c.Run(); err != nil {
		return false, fmt.Errorf("k3d cluster list: %w", err)
	}
	var clusters []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out.Bytes(), &clusters); err != nil {
		return false, fmt.Errorf("parse k3d cluster list: %w", err)
	}
	for _, c := range clusters {
		if c.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// CreateCluster writes the rendered config to a temp file (k3d's
// --config wants a path, not stdin) and runs `k3d cluster create`.
// nodeImage overrides the pinned k3s image when set.
func (k *K3d) CreateCluster(ctx context.Context, name, config, nodeImage string) error {
	f, err := os.CreateTemp("", "k3d-"+name+"-*.yaml")
	if err != nil {
		return fmt.Errorf("temp k3d config: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(config); err != nil {
		f.Close()
		return fmt.Errorf("write k3d config: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if nodeImage == "" {
		nodeImage = k.DefaultNodeImage()
	}
	args := []string{"cluster", "create", "--config", f.Name()}
	if nodeImage != "" {
		args = append(args, "--image", nodeImage)
	}
	c := k.cmd(ctx, args...)
	c.Stdout = k.Out
	c.Stderr = k.Out
	if err := c.Run(); err != nil {
		return fmt.Errorf("k3d cluster create %s: %w", name, err)
	}
	return nil
}

// DeleteCluster tears down the named k3d cluster. Idempotent — a
// missing cluster is not an error.
func (k *K3d) DeleteCluster(ctx context.Context, name string) error {
	exists, err := k.ClusterExists(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintf(k.Out, "k3d cluster %q not present — nothing to delete\n", name)
		return nil
	}
	c := k.cmd(ctx, "cluster", "delete", name)
	c.Stdout = k.Out
	c.Stderr = k.Out
	return c.Run()
}

// WriteKubeconfig invokes `k3d kubeconfig get` for the cluster and
// writes the result to path with 0600 permissions.
func (k *K3d) WriteKubeconfig(ctx context.Context, name, path string) error {
	c := k.cmd(ctx, "kubeconfig", "get", name)
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = io.Discard
	if err := c.Run(); err != nil {
		return fmt.Errorf("k3d kubeconfig get %s: %w", name, err)
	}
	if strings.TrimSpace(out.String()) == "" {
		return fmt.Errorf("k3d kubeconfig get %s: empty kubeconfig", name)
	}
	return os.WriteFile(path, out.Bytes(), 0o600)
}
