package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/mwiget/kindbnkctl/internal/version"
)

// jsonUnmarshalImpl is split out so jsonUnmarshal can stub in tests
// without import cycles. No behavior change.
var jsonUnmarshalImpl = json.Unmarshal

// EnvInfo captures the host + cluster environment a report was
// produced against. Every field is best-effort: a missing probe
// (no kubectl, kubeconfig not ready, /proc not mounted) leaves the
// field empty rather than failing the run. The fields are designed
// to render cleanly in markdown even when partially populated.
type EnvInfo struct {
	// Host-side (collected before phases run).
	OS         string `json:"os,omitempty"`         // GOOS
	Arch       string `json:"arch,omitempty"`       // GOARCH
	Kernel     string `json:"kernel,omitempty"`     // uname -r
	Hostname   string `json:"hostname,omitempty"`   // os.Hostname()
	CPUCores   int    `json:"cpu_cores,omitempty"`  // runtime.NumCPU()
	CPUModel   string `json:"cpu_model,omitempty"`  // /proc/cpuinfo "model name"
	MemTotalKB int64  `json:"mem_total_kb,omitempty"`
	DockerVer  string `json:"docker_version,omitempty"`
	KindVer    string `json:"kind_version,omitempty"`
	KubectlVer string `json:"kubectl_client_version,omitempty"`
	GoVer      string `json:"go_version,omitempty"`

	// kindbnkctl + BNK metadata (compiled-in).
	KindBNKCtlVersion  string `json:"kindbnkctl_version,omitempty"`
	BNKVersion         string `json:"bnk_version,omitempty"`
	CNEManifestVersion string `json:"cne_manifest_version,omitempty"`

	// Cluster-side (collected after deploy succeeds — empty otherwise).
	K8sServerVersion string `json:"k8s_server_version,omitempty"`
	KindClusterName  string `json:"kind_cluster_name,omitempty"`

	// Topology — populated alongside K8sServerVersion.
	Nodes        []NodeInfo `json:"nodes,omitempty"`
	PodNamespace []NSCount  `json:"pod_namespaces,omitempty"`
	KeyPods      []PodInfo  `json:"key_pods,omitempty"`
}

// NodeInfo is one row in the cluster-topology table.
type NodeInfo struct {
	Name       string `json:"name"`
	Role       string `json:"role"`              // "control-plane" | "worker"
	Ready      string `json:"ready"`             // "True" | "False" | "Unknown"
	K8sVersion string `json:"k8s_version"`       // kubelet version reported
	OSImage    string `json:"os_image,omitempty"`
	Runtime    string `json:"container_runtime,omitempty"`
	Pods       int    `json:"pods,omitempty"`
}

// NSCount is one row in the namespace pod-count table.
type NSCount struct {
	Namespace string `json:"namespace"`
	Count     int    `json:"count"`
}

// PodInfo is one row in the key-F5-pods table.
type PodInfo struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Node      string `json:"node,omitempty"`
	Ready     string `json:"ready"`  // "6/6"
	Status    string `json:"status"` // "Running"
}

// collectHostInfo populates the host-side fields. Best-effort: a
// failed probe leaves its field empty. Order matters only insofar
// as the function never blocks on a single slow command — each
// shell-out gets a short context-derived timeout via the caller's
// ctx.
func collectHostInfo(ctx context.Context) EnvInfo {
	e := EnvInfo{
		OS:                 runtime.GOOS,
		Arch:               runtime.GOARCH,
		CPUCores:           runtime.NumCPU(),
		GoVer:              runtime.Version(),
		KindBNKCtlVersion:  version.Version,
		BNKVersion:         version.BNKVersion,
		CNEManifestVersion: version.CNEManifestVersion,
	}
	if h, err := os.Hostname(); err == nil {
		e.Hostname = h
	}
	if k := readKernel(); k != "" {
		e.Kernel = k
	}
	if m := readCPUModel(); m != "" {
		e.CPUModel = m
	}
	if mt := readMemTotalKB(); mt > 0 {
		e.MemTotalKB = mt
	}
	if v := firstLine(captureCmd(ctx, "docker", "version", "--format", "{{.Server.Version}}")); v != "" {
		e.DockerVer = v
	}
	if v := firstLine(captureCmd(ctx, "kind", "version")); v != "" {
		// kind prints "kind v0.27.0 go1.23.0 linux/amd64" — keep
		// the kind-specific prefix only for brevity.
		if i := strings.Index(v, " go"); i > 0 {
			v = v[:i]
		}
		e.KindVer = v
	}
	if v := firstLine(captureCmd(ctx, "kubectl", "version", "--client=true", "--output=yaml")); v != "" {
		// `--output=yaml` prints "clientVersion:\n  gitVersion: vX.Y.Z\n  …"
		// The single-line `--output=json | jq` would need jq; parse the
		// yaml's gitVersion ourselves.
		if g := scanForLineValue(captureCmd(ctx, "kubectl", "version", "--client=true", "--output=yaml"),
			"gitVersion:"); g != "" {
			e.KubectlVer = g
		}
	}
	return e
}

