package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mwiget/kindbnkctl/internal/version"
)

// ReleaseManifest is the parsed contents of the f5-bigip-k8s-manifest
// release-manifest chart. F5 ships one manifest per BNK release; it
// pins the exact chart and image versions that constitute that release.
type ReleaseManifest struct {
	HelmRepo    string
	DockerRepo  string
	Version     string
	HelmCharts  map[string]string
	DockerImgs  map[string]string
	rawManifest []byte
}

type rawReleaseManifest struct {
	F5HelmRepo   string `yaml:"f5_helm_repo"`
	F5DockerRepo string `yaml:"f5_docker_repo"`
	Releases     []struct {
		Version    string `yaml:"version"`
		HelmCharts []struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
		} `yaml:"helm_charts"`
		DockerImages []struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
		} `yaml:"docker_images"`
	} `yaml:"releases"`
}

func ParseReleaseManifest(body []byte) (*ReleaseManifest, error) {
	var raw rawReleaseManifest
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse release-manifest yaml: %w", err)
	}
	if len(raw.Releases) == 0 {
		return nil, fmt.Errorf("release-manifest has no `releases[]` entries")
	}
	rel := raw.Releases[0]
	if rel.Version == "" {
		return nil, fmt.Errorf("release-manifest releases[0].version is empty")
	}
	m := &ReleaseManifest{
		HelmRepo:    raw.F5HelmRepo,
		DockerRepo:  raw.F5DockerRepo,
		Version:     rel.Version,
		HelmCharts:  make(map[string]string, len(rel.HelmCharts)),
		DockerImgs:  make(map[string]string, len(rel.DockerImages)),
		rawManifest: body,
	}
	for _, c := range rel.HelmCharts {
		if c.Name != "" {
			m.HelmCharts[c.Name] = c.Version
		}
	}
	for _, i := range rel.DockerImages {
		if i.Name != "" {
			m.DockerImgs[i.Name] = i.Version
		}
	}
	return m, nil
}

func (m *ReleaseManifest) Chart(name string) string  { return m.HelmCharts[name] }
func (m *ReleaseManifest) Image(name string) string  { return m.DockerImgs[name] }
func (m *ReleaseManifest) RawYAML() []byte           { return m.rawManifest }

// PullReleaseManifest authenticates to repo.f5.com with FAR credentials,
// pulls the f5-bigip-k8s-manifest chart at the requested version, and
// returns the parsed manifest. Uses the operator's local helm binary
// (kindbnkctl doctor verifies it's present). The pulled tgz + extracted
// dir + manifest.yaml are kept under cacheDir for audit.
//
// helmHome, if non-empty, isolates helm's repository config + cache to
// a per-PoC directory so the run doesn't trip over the operator's
// global ~/.config/helm state (a common issue when other helm-using
// projects have left stale or broken repo indexes around).
func PullReleaseManifest(ctx context.Context, auth OCIAuth, manifestVersion, cacheDir, helmHome string) (*ReleaseManifest, error) {
	if manifestVersion == "" {
		manifestVersion = version.CNEManifestVersion
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cache %s: %w", cacheDir, err)
	}
	absCache, err := filepath.Abs(cacheDir)
	if err != nil {
		return nil, err
	}

	helmEnv := append(os.Environ())
	if helmHome != "" {
		_ = os.MkdirAll(helmHome, 0o755)
		helmEnv = append(helmEnv,
			"HELM_REPOSITORY_CONFIG="+helmHome+"/repositories.yaml",
			"HELM_REPOSITORY_CACHE="+helmHome+"/cache",
			"HELM_REGISTRY_CONFIG="+helmHome+"/registry.json",
		)
	}

	// 1. helm registry login (password via stdin so it never hits argv)
	login := exec.CommandContext(ctx, "helm", "registry", "login",
		version.FARRegistryHost, "--username", auth.Username, "--password-stdin")
	login.Stdin = strings.NewReader(auth.Password + "\n")
	login.Env = helmEnv
	var loginErr bytes.Buffer
	login.Stderr = &loginErr
	login.Stdout = io.Discard
	if err := login.Run(); err != nil {
		return nil, fmt.Errorf("helm registry login %s: %w\n%s",
			version.FARRegistryHost, err, strings.TrimSpace(loginErr.String()))
	}

	// 2. Clean stale artifacts so a stale dir doesn't survive a version bump.
	tgzPath := filepath.Join(absCache, fmt.Sprintf("f5-bigip-k8s-manifest-%s.tgz", manifestVersion))
	extractedDir := filepath.Join(absCache, fmt.Sprintf("f5-bigip-k8s-manifest-%s", manifestVersion))
	_ = os.Remove(tgzPath)
	_ = os.RemoveAll(extractedDir)

	// 3. helm pull
	pull := exec.CommandContext(ctx, "helm", "pull",
		version.ReleaseManifestRepo+"/"+version.ReleaseManifestChart,
		"--version", manifestVersion,
		"-d", absCache)
	pull.Env = helmEnv
	var pullErr bytes.Buffer
	pull.Stderr = &pullErr
	pull.Stdout = io.Discard
	if err := pull.Run(); err != nil {
		return nil, fmt.Errorf("helm pull release-manifest %s: %w\n%s",
			manifestVersion, err, strings.TrimSpace(pullErr.String()))
	}

	// 4. Extract via `tar` — local helm doesn't auto-extract.
	tar := exec.CommandContext(ctx, "tar", "-xzf", tgzPath, "-C", absCache)
	if err := tar.Run(); err != nil {
		return nil, fmt.Errorf("tar -xzf %s: %w", tgzPath, err)
	}

	// 5. Read the manifest YAML inside the extracted dir.
	manifestPath := filepath.Join(extractedDir,
		fmt.Sprintf("bigip-k8s-manifest-%s.yaml", manifestVersion))
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", manifestPath, err)
	}
	m, err := ParseReleaseManifest(body)
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(filepath.Join(absCache, "manifest.yaml"), body, 0o644); err != nil {
		return nil, fmt.Errorf("write manifest cache: %w", err)
	}
	return m, nil
}

// ExtractFARRegistryAuth reads a FAR tgz and returns OCIAuth suitable
// for `helm registry login -u _json_key --password-stdin`.
func ExtractFARRegistryAuth(farTgzPath string) (OCIAuth, error) {
	docker, err := ExtractFARDockerConfig(farTgzPath)
	if err != nil {
		return OCIAuth{}, err
	}
	saJSON, err := UnwrapGARAuth(docker)
	if err != nil {
		return OCIAuth{}, err
	}
	return OCIAuth{
		RegistryHost: version.FARRegistryHost,
		Username:     "_json_key",
		Password:     saJSON,
	}, nil
}

func (m *ReleaseManifest) SinkSummary(w io.Writer) {
	fmt.Fprintf(w, "Release-manifest:  %s\n", m.Version)
	fmt.Fprintf(w, "  helm repo:       %s\n", m.HelmRepo)
	fmt.Fprintf(w, "  docker repo:     %s\n", m.DockerRepo)
	fmt.Fprintf(w, "  helm charts:     %d\n", len(m.HelmCharts))
	fmt.Fprintf(w, "  docker images:   %d\n", len(m.DockerImgs))
	for _, name := range []string{
		"charts/f5-lifecycle-operator",
		"utils/f5-cert-gen",
		"charts/cwc",
		"charts/f5-cert-manager",
	} {
		if v := m.Chart(name); v != "" {
			fmt.Fprintf(w, "    %-35s %s\n", name, v)
		}
	}
}
