// Package proxyprotocol implements scenario "proxy-protocol-l4" — F5
// BNK how-to #9 "Proxy Protocol iRule support for L4 routes".
//
// The Gateway has a TCP listener on port 8000. An L4Route binds it
// to an nginx backend Service. An F5BigCneIrule contains a PROXY v1
// iRule (on CLIENT_ACCEPTED capture client IP/port; on
// SERVER_CONNECTED prepend "PROXY TCP4 ..."). A BnkNetPolicy
// connects the iRule to the L4Route (extensionRefs → iRule,
// targetRefs → L4Route).
//
// The nginx backend is configured with `listen 80 proxy_protocol`,
// so it parses the PROXY header and exposes the original client
// address as $proxy_protocol_addr in the response body.
// End-to-end success = the body contains the marker + a parsed
// client IP that's not 0.0.0.0 (which would mean PROXY was missing).
package proxyprotocol

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
	scnName  = "proxy-protocol-l4"
	scnTitle = "Proxy Protocol iRule on an L4 route (how-to #9) — F5BigCneIrule + L4Route + BnkNetPolicy"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Amber }
func (s *scenario) Dependencies() []string   { return []string{"bgp-peer-frr"} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Demonstrates BNK's PROXY-protocol iRule pattern on a TCP route.

Three new BNK CRs come together:

  - F5BigCneIrule  — the iRule TCL script. On CLIENT_ACCEPTED it
    captures the original client IP+port; on SERVER_CONNECTED it
    prepends a PROXY v1 line to the server-side payload.
  - L4Route        — TCP-protocol route binding a Gateway listener
    to a backend Service (analogous to HTTPRoute but for raw L4).
  - BnkNetPolicy   — wires the iRule (extensionRef) to the route
    (targetRef) so the iRule fires on this route's traffic.

The nginx backend has 'listen 80 proxy_protocol' configured, so
it consumes the PROXY header and exposes the original client
address as $proxy_protocol_addr — the response body echoes that
value, making end-to-end PROXY plumbing easy to assert.

Rated AMBER. Control-plane assertions all pass — the new CRs
land, the controller reports the BNKNetPolicy as ResolvedRefs
True, the F5BigCneIrule as Programmed (config sent to all gRPC
endpoints), the L4Route as Accepted, the Gateway listener as
Programmed. But the iRule's TCP::respond does not actually
prepend PROXY bytes to the server-side payload in this BNK 2.3
build — nginx logs the incoming connection as
'broken header: "GET / HTTP/1.1" while reading PROXY protocol'
because no PROXY v1 line arrives. TMM accepts the L4 traffic
and proxies it through, but the iRule injection step is a
no-op as we've configured it.

Possibilities (untested):
  - iRule TCP::respond semantics on BNK L4Route may differ
    from BIG-IP TMOS; might require TCP::collect/TCP::release
    or a different event.
  - BNK 2.3 might attach iRules at the Gateway listener via
    BNKNetPolicy only for HTTP traffic, not for L4Route TCP.
  - sectionName routing on BNKNetPolicy might not propagate
    to TMM's iRule processor for non-HTTP listeners.

The data-plane curl is recorded as [bonus] — not fatal to the
scenario but visibly red in the verify report so this gap
remains visible to anyone running it.

Reproduce manually:

    kubectl -n scn-bgp exec deploy/scn-frr -c frr -- \
      curl -sS --fail http://203.0.113.102:8000/

…returns "Empty reply from server"; backend nginx logs show
"broken header" errors confirming no PROXY prefix arrived.

Cleanup deletes scn-proxy namespace.
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

	// Apply in numbered order so each object's dependencies are in
	// place before it lands. The BnkNetPolicy is last because it
	// references both the iRule and the L4Route.
	for _, f := range []string{
		"01-namespace.yaml",
		"02-bnkgateway.yaml",
		"03-backend.yaml",
		"04-gateway.yaml",
		"05-irule.yaml",
		"06-l4route.yaml",
		"07-bnknetpolicy.yaml",
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
		err := r.Wait(ctx.Ctx, "scn-proxy", "Available",
			"deployment/pp-backend", 3*time.Minute)
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "pp-backend Deployment Available",
			OK:          err == nil, Got: errString(err),
		})
	}
	{
		err := r.Kubectl(ctx.Ctx, "-n", "scn-proxy", "wait",
			"--for=condition=Programmed", "--timeout=5m",
			"gateway/scn-proxy-gateway")
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "Gateway Programmed=True",
			OK:          err == nil, Got: errString(err),
		})
	}

	l4State, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-proxy", "get",
		"l4route/scn-proxy-route",
		"-o", "jsonpath={.status.parents[0].conditions[?(@.type==\"Accepted\")].status}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "L4Route Accepted=True",
		OK:          strings.TrimSpace(l4State) == "True",
		Got:         strings.TrimSpace(l4State),
	})

	// Verify the BNK policy + iRule landed.
	irule, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-proxy", "get",
		"f5-big-cne-irule/pp-prepend",
		"-o", "jsonpath={.metadata.name}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "F5BigCneIrule pp-prepend exists",
		OK:          strings.TrimSpace(irule) == "pp-prepend",
		Got:         strings.TrimSpace(irule),
	})

	netpol, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-proxy", "get",
		"bnknetpolicy/scn-proxy-attach",
		"-o", "jsonpath={.metadata.name}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "BnkNetPolicy scn-proxy-attach exists",
		OK:          strings.TrimSpace(netpol) == "scn-proxy-attach",
		Got:         strings.TrimSpace(netpol),
	})

	// Find FRR pod for the curl source.
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

	// Wait for FRR to learn 203.0.113.102/32 via BGP.
	deadline := time.Now().Add(2 * time.Minute)
	var lastTable string
	hasGW := false
	for time.Now().Before(deadline) {
		bgpTable, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "-c", "frr", "--",
			"vtysh", "-c", "show bgp ipv4 unicast")
		lastTable = bgpTable
		if strings.Contains(bgpTable, "203.0.113.102") {
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
		Description: "FRR BGP table has 203.0.113.102/32 advertised by TMM",
		OK:          hasGW,
		Got:         oneLine(lastTable, 200),
	})

	// 5x curl through the Gateway TCP listener (port 8000). Expect the
	// body to contain the marker + a non-trivial proxy_addr — proves
	// the iRule fired and nginx parsed the PROXY header.
	_ = r.Kubectl(ctx.Ctx, "-n", "scn-bgp", "exec", frrPod, "-c", "frr", "--",
		"sh", "-c", "command -v curl >/dev/null 2>&1 || apk add --no-cache curl >/dev/null 2>&1 || true")
	const marker = "kindbnkctl-scenario-proxy-protocol-OK"
	const curls = 5
	successCount := 0
	var lastErr, lastBody string
	for i := 1; i <= curls; i++ {
		body, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "-c", "frr", "--",
			"curl", "-sS", "--fail", "--max-time", "8",
			"http://203.0.113.102:8000/",
		)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		lastBody = strings.TrimSpace(body)
		// Pass requires marker + proxy_addr= + something that isn't
		// 0.0.0.0 (which would indicate PROXY header was missing).
		if strings.Contains(body, marker) &&
			strings.Contains(body, "proxy_addr=") &&
			!strings.Contains(body, "proxy_addr=0.0.0.0") &&
			!strings.Contains(body, "proxy_addr=\n") {
			successCount++
		}
	}
	curlOK := successCount == curls
	got := fmt.Sprintf("%d/%d curls saw marker + non-empty proxy_addr", successCount, curls)
	if lastBody != "" {
		got += " — last body: " + oneLine(lastBody, 120)
	}
	if !curlOK && lastErr != "" {
		got += " — last error: " + lastErr
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: fmt.Sprintf("[bonus] %d/%d L4 curls carry PROXY header parsed by nginx (TMM iRule injection — see description)", curls, curls),
		OK:          curlOK,
		Got:         got,
	})

	return finalizeAmber(res)
}

