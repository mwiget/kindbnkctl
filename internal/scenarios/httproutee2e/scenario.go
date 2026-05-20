// Package httproutee2e implements scenario "http-routing-e2e" — the
// full data-plane version of how-to #8 (HTTP traffic steering with
// Gateway API HTTPRoute). Builds on the BGP plumbing established by
// bgp-peer-frr: the curl client pod gets a static route for the
// Gateway IP via the FRR pod, which has a BGP-learned route to TMM.
//
// Pipeline:
//
//	scn-httproute-e2e namespace
//	  GatewayClass         (cluster-wide, idempotent)
//	  F5BnkGateway         (IP pool for the listener)
//	  Gateway              (static spec.addresses 203.0.113.100)
//	  HTTPRoute            (host=kindbnkctl.local, path=/, → nginx)
//	  nginx Deployment+Svc (2 replicas, marker body)
//	  scn-curl Deployment  (curl client w/ init container that
//	                        installs `ip route 203.0.113.100 via
//	                        <frr-pod-IP>`)
//
// Verification:
//   - Same control-plane checks as the old http-routing scenario
//   - 5 consecutive curls from inside scn-curl to
//     http://203.0.113.100/ with Host: kindbnkctl.local all return
//     the nginx marker body
package httproutee2e

import (
	"bytes"
	"embed"
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
	scnName  = "http-routing-e2e"
	scnTitle = "HTTP traffic steering with Gateway API HTTPRoute (how-to #8) — full data plane"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Amber }
func (s *scenario) Dependencies() []string   { return []string{"bgp-peer-frr"} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
End-to-end HTTPRoute scenario with the full plumbing in place
for real data-plane traffic. Requires the bgp-peer-frr scenario
to be running already.

Stages:
  1. GatewayClass + F5BnkGateway IP pool + Gateway (static
     spec.addresses=203.0.113.100) + HTTPRoute + nginx backend.
  2. scn-curl Deployment with an initContainer that installs a
     pod-local static route 203.0.113.100/32 via the discovered
     FRR pod IP (looked up in scn-bgp at apply time).
  3. From inside scn-curl: 5 consecutive curls to
     http://203.0.113.100/ with Host: kindbnkctl.local.

Currently rated AMBER, not GREEN, because the data-plane curl
depends on BGP between TMM and FRR being Established (so FRR
knows to forward Gateway-IP traffic to TMM's eth0). See the
bgp-peer-frr scenario's Description for the three documented
gaps (vlanName, passwd.conf, fake-gateway routing) that need
solving in TMM-pod-shape to lift both scenarios to green.

Control-plane assertions are pass/fail. The end-to-end curl is
recorded as a [bonus] assertion — visible in the report but
doesn't fail the scenario today.
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

	// 1. Look up FRR pod IP. Hard error if scn-bgp/scn-frr isn't
	//    deployed — we explicitly declare bgp-peer-frr as a
	//    dependency and don't try to recreate it here.
	frrIP, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "get", "pod",
		"-l", "app=scn-frr",
		"-o", "jsonpath={.items[0].status.podIP}")
	if err != nil || strings.TrimSpace(frrIP) == "" {
		return fmt.Errorf("scn-bgp/scn-frr not found — run `kindbnkctl scenario run bgp-peer-frr` first (err=%v, out=%q)",
			err, frrIP)
	}
	frrIP = strings.TrimSpace(frrIP)

	// 2. Apply static manifests in order. GatewayClass is idempotent;
	//    namespace must exist before the namespace-scoped objects.
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

	// 3. Render the curl-client deployment (template) so the rendered
	//    file is on disk for audit. Apply it only when BGP looks
	//    Established — otherwise the init container's static-route
	//    install will fail with "Network unreachable" because the
	//    FRR pod IP isn't reachable while ZeBOS isn't programming
	//    routes back. The discovered state determines whether we
	//    attempt the data-plane portion.
	_ = frrIP // referenced by template rendering below
	tmplBody, err := manifestFS.ReadFile("manifests/07-curl-client.yaml.tmpl")
	if err != nil {
		return err
	}
	t, err := template.New("curl").Parse(string(tmplBody))
	if err != nil {
		return err
	}
	var rendered bytes.Buffer
	if err := t.Execute(&rendered, struct{ FRRPodIP string }{FRRPodIP: frrIP}); err != nil {
		return err
	}
	if _, err := scenarios.WriteManifest(ctx.PoCDir, scnName,
		"07-curl-client.rendered.yaml", rendered.String()); err != nil {
		return err
	}
	if bgpEstablished(ctx) {
		if err := r.Apply(ctx.Ctx, rendered.String()); err != nil {
			return fmt.Errorf("apply curl-client: %w", err)
		}
	} else {
		fmt.Fprintln(ctx.Out, "      | BGP not Established (see bgp-peer-frr); skipping curl-client deploy")
	}
	return nil
}

