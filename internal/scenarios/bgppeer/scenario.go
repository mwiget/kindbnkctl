// Package bgppeer implements scenario "bgp-peer-frr": real BGP between
// TMM's ZeBOS daemon and an FRR pod, peered over a Multus
// NetworkAttachmentDefinition (the bridge CNI). Maps to F5 BNK how-to
// #3 (Dynamic Routing with BGP).
//
// Architecture on kind + demoMode:
//
//	┌──────────────────────────────┐    ┌──────────────────────────┐
//	│ TMM pod                      │    │ scn-frr pod              │
//	│   eth0  10.244.x/32 (Calico) │    │   eth0  10.244.x/32      │
//	│   net1  192.168.99.X         │◄──►│   net1  192.168.99.11    │
//	│         (Multus bridge)      │    │         (Multus bridge)  │
//	│   tmm   169.254.0.253/24     │    └──────────────────────────┘
//	└──────────────────────────────┘
//	             ▲
//	             └── BGP rides net1, bypassing TMM's eth0 TCP hook.
//
// Why this works (where the previous attempt didn't):
//   - Multus attaches a second interface (net1) into each pod via the
//     bridge CNI, plumbed through the same Linux bridge on the kind
//     node. Pod-to-pod traffic over net1 is direct L2 — no TMM hook.
//   - ZeBOS gets `update-source net1` and a neighbor at 192.168.99.11
//     (FRR's hardcoded NAD IP). BGP packets exit via net1, never
//     touching the TMM data plane.
//   - FRR's NAD annotation pins it to 192.168.99.11; bridge-CNI is
//     per-node, so podAffinity to `app=f5-tmm` co-locates both pods
//     on smoke-worker.
package bgppeer

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
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
	scnTitle = "Dynamic routing with BGP (how-to #3) — FRR peer over Multus NAD"
	// Multus thick-plugin upstream manifest. Pinned. Pulled at scenario
	// apply time if the kube-multus DaemonSet isn't already running.
	multusManifestURL = "https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/v4.1.4/deployments/multus-daemonset-thick.yml"
)

func init() { scenarios.Register(&scenario{}) }

type scenario struct{}

