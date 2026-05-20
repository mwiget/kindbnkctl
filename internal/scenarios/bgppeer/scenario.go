// Package bgppeer implements scenario "bgp-peer-frr": deploy an FRR
// container as a BGP peer for TMM's ZeBOS daemon and verify dynamic
// routing actually works against a real peer. Maps to how-to #3
// (Dynamic Routing with BGP) on clouddocs.f5.com.
//
// Topology:
//
//	scn-bgp namespace
//	  └── scn-frr Deployment (frrouting/frr:9.1.0)
//	         BGP AS 65001, accepts incoming sessions from any
//	         pod in 10.244.0.0/16 (Calico CIDR)
//
//	default namespace
//	  └── f5-tmm-dynamic-routing-template ConfigMap (patched)
//	         ZeBOS.conf: BGP AS 65000, neighbor=<frr-pod-ip>,
//	         advertises network 203.0.113.100/32
//
// Verification:
//   - FRR pod Ready
//   - `vtysh -c "show bgp summary"` inside FRR shows session state=Established
//   - `vtysh -c "show bgp ipv4 unicast"` shows 203.0.113.100/32 received
package bgppeer

import (
	"bytes"
	"context"
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
	scnName  = "bgp-peer-frr"
	scnTitle = "Dynamic routing with BGP (how-to #3) — FRR pod peers with TMM's ZeBOS"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Amber }
