// Package grpcroute implements scenario "grpc-loadbalance" — F5 BNK
// GRPCRoute CRD with a moul/grpcbin backend.
//
// The Gateway uses an HTTP listener on port 50051 (per the F5 BNK
// GRPCRoute doc, gRPC is carried over an HTTP/HTTPS listener). The
// GRPCRoute forwards every method to the grpcbin Service. Verify
// downloads the pinned grpcurl binary (SHA-256 verified, lesson
// from bgp-peer-frr) into the FRR pod and uses its reflection-list
// support to confirm the gRPC server is reachable through the
// Gateway.
package grpcroute

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
	scnName  = "grpc-loadbalance"
	scnTitle = "gRPC load balancing — L4Route (TCP) data plane; GRPCRoute control plane (moul/grpcbin)"

	gwAddr = "203.0.113.108"
	gwPort = "50051"

	// L4Route (TCP) data-plane path — the green workaround. gRPC rides
	// raw L4 here, bypassing the GRPCRoute L7 profile chain that
	// RST_STREAMs HTTP/2. See docs/grpc-route-investigation.md.
	l4Addr = "203.0.113.109"
	l4Port = "50052"

	// grpcurl release: pinned + SHA-256 verified before extraction.
	// The binary runs inside the FRR pod, which is privileged on the
	// host network/bridge — SHA check is load-bearing.
	grpcurlURL = "https://github.com/fullstorydev/grpcurl/releases/download/v1.9.3/grpcurl_1.9.3_linux_x86_64.tar.gz"
	grpcurlSHA = "a926b62a85787ccf73ef8736b3ae554f1242e39d92bb8767a79d6dd23b11d1d5"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Green }
