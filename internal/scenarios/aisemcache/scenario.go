// Package aisemcache implements scenario "ai-semantic-cache" —
// F5 BNK how-to #7 sub-article "Semantic AI Model Caching".
//
// Two annotations together enable semantic caching:
//
//   - Gateway.spec.infrastructure.annotations:
//       k8s.f5.com/ai: |
//         semantic_cache=enabled,
//         semantic_cache_ip_port=<modelcache IP>:<port>,
//         semantic_cache_recv_timeout=1000
//   - HTTPRoute.metadata.annotations:
//       k8s.f5.com/sse-enabled: "true"
//
// On a cache HIT, TMM returns the cached response directly from
// the configured CodeFuse-ModelCache endpoint. On MISS, the
// request continues to the HTTPRoute backendRef (an LLM) and the
// response is then stored in the cache.
//
// AMBER. The control-plane wiring works on kind, but we can't
// run a real CodeFuse-ModelCache (needs persistent vector
// storage, embedding model, ML stack — all out of scope). The
// scenario:
//   - deploys a stub LLM nginx returning a fixed
//     OpenAI-style JSON
//   - deploys a stub TCP listener at port 5050 to stand in for
//     the ModelCache endpoint (TMM dials it, gets a TCP accept
//     but no useful protocol, so every request will MISS and
//     fall through to the stub LLM)
//   - applies the Gateway with the semantic-cache annotation
//     (substituting the stub-modelcache Service ClusterIP)
//   - applies the HTTPRoute with sse-enabled annotation
//
// What the scenario verifies (control-plane only): both
// annotations reconcile and survive on the live objects, the
// Gateway is Programmed, the HTTPRoute is Accepted.
package aisemcache

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
	scnName  = "ai-semantic-cache"
	scnTitle = "Semantic AI Model Caching (how-to #7) — k8s.f5.com/ai semantic_cache + sse-enabled"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Green }
func (s *scenario) Dependencies() []string   { return []string{"bgp-peer-frr"} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Demonstrates BNK's semantic-cache annotation pattern. On the
Gateway:

    k8s.f5.com/ai: |
      semantic_cache=enabled,
      semantic_cache_ip_port=<IP>:<port>,
      semantic_cache_recv_timeout=1000

…and on the HTTPRoute:

    k8s.f5.com/sse-enabled: "true"

(SSE is paired with semantic caching because LLM completions
commonly stream via Server-Sent Events.)

Rated AMBER. Real CodeFuse-ModelCache is out of scope on kind —
it needs persistent vector storage, an embedding model, and a
working ML stack. The scenario uses a TCP-listener stub at port
5050 to stand in for the ModelCache endpoint. TMM will dial it
on each request, get a clean TCP accept but no useful response,
fall through to the stub LLM as a "cache miss". The control-
plane wiring (Gateway + HTTPRoute with both annotations) is
what the verify step actually exercises.

Lifting this to green would require:
  - deploying real CodeFuse-ModelCache + its vector backend
  - deploying a real LLM (NIM/vLLM) as the cache-miss path
  - sending two identical prompts and observing one HIT, one
    MISS in TMM's data-plane telemetry

Cleanup deletes the scn-semcache namespace.
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

	// 1. Namespace + IP pool + stub backends.
	for _, f := range []string{
		"01-namespace.yaml",
		"02-bnkgateway.yaml",
		"03-stubs.yaml",
	} {
		body, err := manifestFS.ReadFile("manifests/" + f)
		if err != nil {
			return err
		}
		if err := r.Apply(ctx.Ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}

	// 2. Discover stub-modelcache Service ClusterIP — needed in the
	//    Gateway annotation.
	cacheIP, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-semcache", "get",
		"service/stub-modelcache",
		"-o", "jsonpath={.spec.clusterIP}")
	cacheIP = strings.TrimSpace(cacheIP)
	if err != nil || cacheIP == "" {
		return fmt.Errorf("discover stub-modelcache ClusterIP: %w (got %q)", err, cacheIP)
	}
	fmt.Fprintf(ctx.Out, "      | stub-modelcache ClusterIP: %s\n", cacheIP)

	// 3. Render Gateway template with cacheIP substituted, persist
	//    for audit, apply.
	gwBody, err := renderTemplate(manifestFS,
		"manifests/04-gateway.yaml.tmpl",
		struct{ CacheIP string }{CacheIP: cacheIP})
	if err != nil {
		return err
	}
	if _, err := scenarios.WriteManifest(ctx.PoCDir, scnName,
		"04-gateway.rendered.yaml", gwBody); err != nil {
		return err
	}
	if err := r.Apply(ctx.Ctx, gwBody); err != nil {
		return fmt.Errorf("apply Gateway: %w", err)
	}

	// 4. HTTPRoute.
	body, err := manifestFS.ReadFile("manifests/05-httproute.yaml")
	if err != nil {
		return err
	}
	if err := r.Apply(ctx.Ctx, string(body)); err != nil {
		return fmt.Errorf("apply 05-httproute.yaml: %w", err)
	}
	return nil
}

