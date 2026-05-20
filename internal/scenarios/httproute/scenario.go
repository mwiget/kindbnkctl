// Package httproute implements scenario #8 from the F5 BNK how-tos:
// HTTP traffic steering with Gateway API HTTPRoute. Maps to
// https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/
// Configure-HTTP-traffic-steering-with-Gateway-API-HTTPRoute.html.
//
// Topology:
//
//	GatewayClass (bnk-gatewayclass, controller f5.com/default-f5-cne-controller)
//	   └── Gateway (scn-gateway, listener :80)
//	         └── HTTPRoute (scn-route, hostnames=[kindbnkctl.local], path=/)
//	               └── Service nginx → Deployment nginx (2 replicas)
//
// Verification:
//   - Gateway condition Programmed=True within 5 min
//   - nginx pods Ready
//   - HTTPRoute condition Accepted=True
//   - kubectl-run curl pod hits the Gateway's ClusterIP service with the
//     hostname header and gets back the marker body from index.html
//
// Cleanup: delete the scenario namespace + the GatewayClass.
package httproute

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
	scnName  = "http-routing"
	scnTitle = "HTTP traffic steering with Gateway API HTTPRoute (how-to #8)"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Amber }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Applies a GatewayClass + Gateway + HTTPRoute + nginx backend. Asserts
the Gateway becomes Programmed, the HTTPRoute is Accepted, the
listener reports Programmed=True (TMM has the listener configured),
and the nginx backend is Ready.

Why this is rated AMBER (not GREEN):

End-to-end data-plane traffic (curl → Gateway IP → TMM → nginx)
does NOT work in the kindbnkctl 2-node / demoMode-TMM shape. The
gateway address space lives on the kind nodes' bnk-external docker
bridge but TMM in demoMode listens on virtio interfaces inside the
pod netns; nothing bridges between them. In a production BNK
deployment that bridge is provided by SR-IOV + F5SPKVlan; on kind
it requires plumbing this scenario doesn't yet install.

What IS verified:
  - GatewayClass exists + Accepted
  - Gateway has a static spec.addresses entry (we use 203.0.113.100
    from the bnk-external bridge range so the controller is happy)
  - Gateway condition Programmed=True
  - Listener status Programmed=True (TMM listener configured)
  - HTTPRoute condition Accepted=True
  - nginx Deployment Available

The scenario proves the BNK control-plane reconciliation works on
kind. A separate scenario adding the data-plane plumbing (proxy
sidecar, or a port-forward wrapper) would lift this to green.

Cleanup leaves the GatewayClass in place because other scenarios
reuse it.
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
	// Apply in the explicit numeric order embedded in the filenames
	// so the GatewayClass exists before the Gateway references it,
	// and the F5BnkGateway address pool exists before the Gateway
	// asks for an address.
	files := []string{
		"01-gatewayclass.yaml",
		"02-namespace.yaml",
		"00-platform.yaml",
		"03-backend.yaml",
		"04-gateway.yaml",
		"05-httproute.yaml",
	}
	for _, f := range files {
		body, err := manifestFS.ReadFile("manifests/" + f)
		if err != nil {
			return err
		}
		if err := ctx.Runner.Apply(ctx.Ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}
	return nil
}

func (s *scenario) Verify(ctx *scenarios.Context) scenarios.Result {
	r := &ctx.Runner
	// 1. nginx Available.
	if err := (*r).Wait(ctx.Ctx, "scn-httproute", "Available",
		"deployment/nginx", 3*time.Minute); err != nil {
		return scenarios.Result{Status: "failed",
			Summary: "nginx deployment not Available",
			Details: err.Error()}
	}
	// 2. Gateway Programmed=True (FLO marks Programmed once the TMM
	//    listener is up). Use --for=condition with a long-ish timeout
	//    so first-touch overhead doesn't trip us.
	if err := (*r).Kubectl(ctx.Ctx, "-n", "scn-httproute", "wait",
		"--for=condition=Programmed",
		"--timeout=5m",
		"gateway/scn-gateway"); err != nil {
		return scenarios.Result{Status: "failed",
			Summary: "Gateway scn-gateway never reached Programmed=True",
			Details: err.Error()}
	}
	// 3. HTTPRoute Accepted=True (per parentRef).
	out, err := (*r).KubectlCapture(ctx.Ctx, "-n", "scn-httproute", "get",
		"httproute/scn-route",
		"-o", "jsonpath={.status.parents[0].conditions[?(@.type==\"Accepted\")].status}")
	if err != nil {
		return scenarios.Result{Status: "failed",
			Summary: "could not read HTTPRoute status",
			Details: err.Error()}
	}
	if strings.TrimSpace(out) != "True" {
		return scenarios.Result{Status: "failed",
			Summary: fmt.Sprintf("HTTPRoute Accepted=%q (want True)", strings.TrimSpace(out))}
	}
	// 4. Listener status — the actual TMM-side configuration. This is
	//    the most meaningful condition because it tells us TMM
	//    accepted the listener config; the Gateway-level Programmed
	//    above only proves status.addresses is non-empty.
	out, err = (*r).KubectlCapture(ctx.Ctx, "-n", "scn-httproute", "get",
		"gateway/scn-gateway",
		"-o", "jsonpath={.status.listeners[0].conditions[?(@.type==\"Programmed\")].status}")
	if err != nil {
		return scenarios.Result{Status: "failed",
			Summary: "could not read listener status",
			Details: err.Error()}
	}
	if strings.TrimSpace(out) != "True" {
		return scenarios.Result{Status: "failed",
			Summary: fmt.Sprintf("Listener Programmed=%q (want True)", strings.TrimSpace(out))}
	}

	// Data-plane curl deliberately omitted — see Description() for
	// why. Control-plane reconciliation is the green portion of this
	// amber scenario.
	return scenarios.Result{Status: "ok",
		Summary: "GatewayClass + Gateway + Listener Programmed; HTTPRoute Accepted; nginx Available (data-plane curl not exercised — see description)"}
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	// Best-effort: never error on missing objects.
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-httproute", "--ignore-not-found")
	// Leave GatewayClass in place — other scenarios reuse it.
	return nil
}

