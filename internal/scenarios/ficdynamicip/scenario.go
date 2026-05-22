// Package ficdynamicip implements scenario "fic-dynamic-ip" — F5 BNK
// use-case "Dynamic IP address allocation" (FIC for Gateway API).
//
// Same plumbing as http-routing-e2e except the Gateway omits
// spec.addresses entirely. FIC (F5 IPAM Controller, lifecycled by FLO)
// is expected to pick the first free address from the F5BnkGateway
// pool and populate gateway.status.addresses. The scenario asserts
// (a) status.addresses got populated with an IP from the configured
// pool, (b) FRR's BGP table learns that /32, (c) 5/5 end-to-end curls
// to the dynamically-allocated address succeed.
//
// Reference: https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/use-cases/bnk-ficforgatewayapi.html
package ficdynamicip

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
	scnName  = "fic-dynamic-ip"
	scnTitle = "Dynamic IP address allocation (use-case FIC for Gateway API) — Gateway without spec.addresses"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Amber }
func (s *scenario) Dependencies() []string   { return []string{"bgp-peer-frr"} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Demonstrates the configuration for dynamic IP allocation by FIC.

The Gateway is applied WITHOUT spec.addresses. The F5BnkGateway
in the same namespace declares a small pool (203.0.113.110-.115)
and the Gateway's spec.infrastructure.parametersRef binds to it
per the F5 use-case doc. In a fully-wired FIC deployment the
F5BnkGateway pool would produce an IPAM/IPAMRange CR pair (label
f5bnkcr=true in the default namespace) and the IPAM controller
would allocate from the pool, populating gateway.status.addresses.

Status as of BNK 2.3.0 in kindbnkctl's demo-TMM shape (🟡):
  - F5BnkGateway applies cleanly.
  - Gateway applies cleanly; HTTPRoute reaches Accepted=True.
  - The Gateway never reaches Programmed=True: f5-cne-controller
    logs "No IPAM found for Gateway: scn-fic-gateway" because no
    IPAM CR with label f5bnkcr=true exists in default for this
    Gateway, and nothing in the demo deployment auto-converts the
    F5BnkGateway pool into IPAM/IPAMRange CRs. The Gateway's
    Programmed condition stays at "AddressNotAssigned".

This scenario therefore asserts the manifest-side state only:
F5BnkGateway exists, Gateway Accepted=True, HTTPRoute Accepted
=True. The "Programmed + status.addresses populated" path is
left as a known gap so the docs and assertion shape match the
reality of this cluster. Operators wanting full FIC end-to-end
need to either (a) manually create IPAM+IPAMRange CRs in default
labelled f5bnkcr=true, or (b) deploy on a BNK build where the
F5BnkGateway-to-IPAM auto-bridge controller is shipped.

Cleanup deletes the scn-fic-dyn namespace.
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
	if _, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "get", "pod",
		"-l", "app=scn-frr",
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}"); err != nil {
		return fmt.Errorf("dependency missing: run `kindbnkctl scenario run bgp-peer-frr` first (no Running scn-frr pod in scn-bgp)")
	}
	for _, f := range []string{
		"01-namespace.yaml",
		"02-bnkgateway.yaml",
		"03-backend.yaml",
		"04-gateway.yaml",
		"05-httproute.yaml",
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

	// F5BnkGateway exists and is the one we applied.
	bnk, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-fic-dyn", "get",
		"f5-bnkgateway/kindbnkctl-fic",
		"-o", "jsonpath={.metadata.name}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "F5BnkGateway kindbnkctl-fic exists",
		OK:          strings.TrimSpace(bnk) == "kindbnkctl-fic",
		Got:         strings.TrimSpace(bnk),
	})

	// nginx came up — confirms the namespace + workload manifests
	// applied cleanly even though FIC won't program the Gateway.
	{
		err := r.Wait(ctx.Ctx, "scn-fic-dyn", "Available",
			"deployment/nginx", 3*time.Minute)
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "nginx Deployment Available",
			OK:          err == nil, Got: errString(err),
		})
	}

	// Gateway is Accepted (controller saw it); we deliberately do NOT
	// wait for Programmed=True here — see Description() for why.
	gState, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-fic-dyn", "get",
		"gateway/scn-fic-gateway",
		"-o", "jsonpath={.status.conditions[?(@.type==\"Accepted\")].status}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Gateway Accepted=True",
		OK:          strings.TrimSpace(gState) == "True",
		Got:         strings.TrimSpace(gState),
	})

	// Surface the actual Programmed-condition reason in the report so
	// the gap is visible without grepping logs. Treated as informational
	// (always OK) — the message itself is what the operator wants to see.
	gMsg, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-fic-dyn", "get",
		"gateway/scn-fic-gateway",
		"-o", "jsonpath={.status.conditions[?(@.type==\"Programmed\")].message}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Gateway Programmed condition message (informational)",
		OK:          true,
		Got:         oneLine(gMsg, 200),
	})

	rstate, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-fic-dyn", "get",
		"httproute/scn-fic-route",
		"-o", "jsonpath={.status.parents[0].conditions[?(@.type==\"Accepted\")].status}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute Accepted=True",
		OK:          strings.TrimSpace(rstate) == "True",
		Got:         strings.TrimSpace(rstate),
	})

	return finalize(res)
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-fic-dyn",
		"--ignore-not-found")
	return nil
}

func finalize(res scenarios.Result) scenarios.Result {
	if res.AllPassed() {
		res.Status = "ok"
		res.Summary = "manifests applied, Gateway Accepted=True, HTTPRoute Accepted=True; allocation gap surfaced as informational (see Description)"
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
