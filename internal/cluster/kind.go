package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/template"

	"github.com/mwiget/kindbnkctl/internal/embedded"
)

// KindConfig is the input shape for the embedded kind.yaml.tmpl.
type KindConfig struct {
	Name string
}

// RenderKindConfig substitutes the embedded template.
func RenderKindConfig(in KindConfig) (string, error) {
	raw, err := embedded.Templates.ReadFile("templates/kind.yaml.tmpl")
	if err != nil {
		return "", err
	}
	t, err := template.New("kind").Parse(string(raw))
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, in); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Kind wraps the `kind` CLI for the runtime we picked.
type Kind struct {
	Runtime Runtime
	Out     io.Writer
}

func (k *Kind) cmd(ctx context.Context, args ...string) *exec.Cmd {
	c := exec.CommandContext(ctx, "kind", args...)
	c.Env = append(os.Environ(), k.Runtime.KindEnv()...)
	return c
}

// EnsurePresent verifies the `kind` binary is on PATH.
func (k *Kind) EnsurePresent() error {
	if _, err := exec.LookPath("kind"); err != nil {
		return fmt.Errorf("kind not found on PATH — install kind (https://kind.sigs.k8s.io/docs/user/quick-start/) and retry")
	}
	return nil
}

// ClusterExists reports whether `kind get clusters` lists name.
func (k *Kind) ClusterExists(ctx context.Context, name string) (bool, error) {
	c := k.cmd(ctx, "get", "clusters")
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = io.Discard
	if err := c.Run(); err != nil {
		return false, fmt.Errorf("kind get clusters: %w", err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

// CreateCluster runs `kind create cluster` with the rendered config
// piped to stdin so the operator's PoC repo is the source of truth.
// nodeImage overrides the kindest/node image (we want a pinned version,
// not whatever kind ships by default).
func (k *Kind) CreateCluster(ctx context.Context, name, config, nodeImage string) error {
	args := []string{"create", "cluster", "--name", name, "--config", "-"}
	if nodeImage != "" {
		args = append(args, "--image", nodeImage)
	}
	c := k.cmd(ctx, args...)
	c.Stdin = strings.NewReader(config)
	c.Stdout = k.Out
	c.Stderr = k.Out
	if err := c.Run(); err != nil {
		return fmt.Errorf("kind create cluster %s: %w", name, err)
	}
	return nil
}

// DeleteCluster tears down the named kind cluster. Idempotent — a
// missing cluster is not an error.
func (k *Kind) DeleteCluster(ctx context.Context, name string) error {
	exists, err := k.ClusterExists(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Fprintf(k.Out, "kind cluster %q not present — nothing to delete\n", name)
		return nil
	}
	c := k.cmd(ctx, "delete", "cluster", "--name", name)
	c.Stdout = k.Out
	c.Stderr = k.Out
	return c.Run()
}

// WriteKubeconfig invokes `kind get kubeconfig` for the cluster and
// writes the result to path with 0600 permissions.
func (k *Kind) WriteKubeconfig(ctx context.Context, name, path string) error {
	c := k.cmd(ctx, "get", "kubeconfig", "--name", name)
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = io.Discard
	if err := c.Run(); err != nil {
		return fmt.Errorf("kind get kubeconfig %s: %w", name, err)
	}
	return os.WriteFile(path, out.Bytes(), 0o600)
}
