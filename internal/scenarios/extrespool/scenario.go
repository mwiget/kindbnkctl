// Package extrespool implements scenario "external-resource-pool" —
// the BNK how-to #10 "Configure Load Balance Traffic to External
// Resources". The novelty vs http-routing-e2e is the BNK Pool CR:
// HTTPRoute.backendRefs points at a Pool (group: k8s.f5net.com,
// kind: Pool) rather than at a Service, and the Pool's spec.members
// lists the backend endpoints by IP+port.
//
// In a production BNK deployment the Pool's members live outside the
// cluster (VMs, bare-metal, other clusters). On kind we simulate
// "outside" with a backend pod attached to the bnk-bgp Multus NAD
// — same bridge TMM has its net1 on, so TMM reaches the backend
// at L2 without any kernel-route plumbing. The Pool member entry
// points at the backend pod's NAD IP (discovered at runtime).
//
// Pipeline:
//
//   1. scn-extres namespace + F5BnkGateway IP pool for .101
//   2. ext-backend Deployment (nginx) with the bnk-bgp NAD on net1
//   3. Gateway with static addresses=[203.0.113.101] +
//      HTTPRoute backendRef → Pool
//   4. Discover ext-backend's NAD IP; render the Pool CR with that
//      IP as the single member
//
// Verification:
//   - Gateway Programmed, HTTPRoute Accepted, ext-backend Available
//   - Pool exists and surfaces a member matching the backend's IP
//   - FRR's BGP table has 203.0.113.101/32 advertised by TMM
//   - 5 consecutive curls from inside the FRR pod to
//     http://203.0.113.101/ with Host: extres.kindbnkctl.local
//     return the ext-backend's marker body
package extrespool

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"text/template"
	"time"

	"github.com/mwiget/kindbnkctl/internal/scenarios"
)

//go:embed manifests/*.yaml manifests/*.yaml.tmpl
var manifestFS embed.FS

const (
	scnName  = "external-resource-pool"
	scnTitle = "Load Balance Traffic to External Resources (how-to #10) — Pool CR + HTTPRoute backendRef"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Green }