// collectClusterInfo fills in the fields that require a live
// API server. Called after deploy-cne; takes a Runner so it uses
// the same kubeconfig the deploy phases used.
func collectClusterInfo(ctx context.Context, kubectl func(args ...string) (string, error), e *EnvInfo) {
	if e == nil {
		return
	}
	// K8s server version via `kubectl version`.
	if y, err := kubectl("version", "--output=yaml"); err == nil {
		// gitVersion appears twice (client + server); after we strip
		// the first block the second occurrence is the server.
		if i := strings.Index(y, "serverVersion:"); i >= 0 {
			if g := scanForLineValue(y[i:], "gitVersion:"); g != "" {
				e.K8sServerVersion = g
			}
		}
	}
	// kind cluster name: read from any node label, all kind nodes
	// share `kind.x-k8s.io/cluster-name: <name>`.
	if v, err := kubectl("get", "node", "-o",
		`jsonpath={.items[0].metadata.labels.kind\.x-k8s\.io/cluster-name}`); err == nil {
		if v = strings.TrimSpace(v); v != "" {
			e.KindClusterName = v
		}
	}

	// Nodes + topology.
	if data, err := kubectl("get", "nodes", "-o", "json"); err == nil {
		if nodes := parseNodes(data); len(nodes) > 0 {
			e.Nodes = nodes
		}
	}
	// All pods, used both for per-namespace counts AND key-pods lookup.
	if data, err := kubectl("get", "pods", "-A", "-o", "json"); err == nil {
		ns, key, byNode := parsePods(data)
		e.PodNamespace = ns
		e.KeyPods = key
		// Backfill pod counts onto nodes.
		for i := range e.Nodes {
			e.Nodes[i].Pods = byNode[e.Nodes[i].Name]
		}
	}
}

// parseNodes pulls node summary rows from `kubectl get nodes -o json`.
// Returns rows sorted by role then name (control-plane first).
func parseNodes(data string) []NodeInfo {
	type nodeList struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				NodeInfo struct {
					KubeletVersion          string `json:"kubeletVersion"`
					OSImage                 string `json:"osImage"`
					ContainerRuntimeVersion string `json:"containerRuntimeVersion"`
				} `json:"nodeInfo"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	var nl nodeList
	if err := jsonUnmarshal(data, &nl); err != nil {
		return nil
	}
	out := make([]NodeInfo, 0, len(nl.Items))
	for _, it := range nl.Items {
		role := "worker"
		if _, ok := it.Metadata.Labels["node-role.kubernetes.io/control-plane"]; ok {
			role = "control-plane"
		}
		ready := "Unknown"
		for _, c := range it.Status.Conditions {
			if c.Type == "Ready" {
				ready = c.Status
			}
		}
		out = append(out, NodeInfo{
			Name:       it.Metadata.Name,
			Role:       role,
			Ready:      ready,
			K8sVersion: it.Status.NodeInfo.KubeletVersion,
			OSImage:    it.Status.NodeInfo.OSImage,
			Runtime:    it.Status.NodeInfo.ContainerRuntimeVersion,
		})
	}
	// control-plane first, then alphabetical within role.
	sortNodes(out)
	return out
}

// parsePods walks `kubectl get pods -A -o json` and produces:
//   - per-namespace pod counts (alpha-sorted)
//   - the F5 control-plane key pods (TMM, FLO, CNE controller, AFM, CWC, …)
//   - a name→count map keyed by spec.nodeName (used to backfill Node rows)
//
// Pods in *-system / scenario namespaces are still counted toward
// totals but not surfaced individually; key-pod selection is
// deliberately narrow to keep the table short.
func parsePods(data string) (ns []NSCount, keyPods []PodInfo, byNode map[string]int) {
	type podList struct {
		Items []struct {
			Metadata struct {
				Namespace string            `json:"namespace"`
				Name      string            `json:"name"`
				Labels    map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
			Status struct {
				Phase             string `json:"phase"`
				ContainerStatuses []struct {
					Ready bool `json:"ready"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	var pl podList
	if err := jsonUnmarshal(data, &pl); err != nil {
		return nil, nil, nil
	}
	nsCount := map[string]int{}
	byNode = map[string]int{}
	for _, it := range pl.Items {
		nsCount[it.Metadata.Namespace]++
		if it.Spec.NodeName != "" {
			byNode[it.Spec.NodeName]++
		}
		if isKeyF5Pod(it.Metadata.Namespace, it.Metadata.Name, it.Metadata.Labels) {
			ready := readyRatio(it.Status.ContainerStatuses)
			keyPods = append(keyPods, PodInfo{
				Namespace: it.Metadata.Namespace,
				Name:      it.Metadata.Name,
				Node:      it.Spec.NodeName,
				Ready:     ready,
				Status:    it.Status.Phase,
			})
		}
	}
	for k, v := range nsCount {
		ns = append(ns, NSCount{Namespace: k, Count: v})
	}
	sortNSCount(ns)
	sortKeyPods(keyPods)
	return ns, keyPods, byNode
}

