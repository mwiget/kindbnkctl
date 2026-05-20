// Package cluster wraps kind + docker for kindbnkctl's two-node BNK
// deployment. The cluster is fixed at one combined control-plane +
// worker and one worker labelled for TMM. Topology is not configurable.
package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Runtime describes the container provider kind talks to. "docker"
// is the default; "podman" is wired via the
// KIND_EXPERIMENTAL_PROVIDER=podman env var.
type Runtime string

const (
	RuntimeDocker Runtime = "docker"
	RuntimePodman Runtime = "podman"
)

// Detect picks an available container runtime, preferring the one the
// caller asks for. Returns the first that responds to `<rt> version`.
func Detect(ctx context.Context, prefer Runtime) (Runtime, error) {
	candidates := []Runtime{prefer}
	if prefer != RuntimeDocker {
		candidates = append(candidates, RuntimeDocker)
	}
	if prefer != RuntimePodman {
		candidates = append(candidates, RuntimePodman)
	}
	var firstErr error
	for _, rt := range candidates {
		if rt == "" {
			continue
		}
		if _, err := exec.LookPath(string(rt)); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s not found on PATH", rt)
			}
			continue
		}
		cmd := exec.CommandContext(ctx, string(rt), "version", "--format", "{{.Client.Version}}")
		if rt == RuntimePodman {
			// podman's version flag shape differs; fall back to plain.
			cmd = exec.CommandContext(ctx, string(rt), "version")
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		cmd.Stdout = io.Discard
		if err := cmd.Run(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s version: %w (%s)", rt, err, strings.TrimSpace(stderr.String()))
			}
			continue
		}
		return rt, nil
	}
	if firstErr == nil {
		return "", errors.New("no container runtime found (tried docker and podman)")
	}
	return "", firstErr
}

// KindEnv returns the env vars the runtime requires when invoking kind.
// docker is kind's default and needs no extra env; podman needs the
// experimental-provider toggle.
func (rt Runtime) KindEnv() []string {
	switch rt {
	case RuntimePodman:
		return []string{"KIND_EXPERIMENTAL_PROVIDER=podman"}
	default:
		return nil
	}
}