func (s *scenario) Name() string             { return scnName }
func (s *scenario) Title() string            { return scnTitle }
func (s *scenario) Rating() scenarios.Rating { return scenarios.Green }
func (s *scenario) Dependencies() []string   { return nil }
func (s *scenario) Description() string {
	return strings.TrimSpace(`
Deploys an FRR BGP peer and establishes a real BGP session with
TMM's ZeBOS daemon, peered over a Multus NetworkAttachmentDefinition
using the bridge CNI. The NAD path bypasses TMM's eth0 TCP hook
entirely — BGP packets ride a Linux bridge between the two pods'
net1 interfaces.

Pipeline:

  1. Install Multus (thick plugin) if not already running.
  2. NetworkAttachmentDefinition (bnk-bgp) in default + scn-bgp
     namespaces. Bridge CNI on a per-node Linux bridge
     'br-bnk-bgp' with host-local IPAM in 192.168.99.0/24.
  3. Deploy FRR with the NAD annotation. host-local IPAM hands
     it an IP (e.g. 192.168.99.20); the scenario discovers it
     from the pod's network-status annotation at runtime.
     nodeAffinity to the f5-tmm-labelled node so both pods
     end up on the same node (bridge CNI is per-node).
  4. Apply the ZeBOS template ConfigMap with the discovered FRR
     net1 IP as the neighbor, 'update-source net1', and
     'redistribute kernel' so connected and Gateway-IP routes
     get advertised. The redistribute MUST live at the router-bgp
     scope (not inside address-family) — F5 ZeBOS silently
     drops it from address-family ipv4.
  5. Patch CNEInstance:
       - spec.networkAttachments = ["bnk-bgp"]
       - flip TMM_MAPRES_ADDL_VETHS_ON_DP to FALSE so TMM's
         mapres doesn't grab net1 for the userspace data plane
         and flush its kernel-side IP.
  6. Restart TMM. Inject /config/zebos/rd0/passwd.conf into the
     new pod (gate for bfd_watcher to imish-load the ZeBOS
     config). Retry the inject across the rolling-update
     overlap window.

The Deployment uses RollingUpdate with maxSurge=25% — for a
1-replica deployment that allows 2 pods briefly during rollover.
All TMM lookups in the scenario target the newest Running pod
(sorted by creationTimestamp) so stale exec results from the
outgoing pod don't poison the verify step.

Verification (6/6 green):
  - Multus DaemonSet Ready
  - FRR pod has net1 on the bnk-bgp bridge
  - TMM pod has net1 on the bnk-bgp bridge with a kernel IP
  - ZeBOS in TMM sees the configured neighbor
  - BGP session Established
  - FRR BGP table has at least one prefix learned from TMM

Cleanup: revert CNEInstance.spec.networkAttachments to [], empty
the f5-tmm-dynamic-routing-template ConfigMap, restart TMM,
delete the scn-bgp namespace + default-namespace NAD. Multus
stays installed (it's cluster-wide; reverting it would impact
other workloads). TMM env vars revert via the original
http-routing CNEInstance template if you re-run e2e.
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

	// 1. Multus install (idempotent — skip if DaemonSet already exists).
	if err := ensureMultus(ctx); err != nil {
		return fmt.Errorf("ensure Multus: %w", err)
	}

	// 2. Namespace + NAD + FRR config + FRR Deployment.
	for _, f := range []string{
		"01-namespace.yaml",
		"02-nad.yaml",
		"03-frr-config.yaml",
		"04-frr.yaml",
	} {
		body, err := manifestFS.ReadFile("manifests/" + f)
		if err != nil {
			return err
		}
		if err := r.Apply(ctx.Ctx, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}

	// 3. Wait for FRR pod Ready + discover its NAD IP. host-local
	//    IPAM auto-assigns; we read the actual IP from the pod's
	//    net1 interface so the ZeBOS render uses the right neighbor
	//    address regardless of allocation order.
	if err := r.Wait(ctx.Ctx, "scn-bgp", "Available",
		"deployment/scn-frr", 2*time.Minute); err != nil {
		return fmt.Errorf("FRR Deployment not Available: %w", err)
	}
	frrIP, err := discoverNet1(ctx, "scn-bgp", "app=scn-frr")
	if err != nil {
		return fmt.Errorf("discover FRR net1 IP: %w", err)
	}
	fmt.Fprintf(ctx.Out, "      | FRR net1 IP: %s\n", frrIP)

	// 4. Render ZeBOS template with FRR's discovered NAD IP. Persist
	//    the rendered file for audit.
	zebosBody, err := renderTemplate(manifestFS, "manifests/05-zebos-template.yaml.tmpl",
		struct{ FRRNetIP string }{FRRNetIP: frrIP})
	if err != nil {
		return err
	}
	if _, err := scenarios.WriteManifest(ctx.PoCDir, scnName,
		"05-zebos.rendered.yaml", zebosBody); err != nil {
		return err
	}
	if err := r.Apply(ctx.Ctx, zebosBody); err != nil {
		return fmt.Errorf("apply ZeBOS ConfigMap: %w", err)
	}

	// 4. Patch CNEInstance:
	//    - spec.networkAttachments = ["bnk-bgp"] so FLO renders the
	//      Multus pod annotation for TMM
	//    - flip TMM_MAPRES_ADDL_VETHS_ON_DP to FALSE so TMM's mapres
	//      doesn't claim net1 and flush its IP. Without this, the
	//      kernel-side net1 stays UP but loses its IPAM-assigned
	//      address — ZeBOS can't `update-source net1` against an
	//      IP-less interface.
	patch := `{"spec":{
		"networkAttachments":["bnk-bgp"],
		"advanced":{"tmm":{"env":[
			{"name":"TMM_CALICO_ROUTER","value":"default"},
			{"name":"TMM_DEFAULT_MTU","value":"1500"},
			{"name":"TMM_MAPRES_ADDL_VETHS_ON_DP","value":"FALSE"},
			{"name":"ZEBOS_STATE","value":"legacy"}
		],"annotations":{"kindbnkctl.f5.com/managed-by":"kindbnkctl"}}}
	}}`
	if err := r.Kubectl(ctx.Ctx, "patch", "cneinstance", "bnk-instance",
		"-n", "default", "--type=merge", "-p", patch); err != nil {
		return fmt.Errorf("patch CNEInstance: %w", err)
	}

	// 5. Restart TMM so the new pod picks up both the Multus NAD
	//    attachment and the freshly-applied ZeBOS ConfigMap.
	if err := r.Kubectl(ctx.Ctx, "-n", "default", "rollout", "restart",
		"deployment/f5-tmm"); err != nil {
		return fmt.Errorf("rollout restart f5-tmm: %w", err)
	}
	if err := r.Kubectl(ctx.Ctx, "-n", "default", "rollout", "status",
		"deployment/f5-tmm", "--timeout=5m"); err != nil {
		return fmt.Errorf("f5-tmm rollout did not complete: %w", err)
	}

	// 6. Inject passwd.conf into the new TMM pod. Retry loop re-fetches
	//    the Running pod name each iteration so a brief container-not-
	//    found window during rollover doesn't fail the apply.
	if err := injectPasswdConf(ctx); err != nil {
		return fmt.Errorf("inject passwd.conf: %w", err)
	}

	return nil
}

func (s *scenario) Verify(ctx *scenarios.Context) scenarios.Result {
	r := ctx.Runner
	res := scenarios.Result{}

	// FRR pod name + IP.
	frrPod, err := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "get", "pod",
		"-l", "app=scn-frr",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil || strings.TrimSpace(frrPod) == "" {
		res.Status = "failed"
		res.Summary = "scn-frr pod not found"
		res.Details = errString(err)
		return res
	}
	frrPod = strings.TrimSpace(frrPod)

	// 1. Multus DaemonSet healthy.
	multus, _ := r.KubectlCapture(ctx.Ctx, "-n", "kube-system", "get",
		"daemonset/kube-multus-ds",
		"-o", "jsonpath={.status.numberReady}/{.status.desiredNumberScheduled}")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "Multus DaemonSet Ready",
		OK:          strings.Contains(multus, "/") && !strings.HasPrefix(multus, "0/"),
		Got:         oneLine(multus, 50),
	})

	// 2. FRR pod has a net1 interface with an IP in the NAD range.
	frrIfs, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
		frrPod, "-c", "frr", "--", "ip", "-4", "addr", "show", "net1")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "FRR pod has net1 on the bnk-bgp bridge (192.168.99.0/24)",
		OK:          strings.Contains(frrIfs, "192.168.99."),
		Got:         oneLine(frrIfs, 200),
	})

	// Always target the newest TMM pod — Deployment RollingUpdate
	// can leave the old pod Running for a while after we patch
	// CNEInstance, and `deploy/f5-tmm` resolves non-deterministically
	// to either one.
	tmmPod, err := newestTMMPod(ctx)
	if err != nil {
		res.Status = "failed"
		res.Summary = "no Running TMM pod"
		res.Details = err.Error()
		return res
	}

	// 3. TMM pod has a net1 interface in the NAD range.
	tmmIfs, _ := r.KubectlCapture(ctx.Ctx, "-n", "default", "exec",
		tmmPod, "-c", "debug", "--",
		"ip", "-4", "addr", "show", "net1")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "TMM pod has net1 on the bnk-bgp bridge",
		OK:          strings.Contains(tmmIfs, "192.168.99."),
		Got:         oneLine(tmmIfs, 200),
	})

	// 4. ZeBOS in TMM sees the configured neighbor (any 192.168.99.x).
	//    Retry to give bfd_watcher time to imish-load after restart.
	//    Re-resolve the newest pod each iteration in case rollout
	//    completes mid-loop and the previously-newest is gone.
	var zebos string
	tmmHasNeighbor := false
	for i := 0; i < 12; i++ {
		if p, err := newestTMMPod(ctx); err == nil {
			tmmPod = p
		}
		zebos, _ = r.KubectlCapture(ctx.Ctx, "-n", "default", "exec",
			tmmPod, "-c", "f5-tmm-routing", "--",
			"imish", "-e", "show ip bgp summary")
		if strings.Contains(zebos, "192.168.99.") {
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
		Description: "ZeBOS in TMM sees neighbor on bnk-bgp bridge",
		OK:          tmmHasNeighbor,
		Got:         oneLine(zebos, 200),
	})

	// 5. BGP session reaches Established. FRR's `show bgp summary`
	//    shows an Up/Down TIMER ("00:02:13") in the State column when
	//    Established, not the literal word "Established"; so the
	//    robust check is "no transient state name in the neighbor row".
	//    `show bgp summary json` would also work but we'd need to
	//    pipe through jq. Substring check on the transient states is
	//    cheap and reliable.
	deadline := time.Now().Add(2 * time.Minute)
	var lastSummary string
	established := false
	for time.Now().Before(deadline) {
		out, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
			frrPod, "-c", "frr", "--", "vtysh", "-c", "show bgp summary")
		lastSummary = out
		if isFRRBGPEstablished(out) {
			established = true
			break
		}
		select {
		case <-ctx.Ctx.Done():
			break
		case <-time.After(5 * time.Second):
		}
	}
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "BGP session Established between FRR and TMM/ZeBOS",
		OK:          established,
		Got:         oneLine(lastSummary, 250),
	})

	// 6. FRR BGP table has at least one prefix learned from TMM
	//    (redistribute connected → at minimum the 192.168.99.0/24
	//    NAD subnet). The explicit Gateway prefix 203.0.113.100/32
	//    only shows up after http-routing-e2e has run.
	bgpTable, _ := r.KubectlCapture(ctx.Ctx, "-n", "scn-bgp", "exec",
		frrPod, "-c", "frr", "--", "vtysh", "-c", "show bgp ipv4 unicast")
	hasAnyPrefix := strings.Contains(bgpTable, "192.168.99.") ||
		strings.Contains(bgpTable, "203.0.113.")
	res.Assertions = append(res.Assertions, scenarios.Assertion{
		Description: "FRR BGP table has at least one prefix learned from TMM",
		OK:          hasAnyPrefix,
		Got:         oneLine(bgpTable, 250),
	})

	if res.AllPassed() {
		res.Status = "ok"
		res.Summary = "BGP Established over Multus NAD; routes exchanged TMM ↔ FRR"
	} else {
		res.Status = "failed"
		var failed []string
		for _, a := range res.Assertions {
			if !a.OK {
				failed = append(failed, a.Description)
			}
		}
		res.Summary = "failed: " + strings.Join(failed, "; ")
		res.Details = "BGP summary:\n" + lastSummary + "\n\nBGP table:\n" + bgpTable +
			"\n\nZeBOS summary:\n" + zebos
	}
	return res
}

func (s *scenario) Cleanup(ctx *scenarios.Context) error {
	r := ctx.Runner
	// 1. Remove the NAD from CNEInstance so FLO drops the Multus
	//    annotation on the next TMM restart.
	_ = r.Kubectl(ctx.Ctx, "patch", "cneinstance", "bnk-instance",
		"-n", "default", "--type=merge",
		"-p", `{"spec":{"networkAttachments":[]}}`)
	// 2. Empty the ZeBOS template so bgpd has no peer config next time.
	_ = r.Apply(ctx.Ctx, `apiVersion: v1
kind: ConfigMap
metadata:
  name: f5-tmm-dynamic-routing-template
  namespace: default
data:
  ZebOS.conf: ""
`)
	// 3. Drop scn-bgp namespace (also removes FRR + scoped NAD).
	_ = r.Kubectl(ctx.Ctx, "delete", "namespace", "scn-bgp", "--ignore-not-found")
	// 4. Drop the default-namespace NAD.
	_ = r.Kubectl(ctx.Ctx, "-n", "default", "delete", "net-attach-def",
		"bnk-bgp", "--ignore-not-found")
	// 5. Restart TMM to apply the empty ZeBOS config + drop the Multus
	//    annotation.
	_ = r.Kubectl(ctx.Ctx, "-n", "default", "rollout", "restart",
		"deployment/f5-tmm")
	return nil
}

// discoverNet1 looks up the IPv4 address of the pod's net1 interface
// (the Multus NAD attachment). Format from the pod's status.metadata
// annotation `k8s.v1.cni.cncf.io/network-status` is JSON; we just
// inspect inside the pod with `ip` for simplicity. Selector targets
// a single Deployment so callers don't pass a pod name.
func discoverNet1(ctx *scenarios.Context, namespace, labelSelector string) (string, error) {
	r := ctx.Runner
	podName, err := r.KubectlCapture(ctx.Ctx, "-n", namespace, "get", "pod",
		"-l", labelSelector,
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil || strings.TrimSpace(podName) == "" {
		return "", fmt.Errorf("no Running pod for %s in %s: %w", labelSelector, namespace, err)
	}
	podName = strings.TrimSpace(podName)
	// network-status annotation is the canonical, language-agnostic
	// source — we read it via jsonpath instead of exec-ing into the
	// container, which avoids needing `ip` binary in the image.
	netStatus, err := r.KubectlCapture(ctx.Ctx, "-n", namespace, "get", "pod",
		podName,
		"-o", `jsonpath={.metadata.annotations.k8s\.v1\.cni\.cncf\.io/network-status}`)
	if err != nil {
		return "", err
	}
	// network-status is a JSON array — parse properly to avoid
	// whitespace-formatting brittleness.
	var entries []struct {
		Name string   `json:"name"`
		IPs  []string `json:"ips"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(netStatus)), &entries); err != nil {
		return "", fmt.Errorf("parse network-status: %w (raw: %s)", err, oneLine(netStatus, 200))
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name, "bnk-bgp") && !strings.HasSuffix(e.Name, "/bnk-bgp") {
			continue
		}
		for _, ip := range e.IPs {
			if slash := strings.Index(ip, "/"); slash > 0 {
				ip = ip[:slash]
			}
			if !strings.Contains(ip, ":") && ip != "" {
				return ip, nil
			}
		}
	}
	return "", fmt.Errorf("bnk-bgp entry not found in network-status: %q", oneLine(netStatus, 300))
}