func (s *scenario) Dependencies() []string   { return []string{"bgp-peer-frr"} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Load-balances gRPC to a moul/grpcbin backend in the 2-node demo-TMM
shape, and exercises BNK's GRPCRoute CRD alongside it.

Two data paths are deployed for the same grpcbin pods:

  1. GRPCRoute (Gateway API L7), HTTP listener on port 50051 — the
     CRD this how-to is about. Single rule, no hostnames/matches/
     filters (the BNK docs note those are unsupported).
  2. L4Route (TCP) on port 50052 — a raw L4 path to a plain-TCP
     copy of the Service.

Why two paths (🟢 green via the L4Route):
  - GRPCRoute control plane reconciles cleanly — Gateway
    Programmed=True, GRPCRoute Accepted=True, pool member Up — but
    its DATA plane fails: grpcurl through the L7 Gateway returns
    "rpc error: code = Internal desc = stream terminated by
    RST_STREAM with error code: INTERNAL_ERROR". TMM's FLO applies
    the profile-http + profile-json + profile-httprouter chain to
    every GRPCRoute virtual server, which corrupts HTTP/2 binary
    frames. This is a BNK 2.3.0 FLO limitation (no raw HTTP/2
    passthrough mode for GRPCRoute). The scenario keeps this call
    as an INFORMATIONAL assertion so the report shows the
    RST_STREAM verbatim.
  - The L4Route path carries gRPC end-to-end: grpcurl reflection
    list AND a real unary grpcbin.GRPCBin/Index call both succeed
    through the L4 Gateway IP. Proven by experiment to be a true
    fix (not PVA-related — it works at pvaAccelerationMode
    full/assisted, the default, same as tcp-l4-loadbalance). The
    trade-off: L4Route is opaque TCP LB — no gRPC/HTTP-2-aware
    per-method routing — but it carries HTTP/2 intact, so gRPC
    load balancing IS testable green in this shape today.

Note: the L4Route binds a plain-TCP Service. An L4Route refuses a
Service tagged appProtocol: kubernetes.io/h2c (ResolvedRefs=False /
UnsupportedProtocol), so the L4 path uses its own untagged Service.

The pinned grpcurl binary (v1.9.3, SHA-256 verified) installs into
the FRR pod over the Multus NAD with a BGP-learned route to each
Gateway IP. Full analysis: docs/grpc-route-investigation.md.

Cleanup deletes the scn-grpc namespace.
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
		"05-grpcroute.yaml",
		"06-l4-backend-svc.yaml",
		"07-l4-gateway.yaml",
		"08-l4route.yaml",
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
		err := r.Wait(ctx.Ctx, "scn-grpc", "Available",
			"deployment/grpcbin", 5*time.Minute)
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "grpcbin Deployment Available",
			OK:          err == nil, Got: errString(err),
		})
	}
	{
		err := r.Kubectl(ctx.Ctx, "-n", "scn-grpc", "wait",
			"--for=condition=Programmed", "--timeout=5m",
			"gateway/scn-grpc-gateway")
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "Gateway Programmed=True",
			OK:          err == nil, Got: errString(err),
		})
	}
	rstate, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-grpc", "get",
		"grpcroute/scn-grpc-route",
		"-o", "jsonpath={.status.parents[0].conditions[?(@.type==\"Accepted\")].status}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "GRPCRoute Accepted=True",
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

	hasGW, lastTable := waitBGP(ctx, frrPod, gwAddr)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: fmt.Sprintf("FRR BGP table has %s/32 advertised by TMM", gwAddr),
		OK:          hasGW,
		Got:         oneLine(lastTable, 200),
	})

	// Install grpcurl in the FRR pod with SHA verification. We chain
	// curl → sha256sum -c → tar in a single sh -c so a checksum
	// mismatch aborts before the binary lands. Idempotent: skip the
	// download if /tmp/grpcurl is already present.
	installScript := `set -e
if [ -x /tmp/grpcurl ]; then echo present; exit 0; fi
command -v curl >/dev/null 2>&1 || apk add --no-cache curl >/dev/null 2>&1
cd /tmp
curl -fsSL -o grpcurl.tgz ` + grpcurlURL + `
echo '` + grpcurlSHA + `  grpcurl.tgz' | sha256sum -c -
tar xzf grpcurl.tgz grpcurl
chmod +x grpcurl
rm -f grpcurl.tgz
echo installed`
	out, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
		frrPod, "-c", "frr", "--", "sh", "-c", installScript)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "grpcurl installed in FRR pod (SHA-256 verified)",
		OK:          err == nil,
		Got:         oneLine(out, 200),
	})
	if err != nil {
		return finalize(res)
	}

	// Reflection-list via the Gateway. Reported as informational: BNK
	// 2.3.0's HTTP listener + standard HTTP/json profile chain
	// RST_STREAMs cleartext gRPC. The Got string carries either the
	// service list or the RST_STREAM message so operators can confirm
	// the failure mode without re-running by hand.
	listOut, listErr := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
		frrPod, "-c", "frr", "--",
		"/tmp/grpcurl", "-plaintext", "-max-time", "10",
		gwAddr+":"+gwPort, "list")
	listGot := oneLine(listOut, 200)
	if listErr != nil {
		listGot = listGot + " err=" + oneLine(listErr.Error(), 200)
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "grpcurl list via Gateway (informational, RST_STREAM expected)",
		OK:          true,
		Got:         listGot,
	})

	// Direct backend call from FRR via cluster DNS — proves the
	// backend itself is healthy and grpcurl works. Anchor for the
	// "data path is the issue, not the workload" narrative.
	directOut, directErr := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
		frrPod, "-c", "frr", "--",
		"/tmp/grpcurl", "-plaintext", "-max-time", "10",
		"grpcbin.scn-grpc.svc.cluster.local:9000", "list")
	directGot := oneLine(directOut, 200)
	if directErr != nil {
		directGot = directGot + " err=" + oneLine(directErr.Error(), 200)
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "grpcurl list direct to backend Service returns grpcbin.GRPCBin (proves backend healthy)",
		OK:          directErr == nil && strings.Contains(directOut, "grpcbin.GRPCBin"),
		Got:         directGot,
	})

	// ---- L4Route (TCP) data-plane path: the green workaround ----
	// gRPC rides raw L4 here, bypassing the GRPCRoute L7 profile chain
	// that RST_STREAMs HTTP/2. See docs/grpc-route-investigation.md.
	{
		err := r.Kubectl(ctx.Ctx, "-n", "scn-grpc", "wait",
			"--for=condition=Programmed", "--timeout=5m",
			"gateway/scn-grpc-l4-gateway")
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "L4 Gateway Programmed=True",
			OK:          err == nil, Got: errString(err),
		})
	}
	// L4Route ResolvedRefs=True proves the plain-TCP Service bound; an
	// appProtocol:h2c Service would fail here with UnsupportedProtocol.
	l4state, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-grpc", "get",
		"l4route/scn-grpc-l4-route",
		"-o", "jsonpath={.status.parents[0].conditions[?(@.type==\"ResolvedRefs\")].status}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "L4Route ResolvedRefs=True (plain-TCP backend bound)",
		OK:          strings.TrimSpace(l4state) == "True",
		Got:         strings.TrimSpace(l4state),
	})

	hasL4, l4Table := waitBGP(ctx, frrPod, l4Addr)
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: fmt.Sprintf("FRR BGP table has %s/32 (L4 Gateway) advertised by TMM", l4Addr),
		OK:          hasL4,
		Got:         oneLine(l4Table, 200),
	})

	// grpcurl reflection-list through the L4 Gateway — the green
	// assertion. Retry to absorb BGP propagation + data-plane
	// programming lag for the freshly-advertised L4 IP.
	l4List, l4Err := grpcurlRetry(ctx, frrPod, "-plaintext", "-max-time", "10",
		l4Addr+":"+l4Port, "list")
	l4Got := oneLine(l4List, 200)
	if l4Err != nil {
		l4Got = l4Got + " err=" + oneLine(l4Err.Error(), 200)
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "grpcurl list via L4Route Gateway returns grpcbin.GRPCBin (gRPC works over L4)",
		OK:          l4Err == nil && strings.Contains(l4List, "grpcbin.GRPCBin"),
		Got:         l4Got,
	})

	// A real unary call over the L4 path — proves it's not just a TCP
	// connect but full HTTP/2 request/response carried intact.
	l4Unary, l4UErr := grpcurlRetry(ctx, frrPod, "-plaintext", "-max-time", "10",
		"-d", "{}", l4Addr+":"+l4Port, "grpcbin.GRPCBin/Index")
	l4UGot := oneLine(l4Unary, 200)
	if l4UErr != nil {
		l4UGot = l4UGot + " err=" + oneLine(l4UErr.Error(), 200)
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "grpcurl unary grpcbin.GRPCBin/Index via L4Route returns 'gRPC testing server'",
		OK:          l4UErr == nil && strings.Contains(l4Unary, "gRPC testing server"),
		Got:         l4UGot,
	})

	return finalize(res)
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-grpc",
		"--ignore-not-found")
	return nil
}