// isKeyF5Pod selects pods worth surfacing in the report's
// "Key F5 pods" table. The heuristic is conservative: F5
// namespaces or pod names starting with f5- (excluding leases
// and one-shot installer jobs).
func isKeyF5Pod(ns, name string, _ map[string]string) bool {
	if strings.HasPrefix(ns, "f5-") || ns == "default" {
		if strings.HasPrefix(name, "f5-") {
			// f5-flo and f5-* control-plane Deployments, but skip
			// one-shot installer Jobs whose pod name pattern is
			// "f5-flo-bnk-install-XXXX" with random suffix.
			if strings.Contains(name, "install-") {
				return false
			}
			return true
		}
	}
	return false
}

func readyRatio(cs []struct {
	Ready bool `json:"ready"`
}) string {
	ready := 0
	for _, c := range cs {
		if c.Ready {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, len(cs))
}

// jsonUnmarshal is a tiny shim so test-doubles can stub if needed
// later; today it just delegates to the std library.
func jsonUnmarshal(data string, v interface{}) error {
	return jsonUnmarshalImpl([]byte(data), v)
}

func readKernel() string {
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

func readCPUModel() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "model name") {
			if i := strings.Index(line, ":"); i >= 0 {
				return strings.TrimSpace(line[i+1:])
			}
		}
	}
	return ""
}

func readMemTotalKB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					return v
				}
			}
		}
	}
	return 0
}

func captureCmd(ctx context.Context, name string, args ...string) string {
	if _, err := exec.LookPath(name); err != nil {
		return ""
	}
	var stdout, stderr bytes.Buffer
	c := exec.CommandContext(ctx, name, args...)
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return ""
	}
	return stdout.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i]
	}
	return s
}

// scanForLineValue returns the value of the first line in y whose
// stripped left-hand-side matches key (e.g. "gitVersion:"). Used
// for YAML scraping without pulling in a parser.
func scanForLineValue(y, key string) string {
	sc := bufio.NewScanner(strings.NewReader(y))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, key) {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, key))
		v = strings.Trim(v, `"'`)
		return v
	}
	return ""
}

// formatMemMiB returns a "12345 MiB (12.06 GiB)" string for the
// markdown report. Returns "" when the input is zero.
func formatMemMiB(kb int64) string {
	if kb <= 0 {
		return ""
	}
	mib := kb / 1024
	gib := float64(mib) / 1024.0
	return fmt.Sprintf("%d MiB (%.2f GiB)", mib, gib)
}

func sortNodes(n []NodeInfo) {
	sort.SliceStable(n, func(i, j int) bool {
		ri, rj := nodeRoleRank(n[i].Role), nodeRoleRank(n[j].Role)
		if ri != rj {
			return ri < rj
		}
		return n[i].Name < n[j].Name
	})
}

func nodeRoleRank(role string) int {
	if role == "control-plane" {
		return 0
	}
	return 1
}

func sortNSCount(n []NSCount) {
	sort.Slice(n, func(i, j int) bool {
		if n[i].Count != n[j].Count {
			return n[i].Count > n[j].Count
		}
		return n[i].Namespace < n[j].Namespace
	})
}

