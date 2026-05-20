// Package poc defines the on-disk schema for a kindbnkctl PoC repo
// (poc.yaml). poc.yaml is the source of truth — tear-down and redeploy
// read only this file. Anything not captured here is not part of the
// PoC.
package poc

import "time"

const (
	APIVersion = "kindbnkctl.f5.com/v1alpha1"
	Kind       = "PoC"
	FileName   = "poc.yaml"
)

type PoC struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       string    `yaml:"kind"`
	Metadata   Metadata  `yaml:"metadata"`
	Versions   Versions  `yaml:"versions"`
	Cluster    Cluster   `yaml:"cluster"`
	Networks   Networks  `yaml:"networks"`
	BNK        BNK       `yaml:"bnk"`
	BNKForge   BNKForge  `yaml:"bnk_forge,omitempty"`
	Status     Status    `yaml:"status"`
}

type Metadata struct {
	Name              string    `yaml:"name"`
	Customer          string    `yaml:"customer,omitempty"`
	Created           time.Time `yaml:"created"`
	KindbnkctlVersion string    `yaml:"kindbnkctl_version,omitempty"`
	BNKVersion        string    `yaml:"bnk_version"`
}

// Versions captures the BNK 2.3.0 component pins. FLOChart is resolved
// from the release-manifest chart at deploy time — empty here means
// "not yet resolved".
type Versions struct {
	K8s          string `yaml:"k8s"`
	KindNodeImage string `yaml:"kind_node_image"`
	FLOChart     string `yaml:"flo_chart,omitempty"`
	CNEManifest  string `yaml:"cne_manifest"`
}

// Cluster is the kind cluster shape. Topology is hard-coded at two
// nodes (one combined control-plane+worker, one worker labelled for
// TMM) — the only knobs an operator turns are the cluster name and
// which container provider kind uses (docker vs podman).
type Cluster struct {
	Name     string `yaml:"name"`
	Provider string `yaml:"provider"` // "docker" or "podman"
}

// Networks are the two docker bridge networks attached to both kind
// node containers. They exist as "scenery" — a routable space where
// operators can spin up test clients, routers, or upstreams alongside
// TMM. TMM itself runs in demo mode and uses virtio inside its pod
// netns, so it does not attach to these networks.
type Networks struct {
	Internal DockerNetwork `yaml:"internal"`
	External DockerNetwork `yaml:"external"`
}

type DockerNetwork struct {
	Name   string `yaml:"name"`
	Subnet string `yaml:"subnet"`
}

type BNK struct {
	FARKeyRef string `yaml:"far_key_ref"`
	JWTRef    string `yaml:"jwt_ref"`
	// DemoMode toggles CNEInstance.advanced.demoMode.enabled. Without
	// it, TMM expects SR-IOV / DPU-backed interfaces; with it, TMM
	// uses virtio inside the pod netns — the only mode that works on
	// kind. Default true; leaving it false is an explicit choice the
	// operator makes.
	DemoMode bool `yaml:"demo_mode"`
	// TMM node label: kindbnkctl labels the kind worker container
	// `app=f5-tmm` so TMM nodeSelector lands TMM there. The label is
	// hard-coded matching f5-bnk-udf convention; only surfaced here
	// so an operator who wants to override it (e.g. test multi-node)
	// can do so without recompiling.
	TMMNodeLabelKey   string `yaml:"tmm_node_label_key,omitempty"`
	TMMNodeLabelValue string `yaml:"tmm_node_label_value,omitempty"`
}

// BNKForge mirrors dpubnkctl's bnk_forge block. Auto-populated by
// `kindbnkctl init` when $KINDBNKCTL_BNK_FORGE_PATH or ~/git/bnk-forge
// exists locally; never blocks deployment when absent.
type BNKForge struct {
	Enabled       bool   `yaml:"enabled"`
	RepoPath      string `yaml:"repo_path,omitempty"`
	URL           string `yaml:"url,omitempty"`
	AdminUsername string `yaml:"admin_username,omitempty"`
	AdminPassword string `yaml:"admin_password,omitempty"`
	ProjectColor  string `yaml:"project_color,omitempty"`
	ProjectIcon   string `yaml:"project_icon,omitempty"`
}

type Status struct {
	Cluster     string    `yaml:"cluster"`
	Deploy      string    `yaml:"deploy"`
	LastPhaseAt time.Time `yaml:"last_phase_at,omitempty"`
}

// Phase tags used by status updates + e2e state.
const (
	PhaseCluster = "cluster"
	PhaseDeploy  = "deploy"
)

// TMMLabel returns the (key, value) the worker node is labelled with
// for TMM nodeSelector. Defaults match f5-bnk-udf convention.
func (b BNK) TMMLabel() (string, string) {
	k := b.TMMNodeLabelKey
	v := b.TMMNodeLabelValue
	if k == "" {
		k = "app"
	}
	if v == "" {
		v = "f5-tmm"
	}
	return k, v
}