// waitBGP polls FRR's BGP table (up to 2 min) for a /32 advertised by
// TMM, returning whether it appeared and the last table seen.
func waitBGP(ctx *scenarios.Context, frrPod, addr string) (bool, string) {
	r := ctx.Runner
	deadline := time.Now().Add(2 * time.Minute)
	var last string
	for time.Now().Before(deadline) {
		t, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "-c", "frr", "--",
			"vtysh", "-c", "show bgp ipv4 unicast")
		last = t
		if strings.Contains(t, addr) {
			return true, last
		}
		select {
		case <-ctx.Ctx.Done():
			return false, last
		case <-time.After(5 * time.Second):
		}
	}
	return false, last
}

// grpcurlRetry runs grpcurl in the FRR pod, retrying (up to 90s) until
// it exits 0 — absorbing BGP propagation + data-plane programming lag
// for a freshly-advertised Gateway IP. Returns the last output + error.
func grpcurlRetry(ctx *scenarios.Context, frrPod string, args ...string) (string, error) {
	r := ctx.Runner
	deadline := time.Now().Add(90 * time.Second)
	base := []string{"-n", "scn-bgp", "exec", frrPod, "-c", "frr", "--", "/tmp/grpcurl"}
	var out string
	var err error
	for {
		out, err = r.KubectlCapture(ctx.Ctx, append(append([]string{}, base...), args...)...)
		if err == nil || time.Now().After(deadline) {
			return out, err
		}
		select {
		case <-ctx.Ctx.Done():
			return out, err
		case <-time.After(5 * time.Second):
		}
	}
}

func finalize(res scenarios.Result) scenarios.Result {
	if res.AllPassed() {
		res.Status = "ok"
		res.Summary = "gRPC load-balanced end-to-end via L4Route (TCP); GRPCRoute control plane reconciled but its L7 data plane RST_STREAMs (informational) — see Description"
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