func sortKeyPods(p []PodInfo) {
	sort.Slice(p, func(i, j int) bool {
		if p[i].Namespace != p[j].Namespace {
			return p[i].Namespace < p[j].Namespace
		}
		return p[i].Name < p[j].Name
	})
}

// renderTopologyDiagram emits an ASCII block diagram of the kind
// cluster: one box per node with its key F5 pods listed underneath.
// Wrapped in a code fence by the caller; never escapes the table
// view. Best-effort: empty fields render gracefully.
func renderTopologyDiagram(e *EnvInfo) string {
	var b strings.Builder
	cluster := e.KindClusterName
	if cluster == "" {
		cluster = "kind cluster"
	} else {
		cluster = "kind cluster: " + cluster
	}
	// Group key pods by node name.
	byNode := map[string][]PodInfo{}
	for _, p := range e.KeyPods {
		byNode[p.Node] = append(byNode[p.Node], p)
	}

	// Compute width: longest "ns/pod  ready  status" line, with a sane min.
	maxLine := len(cluster) + 4
	for _, pods := range byNode {
		for _, p := range pods {
			line := fmt.Sprintf("%s/%s  %s %s",
				p.Namespace, trimHash(p.Name), p.Ready, p.Status)
			if l := len(line); l > maxLine {
				maxLine = l
			}
		}
	}
	for _, n := range e.Nodes {
		label := fmt.Sprintf("%s  (%s · Ready=%s · %s · %d pods)",
			n.Name, n.Role, n.Ready, n.K8sVersion, n.Pods)
		if l := len(label); l > maxLine {
			maxLine = l
		}
	}
	width := maxLine + 4 // 2 chars padding each side
	if width < 64 {
		width = 64
	}

	// Top frame.
	header := " " + cluster + " "
	pad := width - 4 - len(header)
	if pad < 0 {
		pad = 0
	}
	fmt.Fprintf(&b, "┌─%s%s─┐\n", header, strings.Repeat("─", pad))

	for i, n := range e.Nodes {
		label := fmt.Sprintf("%s  (%s · Ready=%s · %s · %d pods)",
			n.Name, n.Role, n.Ready, n.K8sVersion, n.Pods)
		writeBoxLine(&b, label, width)
		pods := byNode[n.Name]
		for j, p := range pods {
			prefix := "├─"
			if j == len(pods)-1 {
				prefix = "└─"
			}
			line := fmt.Sprintf("   %s %s/%s  %s %s",
				prefix, p.Namespace, trimHash(p.Name), p.Ready, p.Status)
			writeBoxLine(&b, line, width)
		}
		if i < len(e.Nodes)-1 {
			writeBoxLine(&b, "", width)
		}
	}

	// Bottom frame.
	fmt.Fprintf(&b, "└%s┘\n", strings.Repeat("─", width-2))
	return b.String()
}

// writeBoxLine pads `s` to width-4 chars and surrounds with "│ " / " │"
// so each line is exactly `width` runes including the borders.
func writeBoxLine(b *strings.Builder, s string, width int) {
	inner := width - 4
	if len(s) > inner {
		s = s[:inner]
	}
	fmt.Fprintf(b, "│ %-*s │\n", inner, s)
}

// trimHash strips Kubernetes auto-generated suffixes from a pod
// name so the diagram shows e.g. `f5-tmm` instead of
// `f5-tmm-86d57455b8-bfzx2`. Recognizes:
//   - Deployment-owned: <stem>-<8-11 hex chars>-<5 lc alnum>
//   - DaemonSet/Job-owned: <stem>-<5 lc alnum>
// Leaves StatefulSet pods (e.g. `f5-dssm-db-0`) untouched so the
// ordinal stays visible.
func trimHash(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) >= 3 {
		last := parts[len(parts)-1]
		prev := parts[len(parts)-2]
		if looksLikePodHash(last) && looksLikeRSHash(prev) {
			return strings.Join(parts[:len(parts)-2], "-")
		}
		if looksLikePodHash(last) && !isStatefulOrdinal(last) {
			return strings.Join(parts[:len(parts)-1], "-")
		}
	}
	return name
}

