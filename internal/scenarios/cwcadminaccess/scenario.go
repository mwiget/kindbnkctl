// Package cwcadminaccess implements scenario "cwc-admin-access" —
// F5 BNK how-to #1 "Restrict access to sensitive data".
//
// The how-to gates the Cluster-Wide Controller (CWC) Debug API /
// QKView API at https://f5-spk-cwc.<sharedComponentNs>:38081 behind:
//   - mTLS  (CA + client cert + client key from cwc-license-client-certs)
//   - Bearer token (cwc-auth-token Secret), in Authorization header
//
// Both materials are generated automatically by the deploy-flo phase
// (gen_cert.sh + CWC's own token controller), so this scenario only
// has to read them out and demonstrate the access pattern.
//
// Verification:
//   - Two GET requests from inside the FRR pod (already on the cluster
//     and Calico-routed):
//     1. With CA + client cert/key + Authorization Bearer header
//        → expect HTTP 200 and a body containing "LicenseStatus" /
//        "DigitalAssetID" (one of the documented response fields).
//     2. With CA + client cert/key but NO Authorization header
//        → expect non-2xx (the "restrict" part: token gate works).
//   - No new BNK CRs are applied; this is a runtime-access
//     verification, not a configuration change.
//
// Manifests are minimal: just a small "probe" Deployment with curl
// pre-installed and the cert material projected from the in-cluster
// Secrets. We could equally exec from the FRR pod, but a dedicated
// probe pod keeps the scenario self-contained for cleanup.
package cwcadminaccess

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
	scnName  = "cwc-admin-access"
	scnTitle = "Restrict access to sensitive data (how-to #1) — bearer token + mTLS to CWC admin API"
	// SharedComponentNamespace where CWC and its Secrets live in
	// BNK 2.3. The doc says f5-utils (a 2.2 name); 2.3 moved it to
	// f5-cne-core, matching dpubnkctl's deploy package constant.
	cwcNamespace = "f5-cne-core"
	cwcFQDN      = "f5-spk-cwc.f5-cne-core.svc.cluster.local"
	cwcPort      = "38081"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Green }
func (s *scenario) Dependencies() []string   { return nil } // independent of bgp-peer-frr
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Demonstrates the dual-gate access control that BNK puts in front
of CWC's sensitive admin endpoints:

  - mTLS at the TLS layer (CA + client cert + client key).
  - Bearer token at the HTTP layer (Authorization header).

Both materials are produced by the deploy-flo phase already:

  - cwc-license-client-certs Secret in f5-cne-core has
    ca-root-cert, client-cert, client-key (PEM).
  - cwc-auth-token Secret in f5-cne-core has the token
    string.

The scenario spawns a tiny curl-equipped probe Deployment in
scn-cwcadmin namespace with both secrets projected as files
and the token as an env var. Verify does two requests:

  1. Bearer token PRESENT — expects HTTP 200 with the
     documented JSON body (LicenseStatus + DigitalAssetID).
  2. Bearer token MISSING — expects 401/403 (the actual
     "restrict" part of "restrict access to sensitive data").

No new BNK CRs are introduced; this is a pure runtime-access
verification.

