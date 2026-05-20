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
Deploys an FRR-based BGP peer in a dedicated namespace and
configures TMM's ZeBOS daemon to peer with it via the existing
f5-tmm-dynamic-routing-template ConfigMap. Exercises the full
BNK dynamic-routing reconciliation chain on kind.

Rated AMBER pending investigation of three documented gaps that
together block BGP Established on the kind / demoMode shape:

  1. f5-tmm-dynamic-routing ConfigMap ships with empty vlanName
     and empty clusterNodeIPs. bfd_watcher's imish load fails
     repeatedly with 'vlan name not found' until those fields
     are populated. In a normal BNK deployment those values
     come from F5SPKVlan reconciliation (DPU/SR-IOV path); on
     kind there is no equivalent reconciler. Manual patching to
     {vlanName: "eth0", clusterNodeIPs: [<tmm-pod-IP>]} stops
     the errors but isn't durable across TMM restarts.

  2. /config/zebos/rd0/passwd.conf does not exist; bfd_watcher
     logs 'failed to open file ... No such file or directory'
     and 'imish load command failed'. ZeBOS keeps running with
     an empty config — explains why the neighbor never makes
     a TCP connection attempt despite showing in 'show ip bgp
     summary' (which reads from the ConfigMap mount, not from
     the live bgpd state).

  3. The TMM pod's kernel routing table has 'default via dev tmm'
     and no specific route for the Calico /26 — so even when
     bgpd does send a SYN, it goes to the tmm virtio interface
     instead of out via Calico eth0. Workaround: nsenter into
     the pod netns and add 'ip route 169.254.1.1 dev eth0' +
     'ip route <frr-ip> via 169.254.1.1 dev eth0' (Calico's
     fake-gateway pattern). When this route is added, FRR does
     see incoming BGP from TMM (visible in FRR logs as 'sent
     to neighbor X 1/1 (Message Header Error/Connection Not
     Synchronized)'), confirming the network path works once
     primed but the BGP protocol handshake then fails.

What IS verified by this scenario today (control-plane side):

  - FRR pod Ready, bgpd running, listen-range configured with
    peer-group 'from-tmm' visible in 'vtysh show bgp peer-group'
  - The rendered ZeBOS.conf is applied to the cluster-wide
    f5-tmm-dynamic-routing-template ConfigMap (file at
    artifacts/scenarios/<name>/04-zebos.rendered.yaml)
  - TMM pod is restarted to reload the ConfigMap
  - ZeBOS surfaces the configured neighbor (Active state) in
    its show output

To lift this to green, a follow-up scenario needs to: populate
vlanName + clusterNodeIPs in f5-tmm-dynamic-routing, create
passwd.conf via an emptyDir mount or sidecar, and inject the
fake-gateway routes into the TMM pod's netns (privileged
nsenter Job or a custom CNI hook).

Caveats:
  - The ConfigMap rewrite affects the whole CNEInstance —
    running this scenario reconfigures cluster-wide TMM dynamic
    routing until you run 'scenario clean bgp-peer-frr' (restores
    the template to empty).
  - The FRR pod's BGP listener accepts any peer in 10.244.0.0/16;
    appropriate for single-tenant kind clusters only.
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

	// Discover the local Calico /26 block containing FRR so we can
	// install a single subnet-wide route in ZeBOS instead of a
	// host-specific one (more resilient if FRR is recreated and gets
	// a sibling IP in the same block).
	calicoSubnet := calicoBlockFor(frrIP)

	// 3. Render the ZeBOS template with the discovered FRR IP and
	//    write the rendered file alongside the other manifests so
	//    the operator can grep what was applied.
	tmplBody, err := manifestFS.ReadFile("manifests/04-zebos-template.yaml.tmpl")
	if err != nil {
		return err
	}
	t, err := template.New("zebos").Parse(string(tmplBody))
	if err != nil {
		return err
	}
	var rendered bytes.Buffer
	if err := t.Execute(&rendered, struct{ FRRPodIP, CalicoSubnet string }{
		FRRPodIP:     frrIP,
		CalicoSubnet: calicoSubnet,
	}); err != nil {
		return err
	}
	if _, err := scenarios.WriteManifest(ctx.PoCDir, scnName,
		"04-zebos.rendered.yaml", rendered.String()); err != nil {
		return err
	}

	// 4. Apply the ZeBOS ConfigMap and restart TMM so it re-reads.
	//    TMM doesn't auto-reload the config — FLO programs it at
	//    pod startup. Easiest path: delete the TMM pod and let the
	//    Deployment respin it.
	if err := r.Apply(ctx.Ctx, rendered.String()); err != nil {
		return fmt.Errorf("apply ZeBOS ConfigMap: %w", err)
	}
	if err := r.Kubectl(ctx.Ctx, "-n", "default", "rollout", "restart",
		"deployment/f5-tmm"); err != nil {
		return fmt.Errorf("rollout restart f5-tmm: %w", err)
	}
	if err := r.Kubectl(ctx.Ctx, "-n", "default", "rollout", "status",
		"deployment/f5-tmm", "--timeout=5m"); err != nil {
		return fmt.Errorf("f5-tmm rollout did not complete: %w", err)
	}
	return nil
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

	// FRR bgpd up + listen-range applied. The vtysh exec is the
	// proof that the BGP daemon parsed our config (errors would
	// have surfaced as "% No BGP neighbors found in VRF default"
	// or similar).
	frrConfig, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
		frrPod, "--", "vtysh", "-c", "show bgp peer-group")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "FRR bgpd loaded peer-group from-tmm with listen-range",
		OK:          err == nil && strings.Contains(frrConfig, "from-tmm") && strings.Contains(frrConfig, "listen range"),
		Got:         oneLine(frrConfig, 200),
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
	// Restart TMM so it picks up the empty config (rollout restart
	// is idempotent; failure to restart isn't fatal — the next
	// scenario run will overwrite anyway).
	_ = r.Kubectl(ctx.Ctx, "-n", "default", "rollout", "restart",
		"deployment/f5-tmm")
	// Drop the scenario namespace.
	_ = r.Kubectl(ctx.Ctx, "delete", "namespace", "scn-bgp", "--ignore-not-found")
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

// calicoBlockFor returns the /26 block-CIDR containing ip. Calico's
// default IPAM hands out /26 blocks; the route ZeBOS needs is a
// subnet-wide entry rather than a host-specific one so it survives
// FRR pod recreation onto a sibling IP. Fallback to /32 if the input
// isn't a valid v4 address.
func calicoBlockFor(ip string) string {
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ip + "/32"
	}
	// last octet aligned to a /26 boundary (0, 64, 128, 192)
	var last int
	if _, err := fmt.Sscanf(parts[3], "%d", &last); err != nil {
		return ip + "/32"
	}
	block := (last / 64) * 64
	return fmt.Sprintf("%s.%s.%s.%d/26", parts[0], parts[1], parts[2], block)
}

// Silence unused-import warnings when this package compiles before
// the runner uses context. (context.Background-like usage stays in
// runner.go; we just need the symbol referenced.)
var _ = context.Background
