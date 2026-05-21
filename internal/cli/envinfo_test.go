package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mwiget/kindbnkctl/internal/scenarios"
)

func TestCollectHostInfo_PopulatesCompiledInFields(t *testing.T) {
	// The collector probes the host best-effort, but the four
	// compiled-in fields must always populate regardless of the
	// build environment (no docker, no kind, no /proc, etc.).
	e := collectHostInfo(context.Background())

	if e.OS == "" {
		t.Error("OS empty (runtime.GOOS should always be set)")
	}
	if e.Arch == "" {
		t.Error("Arch empty (runtime.GOARCH should always be set)")
	}
	if e.CPUCores < 1 {
		t.Errorf("CPUCores=%d, want >=1", e.CPUCores)
	}
	if !strings.HasPrefix(e.GoVer, "go") {
		t.Errorf("GoVer=%q, want go-version string", e.GoVer)
	}
	if e.KindBNKCtlVersion == "" {
		t.Error("KindBNKCtlVersion empty")
	}
	if e.BNKVersion == "" {
		t.Error("BNKVersion empty")
	}
	if e.CNEManifestVersion == "" {
		t.Error("CNEManifestVersion empty")
	}
}

func TestRenderEnvironment_AllFieldsPresent(t *testing.T) {
	e := &EnvInfo{
		OS:                 "linux",
		Arch:               "amd64",
		Kernel:             "6.8.0-117-generic",
		Hostname:           "test-host",
		CPUCores:           24,
		CPUModel:           "Intel(R) Test CPU",
		MemTotalKB:         32 * 1024 * 1024,
		DockerVer:          "27.5.1",
		KindVer:            "kind v0.27.0",
		KubectlVer:         "v1.31.4",
		GoVer:              "go1.23.4",
		KindBNKCtlVersion:  "dev",
		BNKVersion:         "2.3.0",
		CNEManifestVersion: "2.3.0-3.2598.3-0.0.170",
		K8sServerVersion:   "v1.30.8",
		KindClusterName:    "smoke",
	}
	md := renderEnvironment(e)
	for _, want := range []string{
		"## Environment",
		"### Versions",
		"### Host",
		"kindbnkctl", "BNK", "CNE manifest",
		"kind", "kubectl (client)", "Kubernetes (server)", "Docker", "Go (build)", "kind cluster",
		"linux/amd64",
		"6.8.0-117-generic",
		"32768 MiB", "32.00 GiB",
		"test-host",
		"24",
		"v0.27.0",
		"v1.31.4",
		"v1.30.8",
		"27.5.1",
		"smoke",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("renderEnvironment missing %q in output:\n%s", want, md)
		}
	}
}

func TestRenderEnvironment_MissingFieldsRenderAsDash(t *testing.T) {
	// Only compiled-in fields populated — host probes failed
	// (no /proc, no docker, no kind). Output should still render
	// without panic and show "—" for missing strings.
	e := &EnvInfo{
		OS:                 "darwin",
		Arch:               "arm64",
		CPUCores:           8,
		GoVer:              "go1.23.4",
		KindBNKCtlVersion:  "dev",
		BNKVersion:         "2.3.0",
		CNEManifestVersion: "2.3.0-3.2598.3-0.0.170",
		// Kernel, Hostname, CPUModel, MemTotalKB, Docker, kind,
		// kubectl, server version, cluster name all empty.
	}
	md := renderEnvironment(e)
	if !strings.Contains(md, "| Docker | — |") {
		t.Errorf("expected dash for missing Docker, got:\n%s", md)
	}
	if !strings.Contains(md, "| Hostname | — |") {
		t.Errorf("expected dash for missing Hostname")
	}
	// Conditional rows must NOT appear when their value is zero/empty.
	if strings.Contains(md, "| CPU model |") {
		t.Errorf("CPU model row should be omitted when empty")
	}
	if strings.Contains(md, "| Memory |") {
		t.Errorf("Memory row should be omitted when zero")
	}
	if strings.Contains(md, "| kind cluster |") {
		t.Errorf("kind cluster row should be omitted when empty")
	}
}

func TestFormatMemMiB(t *testing.T) {
	cases := []struct {
		kb   int64
		want string
	}{
		{0, ""},
		{1024, "1 MiB (0.00 GiB)"},
		{32 * 1024 * 1024, "32768 MiB (32.00 GiB)"},
	}
	for _, c := range cases {
		if got := formatMemMiB(c.kb); got != c.want {
			t.Errorf("formatMemMiB(%d)=%q, want %q", c.kb, got, c.want)
		}
	}
}

const sampleNodesJSON = `{
  "items": [
    {
      "metadata": {
        "name": "smoke-worker",
        "labels": {}
      },
      "status": {
        "nodeInfo": {
          "kubeletVersion": "v1.30.8",
          "osImage": "Debian GNU/Linux 12 (bookworm)",
          "containerRuntimeVersion": "containerd://1.7.18"
        },
        "conditions": [
          {"type": "Ready", "status": "True"}
        ]
      }
    },
    {
      "metadata": {
        "name": "smoke-control-plane",
        "labels": {"node-role.kubernetes.io/control-plane": ""}
      },
      "status": {
        "nodeInfo": {
          "kubeletVersion": "v1.30.8",
          "osImage": "Debian GNU/Linux 12 (bookworm)",
          "containerRuntimeVersion": "containerd://1.7.18"
        },
        "conditions": [
          {"type": "Ready", "status": "True"}
        ]
      }
    }
  ]
}`

