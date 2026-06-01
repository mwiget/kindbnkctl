package cluster

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// DockerCLI wraps the small docker (or podman) surface we need.
type DockerCLI struct {
	Runtime Runtime
	Out     io.Writer
}

func (d *DockerCLI) cmd(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, string(d.Runtime), args...)
}

// NetworkExists returns true iff a docker network named `name` is
// already known to the runtime. Uses `network ls --filter name=^X$` so
// "not found" is an empty result line rather than an error — avoids
// the pile of message variants docker/podman use for missing networks
// (e.g. "Error response from daemon: network <name> not found").
func (d *DockerCLI) NetworkExists(ctx context.Context, name string) (bool, error) {
	c := d.cmd(ctx, "network", "ls",
		"--filter", "name=^"+name+"$",
		"--format", "{{.Name}}")
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return false, fmt.Errorf("network ls: %w (%s)",
			err, strings.TrimSpace(stderr.String()))
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

// CreateBridgeNetwork creates a bridge-driver network with the given
// subnet. Idempotent — if a network with that name already exists,
// it is left in place (subnet not re-validated).
func (d *DockerCLI) CreateBridgeNetwork(ctx context.Context, name, subnet string) error {
	exists, err := d.NetworkExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		fmt.Fprintf(d.Out, "  network %s already exists — leaving in place\n", name)
		return nil
	}
	args := []string{"network", "create", "-d", "bridge", name}
	if subnet != "" {
		args = append(args, "--subnet", subnet)
	}
	c := d.cmd(ctx, args...)
	c.Stdout = d.Out
	c.Stderr = d.Out
	if err := c.Run(); err != nil {
		return fmt.Errorf("create network %s: %w", name, err)
	}
	return nil
}

// RemoveNetwork removes a docker network by name. Idempotent.
func (d *DockerCLI) RemoveNetwork(ctx context.Context, name string) error {
	exists, err := d.NetworkExists(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	c := d.cmd(ctx, "network", "rm", name)
	c.Stdout = d.Out
	c.Stderr = d.Out
	if err := c.Run(); err != nil {
		return fmt.Errorf("network rm %s: %w", name, err)
	}
	return nil
}

// ConnectNetwork attaches the named docker network to a container
// (kind node). Idempotent — if the container is already on the
// network, this is a no-op.
func (d *DockerCLI) ConnectNetwork(ctx context.Context, network, container string) error {
	if attached, err := d.IsAttached(ctx, network, container); err != nil {
		return err
	} else if attached {
		fmt.Fprintf(d.Out, "  %s already attached to %s\n", container, network)
		return nil
	}
	c := d.cmd(ctx, "network", "connect", network, container)
	c.Stdout = d.Out
	c.Stderr = d.Out
	if err := c.Run(); err != nil {
		return fmt.Errorf("connect %s → %s: %w", container, network, err)
	}
	return nil
}

// IsAttached reports whether `container` is on `network`.
func (d *DockerCLI) IsAttached(ctx context.Context, network, container string) (bool, error) {
	// `<runtime> network inspect <net> --format '{{range .Containers}}{{.Name}} {{end}}'`
	c := d.cmd(ctx, "network", "inspect", network, "--format",
		"{{range .Containers}}{{.Name}} {{end}}")
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return false, fmt.Errorf("network inspect %s: %w (%s)",
			network, err, strings.TrimSpace(stderr.String()))
	}
	for _, name := range strings.Fields(stdout.String()) {
		if name == container {
			return true, nil
		}
	}
	return false, nil
}

// NodeContainers returns the docker container names of the cluster's
// nodes, matched by the backend's node-container label filter (kind:
// `io.x-k8s.kind.cluster=<name>` → `<cluster>-control-plane` /
// `<cluster>-worker`; k3d: `k3d.cluster=<name>` → `k3d-<cluster>-server-0`
// / `k3d-<cluster>-agent-0`). The filter is supplied by the
// Provisioner via NodeContainerLabel.
func (d *DockerCLI) NodeContainers(ctx context.Context, labelFilter string) ([]string, error) {
	// We can't trust `kind get nodes` for the exact container names on
	// older kind versions (and k3d names them differently); ask the
	// runtime directly with the backend's node-container label.
	c := d.cmd(ctx, "ps", "--filter", "label="+labelFilter,
		"--format", "{{.Names}}")
	var stdout bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = io.Discard
	if err := c.Run(); err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		n = strings.TrimSpace(n)
		if n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}
