package version

// Build-time stamped values (see Makefile LDFLAGS).
var (
	Version    = "dev"
	Commit     = "none"
	BuildDate  = "unknown"
	BNKVersion = "2.3.0"
)

// Pinned defaults for BNK 2.3.0 running in demo-mode on kind. The FLO,
// CIS, and cert-gen chart versions are NOT pinned here — they're
// resolved at deploy time from the f5-bigip-k8s-manifest release-manifest
// chart pulled from repo.f5.com (see internal/deploy/manifest.go),
// keyed off CNEManifestVersion below.
const (
	// K8sVersion is what we tell operators in docs/status.
	// K8sNodeImage is what kind uses to spin up each node container.
	// Pinned to v1.30.8 — the 1.30.x image kind v0.26 ships with verified
	// compatibility. dpubnkctl runs 1.30.14 on bare metal (kubespray
	// is unbound by kind's published image set); the BNK side of the
	// matrix is otherwise identical.
	K8sVersion   = "1.30"
	K8sNodeImage = "kindest/node:v1.30.8"

	// K8sToolsImage bundles kubectl + helm + openssl + apk so the CWC
	// cert-gen step (which shells gen_cert.sh inside this image) has
	// everything it needs. Docker is already required for kind, so the
	// extra image pull at deploy time is cheap. Pinned one minor ahead
	// of K8sVersion so `kubectl wait --for=create` works against the
	// cluster (added in kubectl 1.31; supported back-skew is ±1 minor).
	K8sToolsImage = "alpine/k8s:1.31.5"

	// Cert-manager — required dependency for FLO + CWC. Same pin as
	// dpubnkctl (jetstack repo, not part of F5's release manifest).
	CertManagerChart   = "cert-manager"
	CertManagerRepo    = "https://charts.jetstack.io"
	CertManagerVersion = "v1.16.2"

	// Release manifest — the F5 bill-of-materials chart that pins the
	// FLO + CIS + cert-gen + image versions for this BNK release. Pull
	// at deploy time; do NOT hardcode FLO chart version here.
	ReleaseManifestRepo  = "oci://repo.f5.com/release"
	ReleaseManifestChart = "f5-bigip-k8s-manifest"

	// CNEManifestVersion is the version coordinate inside the release
	// manifest. CNEInstance.spec.manifestVersion references it directly;
	// PullReleaseManifest uses it as helm pull --version arg.
	CNEManifestVersion = "2.3.0-3.2598.3-0.0.170"

	// FARRegistryHost is the OCI registry hostname for all F5-published
	// charts and images.
	FARRegistryHost = "repo.f5.com"

	// FLOChartOCIRef is the full OCI reference for the FLO chart. The
	// version is resolved at deploy time from the release-manifest chart.
	FLOChartOCIRef = "oci://repo.f5.com/charts/f5-lifecycle-operator"

	// CalicoManifestURL is the upstream Calico manifest applied right
	// after `kind create cluster` (which is configured with
	// disableDefaultCNI: true). Pinned to the same minor track BNK 2.3
	// declares as supported on the cluster side.
	CalicoManifestURL = "https://raw.githubusercontent.com/projectcalico/calico/v3.28.2/manifests/calico.yaml"

	// DockerNetworkInternal / External are the default names used when
	// poc.yaml leaves networks.{internal,external}.name unset. The kind
	// cluster's two node containers both join these networks so
	// operator-supplied test client / router containers have a routable
	// path alongside TMM (which itself runs in demo mode and uses
	// virtio interfaces inside the pod netns, not these networks).
	DockerNetworkInternal = "bnk-internal"
	DockerNetworkExternal = "bnk-external"

	// Default subnets for the internal/external docker networks. RFC
	// 3849 / RFC 2544 style placeholders are intentional — these are
	// just "scenery" for client containers and are not reachable from
	// outside the laptop.
	DefaultInternalSubnet = "198.18.100.0/24"
	DefaultExternalSubnet = "203.0.113.0/24"
)

// ResourceSpec describes the operator-workstation floor a kindbnkctl
// deployment expects.
type ResourceSpec struct {
	Cores    int
	MemoryGB int
}

// MinBaseline + MinWithBNKForge are first-measurement floors captured
// from a verified end-to-end smoke deployment on linux/amd64:
//
//   Cluster steady-state (after CNEInstance.Available=True, all 16
//   components green, TMM 6/6 Running, License Active):
//     - worker:         ~3.0 GB RSS, ~120m CPU sustained, peaks to ~1.2c
//     - control-plane:  ~1.5 GB RSS, ~330m CPU sustained
//     - total:          ~4.5 GB pod-attributed + ~1 GB kernel overhead
//     - TMM alone:      1.17 GB RSS, 100m CPU
//
// Floor below adds ~1.5 GB / ~1 core of headroom for `kubectl top`,
// `docker pull` bursts, and one operator-supplied test client. macOS
// Docker Desktop adds VM overhead on top — bump 2 GB for that.
//
// MinWithBNKForge adds a modest extra for the in-cluster bnk-forge
// agent plus the host-side bnk-forge stack. The host-side numbers are
// still TBD; treat the extra as a guess until measured.
var (
	MinBaseline     = ResourceSpec{Cores: 4, MemoryGB: 6}
	MinWithBNKForge = ResourceSpec{Cores: 5, MemoryGB: 8}
)

// Measured indicates whether the floor numbers above are real measured
// values or still placeholders.
func Measured() bool {
	return MinBaseline.Cores > 0 && MinBaseline.MemoryGB > 0
}