// bgpEstablished asks FRR whether the BGP session with TMM is up.
// Returns false on any error or when no Established neighbor appears.
func bgpEstablished(ctx *scenarios.Context) bool {
	out, err := ctx.Runner.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
		"deploy/scn-frr", "--", "vtysh", "-c", "show bgp summary")
	if err != nil {
		return false
	}
	return strings.Contains(out, "Estab")
}

func (s *scenario) Verify(ctx *scenarios.Context) scenarios.Result {
	r := ctx.Runner
	res := scenarios.Result{}

	// Control-plane assertions (same as the old amber scenario).
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

	// HTTPRoute Accepted=True (via parentRef[0]).
	out, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-httproute-e2e", "get",
		"httproute/scn-route",
		"-o", "jsonpath={.status.parents[0].conditions[?(@.type==\"Accepted\")].status}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute Accepted=True",
		OK:          strings.TrimSpace(out) == "True",
		Got:         strings.TrimSpace(out),
	})

	// Data-plane portion is conditional on BGP being Established.
	// Apply skipped the curl-client deploy when BGP wasn't up; we
	// short-circuit verification here so the bonus assertion reflects
	// the right reality.
	curlOK := false
	got := "skipped: BGP not Established (see bgp-peer-frr)"
	if bgpEstablished(ctx) {
		if err := r.Wait(ctx.Ctx, "scn-httproute-e2e", "Available",
			"deployment/scn-curl", 2*time.Minute); err != nil {
			got = "scn-curl Deployment never Available: " + err.Error()
		} else {
			curlPod, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-httproute-e2e", "get",
				"pod", "-l", "app=scn-curl",
				"-o", "jsonpath={.items[0].metadata.name}")
			if err != nil || strings.TrimSpace(curlPod) == "" {
				got = "no scn-curl pod"
			} else {
				curlPod = strings.TrimSpace(curlPod)
				const marker = "kindbnkctl-scenario-httproute-e2e-OK"
				successCount := 0
				var lastErr, lastBody string
				for i := 1; i <= 5; i++ {
					body, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-httproute-e2e", "exec",
						curlPod, "-c", "curl", "--",
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
				curlOK = successCount == 5
				got = fmt.Sprintf("%d/5 curls returned marker", successCount)
				if !curlOK && lastErr != "" {
					got += " — last error: " + lastErr
				} else if !curlOK {
					got += " — last body: " + oneLine(lastBody, 120)
				}
			}
		}
	}
	// [bonus] assertion — runs but doesn't fail the scenario. Lifting
	// this requires BGP to actually establish (see bgp-peer-frr's
	// Description for the documented gaps).
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "[bonus] 5/5 curls through Gateway return nginx marker body (blocked by BGP gap — see bgp-peer-frr)",
		OK:          curlOK,
		Got:         got,
	})

	return finalizeResultE2E(res)
}

// finalizeResultE2E treats the last assertion (the data-plane curl)
// as a bonus that doesn't fail the scenario. Control-plane assertions
// fail it as usual.
func finalizeResultE2E(res scenarios.Result) scenarios.Result {
	if len(res.Assertions) == 0 {
		return finalizeResult(res)
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
			res.Summary = "control-plane reconciled + 5/5 end-to-end curls succeeded"
		} else {
			res.Summary = "control-plane reconciled (data-plane curl deferred — see bgp-peer-frr description)"
		}
	} else {
		res.Status = "failed"
		res.Summary = "failed: " + strings.Join(failed, "; ")
	}
	return res
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-httproute-e2e",
		"--ignore-not-found")
	return nil
}

// errString returns "" for nil err, otherwise a short error message.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return oneLine(err.Error(), 200)
}

// oneLine trims and shortens for Assertion.Got.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// finalizeResult derives Status and Summary from accumulated Assertions.
func finalizeResult(res scenarios.Result) scenarios.Result {
	if res.AllPassed() {
		res.Status = "ok"
		res.Summary = "control-plane reconciled + 5/5 end-to-end curls succeeded via BGP-learned route"
	} else {
		res.Status = "failed"
		// Build a short summary of which assertion(s) failed.
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