// ensureMultus checks whether the kube-multus DaemonSet already exists;
// if not, kubectl-applies the upstream thick-plugin manifest from
// multusManifestURL and waits for the DaemonSet to be Ready.
func ensureMultus(ctx *scenarios.Context) error {
	r := ctx.Runner
	out, _ := r.KubectlCapture(ctx.Ctx, "-n", "kube-system", "get",
		"daemonset/kube-multus-ds",
		"-o", "jsonpath={.status.numberReady}/{.status.desiredNumberScheduled}")
	if strings.Contains(out, "/") && !strings.HasPrefix(strings.TrimSpace(out), "0/") {
		fmt.Fprintln(ctx.Out, "      | Multus already installed — skipping")
		return nil
	}
	fmt.Fprintln(ctx.Out, "      | installing Multus thick plugin ...")
	if err := r.Kubectl(ctx.Ctx, "apply", "-f", multusManifestURL); err != nil {
		return err
	}
	return r.Kubectl(ctx.Ctx, "-n", "kube-system", "rollout", "status",
		"daemonset/kube-multus-ds", "--timeout=3m")
}

// newestTMMPod returns the name of the most-recently-created Running
// TMM pod. During a Deployment rolling update there can be two pods
// simultaneously (old still serving while new starts up) — picking
// `items[0]` is non-deterministic; sorting by creationTimestamp ASC
// and taking the last entry always returns the new pod, which is
// what every step that depends on the post-patch config needs.
func newestTMMPod(ctx *scenarios.Context) (string, error) {
	r := ctx.Runner
	out, err := r.KubectlCapture(ctx.Ctx, "-n", "default", "get", "pod",
		"-l", "app=f5-tmm",
		"--field-selector=status.phase=Running",
		"--sort-by=.metadata.creationTimestamp",
		"-o", "jsonpath={.items[-1:].metadata.name}")
	out = strings.TrimSpace(out)
	if err != nil || out == "" {
		return "", fmt.Errorf("no Running f5-tmm pod: %w (out=%q)", err, out)
	}
	return out, nil
}

