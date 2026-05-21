package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/mwiget/kindbnkctl/internal/version"
)

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
	return b.String()
}