func (s *scenario) Dependencies() []string   { return []string{"bgp-peer-frr"} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Demonstrates BNK's Pool CR as an HTTPRoute backend, the BNK-native
way to load-balance to endpoints that aren't k8s Services.

Production BNK uses Pool to point at out-of-cluster resources
(VMs, other clusters, bare-metal hosts). On kind we stand in
with an nginx pod attached to the bnk-bgp Multus NAD — TMM
already has a net1 interface on that bridge (from the
bgp-peer-frr setup), so the backend pod's NAD IP is reachable
from TMM at L2.

Pipeline:
  - F5BnkGateway IP pool for the .101 range
  - ext-backend Deployment (nginx on bnk-bgp NAD)
  - Gateway with spec.addresses=[203.0.113.101]
  - HTTPRoute hostname=extres.kindbnkctl.local, backendRefs to
    {group:k8s.f5net.com, kind:Pool, name:ext-backend-pool}
  - Pool CR with one member at the discovered backend NAD IP

Verification curls from inside the FRR pod (same client used by
http-routing-e2e): the kernel route to 203.0.113.101/32 lands
via BGP+net1, TMM proxies through to the Pool member IP. No
new client plumbing.

Cleanup deletes the scn-extres namespace; cluster-wide
GatewayClass + bnk-bgp NAD stay (reused by other scenarios).
`)
}

func (s *scenario) Manifests(ctx *scenarios.Context) ([]string, error) {
	var paths []string
	err := fs.WalkDir(manifestFS, "manifests", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, e := manifestFS.ReadFile(p)
		if e != nil {
			return e
		}
		base := p[len("manifests/"):]
		out, e := scenarios.WriteManifest(ctx.PoCDir, scnName, base, string(body))
		if e != nil {
			return e
		}
		paths = append(paths, out)
		return nil
	})
	return paths, err
}

func (s *scenario) Apply(ctx *scenarios.Context) error {
	r := ctx.Runner

	// Dependency check.
	if _, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "get", "pod",
		"-l", "app=scn-frr",
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}"); err != nil {
		return fmt.Errorf("dependency missing: run `kindbnkctl scenario run bgp-peer-frr` first (no Running scn-frr pod)")
	}

	// 1. Namespace + F5BnkGateway IP pool + backend.
	for _, f := range []string{
		"01-namespace.yaml",
		"02-bnkgateway.yaml",
		"03-backend.yaml",
	} {
		body, err := manifestFS.ReadFile("manifests/" + f)
		if err != nil {
			return err
		}
		if err := r.Apply(ctx.Ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}

	// 2. Wait for the backend pod, discover its NAD IP.
	if err := r.Wait(ctx.Ctx, "scn-extres", "Available",
		"deployment/ext-backend", 2*time.Minute); err != nil {
		return fmt.Errorf("ext-backend Deployment not Available: %w", err)
	}
	backendIP, err := discoverNADIP(ctx, "scn-extres", "app=ext-backend", "bnk-bgp")
	if err != nil {
		return fmt.Errorf("discover ext-backend NAD IP: %w", err)
	}
	fmt.Fprintf(ctx.Out, "      | ext-backend NAD IP: %s\n", backendIP)

	// 3. Render Pool template, persist + apply.
	poolBody, err := renderTemplate(manifestFS, "manifests/05-pool.yaml.tmpl",
		struct{ BackendIP string }{BackendIP: backendIP})
	if err != nil {
		return err
	}
	if _, err := scenarios.WriteManifest(ctx.PoCDir, scnName,
		"05-pool.rendered.yaml", poolBody); err != nil {
		return err
	}
	if err := r.Apply(ctx.Ctx, poolBody); err != nil {
		return fmt.Errorf("apply Pool CR: %w", err)
	}

	// 4. Gateway + HTTPRoute.
	for _, f := range []string{
		"04-gateway.yaml",
		"06-httproute.yaml",
	} {
		body, err := manifestFS.ReadFile("manifests/" + f)
		if err != nil {
			return err
		}
		if err := r.Apply(ctx.Ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}

	return nil
}

func (s *scenario) Verify(ctx *scenarios.Context) scenarios.Result {
	r := ctx.Runner
	res := scenarios.Result{}

	// Control-plane assertions.
	{
		err := r.Wait(ctx.Ctx, "scn-extres", "Available",
			"deployment/ext-backend", 2*time.Minute)
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "ext-backend Deployment Available",
			OK:          err == nil,
			Got:         errString(err),
		})
	}
	{
		err := r.Kubectl(ctx.Ctx, "-n", "scn-extres", "wait",
			"--for=condition=Programmed", "--timeout=5m",
			"gateway/scn-extres-gateway")
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "Gateway Programmed=True",
			OK:          err == nil,
			Got:         errString(err),
		})
	}

	out, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-extres", "get",
		"httproute/scn-extres-route",
		"-o", "jsonpath={.status.parents[0].conditions[?(@.type==\"Accepted\")].status}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute Accepted=True",
		OK:          strings.TrimSpace(out) == "True",
		Got:         strings.TrimSpace(out),
	})

	// Pool CR exists with at least one member.
	memberCount, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-extres", "get",
		"pool.k8s.f5net.com/ext-backend-pool",
		"-o", "jsonpath={range .spec.members[*]}{.address}:{.port}{\"\\n\"}{end}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Pool CR has at least one member entry",
		OK:          strings.Contains(memberCount, "192.168.99."),
		Got:         oneLine(memberCount, 200),
	})

	// FRR pod available for the curl source.
	frrPod, ferr := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "get", "pod",
		"-l", "app=scn-frr",
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}")
	if ferr != nil || strings.TrimSpace(frrPod) == "" {
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "scn-bgp/scn-frr pod available",
			OK:          false,
			Got:         "missing (bgp-peer-frr not running?)",
		})
		return finalize(res)
	}
	frrPod = strings.TrimSpace(frrPod)

	// Wait for FRR to learn 203.0.113.101/32 via BGP.
	deadline := time.Now().Add(2 * time.Minute)
	var lastTable string
	hasGW := false
	for time.Now().Before(deadline) {
		bgpTable, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "-c", "frr", "--",
			"vtysh", "-c", "show bgp ipv4 unicast")
		lastTable = bgpTable
		if strings.Contains(bgpTable, "203.0.113.101") {
			hasGW = true
			break
		}
		select {
		case <-ctx.Ctx.Done():
			break
		case <-time.After(5 * time.Second):
		}
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "FRR BGP table has 203.0.113.101/32 advertised by TMM",
		OK:          hasGW,
		Got:         oneLine(lastTable, 200),
	})

	// 5x curl from inside FRR. apk-add curl in case it isn't already.
	_ = r.Kubectl(ctx.Ctx, "-n", "scn-bgp", "exec", frrPod, "-c", "frr", "--",
		"sh", "-c", "command -v curl >/dev/null 2>&1 || apk add --no-cache curl >/dev/null 2>&1 || true")
	const marker = "kindbnkctl-scenario-extres-pool-OK"
	const curls = 5
	successCount := 0
	var lastErr, lastBody string
	for i := 1; i <= curls; i++ {
		body, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "-c", "frr", "--",
			"curl", "-sS", "--fail", "--max-time", "8",
			"-H", "Host: extres.kindbnkctl.local",
			"http://203.0.113.101/",
		)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		lastBody = strings.TrimSpace(body)
		if strings.Contains(body, marker) {
			successCount++
		}
	}
	curlOK := successCount == curls
	got := fmt.Sprintf("%d/%d curls returned marker", successCount, curls)
	if !curlOK && lastErr != "" {
		got += " — last error: " + lastErr
	} else if !curlOK {
		got += " — last body: " + oneLine(lastBody, 120)
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: fmt.Sprintf("%d/%d curls via Gateway return ext-backend marker body", curls, curls),
		OK:          curlOK,
		Got:         got,
	})

	return finalize(res)
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	// Dropping the namespace removes Pool, HTTPRoute, Gateway,
	// F5BnkGateway, backend Deployment + Service + ConfigMap in one
	// shot.
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-extres",
		"--ignore-not-found")
	return nil
}

// discoverNADIP returns the IPv4 address of the named NAD attachment
// on a single pod selected by labelSelector in namespace.
func discoverNADIP(ctx *scenarios.Context, namespace, labelSelector, nadName string) (string, error) {
	r := ctx.Runner
	podName, err := r.KubectlCapture(ctx.Ctx, "-n", namespace, "get", "pod",
		"-l", labelSelector,
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil || strings.TrimSpace(podName) == "" {
		return "", fmt.Errorf("no Running pod for %s in %s: %w", labelSelector, namespace, err)
	}
	podName = strings.TrimSpace(podName)
	netStatus, err := r.KubectlCapture(ctx.Ctx, "-n", namespace, "get", "pod",
		podName,
		"-o", `jsonpath={.metadata.annotations.k8s\.v1\.cni\.cncf\.io/network-status}`)
	if err != nil {
		return "", err
	}
	var entries []struct {
		Name string   `json:"name"`
		IPs  []string `json:"ips"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(netStatus)), &entries); err != nil {
		return "", fmt.Errorf("parse network-status: %w (raw: %s)", err, oneLine(netStatus, 200))
	}
	for _, e := range entries {
		// Multus surfaces the NAD as <ns>/<name>; match the suffix.
		if !strings.HasSuffix(e.Name, "/"+nadName) && e.Name != nadName {
			continue
		}
		for _, ip := range e.IPs {
			if slash := strings.Index(ip, "/"); slash > 0 {
				ip = ip[:slash]
			}
			if !strings.Contains(ip, ":") && ip != "" {
				return ip, nil
			}
		}
	}
	return "", fmt.Errorf("NAD %s not found in network-status: %q", nadName, oneLine(netStatus, 300))
}

func renderTemplate(fsys embed.FS, path string, data any) (string, error) {
	raw, err := fsys.ReadFile(path)
	if err != nil {
		return "", err
	}
	t, err := template.New(path).Parse(string(raw))
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

func finalize(res scenarios.Result) scenarios.Result {
	if res.AllPassed() {
		res.Status = "ok"
		res.Summary = "Pool reconciled, BGP route present, 5/5 curls to ext-backend via Pool succeeded"
	} else {
		res.Status = "failed"
		var failed []string
		for _, a := range res.Assertions {
			if !a.OK {
				failed = append(failed, a.Description)
			}
		}
		res.Summary = "failed: " + strings.Join(failed, "; ")
	}
	return res
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return oneLine(err.Error(), 200)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