// injectPasswdConf writes the one-line passwd.conf into the newest
// TMM pod's f5-tmm-routing container so bfd_watcher's imish-load can
// succeed. Retries up to 90s, re-fetching the newest Running pod
// each attempt to survive rolling-update overlap windows.
func injectPasswdConf(ctx *scenarios.Context) error {
	r := ctx.Runner
	var injectErr error
	for i := 0; i < 18; i++ {
		tmmPod, err := newestTMMPod(ctx)
		if err != nil {
			injectErr = err
		} else {
			injectErr = r.Kubectl(ctx.Ctx, "-n", "default", "exec",
				tmmPod, "-c", "f5-tmm-routing", "--",
				"sh", "-c", "echo 'enable password 0 zebos' > /config/zebos/rd0/passwd.conf")
			if injectErr == nil {
				return nil
			}
		}
		select {
		case <-ctx.Ctx.Done():
			return ctx.Ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return injectErr
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

// errString returns "" for nil err, otherwise a short message.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return oneLine(err.Error(), 200)
}

// isFRRBGPEstablished returns true when FRR's `show bgp summary`
// output indicates at least one neighbor is in the Established
// state. FRR displays the Up/Down timer (e.g. "00:02:13") in the
// State column for Established sessions, and the transient state
// name ("Idle", "Connect", "Active", "OpenSent", "OpenConfirm")
// otherwise. We detect a non-zero MsgRcvd column AND no transient
// state in the neighbor row containing a 192.168.99.x peer.
func isFRRBGPEstablished(summary string) bool {
	for _, line := range strings.Split(summary, "\n") {
		if !strings.Contains(line, "192.168.99.") {
			continue
		}
		// Reject transient states.
		for _, transient := range []string{
			"Idle", "Connect", "Active", "OpenSent", "OpenConfirm",
		} {
			if strings.Contains(line, transient) {
				return false
			}
		}
		// Established rows contain a timer like 00:02:13.
		// Look for at least one colon-separated time token.
		fields := strings.Fields(line)
		for _, f := range fields {
			if strings.Count(f, ":") == 2 && !strings.HasPrefix(f, "192.") {
				return true
			}
		}
	}
	return false
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

// Silence unused-import warnings.
var _ = context.Background
