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
func (s *scenario) Rating() scenarios.Rating { return scenarios.Amber }
func (s *scenario) Dependencies() []string   { return nil }
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
	// Gateway. FLO may or may not propagate this further — verify
	// it lands and survives the reconcile.
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

	return finalize(res)
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-tokencount",
		"--ignore-not-found")
	return nil
}

func finalize(res scenarios.Result) scenarios.Result {
	if res.AllPassed() {
		res.Status = "ok"
		res.Summary = "Gateway with k8s.f5.com/ai-token-counting annotation reconciled (data-plane counting not verified — F5 doc has no verify command)"
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
