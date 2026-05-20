// Package aitokencount implements scenario "ai-token-counting" —
// F5 BNK how-to #6 "Configure Token Counting and Enforcement".
//
// The how-to is annotation-driven: a single
// `k8s.f5.com/ai-token-counting` annotation on
// `Gateway.spec.infrastructure.annotations` tells TMM to count
// per-user inference tokens and enforce quotas. No dedicated CR
// is introduced.
//
// AMBER rating, because the F5 doc itself provides:
//   - no example LLM backend to point at
//   - no curl/openssl/kubectl verification command
//   - no concrete expected response
//
// What the scenario CAN verify is that the Gateway + HTTPRoute
// reconcile cleanly with the annotation in place, and that the
// annotation survives the FLO reconcile loop. The actual TMM
// token-counting behaviour requires a real LLM that emits
// varying `usage.prompt_tokens` / `usage.completion_tokens` per
// request — we point at a stub nginx that returns one fixed
// OpenAI-style JSON, which is enough to prove the route resolves
// but not enough to validate the counting math.
package aitokencount

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
	scnName  = "ai-token-counting"
	scnTitle = "Token Counting and Enforcement (how-to #6) — k8s.f5.com/ai-token-counting Gateway annotation"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Green }
func (s *scenario) Dependencies() []string   { return []string{"bgp-peer-frr"} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Demonstrates BNK's AI token-counting feature. The mechanism is
a single annotation on the Gateway's spec.infrastructure
section:

    k8s.f5.com/ai-token-counting: |
      token_counting=enabled,
      user_id_source=api_key,
      user_id_header=Authorization,
      fallback_to_ip=true,
      hsl_pool=hsl-logging-pool

No dedicated BNK CR is introduced. TMM reads the annotation,
parses incoming OpenAI-style /v1/chat/completions responses,
counts the per-user tokens, and (if 'enabled') enforces quotas
per user.

Rated AMBER because the F5 doc itself doesn't give us a
runnable backend or verification command. The scenario stands
up a stub nginx that returns a fixed OpenAI-style JSON with
usage.prompt_tokens=42 / usage.completion_tokens=11, applies
the Gateway + HTTPRoute, and asserts:

  - Gateway Programmed=True with the annotation present
  - HTTPRoute Accepted=True
  - the annotation actually carried into the live Gateway
    object after FLO reconciles

Token counting itself is a TMM data-plane feature that needs
varying token counts to validate (the stub always returns the
same fixed counts, and we have no HSL receiver to inspect
emitted log records). Lifting this to green would require:

  - a real LLM backend that emits varied token counts, OR
  - a stub backend that varies usage based on prompt size
  - an HSL log receiver to capture the per-user counters
  - F5-side tooling to introspect TMM's user_id buckets

Cleanup deletes the scn-tokencount namespace.
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

	{
		err := r.Wait(ctx.Ctx, "scn-tokencount", "Available",
			"deployment/stub-llm", 2*time.Minute)
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "stub-llm Deployment Available",
			OK:          err == nil, Got: errString(err),
		})
	}
	{
		err := r.Kubectl(ctx.Ctx, "-n", "scn-tokencount", "wait",
			"--for=condition=Programmed", "--timeout=3m",
			"gateway/scn-tokencount-gateway")
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "Gateway Programmed=True",
			OK:          err == nil, Got: errString(err),
		})
	}

	httpRouteState, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-tokencount", "get",
		"httproute/scn-tokencount-route",
		"-o", "jsonpath={.status.parents[0].conditions[?(@.type==\"Accepted\")].status}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute Accepted=True",
		OK:          strings.TrimSpace(httpRouteState) == "True",
		Got:         strings.TrimSpace(httpRouteState),
	})

	// The token-counting annotation should be present on the live
	// Gateway. FLO propagates the annotation value into the
	// auto-generated iRule's RULE_INIT (the TOKEN COUNTING IRULE INIT
	// log line on TMM).
	annValue, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-tokencount", "get",
		"gateway/scn-tokencount-gateway",
		"-o", `jsonpath={.spec.infrastructure.annotations.k8s\.f5\.com/ai-token-counting}`)
	hasAnn := strings.Contains(annValue, "token_counting=enabled") &&
		strings.Contains(annValue, "user_id_source=api_key")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "k8s.f5.com/ai-token-counting annotation present on Gateway",
		OK:          hasAnn,
		Got:         oneLine(annValue, 200),
	})

	// Find FRR pod for the curl source (the curl traverses the NAD
	// bridge into TMM exactly as http-routing-e2e does).
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

	// Wait for FRR to learn 203.0.113.103/32 via BGP.
	deadline := time.Now().Add(2 * time.Minute)
	gwLearned := false
	for time.Now().Before(deadline) {
		bgpTable, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "-c", "frr", "--",
			"vtysh", "-c", "show bgp ipv4 unicast")
		if strings.Contains(bgpTable, "203.0.113.103") {
			gwLearned = true
			break
		}
		select {
		case <-ctx.Ctx.Done():
			break
		case <-time.After(5 * time.Second):
		}
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "FRR BGP table has 203.0.113.103/32 advertised by TMM",
		OK:          gwLearned,
		Got:         fmt.Sprintf("learned=%v", gwLearned),
	})

	// Send a few curls through the Gateway with distinct Authorization
	// tokens. Then scrape TMM logs for the iRule's TOKEN(...) log
	// lines — these prove the data-plane counting fired and the
	// per-user / per-model / global cumulative counters incremented.
	_ = r.Kubectl(ctx.Ctx, "-n", "scn-bgp", "exec", frrPod, "-c", "frr", "--",
		"sh", "-c", "command -v curl >/dev/null 2>&1 || apk add --no-cache curl >/dev/null 2>&1 || true")
	const curls = 3
	successBodies := 0
	for i := 0; i < curls; i++ {
		body, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "-c", "frr", "--",
			"curl", "-sS", "--fail", "--max-time", "8",
			"-H", "Authorization: Bearer kindbnkctl-test-user",
			"-H", "Host: tokencount.kindbnkctl.local",
			"-d", `{"model":"gpt-stub","messages":[{"role":"user","content":"ping"}]}`,
			"http://203.0.113.103:8000/v1/chat/completions",
		)
		if err == nil && strings.Contains(body, "kindbnkctl-scenario-tokencount-OK") {
			successBodies++
		}
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: fmt.Sprintf("%d/%d curls through Gateway return stub-llm body", curls, curls),
		OK:          successBodies == curls,
		Got:         fmt.Sprintf("%d/%d", successBodies, curls),
	})

	// Scrape TMM logs for the token-counting iRule's TOKEN(...) lines.
	tmm, _ := r.KubectlCapture(ctx.Ctx, "-n", "default", "get", "pod",
		"-l", "app=f5-tmm",
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}")
	tmm = strings.TrimSpace(tmm)
	tmmLogs, _ := r.KubectlCapture(ctx.Ctx, "-n", "default", "logs", tmm,
		"-c", "f5-tmm", "--since=60s")
	hasToken := strings.Contains(tmmLogs, "TOKEN COUNTING IRULE INIT") &&
		strings.Contains(tmmLogs, "scn-tokencount-gateway-scn-tokencount-token-counting") &&
		strings.Contains(tmmLogs, "TOKEN(") &&
		strings.Contains(tmmLogs, "cumulative")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "TMM token-counting iRule fired (TOKEN(...) cumulative log lines)",
		OK:          hasToken,
		Got:         oneLine(extractMatching(tmmLogs, "TOKEN("), 250),
	})

	return finalize(res)
}

// extractMatching returns lines from s that contain needle, joined
// with " | ". Best-effort, capped at the first 4 matches.
func extractMatching(s, needle string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			out = append(out, line)
			if len(out) >= 4 {
				break
			}
		}
	}
	return strings.Join(out, " | ")
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-tokencount",
		"--ignore-not-found")
	return nil
}

func finalize(res scenarios.Result) scenarios.Result {
	if res.AllPassed() {
		res.Status = "ok"
		res.Summary = "Gateway annotation reconciled + TMM data-plane TOKEN(...) counters fired"
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