func TestParseNodes_OrdersControlPlaneFirst(t *testing.T) {
	n := parseNodes(sampleNodesJSON)
	if len(n) != 2 {
		t.Fatalf("got %d nodes, want 2", len(n))
	}
	if n[0].Name != "smoke-control-plane" {
		t.Errorf("first node should be control-plane, got %q", n[0].Name)
	}
	if n[0].Role != "control-plane" {
		t.Errorf("role mismatch: %q", n[0].Role)
	}
	if n[1].Role != "worker" {
		t.Errorf("second node should be worker, got %q", n[1].Role)
	}
	if n[0].Ready != "True" || n[0].K8sVersion != "v1.30.8" {
		t.Errorf("unexpected node status: %+v", n[0])
	}
}

const samplePodsJSON = `{
  "items": [
    {
      "metadata": {"namespace": "default", "name": "f5-tmm-abc"},
      "spec": {"nodeName": "smoke-worker"},
      "status": {"phase": "Running", "containerStatuses": [
        {"ready": true},{"ready": true},{"ready": true},
        {"ready": true},{"ready": true},{"ready": true}
      ]}
    },
    {
      "metadata": {"namespace": "default", "name": "f5-cne-controller-xyz"},
      "spec": {"nodeName": "smoke-worker"},
      "status": {"phase": "Running", "containerStatuses": [
        {"ready": true},{"ready": true},{"ready": true},{"ready": true}
      ]}
    },
    {
      "metadata": {"namespace": "default", "name": "f5-flo-bnk-install-12345"},
      "spec": {"nodeName": "smoke-worker"},
      "status": {"phase": "Succeeded", "containerStatuses": [{"ready": false}]}
    },
    {
      "metadata": {"namespace": "kube-system", "name": "kube-proxy-1"},
      "spec": {"nodeName": "smoke-worker"},
      "status": {"phase": "Running", "containerStatuses": [{"ready": true}]}
    },
    {
      "metadata": {"namespace": "kube-system", "name": "kube-proxy-2"},
      "spec": {"nodeName": "smoke-control-plane"},
      "status": {"phase": "Running", "containerStatuses": [{"ready": true}]}
    }
  ]
}`

func TestParsePods_KeyPodsExcludeInstallerAndNonF5(t *testing.T) {
	ns, key, byNode := parsePods(samplePodsJSON)
	// 4 worker pods + 1 control-plane pod
	if byNode["smoke-worker"] != 4 || byNode["smoke-control-plane"] != 1 {
		t.Errorf("byNode wrong: %+v", byNode)
	}
	// Two key pods: f5-tmm + f5-cne-controller. NOT the installer Job
	// (name contains "install-") and NOT kube-proxy.
	if len(key) != 2 {
		t.Fatalf("got %d key pods, want 2 (%+v)", len(key), key)
	}
	names := map[string]bool{}
	for _, p := range key {
		names[p.Name] = true
	}
	if !names["f5-tmm-abc"] || !names["f5-cne-controller-xyz"] {
		t.Errorf("key pod set wrong: %+v", names)
	}
	// Namespace counts sorted by count desc.
	if ns[0].Count < ns[1].Count {
		t.Errorf("ns count not sorted desc: %+v", ns)
	}
	// Readiness format check.
	var tmm PodInfo
	for _, p := range key {
		if p.Name == "f5-tmm-abc" {
			tmm = p
		}
	}
	if tmm.Ready != "6/6" {
		t.Errorf("TMM ready %q, want 6/6", tmm.Ready)
	}
}

func TestRenderEnvironment_WithTopology(t *testing.T) {
	e := &EnvInfo{
		OS: "linux", Arch: "amd64", CPUCores: 8,
		KindBNKCtlVersion: "dev", BNKVersion: "2.3.0",
		CNEManifestVersion: "2.3.0-3.2598.3-0.0.170",
		Nodes: []NodeInfo{
			{Name: "smoke-control-plane", Role: "control-plane", Ready: "True",
				K8sVersion: "v1.30.8", Runtime: "containerd://1.7.18", Pods: 7},
			{Name: "smoke-worker", Role: "worker", Ready: "True",
				K8sVersion: "v1.30.8", Runtime: "containerd://1.7.18", Pods: 28},
		},
		KeyPods: []PodInfo{
			{Namespace: "default", Name: "f5-tmm-abc", Node: "smoke-worker", Ready: "6/6", Status: "Running"},
		},
		PodNamespace: []NSCount{
			{Namespace: "default", Count: 12},
			{Namespace: "kube-system", Count: 14},
		},
	}
	md := renderEnvironment(e)
	for _, want := range []string{
		"### Cluster nodes",
		"### F5 control-plane pods",
		"### Pods by namespace",
		"smoke-control-plane",
		"smoke-worker",
		"control-plane",
		"6/6",
		"Running",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("renderEnvironment missing %q in:\n%s", want, md)
		}
	}
}

