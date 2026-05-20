// Package clusterwidewatch implements scenario "cluster-wide-watch" —
// F5 BNK how-to #2 "Components needing cluster-wide access".
//
// The F5 doc is pure architectural prose with no YAML or kubectl
// commands. It states the load-bearing claim that "a single
// BIG-IP Next for Kubernetes Controller [...] watches multiple
// namespaces" and that all BNK components share a namespace.
// Our deploy already runs FLO in that posture
// (CNEInstance.spec.watchNamespaces=["All"], single
// f5-cne-controller Deployment in `default`).
//
// The scenario doesn't reconfigure anything — it asserts the
// claim concretely by:
//
//   1. Reading the CNEInstance and confirming watchNamespaces
//      includes "All".
//   2. Confirming the f5-cne-controller Deployment is a single
//      replica (the "one controller" claim).
//   3. Applying a brand-new namespace `scn-cwatch` with its own
//      Gateway + HTTPRoute + nginx backend, then asserting the
//      same controller reconciles those resources without any
//      per-namespace controller install.
//
// This is the most honest interpretation we can give of how-to #2
// on kind: the docs are conceptual, the running cluster already
// embodies the configuration, so the scenario stands witness to it.
package clusterwidewatch

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
	scnName  = "cluster-wide-watch"
	scnTitle = "Components needing cluster-wide access (how-to #2) — single controller reconciling a new namespace"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Green }
func (s *scenario) Dependencies() []string   { return nil }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Demonstrates BNK's "Whole Cluster"/cluster-wide-access pattern.
The F5 how-to itself is architectural prose with no YAML or
kubectl commands; it states two load-bearing claims:

  - "A single BIG-IP Next for Kubernetes Controller [...]
    watches multiple namespaces."
  - "All BIG-IP Next for Kubernetes pods run in the same
    namespace."

Our kindbnkctl deploy already runs FLO in that posture:
  - CNEInstance.spec.watchNamespaces = ["All"]
  - f5-cne-controller is a single Deployment in the
    default namespace
  - f5-cne-core hosts the shared CWC + DSSM + RabbitMQ +
    cert-manager + Fluentd
  - the same controller reconciles Gateways/HTTPRoutes in
    every namespace (http-routing-e2e, external-resource-pool,
    ai-token-counting, etc. already prove this incidentally)

This scenario makes the claim explicit: applies a brand-new
namespace + Gateway + HTTPRoute + nginx backend, then asserts
that the existing single controller picks them up
(Gateway Programmed=True, HTTPRoute Accepted=True) without
any per-namespace controller install or configuration.

Cleanup deletes the scn-cwatch namespace.
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

	// 1. CNEInstance.spec.watchNamespaces contains "All".
	watch, _ := r.KubectlCapture(ctx.Ctx, "-n", "default", "get",
		"cneinstance", "bnk-instance",
		"-o", `jsonpath={.spec.watchNamespaces}`)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: `CNEInstance.spec.watchNamespaces contains "All"`,
		OK:          strings.Contains(watch, "All"),
		Got:         oneLine(watch, 120),
	})

	// 2. f5-cne-controller is exactly one Deployment with one
	//    replica — proves the "single controller" claim.
	ctrlReplicas, _ := r.KubectlCapture(ctx.Ctx, "-n", "default", "get",
		"deployment/f5-cne-controller",
		"-o", "jsonpath={.spec.replicas}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "f5-cne-controller Deployment is a single replica",
		OK:          strings.TrimSpace(ctrlReplicas) == "1",
		Got:         "spec.replicas=" + strings.TrimSpace(ctrlReplicas),
	})

	// 3. nginx in the new namespace becomes Available — proves the
	//    namespace itself is healthy and the backend serves.
	{
		err := r.Wait(ctx.Ctx, "scn-cwatch", "Available",
			"deployment/nginx", 2*time.Minute)
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "nginx Deployment in scn-cwatch Available",
			OK:          err == nil, Got: errString(err),
		})
	}

	// 4. Gateway in scn-cwatch reaches Programmed=True — the
	//    single default-namespace controller picked it up.
	{
		err := r.Kubectl(ctx.Ctx, "-n", "scn-cwatch", "wait",
			"--for=condition=Programmed", "--timeout=3m",
			"gateway/scn-cwatch-gateway")
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "Gateway in scn-cwatch Programmed=True (cross-namespace reconcile)",
			OK:          err == nil, Got: errString(err),
		})
	}

	// 5. HTTPRoute in scn-cwatch is Accepted=True.
	hrState, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-cwatch", "get",
		"httproute/scn-cwatch-route",
		"-o", "jsonpath={.status.parents[0].conditions[?(@.type==\"Accepted\")].status}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute in scn-cwatch Accepted=True",
		OK:          strings.TrimSpace(hrState) == "True",
		Got:         strings.TrimSpace(hrState),
	})

	return finalize(res)
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-cwatch",
		"--ignore-not-found")
	return nil
}

func finalize(res scenarios.Result) scenarios.Result {
	if res.AllPassed() {
		res.Status = "ok"
		res.Summary = "Single cluster-wide controller reconciled a Gateway+HTTPRoute in a brand-new namespace"
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
