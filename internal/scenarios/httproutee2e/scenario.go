// Package httproutee2e implements scenario "http-routing-e2e" — the
// full data-plane version of how-to #8 (HTTP traffic steering with
// Gateway API HTTPRoute).
//
// Builds on the BGP plumbing established by bgp-peer-frr:
//   - FRR pod has a kernel route for 203.0.113.100/32 via TMM's
//     net1 NAD IP (learned via BGP from ZeBOS's redistribute kernel).
//   - That path bypasses TMM's eth0 TCP hook — packets reach TMM
//     on net1 (NAD bridge), not on Calico eth0.
//   - Verification curls from inside the FRR pod itself: it's already
//     on the NAD bridge, already has the BGP-learned route, and is
//     a regular Linux pod for the kernel TCP stack. No separate
//     curl-client deployment needed.
//
// Pipeline (Apply):
//
//	1. GatewayClass + F5BnkGateway IP pool
//	2. nginx Deployment+Service (2 replicas, marker body)
//	3. Gateway with static spec.addresses=203.0.113.100
//	4. HTTPRoute (host=kindbnkctl.local, path=/, → nginx)
//	5. Wait for FRR's BGP table to include 203.0.113.100/32
//	   (proof TMM is now advertising it after the Gateway apply)
//
// Verification:
//   - control plane (GatewayClass + Gateway Programmed + HTTPRoute
//     Accepted + nginx Available)
//   - FRR has 203.0.113.100/32 in its BGP table
//   - 5 consecutive curls from inside FRR to
//     http://203.0.113.100/ with Host: kindbnkctl.local all return
//     the nginx marker body
package httproutee2e

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/mwiget/kindbnkctl/internal/scenarios"
)

//go:embed manifests/*.yaml
var manifestFS embed.FS

const (
	scnName  = "http-routing-e2e"
	scnTitle = "HTTP traffic steering with Gateway API HTTPRoute (how-to #8) — full data plane via NAD"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Green }
func (s *scenario) Dependencies() []string   { return []string{"bgp-peer-frr"} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
End-to-end HTTPRoute scenario with real data-plane traffic.

Requires bgp-peer-frr to be running already — relies on the
FRR pod in scn-bgp having a BGP session with TMM over the
bnk-bgp NAD. When this scenario applies the Gateway with
static spec.addresses=203.0.113.100, TMM (via ZeBOS's
redistribute kernel) advertises that /32 over BGP. FRR
receives it with next-hop set to TMM's net1 IP and installs
a kernel route for 203.0.113.100/32 via net1.

The verify step then execs 5 curls from inside the FRR pod
to http://203.0.113.100/ with the configured hostname. The
path is: FRR socket → FRR kernel routing → net1 → bnk-bgp
bridge → TMM net1 → TMM Gateway listener → nginx backend.
TMM's eth0 TCP hook is completely bypassed because the
traffic never touches eth0.

No separate curl-client pod is deployed — the FRR pod is
already exactly the kind of client this scenario needs
(on the NAD, with the BGP-learned route to the Gateway IP).
Operators can reproduce manually:

    kubectl -n scn-bgp exec deploy/scn-frr -c frr -- \
      curl -sS -H 'Host: kindbnkctl.local' http://203.0.113.100/

Cleanup: delete the scn-httproute-e2e namespace. The
GatewayClass stays (cluster-wide; reused by other scenarios).
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

	// Check the dependency is in place before touching anything.
	if _, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "get", "pod",
		"-l", "app=scn-frr",
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}"); err != nil {
		return fmt.Errorf("dependency missing: run `kindbnkctl scenario run bgp-peer-frr` first (no Running scn-frr pod in scn-bgp namespace)")
	}

	// Apply static manifests in order. GatewayClass is idempotent;
	// namespace must exist before namespace-scoped objects.
	for _, f := range []string{
		"01-gatewayclass.yaml",
		"02-namespace.yaml",
		"03-bnkgateway.yaml",
		"04-backend.yaml",
		"05-gateway.yaml",
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
		err := r.Wait(ctx.Ctx, "scn-httproute-e2e", "Available",
			"deployment/nginx", 3*time.Minute)
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "nginx Deployment Available",
			OK:          err == nil,
			Got:         errString(err),
		})
	}
	{
		err := r.Kubectl(ctx.Ctx, "-n", "scn-httproute-e2e", "wait",
			"--for=condition=Programmed", "--timeout=5m",
			"gateway/scn-gateway")
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "Gateway Programmed=True",
			OK:          err == nil,
			Got:         errString(err),
		})
	}

	out, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-httproute-e2e", "get",
		"httproute/scn-route",
		"-o", "jsonpath={.status.parents[0].conditions[?(@.type==\"Accepted\")].status}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute Accepted=True",
		OK:          strings.TrimSpace(out) == "True",
		Got:         strings.TrimSpace(out),
	})

	// Wait for FRR's BGP table to include 203.0.113.100/32 — proof
	// that TMM started advertising it after Gateway+HTTPRoute apply.
	frrPod, ferr := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "get", "pod",
		"-l", "app=scn-frr",
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}")
	if ferr != nil || strings.TrimSpace(frrPod) == "" {
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "scn-bgp/scn-frr pod available", OK: false,
			Got: "missing (bgp-peer-frr not running?)",
		})
		return finalize(res)
	}
	frrPod = strings.TrimSpace(frrPod)

	deadline := time.Now().Add(2 * time.Minute)
	var lastTable string
	hasGW := false
	for time.Now().Before(deadline) {
		bgpTable, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "-c", "frr", "--",
			"vtysh", "-c", "show bgp ipv4 unicast")
		lastTable = bgpTable
		if strings.Contains(bgpTable, "203.0.113.100") {
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
		Description: "FRR BGP table has 203.0.113.100/32 advertised by TMM",
		OK:          hasGW,
		Got:         oneLine(lastTable, 200),
	})

	// Also confirm the kernel route is installed (BGP→FIB transition).
	frrKernelRoute, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
		frrPod, "-c", "frr", "--",
		"ip", "route", "show", "203.0.113.100")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "FRR kernel route 203.0.113.100/32 installed via net1",
		OK:          strings.Contains(frrKernelRoute, "net1"),
		Got:         oneLine(frrKernelRoute, 150),
	})

	// 5 consecutive curls from inside FRR through the Gateway IP.
	const marker = "kindbnkctl-scenario-httproute-e2e-OK"
	const curls = 5
	successCount := 0
	var lastErr, lastBody string
	// Ensure curl is present in the FRR image (alpine-ish base).
	_ = r.Kubectl(ctx.Ctx, "-n", "scn-bgp", "exec", frrPod, "-c", "frr", "--",
		"sh", "-c", "command -v curl >/dev/null 2>&1 || apk add --no-cache curl >/dev/null 2>&1 || true")
	for i := 1; i <= curls; i++ {
		body, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "-c", "frr", "--",
			"curl", "-sS", "--fail", "--max-time", "8",
			"-H", "Host: kindbnkctl.local",
			"http://203.0.113.100/",
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
		Description: fmt.Sprintf("%d/%d end-to-end curls via Gateway return nginx marker body", curls, curls),
		OK:          curlOK,
		Got:         got,
	})

	return finalize(res)
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-httproute-e2e",
		"--ignore-not-found")
	return nil
}

func finalize(res scenarios.Result) scenarios.Result {
	if res.AllPassed() {
		res.Status = "ok"
		res.Summary = "control-plane reconciled + 5/5 end-to-end curls succeeded via NAD"
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
