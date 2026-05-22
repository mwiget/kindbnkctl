// Package udpl4lb implements scenario "udp-l4-loadbalance" — L4Route
// with protocol=UDP routing to an alpine/socat-based UDP echo backend.
//
// Verification: install socat into the FRR pod (one-shot apk add),
// then send a probe datagram and read the echo reply. UDP is
// connectionless so this only proves the data path (no LB-ratio
// assertion).
package udpl4lb

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
	scnName  = "udp-l4-loadbalance"
	scnTitle = "UDP load balancer via L4Route — alpine/socat echo backend"

	gwAddr = "203.0.113.107"
	gwPort = "5005"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Green }
func (s *scenario) Dependencies() []string   { return []string{"bgp-peer-frr"} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Exercises L4Route's UDP protocol support. The Gateway listens on
UDP 5005; an alpine/socat backend forks per datagram and replies
with a marker string. The verify step sends a probe via socat
from inside the FRR pod (which has a BGP-learned route to the
Gateway IP) and asserts the reply contains the marker.

UDP is connectionless and per-flow hashed by the L4 LB, so we
don't assert distribution across the two replicas — the point
here is that the data path works end-to-end.

Cleanup deletes the scn-udp-lb namespace.
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

	{
		err := r.Wait(ctx.Ctx, "scn-udp-lb", "Available",
			"deployment/udp-echo", 3*time.Minute)
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "udp-echo Deployment Available",
			OK:          err == nil, Got: errString(err),
		})
	}
	{
		err := r.Kubectl(ctx.Ctx, "-n", "scn-udp-lb", "wait",
			"--for=condition=Programmed", "--timeout=5m",
			"gateway/scn-udp-lb-gateway")
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "Gateway Programmed=True",
			OK:          err == nil, Got: errString(err),
		})
	}
	rstate, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-udp-lb", "get",
		"l4route/scn-udp-lb-route",
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

	// Install socat in the FRR pod for the UDP probe. Idempotent.
	if err := r.Kubectl(ctx.Ctx, "-n", "scn-bgp", "exec", frrPod, "-c", "frr", "--",
		"sh", "-c", "command -v socat >/dev/null 2>&1 || apk add --no-cache socat >/dev/null 2>&1"); err != nil {
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "socat available in FRR pod", OK: false, Got: err.Error(),
		})
		return finalize(res)
	}

	// Send a single UDP probe; expect the marker in the response.
	// `-t 3` makes socat give up after 3s of idle.
	const marker = "kindbnkctl-scenario-udp-lb-OK"
	const probes = 5
	successCount := 0
	var lastBody string
	for i := 0; i < probes; i++ {
		body, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "-c", "frr", "--",
			"sh", "-c",
			"printf probe | socat -t 3 - UDP4:"+gwAddr+":"+gwPort,
		)
		if err != nil {
			lastBody = err.Error()
			continue
		}
		lastBody = strings.TrimSpace(body)
		if strings.Contains(body, marker) {
			successCount++
		}
	}
	probeOK := successCount == probes
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: fmt.Sprintf("%d/%d UDP probes carried the marker reply", probes, probes),
		OK:          probeOK,
		Got:         fmt.Sprintf("%d/%d (lastBody=%s)", successCount, probes, oneLine(lastBody, 120)),
	})
	return finalize(res)
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-udp-lb",
		"--ignore-not-found")
	return nil
}

func finalize(res scenarios.Result) scenarios.Result {
	if res.AllPassed() {
		res.Status = "ok"
		res.Summary = "L4Route UDP LB reconciled; 5/5 datagrams echoed via Gateway"
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