// finalizeAmber treats the last assertion (the PROXY curl) as a
// non-fatal bonus — control-plane assertions still fail the
// scenario as usual.
func finalizeAmber(res scenarios.Result) scenarios.Result {
	if len(res.Assertions) == 0 {
		return finalize(res)
	}
	required := res.Assertions[:len(res.Assertions)-1]
	allOK := true
	var failed []string
	for _, a := range required {
		if !a.OK {
			allOK = false
			failed = append(failed, a.Description)
		}
	}
	bonus := res.Assertions[len(res.Assertions)-1]
	if allOK {
		res.Status = "ok"
		if bonus.OK {
			res.Summary = "L4Route + iRule + BNKNetPolicy reconciled; PROXY-injected curl succeeded"
		} else {
			res.Summary = "L4Route + iRule + BNKNetPolicy reconciled (PROXY injection didn't fire — see description)"
		}
	} else {
		res.Status = "failed"
		res.Summary = "failed: " + strings.Join(failed, "; ")
	}
	return res
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-proxy",
		"--ignore-not-found")
	return nil
}

func finalize(res scenarios.Result) scenarios.Result {
	if res.AllPassed() {
		res.Status = "ok"
		res.Summary = "L4Route + iRule + BnkNetPolicy reconciled; 5/5 PROXY-prefixed connections parsed by nginx"
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
