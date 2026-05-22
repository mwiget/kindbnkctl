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
	scnTitle = "gRPC routing via GRPCRoute CRD — moul/grpcbin backend with reflection"

	gwAddr = "203.0.113.108"
	gwPort = "50051"

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
func (s *scenario) Rating() scenarios.Rating { return scenarios.Amber }
func (s *scenario) Dependencies() []string   { return []string{"bgp-peer-frr"} }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Exercises BNK's GRPCRoute CRD against a moul/grpcbin backend.

The Gateway has an HTTP listener on port 50051 (per F5 BNK docs,
gRPC is carried over HTTP/HTTPS listeners). The GRPCRoute forwards
every method to the backend without filters or hostnames — the
BNK docs note that hostnames, matches, filters, and multi-rule
GRPCRoutes are not yet supported.

Status as of BNK 2.3.0 in kindbnkctl's demo-TMM shape (🟡):
  - GRPCRoute control plane reconciles cleanly. Gateway reaches
    Programmed=True, GRPCRoute reaches Accepted=True, TMM emits
    audit entries for cl-profile-http2 + srv-profile-http2 +
    profile-http + profile-httprouter + profile-json profile
    chain, and the pool member is marked Up.
  - The pinned grpcurl binary (v1.9.3, SHA-256 verified) installs
    into the FRR pod over the Multus NAD with a BGP-learned route
    to the Gateway IP.
  - Direct grpcurl-to-backend Service via cluster DNS works (lists
    addsvc.Add, grpcbin.GRPCBin, ServerReflection, hello.HelloService).
  - grpcurl through the Gateway returns
    "rpc error: code = Internal desc = stream terminated by
    RST_STREAM with error code: INTERNAL_ERROR". Investigation
    confirmed that TMM's FLO controller unconditionally applies
    profile-http + profile-json + profile-httprouter to all
    listener types (HTTP and HTTPS), which corrupts HTTP/2 binary
    frames regardless of TLS termination. Setting appProtocol
    kubernetes.io/h2c on the backend Service has no effect on the
    client-side profile chain. This is a BNK 2.3.0 FLO limitation.
    Likely needs a "raw HTTP/2 passthrough" mode for GRPCRoute
    listeners, or a BNK profile-override path not yet exposed via
    the Gateway API CRDs.

The scenario therefore asserts the control-plane state only and
attempts the grpcurl call as informational diagnostics so the
report shows the RST_STREAM verbatim.

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

	return finalize(res)
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-grpc",
		"--ignore-not-found")
	return nil
}

func finalize(res scenarios.Result) scenarios.Result {
	if res.AllPassed() {
		res.Status = "ok"
		res.Summary = "GRPCRoute control plane reconciled; direct-to-backend gRPC works; Gateway-path RST_STREAM surfaced as informational (see Description)"
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
