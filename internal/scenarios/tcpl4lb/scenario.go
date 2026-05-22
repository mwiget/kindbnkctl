// Package tcpl4lb implements scenario "tcp-l4-loadbalance" — an L4Route
// with protocol=TCP and two weighted backendRefs. Demonstrates BNK's
// raw TCP load balancing without any iRule machinery.
//
// The Gateway listener is TCP/8080. Two distinct nginx Services have
// marker bodies that identify which backend served the request. The
// verify step issues 20 curls from inside the FRR pod (which sits
// on the bnk-bgp NAD with a BGP-learned route to the Gateway IP)
// and asserts BOTH backends served at least one request — exact
// 70/30 distribution is not asserted because 20 samples is too small
// for a statistical pass.
package tcpl4lb

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
	scnName  = "tcp-l4-loadbalance"
	scnTitle = "TCP load balancer via L4Route — weighted backends (70/30)"

	gwAddr = "203.0.113.106"
	gwPort = "8080"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Green }
func (s *scenario) Dependencies() []string   { return []string{"bgp-peer-frr"} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Exercises L4Route's load-balancing behavior with two weighted
backends (70/30). Each backend is a separate nginx Deployment
serving a distinct marker body (backend=A / backend=B), so the
verify step can identify which backend served each request.

Verification issues 20 curls from inside the FRR pod through the
Gateway's TCP listener at 203.0.113.106:8080. Pass requires:
  - Gateway Programmed=True
  - L4Route Accepted=True
  - FRR has 203.0.113.106/32 in its BGP table
  - Both backend=A and backend=B served at least one request

Exact weighting (70/30) is not asserted — 20 samples is far too
small to call that reliably. The point is that the L4Route weight
field actually splits traffic across multiple endpoints, not just
hashes to one.

Cleanup deletes the scn-tcp-lb namespace.
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
		return fmt.Errorf("dependency missing: run `kindbnkctl scenario run bgp-peer-frr` first (no Running scn-frr pod)")
	}
	for _, f := range []string{
		"01-namespace.yaml",
		"02-bnkgateway.yaml",
		"03-backend.yaml",
		"04-gateway.yaml",
		"05-l4route.yaml",
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

	for _, dep := range []string{"tcp-lb-a", "tcp-lb-b"} {
		err := r.Wait(ctx.Ctx, "scn-tcp-lb", "Available",
			"deployment/"+dep, 3*time.Minute)
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: dep + " Deployment Available",
			OK:          err == nil, Got: errString(err),
		})
	}
	{
		err := r.Kubectl(ctx.Ctx, "-n", "scn-tcp-lb", "wait",
			"--for=condition=Programmed", "--timeout=5m",
			"gateway/scn-tcp-lb-gateway")
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "Gateway Programmed=True",
			OK:          err == nil, Got: errString(err),
		})
	}
	rstate, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-tcp-lb", "get",
		"l4route/scn-tcp-lb-route",
		"-o", "jsonpath={.status.parents[0].conditions[?(@.type==\"Accepted\")].status}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "L4Route Accepted=True",
		OK:          strings.TrimSpace(rstate) == "True",
		Got:         strings.TrimSpace(rstate),
	})

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
		if strings.Contains(bgpTable, gwAddr) {
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
		Description: fmt.Sprintf("FRR BGP table has %s/32 advertised by TMM", gwAddr),
		OK:          hasGW,
		Got:         oneLine(lastTable, 200),
	})

	_ = r.Kubectl(ctx.Ctx, "-n", "scn-bgp", "exec", frrPod, "-c", "frr", "--",
		"sh", "-c", "command -v curl >/dev/null 2>&1 || apk add --no-cache curl >/dev/null 2>&1 || true")
	const total = 20
	hitA, hitB, failures := 0, 0, 0
	var lastBody, lastErr string
	for i := 0; i < total; i++ {
		body, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "-c", "frr", "--",
			"curl", "-sS", "--fail", "--max-time", "8",
			"http://"+gwAddr+":"+gwPort+"/",
		)
		if err != nil {
			failures++
			lastErr = err.Error()
			continue
		}
		lastBody = strings.TrimSpace(body)
		switch {
		case strings.Contains(body, "backend=A"):
			hitA++
		case strings.Contains(body, "backend=B"):
			hitB++
		}
	}
	allOK := failures == 0
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: fmt.Sprintf("%d/%d L4 TCP curls succeeded", total, total),
		OK:          allOK,
		Got: fmt.Sprintf("ok=%d failed=%d (lastErr=%s lastBody=%s)",
			total-failures, failures, lastErr, oneLine(lastBody, 80)),
	})
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "weighted L4 LB hit both backend=A and backend=B at least once",
		OK:          hitA > 0 && hitB > 0,
		Got:         fmt.Sprintf("A=%d B=%d (of %d)", hitA, hitB, total),
	})
	return finalize(res)
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-tcp-lb",
		"--ignore-not-found")
	return nil
}

func finalize(res scenarios.Result) scenarios.Result {
	if res.AllPassed() {
		res.Status = "ok"
		res.Summary = "L4Route TCP LB reconciled; weight 70/30 split observed across A and B"
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