func TestTrimHash(t *testing.T) {
	cases := []struct{ in, want string }{
		// Deployment-owned: drop <rs-hash>-<pod-hash>
		{"f5-tmm-86d57455b8-bfzx2", "f5-tmm"},
		{"f5-afm-7699b8fb47-dcmjd", "f5-afm"},
		{"f5-cne-controller-7dd664dcf9-7jrbn", "f5-cne-controller"},
		{"f5-spk-cwc-589456c5fb-kspgx", "f5-spk-cwc"},
		// DaemonSet-style 5-char suffix only
		{"f5-spk-csrc-6wmg5", "f5-spk-csrc"},
		// StatefulSet ordinal — preserve.
		{"f5-dssm-db-0", "f5-dssm-db-0"},
		{"f5-observer-0", "f5-observer-0"},
		{"f5-dssm-sentinel-2", "f5-dssm-sentinel-2"},
		// Already-clean
		{"kube-proxy", "kube-proxy"},
		// Single-segment
		{"single", "single"},
	}
	for _, c := range cases {
		if got := trimHash(c.in); got != c.want {
			t.Errorf("trimHash(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderTopologyDiagram(t *testing.T) {
	e := &EnvInfo{
		KindClusterName: "smoke",
		Nodes: []NodeInfo{
			{Name: "smoke-control-plane", Role: "control-plane",
				Ready: "True", K8sVersion: "v1.30.8", Pods: 6},
			{Name: "smoke-worker", Role: "worker",
				Ready: "True", K8sVersion: "v1.30.8", Pods: 28},
		},
		KeyPods: []PodInfo{
			{Namespace: "default", Name: "f5-tmm-86d57455b8-bfzx2",
				Node: "smoke-worker", Ready: "6/6", Status: "Running"},
			{Namespace: "f5-cne-core", Name: "f5-observer-0",
				Node: "smoke-worker", Ready: "1/1", Status: "Running"},
		},
	}
	d := renderTopologyDiagram(e)
	for _, want := range []string{
		"kind cluster: smoke",
		"smoke-control-plane",
		"smoke-worker",
		"default/f5-tmm  6/6 Running",     // hash stripped
		"f5-cne-core/f5-observer-0",       // StatefulSet ordinal kept
		"┌─", "└─", "│",                   // box drawing characters
	} {
		if !strings.Contains(d, want) {
			t.Errorf("diagram missing %q:\n%s", want, d)
		}
	}
}

func TestRenderRunMarkdown_CombinedTotal(t *testing.T) {
	now := time.Time{}
	r := runReport{
		PoCName:    "smoke",
		StartedAt:  now,
		FinishedAt: now,
		Phases: []phaseReport{
			{Index: 1, Phase: "validate", Status: "ok"},
			{Index: 2, Phase: "cluster-up", Status: "ok"},
			{Index: 3, Phase: "deploy-prereqs", Status: "ok"},
			{Index: 4, Phase: "deploy-flo", Status: "ok"},
			{Index: 5, Phase: "deploy-cne", Status: "ok"},
		},
		Scenarios: []scenarios.SummaryEntry{
			{Name: "bgp-peer-frr", Rating: "green", Status: "ok"},
			{Name: "ai-semantic-cache", Rating: "green", Status: "ok"},
			{Name: "ai-token-counting", Rating: "green", Status: "failed"},
		},
	}
	md := renderRunMarkdown(r)
	want := "**Result:** 7 ok, 1 failed, 0 skipped (deploy 5/5 ok · scenarios 2/3 ok)"
	if !strings.Contains(md, want) {
		t.Errorf("missing combined total %q in:\n%s", want, md)
	}
}

func TestRenderRunMarkdown_PhaseOnlyHeader(t *testing.T) {
	r := runReport{
		PoCName: "smoke",
		Phases: []phaseReport{
			{Index: 1, Phase: "validate", Status: "ok"},
			{Index: 2, Phase: "cluster-up", Status: "failed"},
		},
	}
	md := renderRunMarkdown(r)
	want := "**Result:** 1 ok, 1 failed, 0 skipped\n"
	if !strings.Contains(md, want) {
		t.Errorf("missing phase-only total %q in:\n%s", want, md)
	}
	// And it must NOT have the parenthesized breakdown.
	if strings.Contains(md, "scenarios") {
		t.Errorf("phase-only run should not mention scenarios in header")
	}
}

func TestSafeSlug(t *testing.T) {
	cases := map[string]string{
		"":               "poc",
		"smoke":          "smoke",
		"smoke-prod":     "smoke-prod",
		"with space":     "with-space",
		"weird/poc!name": "weird-poc-name",
	}
	for in, want := range cases {
		if got := safeSlug(in); got != want {
			t.Errorf("safeSlug(%q)=%q, want %q", in, got, want)
		}
	}
}