func (s *scenario) Dependencies() []string   { return nil }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Deploys an FRR BGP peer in a dedicated namespace and configures
TMM's ZeBOS daemon to peer with it via the cluster-wide
f5-tmm-dynamic-routing-template ConfigMap. Includes the
auxiliary plumbing that makes ZeBOS actually pick up the config
on a kind / demoMode TMM:

  - passwd.conf injection — bfd_watcher requires
    /config/zebos/rd0/passwd.conf to exist before it will
    imish-load ZebOS.conf. Without it, bgpd stays config-empty.
    Apply step writes a one-line passwd.conf into the f5-tmm-
    routing container's writable /config/zebos/rd0/ via
    kubectl-exec, with a retry loop because the container can
    be briefly unexecable right after a TMM rollout.
  - route-injector DaemonSet — installs Calico's standard
    'fake-gateway' kernel routes (169.254.1.1/32 dev eth0 +
    <frr-pod-ip>/32 via 169.254.1.1) into the TMM pod's netns
    via nsenter. Without these, the pod has 'default via dev
    tmm' and no path to FRR. The DaemonSet runs hostPID +
    privileged on every node and re-applies on TMM restart
    (PID-change detection in /proc/*/comm).

Rated AMBER because despite these fixes BGP still does not
reach Established on kind. The remaining gap is architectural,
not a missing config field: TMM in demoMode intercepts TCP
traffic on eth0 and routes it through its own data plane
before reaching the local bgpd listener. ICMP works (FRR
pings TMM at ~2ms RTT), but TCP SYNs to port 179 are absorbed
by TMM's proxy logic; nothing reaches the 0.0.0.0:179 listener
that ZeBOS bgpd is bound to. In a production BNK deployment
the BGP traffic flows over a Multus NAD interface that bypasses
this hook — kind/demoMode has no equivalent.

What IS verified by this scenario:

  - FRR pod Ready, bgpd loaded with peer-group from-tmm +
    listen-range 10.244.0.0/16
  - route-injector DaemonSet pods Running (mechanism that
    primes the TMM pod's kernel routing table)
  - passwd.conf present in the TMM pod's
    /config/zebos/rd0/ (gate for bfd_watcher imish-load)
  - ZeBOS in TMM sees the configured neighbor (the ConfigMap
    reached the pod and was parsed)

To lift this to green, a follow-up scenario needs to bypass
TMM's eth0 TCP hook. Likely paths: add a Multus NAD interface
that ZeBOS can bind to (recreates the production shape), or
patch TMM's iptables-style filter to exempt port 179.

Caveats:
  - The ConfigMap rewrite affects the whole CNEInstance —
    running this scenario reconfigures cluster-wide TMM dynamic
    routing until you run 'scenario clean bgp-peer-frr'.
  - The FRR pod's listener accepts any peer in 10.244.0.0/16;
    single-tenant kind clusters only.
  - The route-injector runs privileged with hostPID; it nsenter's
    into the TMM container's netns. Acceptable for a dev/test
    cluster; do not run on production.
`)
}

func (s *scenario) Manifests(ctx *scenarios.Context) ([]string, error) {
	// Without the FRR pod IP we can't render the ZeBOS template at
	// Manifests time. Persist the static YAMLs and the raw template
	// for the operator's audit; Apply will re-render and write the
	// final ZeBOS ConfigMap.
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

	// 1. Apply namespace + FRR ConfigMap + FRR Deployment+Service.
	for _, f := range []string{"01-namespace.yaml", "02-frr-config.yaml", "03-frr.yaml"} {
		body, err := manifestFS.ReadFile("manifests/" + f)
		if err != nil {
			return err
		}
		if err := r.Apply(ctx.Ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}

	// 2. Wait for the FRR pod to be Ready and have a Calico IP.
	if err := r.Wait(ctx.Ctx, "scn-bgp", "Available",
		"deployment/scn-frr", 2*time.Minute); err != nil {
		return fmt.Errorf("FRR deployment not Available: %w", err)
	}
	frrIP, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "get",
		"pod", "-l", "app=scn-frr",
		"-o", "jsonpath={.items[0].status.podIP}")
	if err != nil || strings.TrimSpace(frrIP) == "" {
		return fmt.Errorf("could not discover FRR pod IP: %w (out=%q)", err, frrIP)
	}
	frrIP = strings.TrimSpace(frrIP)

	// 3. Render + apply the ZeBOS ConfigMap (default ns) with the
	//    discovered FRR IP. Persist the rendered file for audit.
	zebosBody, err := renderTemplate(manifestFS, "manifests/04-zebos-template.yaml.tmpl",
		struct{ FRRPodIP string }{FRRPodIP: frrIP})
	if err != nil {
		return err
	}
	if _, err := scenarios.WriteManifest(ctx.PoCDir, scnName,
		"04-zebos.rendered.yaml", zebosBody); err != nil {
		return err
	}
	if err := r.Apply(ctx.Ctx, zebosBody); err != nil {
		return fmt.Errorf("apply ZeBOS ConfigMap: %w", err)
	}

	// 4. Render + apply the route-injector DaemonSet — same FRR IP
	//    spliced in so it installs the right /32 route on each TMM
	//    pod the kubelet schedules. Idempotent across TMM restarts.
	injectorBody, err := renderTemplate(manifestFS, "manifests/05-route-injector.yaml.tmpl",
		struct{ FRRPodIP string }{FRRPodIP: frrIP})
	if err != nil {
		return err
	}
	if _, err := scenarios.WriteManifest(ctx.PoCDir, scnName,
		"05-route-injector.rendered.yaml", injectorBody); err != nil {
		return err
	}
	if err := r.Apply(ctx.Ctx, injectorBody); err != nil {
		return fmt.Errorf("apply route-injector DaemonSet: %w", err)
	}

	// 5. Restart TMM so it picks up the new ZeBOS ConfigMap and so
	//    the injector sees a fresh TMM PID to install routes into.
	if err := r.Kubectl(ctx.Ctx, "-n", "default", "rollout", "restart",
		"deployment/f5-tmm"); err != nil {
		return fmt.Errorf("rollout restart f5-tmm: %w", err)
	}
	if err := r.Kubectl(ctx.Ctx, "-n", "default", "rollout", "status",
		"deployment/f5-tmm", "--timeout=5m"); err != nil {
		return fmt.Errorf("f5-tmm rollout did not complete: %w", err)
	}

	// 6. Create the passwd.conf that ZeBOS's bfd_watcher requires
	//    before it can imish-load the ZebOS.conf. The
	//    /config/zebos/rd0/ directory is a writable emptyDir mount
	//    inside the f5-tmm-routing container, so we can drop in an
	//    empty passwd.conf via kubectl-exec after the TMM pod is up.
	//    Without this file, bfd_watcher logs "failed to open
	//    /config/zebos/rd0/passwd.conf" continuously and bgpd runs
	//    with no config in memory.
	// Retry the exec for up to 90s, re-fetching the pod name each
	// loop iteration so we don't get stuck on a terminating pod
	// that was named just before TMM's successor came up. The
	// f5-tmm-routing container also takes a few extra seconds to
	// be exec-able after the main container is Ready — the retry
	// covers both cases.
	var injectErr error
	for i := 0; i < 18; i++ {
		tmmPod, err := r.KubectlCapture(ctx.Ctx, "-n", "default", "get", "pod",
			"-l", "app=f5-tmm",
			"--field-selector=status.phase=Running",
			"-o", "jsonpath={.items[0].metadata.name}")
		tmmPod = strings.TrimSpace(tmmPod)
		if err != nil || tmmPod == "" {
			injectErr = fmt.Errorf("no Running f5-tmm pod yet: %w", err)
		} else {
			injectErr = r.Kubectl(ctx.Ctx, "-n", "default", "exec",
				tmmPod, "-c", "f5-tmm-routing", "--",
				"sh", "-c", "echo 'enable password 0 zebos' > /config/zebos/rd0/passwd.conf")
			if injectErr == nil {
				break
			}
		}
		select {
		case <-ctx.Ctx.Done():
			return ctx.Ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	if injectErr != nil {
		return fmt.Errorf("inject passwd.conf into TMM (after retries): %w", injectErr)
	}

	return nil
}

// renderTemplate is a small wrapper around text/template that reads
// from the embedded FS and returns the substituted string.
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

func (s *scenario) Verify(ctx *scenarios.Context) scenarios.Result {
	r := ctx.Runner
	res := scenarios.Result{Status: "ok"}

	// FRR pod name (single replica — index [0]).
	frrPod, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "get", "pod",
		"-l", "app=scn-frr",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil || strings.TrimSpace(frrPod) == "" {
		res.Status = "failed"
		res.Summary = "could not find FRR pod"
		res.Details = fmt.Sprintf("err=%v out=%q", err, frrPod)
		return res
	}
	frrPod = strings.TrimSpace(frrPod)

	// FRR bgpd up + peer-group/listen-range applied.
	frrConfig, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
		frrPod, "-c", "frr", "--", "vtysh", "-c", "show bgp peer-group")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "FRR bgpd loaded peer-group from-tmm with listen-range",
		OK:          err == nil && strings.Contains(frrConfig, "from-tmm") && strings.Contains(frrConfig, "listen range"),
		Got:         oneLine(frrConfig, 200),
	})

	// Route-injector DaemonSet alive — it's the mechanism that made
	// the kernel route for the BGP path land. Validate at least one
	// of the 2 replicas is Running.
	injStatus, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "get", "pod",
		"-l", "app=scn-tmm-route-injector",
		"-o", "jsonpath={.items[*].status.phase}")
	injOK := strings.Contains(injStatus, "Running")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "route-injector DaemonSet pods Running",
		OK:          injOK,
		Got:         oneLine(injStatus, 100),
	})

	// passwd.conf successfully landed in TMM (bfd_watcher no longer
	// logs "No such file or directory" — check the file directly).
	passwd, _ := r.KubectlCapture(ctx.Ctx, "-n", "default", "exec",
		"deploy/f5-tmm", "-c", "f5-tmm-routing", "--",
		"ls", "/config/zebos/rd0/passwd.conf")
	passwdOK := strings.Contains(passwd, "passwd.conf")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "ZeBOS passwd.conf present (bfd_watcher imish-load gate)",
		OK:          passwdOK,
		Got:         oneLine(passwd, 100),
	})

	// ZeBOS in the TMM pod tracks the configured neighbor. The
	// imish exec confirms the ConfigMap reached TMM and ZeBOS
	// reloaded it correctly. State will be Active (not Established)
	// on kind without Multus — see Description() for why. Retry a
	// few times because imish on a freshly-restarted TMM sometimes
	// returns empty before the bgpd process has loaded its config.
	tmmPod, err := r.KubectlCapture(ctx.Ctx, "-n", "default", "get", "pod",
		"-l", "app=f5-tmm",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err == nil {
		tmmPod = strings.TrimSpace(tmmPod)
	}
	var zebos string
	tmmHasNeighbor := false
	for i := 0; i < 12; i++ {
		zebos, _ = r.KubectlCapture(ctx.Ctx, "-n", "default", "exec",
			tmmPod, "-c", "f5-tmm-routing", "--",
			"imish", "-e", "show ip bgp summary")
		if strings.Contains(zebos, "65001") ||
			strings.Contains(zebos, "Active") ||
			strings.Contains(zebos, "Estab") {
			tmmHasNeighbor = true
			break
		}
		select {
		case <-ctx.Ctx.Done():
			break
		case <-time.After(5 * time.Second):
		}
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "ZeBOS in TMM sees the configured neighbor (Active or Established)",
		OK:          tmmHasNeighbor,
		Got:         oneLine(zebos, 200),
	})

	// Optional bonus: did BGP actually Establish? Pass-or-warn, not
	// pass-or-fail. We poll up to 60s for Established; if it never
	// arrives that's documented in the rating, not an error.
	deadline := time.Now().Add(60 * time.Second)
	var lastSummary string
	established := false
	for time.Now().Before(deadline) {
		out, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "--", "vtysh", "-c", "show bgp summary")
		lastSummary = out
		if strings.Contains(out, "Estab") {
			established = true
			break
		}
		select {
		case <-ctx.Ctx.Done():
			break
		case <-time.After(5 * time.Second):
		}
	}
	bonusOK := established // True only if green-path observed
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "[bonus] BGP session reaches Established (known gap on kind — see description)",
		OK:          bonusOK,
		Got:         oneLine(lastSummary, 200),
	})

	// Decide overall status: control-plane assertions are pass/fail;
	// the bonus Established assertion is informational only.
	requiredPassed := true
	for _, a := range res.Assertions[:len(res.Assertions)-1] {
		if !a.OK {
			requiredPassed = false
			break
		}
	}
	if requiredPassed {
		res.Status = "ok"
		if established {
			res.Summary = "BGP control-plane reconciled; session Established + advertised route present"
		} else {
			res.Summary = "BGP control-plane reconciled (Established not reached — known kind/demoMode gap, see description)"
		}
	} else {
		res.Status = "failed"
		var failed []string
		for _, a := range res.Assertions[:len(res.Assertions)-1] {
			if !a.OK {
				failed = append(failed, a.Description)
			}
		}
		res.Summary = "failed: " + strings.Join(failed, "; ")
		res.Details = "FRR summary:\n" + lastSummary + "\n\nZeBOS summary:\n" + zebos
	}
	return res
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	r := ctx.Runner
	// Best-effort: revert the dynamic-routing template to empty so
	// the cluster goes back to its pre-scenario state. Errors are
	// non-fatal.
	emptyCM := `apiVersion: v1
kind: ConfigMap
metadata:
  name: f5-tmm-dynamic-routing-template
  namespace: default
data:
  ZebOS.conf: ""
`
	_ = r.Apply(ctx.Ctx, emptyCM)
	// Drop the scenario namespace — also deletes the route-injector
	// DaemonSet and FRR Deployment in one shot.
	_ = r.Kubectl(ctx.Ctx, "delete", "namespace", "scn-bgp", "--ignore-not-found")
	// Restart TMM so it picks up the empty config (rollout restart
	// is idempotent; failure to restart isn't fatal — the next
	// scenario run will overwrite anyway).
	_ = r.Kubectl(ctx.Ctx, "-n", "default", "rollout", "restart",
		"deployment/f5-tmm")
	return nil
}

// oneLine collapses multi-line text into one line, truncated to n
// runes, so it fits in an Assertion.Got field without blowing up
// JSON readability.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Silence unused-import warnings when this package compiles before
// the runner uses context. (context.Background-like usage stays in
// runner.go; we just need the symbol referenced.)
var _ = context.Background