// looksLikePodHash: exactly 5 chars, lowercase alphanumeric.
// kubelet uses the alphabet [bcdfghjklmnpqrstvwxz2456789] — a strict
// match against THAT set would be more accurate, but length=5 +
// lowercase alnum is cheap and unambiguous enough in practice.
func looksLikePodHash(s string) bool {
	if len(s) != 5 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// looksLikeRSHash: 8-11 chars, all [0-9a-f] — Deployment pod-template
// hash is an FNV-1a32 rendered in hex, always lower-case hex.
func looksLikeRSHash(s string) bool {
	if len(s) < 8 || len(s) > 11 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// isStatefulOrdinal returns true for purely numeric suffixes like
// `0`, `1`, `2` — StatefulSet ordinals we want to keep so the
// diagram shows db-0 / db-1 / db-2 distinctly.
func isStatefulOrdinal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// renderEnvironment produces the "## Environment" markdown section
// for inclusion in a run report. Empty fields render as "—" so the
// reader can tell at a glance which probes didn't run (e.g. cluster
// fields are blank when the run never reached deploy-cne).
func renderEnvironment(e *EnvInfo) string {
	if e == nil {
		return ""
	}
	dash := func(s string) string {
		if s == "" {
			return "—"
		}
		return s
	}
	var b strings.Builder
	b.WriteString("## Environment\n\n")

	b.WriteString("### Versions\n\n")
	b.WriteString("| Component | Version |\n|---|---|\n")
	fmt.Fprintf(&b, "| kindbnkctl | %s |\n", dash(e.KindBNKCtlVersion))
	fmt.Fprintf(&b, "| BNK | %s |\n", dash(e.BNKVersion))
	fmt.Fprintf(&b, "| CNE manifest | %s |\n", dash(e.CNEManifestVersion))
	fmt.Fprintf(&b, "| kind | %s |\n", dash(e.KindVer))
	fmt.Fprintf(&b, "| kubectl (client) | %s |\n", dash(e.KubectlVer))
	fmt.Fprintf(&b, "| Kubernetes (server) | %s |\n", dash(e.K8sServerVersion))
	fmt.Fprintf(&b, "| Docker | %s |\n", dash(e.DockerVer))
	fmt.Fprintf(&b, "| Go (build) | %s |\n", dash(e.GoVer))
	if e.KindClusterName != "" {
		fmt.Fprintf(&b, "| kind cluster | %s |\n", e.KindClusterName)
	}
	b.WriteString("\n")

	b.WriteString("### Host\n\n")
	b.WriteString("| Field | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Hostname | %s |\n", dash(e.Hostname))
	fmt.Fprintf(&b, "| OS / arch | %s/%s |\n", dash(e.OS), dash(e.Arch))
	fmt.Fprintf(&b, "| Kernel | %s |\n", dash(e.Kernel))
	if e.CPUModel != "" {
		fmt.Fprintf(&b, "| CPU model | %s |\n", e.CPUModel)
	}
	if e.CPUCores > 0 {
		fmt.Fprintf(&b, "| CPU cores | %d |\n", e.CPUCores)
	}
	if m := formatMemMiB(e.MemTotalKB); m != "" {
		fmt.Fprintf(&b, "| Memory | %s |\n", m)
	}
	b.WriteString("\n")

	if len(e.Nodes) > 0 {
		b.WriteString("### Topology diagram\n\n")
		b.WriteString("```\n")
		b.WriteString(renderTopologyDiagram(e))
		b.WriteString("```\n\n")

		b.WriteString("### Cluster nodes\n\n")
		b.WriteString("| Node | Role | Ready | Kubelet | Runtime | Pods |\n")
		b.WriteString("|---|---|---|---|---|---:|\n")
		for _, n := range e.Nodes {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %d |\n",
				n.Name, n.Role, n.Ready,
				dash(n.K8sVersion), dash(n.Runtime), n.Pods)
		}
		b.WriteString("\n")
	}

	if len(e.KeyPods) > 0 {
		b.WriteString("### F5 control-plane pods\n\n")
		b.WriteString("| Namespace | Pod | Node | Ready | Status |\n")
		b.WriteString("|---|---|---|---|---|\n")
		for _, p := range e.KeyPods {
			fmt.Fprintf(&b, "| %s | `%s` | %s | %s | %s |\n",
				p.Namespace, p.Name, dash(p.Node), p.Ready, p.Status)
		}
		b.WriteString("\n")
	}

	if len(e.PodNamespace) > 0 {
		b.WriteString("### Pods by namespace\n\n")
		b.WriteString("| Namespace | Pods |\n|---|---:|\n")
		for _, n := range e.PodNamespace {
			fmt.Fprintf(&b, "| %s | %d |\n", n.Namespace, n.Count)
		}
		b.WriteString("\n")
	}
	return b.String()
}