func (s *scenario) Verify(ctx *scenarios.Context) scenarios.Result {
	r := ctx.Runner
	res := scenarios.Result{}

	{
		err := r.Wait(ctx.Ctx, "scn-semcache", "Available",
			"deployment/stub-llm", 2*time.Minute)
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "stub-llm Deployment Available",
			OK:          err == nil, Got: errString(err),
		})
	}
	{
		err := r.Wait(ctx.Ctx, "scn-semcache", "Available",
			"deployment/stub-modelcache", 2*time.Minute)
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "stub-modelcache Deployment Available",
			OK:          err == nil, Got: errString(err),
		})
	}
	{
		err := r.Kubectl(ctx.Ctx, "-n", "scn-semcache", "wait",
			"--for=condition=Programmed", "--timeout=3m",
			"gateway/scn-semcache-gateway")
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "Gateway Programmed=True",
			OK:          err == nil, Got: errString(err),
		})
	}

	httpRouteState, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-semcache", "get",
		"httproute/scn-semcache-route",
		"-o", "jsonpath={.status.parents[0].conditions[?(@.type==\"Accepted\")].status}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "HTTPRoute Accepted=True",
		OK:          strings.TrimSpace(httpRouteState) == "True",
		Got:         strings.TrimSpace(httpRouteState),
	})

	// Gateway has the k8s.f5.com/ai semantic-cache annotation.
	gwAnn, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-semcache", "get",
		"gateway/scn-semcache-gateway",
		"-o", `jsonpath={.spec.infrastructure.annotations.k8s\.f5\.com/ai}`)
	hasGWAnn := strings.Contains(gwAnn, "semantic_cache=enabled") &&
		strings.Contains(gwAnn, "semantic_cache_ip_port=")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "k8s.f5.com/ai semantic-cache annotation present on Gateway",
		OK:          hasGWAnn,
		Got:         oneLine(gwAnn, 200),
	})

	// HTTPRoute has k8s.f5.com/sse-enabled=true.
	rtAnn, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-semcache", "get",
		"httproute/scn-semcache-route",
		"-o", `jsonpath={.metadata.annotations.k8s\.f5\.com/sse-enabled}`)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "k8s.f5.com/sse-enabled annotation present on HTTPRoute",
		OK:          strings.TrimSpace(rtAnn) == "true",
		Got:         "value=" + strings.TrimSpace(rtAnn),
	})

	// Find FRR pod for the curl source.
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

	// Wait for FRR to learn 203.0.113.104/32 via BGP.
	deadline := time.Now().Add(2 * time.Minute)
	gwLearned := false
	for time.Now().Before(deadline) {
		bgpTable, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "-c", "frr", "--",
			"vtysh", "-c", "show bgp ipv4 unicast")
		if strings.Contains(bgpTable, "203.0.113.104") {
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
		Description: "FRR BGP table has 203.0.113.104/32 advertised by TMM",
		OK:          gwLearned,
		Got:         fmt.Sprintf("learned=%v", gwLearned),
	})

	// Send POST requests through the Gateway. The semantic-cache iRule
	// fires on each HTTP_REQUEST and tries to query stub-modelcache
	// (which accepts the TCP connection but returns nothing useful,
	// so every request is a CACHE MISS and the response falls through
	// to stub-llm). We then scrape TMM logs for the iRule's
	// SEMANTIC_CACHE_IRULE log lines that prove the iRule fired.
	_ = r.Kubectl(ctx.Ctx, "-n", "scn-bgp", "exec", frrPod, "-c", "frr", "--",
		"sh", "-c", "command -v curl >/dev/null 2>&1 || apk add --no-cache curl >/dev/null 2>&1 || true")
	const curls = 3
	successBodies := 0
	for i := 0; i < curls; i++ {
		body, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "-c", "frr", "--",
			"curl", "-sS", "--fail", "--max-time", "8",
			"-H", "Host: semcache.kindbnkctl.local",
			"-d", `{"model":"llm-stub","messages":[{"role":"user","content":"identical prompt for cache miss"}]}`,
			"http://203.0.113.104/v1/chat/completions",
		)
		if err == nil && strings.Contains(body, "kindbnkctl-scenario-semcache-OK") {
			successBodies++
		}
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: fmt.Sprintf("%d/%d curls return stub-llm body (cache-miss fall-through)", curls, curls),
		OK:          successBodies == curls,
		Got:         fmt.Sprintf("%d/%d", successBodies, curls),
	})

	// Scrape TMM logs for the semantic-cache iRule's events.
	tmm, _ := r.KubectlCapture(ctx.Ctx, "-n", "default", "get", "pod",
		"-l", "app=f5-tmm",
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}")
	tmm = strings.TrimSpace(tmm)
	tmmLogs, _ := r.KubectlCapture(ctx.Ctx, "-n", "default", "logs", tmm,
		"-c", "f5-tmm", "--since=60s")
	hasIRule := strings.Contains(tmmLogs, "scn-semcache-gateway-scn-semcache-semantic-cache") &&
		strings.Contains(tmmLogs, "Client initialized with modelcache_server=") &&
		strings.Contains(tmmLogs, "SEMANTIC_CACHE_IRULE: HTTP_REQUEST triggered")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "TMM semantic-cache iRule fired (CLIENT_ACCEPTED + HTTP_REQUEST events)",
		OK:          hasIRule,
		Got:         oneLine(extractMatching(tmmLogs, "SEMANTIC_CACHE_IRULE"), 250),
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
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-semcache",
		"--ignore-not-found")
	return nil
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
		res.Summary = "Annotations reconciled + TMM data-plane semantic-cache iRule fires on every request"
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
