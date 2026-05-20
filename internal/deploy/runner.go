// Package deploy installs the BNK platform on top of a kind cluster:
// namespaces, FAR pull secret, cert-manager, FLO, License CR,
// CNEInstance.
//
// Unlike dpubnkctl which shells kubectl/helm through an alpine/k8s
// container (so the operator only needs Docker for the kubespray path),
// kindbnkctl uses the operator's locally installed kubectl + helm.
// Rationale: kind itself runs on Docker so Docker is already required,
// and any operator running kind almost always has kubectl + helm
// installed alongside it. `kindbnkctl doctor` enforces this up front.
package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner wraps the small kubectl/helm surface kindbnkctl needs. The
// kubeconfig path is passed via KUBECONFIG env so neither tool needs to
// be told about it on the command line.
//
// HelmHome, when set, is exported as HELM_REPOSITORY_CONFIG /
// HELM_REPOSITORY_CACHE / HELM_REGISTRY_CONFIG so helm uses a per-PoC
// state directory instead of the operator's global ~/.config/helm and
// ~/.cache/helm. This isolates the run from any stale repo metadata
// the operator may have accumulated (e.g. a broken prometheus-community
// index that breaks every `helm upgrade --repo` invocation).
type Runner struct {
	KubeconfigPath string
	HelmHome       string
	Out            io.Writer
}

// CheckTools verifies kubectl + helm are on the operator's PATH.
func (r *Runner) CheckTools(ctx context.Context) error {
	if _, err := exec.LookPath("kubectl"); err != nil {
		return errors.New("kubectl not found on PATH — install kubectl (see https://kubernetes.io/docs/tasks/tools/) and retry")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		return errors.New("helm not found on PATH — install helm (see https://helm.sh/docs/intro/install/) and retry")
	}
	if _, err := os.Stat(r.KubeconfigPath); err != nil {
		return fmt.Errorf("kubeconfig %s: %w", r.KubeconfigPath, err)
	}
	return nil
}

func (r *Runner) env() []string {
	e := append(os.Environ(), "KUBECONFIG="+r.KubeconfigPath)
	if r.HelmHome != "" {
		// Point helm at a per-PoC state dir so we don't inherit the
		// operator's stale repo cache. MkdirAll is idempotent and
		// cheap; do it lazily on every env() call.
		_ = os.MkdirAll(r.HelmHome, 0o755)
		e = append(e,
			"HELM_REPOSITORY_CONFIG="+r.HelmHome+"/repositories.yaml",
			"HELM_REPOSITORY_CACHE="+r.HelmHome+"/cache",
			"HELM_REGISTRY_CONFIG="+r.HelmHome+"/registry.json",
		)
	}
	return e
}

// Apply pipes manifest YAML to `kubectl apply -f -`.
func (r *Runner) Apply(ctx context.Context, manifest string) error {
	return r.ApplyInNamespace(ctx, "", manifest)
}

// ApplyInNamespace pipes manifest YAML to `kubectl -n <ns> apply -f -`.
// When namespace is empty, behaves like Apply.
func (r *Runner) ApplyInNamespace(ctx context.Context, namespace, manifest string) error {
	args := []string{}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	args = append(args, "apply", "-f", "-")
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Env = r.env()
	cmd.Stdin = strings.NewReader(manifest)
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(r.Out, &out)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply -n %q: %w\n%s", namespace, err, out.String())
	}
	return nil
}

// Kubectl runs an arbitrary kubectl subcommand and streams output.
func (r *Runner) Kubectl(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Env = r.env()
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(r.Out, &out)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return nil
}

// KubectlCapture runs kubectl and returns stdout without streaming.
func (r *Runner) KubectlCapture(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Env = r.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("kubectl %s: %w (stderr: %s)",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Wait runs `kubectl wait` with optional namespace + extra args.
func (r *Runner) Wait(ctx context.Context, namespace, condition, selector string, timeout time.Duration, extraArgs ...string) error {
	args := []string{}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	args = append(args, "wait",
		"--for=condition="+condition,
		"--timeout="+timeout.String(),
		selector)
	args = append(args, extraArgs...)
	return r.Kubectl(ctx, args...)
}

// OCIAuth carries credentials for an authenticated OCI helm chart pull.
type OCIAuth struct {
	RegistryHost string
	Username     string
	Password     string
}

// HelmRegistryLogin runs `helm registry login` with the password fed via
// stdin so it never appears on argv.
func (r *Runner) HelmRegistryLogin(ctx context.Context, auth OCIAuth) error {
	cmd := exec.CommandContext(ctx, "helm",
		"registry", "login", auth.RegistryHost,
		"--username", auth.Username,
		"--password-stdin")
	cmd.Env = r.env()
	cmd.Stdin = strings.NewReader(auth.Password + "\n")
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(r.Out, &out)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm registry login %s: %w\n%s", auth.RegistryHost, err, out.String())
	}
	return nil
}

// HelmUpgrade installs or upgrades a release. Pass repoURL non-empty for
// HTTP charts (uses --repo); leave empty for OCI charts (chart name is
// the full oci:// URL). valuesYAML is optional.
func (r *Runner) HelmUpgrade(ctx context.Context, release, chart, repoURL, namespace, chartVersion, valuesYAML string, extraArgs ...string) error {
	var valuesPath string
	if valuesYAML != "" {
		tmp, err := os.CreateTemp("", "kindbnkctl-helm-values-*.yaml")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.WriteString(valuesYAML); err != nil {
			tmp.Close()
			return err
		}
		tmp.Close()
		valuesPath = tmp.Name()
	}

	args := []string{
		"upgrade", "--install", release, chart,
		"--namespace", namespace, "--create-namespace",
		"--wait", "--timeout=10m",
	}
	if repoURL != "" {
		args = append(args, "--repo", repoURL)
	}
	if chartVersion != "" {
		args = append(args, "--version", chartVersion)
	}
	if valuesPath != "" {
		args = append(args, "-f", valuesPath)
	}
	args = append(args, extraArgs...)

	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Env = r.env()
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(r.Out, &out)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm upgrade %s: %w\n%s", release, err, out.String())
	}
	return nil
}

// HelmUpgradeOCI runs `helm registry login` then `helm upgrade` for an
// OCI chart so the login state is fresh for this invocation.
func (r *Runner) HelmUpgradeOCI(ctx context.Context, auth OCIAuth, release, ociChart, namespace, chartVersion, valuesYAML string, extraArgs ...string) error {
	if err := r.HelmRegistryLogin(ctx, auth); err != nil {
		return err
	}
	return r.HelmUpgrade(ctx, release, ociChart, "", namespace, chartVersion, valuesYAML, extraArgs...)
}

// Helm runs an arbitrary helm subcommand.
func (r *Runner) Helm(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Env = r.env()
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(r.Out, &out)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return nil
}