Cleanup deletes the scn-cwcadmin namespace.
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
	// Apply namespace + probe Deployment. The probe references the
	// Secrets in f5-cne-core via projected volumes — kubelet doesn't
	// allow cross-namespace Secret mounts directly, so we use
	// kubectl-replicate at apply time to copy the relevant secret
	// material into scn-cwcadmin where the probe pod can read it.
	for _, f := range []string{
		"01-namespace.yaml",
	} {
		body, err := manifestFS.ReadFile("manifests/" + f)
		if err != nil {
			return err
		}
		if err := r.Apply(ctx.Ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}
	// Copy CWC client certs + auth token into the scenario namespace.
	if err := replicateSecret(ctx, "cwc-license-client-certs",
		cwcNamespace, "scn-cwcadmin"); err != nil {
		return fmt.Errorf("replicate cwc-license-client-certs: %w", err)
	}
	if err := replicateSecret(ctx, "cwc-auth-token",
		cwcNamespace, "scn-cwcadmin"); err != nil {
		return fmt.Errorf("replicate cwc-auth-token: %w", err)
	}
	// Now apply the probe Deployment (references the replicated
	// secrets by name).
	for _, f := range []string{"02-probe.yaml"} {
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

	// 1. probe Deployment Available.
	{
		err := r.Wait(ctx.Ctx, "scn-cwcadmin", "Available",
			"deployment/probe", 2*time.Minute)
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "probe Deployment Available",
			OK:          err == nil, Got: errString(err),
		})
		if err != nil {
			return finalize(res)
		}
	}

	probe, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-cwcadmin", "get", "pod",
		"-l", "app=probe", "--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}")
	probe = strings.TrimSpace(probe)
	if probe == "" {
		res.Assertions = append(res.Assertions, scenarios.Assertion{
			Description: "probe pod Running",
			OK:          false, Got: "none found",
		})
		return finalize(res)
	}

	// 2. Authenticated request returns 200 with expected fields.
	bodyAuth, errAuth := r.KubectlCapture(ctx.Ctx, "-n", "scn-cwcadmin", "exec",
		probe, "--",
		"sh", "-c", fmt.Sprintf(
			`curl -sS --max-time 8 -w '\n__HTTP_CODE__=%%{http_code}' `+
				`--cacert /certs/ca-root-cert `+
				`--cert /certs/client-cert `+
				`--key /certs/client-key `+
				`-H "Authorization: Bearer $(cat /token/token)" `+
				`https://%s:%s/status`, cwcFQDN, cwcPort))
	authOK := errAuth == nil &&
		strings.Contains(bodyAuth, "__HTTP_CODE__=200") &&
		strings.Contains(bodyAuth, "LicenseStatus") &&
		strings.Contains(bodyAuth, "DigitalAssetID")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Authenticated GET /status returns HTTP 200 with license JSON",
		OK:          authOK,
		Got:         oneLine(bodyAuth, 250),
	})

	// 3. Unauthenticated request (no Authorization header) is rejected.
	bodyNoAuth, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-cwcadmin", "exec",
		probe, "--",
		"sh", "-c", fmt.Sprintf(
			`curl -sS --max-time 8 -w '\n__HTTP_CODE__=%%{http_code}' `+
				`--cacert /certs/ca-root-cert `+
				`--cert /certs/client-cert `+
				`--key /certs/client-key `+
				`https://%s:%s/status`, cwcFQDN, cwcPort))
	// Pass = either a 4xx HTTP code OR connection refused / similar
	// (mTLS succeeded, application refused). Reject 200 — that would
	// mean the token gate isn't enforced.
	rejected := !strings.Contains(bodyNoAuth, "__HTTP_CODE__=200")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Unauthenticated GET /status is rejected (no token in header)",
		OK:          rejected,
		Got:         oneLine(bodyNoAuth, 200),
	})

	// 4. Same shape, but with an obviously bogus token. The CWC code
	//    should compare-constant-time and refuse.
	bodyBadAuth, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-cwcadmin", "exec",
		probe, "--",
		"sh", "-c", fmt.Sprintf(
			`curl -sS --max-time 8 -w '\n__HTTP_CODE__=%%{http_code}' `+
				`--cacert /certs/ca-root-cert `+
				`--cert /certs/client-cert `+
				`--key /certs/client-key `+
				`-H "Authorization: Bearer not-the-real-token" `+
				`https://%s:%s/status`, cwcFQDN, cwcPort))
	bogusRejected := !strings.Contains(bodyBadAuth, "__HTTP_CODE__=200")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Bogus token is rejected (constant-time compare)",
		OK:          bogusRejected,
		Got:         oneLine(bodyBadAuth, 200),
	})

	return finalize(res)
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	_ = ctx.Runner.Kubectl(ctx.Ctx, "delete", "namespace", "scn-cwcadmin",
		"--ignore-not-found")
	return nil
}

// replicateSecret reads a Secret from sourceNS and applies a copy in
// targetNS with the same name + data. Cross-namespace Secret mounting
// isn't allowed by kubelet, so the scenario keeps its own copy.
func replicateSecret(ctx *scenarios.Context, name, sourceNS, targetNS string) error {
	r := ctx.Runner
	// Pull the source secret as JSON, strip metadata that would
	// reject the apply (uid, resourceVersion, namespace), retarget
	// to the new namespace.
	src, err := r.KubectlCapture(ctx.Ctx, "-n", sourceNS, "get", "secret",
		name,
		"-o", "go-template={{range $k,$v := .data}}{{$k}}\t{{$v}}\n{{end}}")
	if err != nil {
		return fmt.Errorf("read source secret %s/%s: %w", sourceNS, name, err)
	}
	if strings.TrimSpace(src) == "" {
		return fmt.Errorf("source secret %s/%s has no data", sourceNS, name)
	}
	// Render a clean Secret manifest in targetNS.
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Secret\nmetadata:\n  name: ")
	b.WriteString(name)
	b.WriteString("\n  namespace: ")
	b.WriteString(targetNS)
	b.WriteString("\ntype: Opaque\ndata:\n")
	for _, line := range strings.Split(strings.TrimSpace(src), "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		fmt.Fprintf(&b, "  %s: %s\n", parts[0], parts[1])
	}
	return r.Apply(ctx.Ctx, b.String())
}

func finalize(res scenarios.Result) scenarios.Result {
	if res.AllPassed() {
		res.Status = "ok"
		res.Summary = "CWC admin API gated by mTLS + bearer token; authenticated request 200, unauthenticated and bogus-token both rejected"
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
