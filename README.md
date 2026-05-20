# kindbnkctl

![BNK](https://img.shields.io/badge/BNK-2.3.0-0a3a5c)
![Kubernetes](https://img.shields.io/badge/Kubernetes-1.30.8-326ce5?logo=kubernetes&logoColor=white)
![kind](https://img.shields.io/badge/kind-v0.26%2B-1f6feb)
![Go](https://img.shields.io/github/go-mod/go-version/mwiget/kindbnkctl)
![License](https://img.shields.io/github/license/mwiget/kindbnkctl)
![Last commit](https://img.shields.io/github/last-commit/mwiget/kindbnkctl)

Single-binary CLI that deploys F5 BIG-IP Next for Kubernetes (BNK) 2.3.0
on a two-node [kind](https://kind.sigs.k8s.io/) cluster — one combined
control-plane + worker, one worker dedicated to TMM running in demo
mode (virtio inside the pod netns; no DPU, no SR-IOV, no Multus).

Aimed at low-spec corporate laptops where dpubnkctl's bare-metal +
DPU pipeline is overkill. Same poc.yaml-driven, resume-safe shape;
much shorter pipeline.

## What this tool does

Drives a BNK deployment in three phases:

1. **cluster up** — `kind create cluster`, install Calico (acts as a
   simulator for larger SR-IOV deployments), create internal + external
   docker bridge networks and attach both to every kind node container,
   label the worker for TMM, fetch kubeconfig.
2. **deploy prereqs** — namespaces, FAR pull secret, cert-manager.
3. **deploy flo + cne** — FLO from the release-manifest chart at
   `repo.f5.com`, License CR with the operator's JWT, CNEInstance with
   `advanced.demoMode.enabled: true` and TMM pinned via `nodeSelector:
   app=f5-tmm`.

Symmetric **`destroy`** unwinds it: bnk-forge unregister → `kind
delete cluster` → docker network rm.

## Pinned versions

| Component | Version |
|---|---|
| BNK | 2.3.0 |
| CNE release manifest | 2.3.0-3.2598.3-0.0.170 |
| Kubernetes (kind node image) | 1.30.8 (kind v0.26 ships this) |
| Calico | v3.28.2 |
| cert-manager | v1.16.2 |
| FLO chart | resolved at deploy time from the release manifest |

## Minimum host resources

First-measurement floor from a verified end-to-end run on linux/amd64
(kindbnkctl init → e2e → CNEInstance.Available=True, all 16 components
green, TMM 6/6 Running, License Active):

| Baseline | With bnk-forge |
|---|---|
| **4 cores** | **5 cores** |
| **6 GB RAM** | **8 GB RAM** |
| **~8 GB free disk** | **~10 GB free disk** |

Where the memory goes (measured at steady state):

| Component | Working set | CPU |
|---|---|---|
| TMM pod (worker)        | ~1.17 GB | ~100m |
| kube-apiserver          | ~900 MB  | ~150m |
| All other F5 pods (20)  | ~1.0 GB  | ~470m |
| Calico + coredns + etcd + kube-* | ~700 MB | ~150m |
| Kernel / runtime per node | ~500 MB × 2 | — |
| **Total cluster**       | **~4.5 GB pod RSS + ~1 GB overhead** | **~900m steady, peaks ~1.2c during TMM init** |

Disk: 1.4 GB (kindest/node image) + ~2.4 GB (F5 container images pulled
to the worker) + ~0.5 GB (cert-manager, alpine/k8s tooling, manifests)
+ ~2 GB headroom for kind cluster state and logs.

macOS Docker Desktop runs the cluster inside a Linux VM — add ~2 GB
to the baseline to cover that VM's own overhead. Same applies to
Rancher Desktop / Colima.

`kindbnkctl doctor` reports the host's actual CPU count and warns
when it falls below `MinBaseline`. Override the constants in
`internal/version/version.go` if your environment is tighter or
fatter than the defaults.

## bnk-forge integration

If a local [bnk-forge](https://github.com/sp-prod-field/bnk-forge)
clone exists at `~/git/bnk-forge` (or `$KINDBNKCTL_BNK_FORGE_PATH`)
when `kindbnkctl init` runs, the new PoC's `bnk_forge:` block is
pre-filled and enabled. On `cluster up`, kindbnkctl best-effort
registers the kind cluster with bnk-forge — if the local bnk-forge
stack isn't running, the auto-hook logs a clean skip and continues.

**`kindbnkctl` never installs or starts bnk-forge for you.** If it's
configured but not running, bring it up manually (`cd ~/git/bnk-forge
&& make deploy`) then `kindbnkctl bnk-forge launch` to register
after the fact.

## Requirements

| Tool | Why |
|---|---|
| **Docker** or **Podman** | kind runs Kubernetes nodes as containers; FLO + cert-gen also shell into an `alpine/k8s:1.31.5` container at deploy time |
| **kind** | cluster bring-up |
| **kubectl** | cluster reads/writes (apply, wait, label) |
| **helm** | cert-manager + FLO install, release-manifest pull |
| **git** *(optional)* | `init` git-inits the PoC repo (skippable with `--no-git`) |

Verify after install:

```bash
kindbnkctl doctor
```

What customers supply themselves, dropped into `keys/` of the PoC repo
(delivered through F5's normal channels):

- FAR tarball — image-pull credentials for `repo.f5.com`
- JWT — TEEM activation token

## Quick start

```bash
# 1. Create a fresh PoC repo. Auto-detects ~/git/bnk-forge.
kindbnkctl init demo --customer "Acme"
cd demo

# 2. Drop the operator-supplied files into keys/.
cp /path/to/f5-far-auth-key.tgz keys/
cp /path/to/license.jwt          keys/.jwt

# 3. Confirm poc.yaml is clean.
kindbnkctl validate

# 4. Run the pipeline (~10–20 min with a warm docker cache).
kindbnkctl e2e --yolo --confirm-cluster demo

# 5. Tear down (symmetric):
kindbnkctl destroy --yolo --confirm-cluster demo
```

## Per-phase invocation

If you'd rather drive the phases one at a time for diagnostics:

```bash
kindbnkctl cluster up      --yolo --confirm-cluster demo
kindbnkctl deploy prereqs  --yolo --confirm-deploy  demo
kindbnkctl deploy flo      --yolo --confirm-deploy  demo
kindbnkctl deploy cne      --yolo --confirm-deploy  demo
```

Every phase is idempotent and gated by `--yolo` plus a typo-guard.

## Repo layout (the binary itself)

```
cmd/kindbnkctl/        main entrypoint
internal/cli/          cobra commands (init, validate, doctor, cluster,
                       deploy, destroy, e2e, bnk-forge, version)
internal/poc/          poc.yaml schema + I/O
internal/cluster/      kind + docker wrappers
internal/deploy/       cert-manager, FLO, License CR, CWC cert-gen
internal/bnkforge/     bnk-forge HTTP client (copy-fork of dpubnkctl)
internal/embedded/     go:embed AGENTS.md, CLAUDE.md, templates/
internal/version/      build-stamped + BNK 2.3.0 pins + min-spec floor
```

## Repo layout (a PoC created by `kindbnkctl init`)

```
poc.yaml         declarative state — source of truth
AGENTS.md        operator + agent guide
CLAUDE.md        @AGENTS.md include
journal/         append-only markdown log
artifacts/       rendered kind.yaml, kubeconfig, helm values, CWC certs
keys/            gitignored — FAR tgz + JWT live here
.gitignore       excludes all secret material
```

## Network topology

The shape after a full `e2e` plus `bgp-peer-frr` (everything the
other scenarios build on). One docker bridge on the host (the
kind cluster's own); two kind node containers; a Multus-managed
Linux bridge inside the worker carries BGP traffic between TMM
and the FRR helper pod. Scenario backends are plain Calico pods
— the Gateway IPs they serve get plumbed via BGP, so the
backends don't need to be on the NAD themselves.

```
+----------------------------------------------------------------------------+
| HOST  (Linux or macOS Docker Desktop)                                      |
|                                                                            |
|   docker bridge: kind  172.18.0.0/16                                       |
|       |                                                                    |
+-------|--------------------------------------------------------------------+
        |
+-------+--------------+   +-------------------------------------------------+
| smoke-control-plane  |   | smoke-worker  (kind node container)             |
| (kind node container)|   | label: app=f5-tmm                               |
| eth0 172.18.0.2      |   | eth0 172.18.0.3                                 |
|                      |   |                                                 |
| pods:                |   |  +-------------------------------------------+  |
|   Calico  Multus     |   |  | TMM pod        ns=default  app=f5-tmm     |  |
|   FLO     CWC        |   |  | 6 containers:                             |  |
|   cert-manager       |   |  |   f5-tmm                                  |  |
|   ...                |   |  |   f5-tmm-routing  (= ZeBOS)               |  |
+----------------------+   |  |   debug  blobd  toda-observer  ipsec      |  |
                           |  | Interfaces:                               |  |
                           |  |   net1   192.168.99.X/24  Multus NAD      |  |
                           |  |          (BGP source, no eth0 hook)       |  |
                           |  |   eth0   10.244.x.x/32   Calico (kube-API |  |
                           |  |          + ZeBOS bgpd kernel listener)    |  |
                           |  |   xeth0  no IP    Calico veth #2, TMM     |  |
                           |  |          userspace raw frames             |  |
                           |  |   tmm    169.254.0.253/24  virtio, pod    |  |
                           |  |          default route to TMM DP          |  |
                           |  |   tunl0  DOWN     Calico IPIP placeholder |  |
                           |  +-------------------------------------------+  |
                           |  +-------------------------------------------+  |
                           |  | FRR pod        ns=scn-bgp  app=scn-frr    |  |
                           |  | 1 container:   frr (zebra + bgpd)         |  |
                           |  |   net1   192.168.99.Y/24  Multus NAD      |  |
                           |  |          (BGP peer + curl source)         |  |
                           |  |   eth0   10.244.x.x/32   Calico           |  |
                           |  +-------------------------------------------+  |
                           |             ^                                   |
                           |             |  BGP TCP/179 + scenario curls     |
                           |             v  over br-bnk-bgp, L2              |
                           |  +========================================+     |
                           |  ||  br-bnk-bgp   Linux bridge in node    ||    |
                           |  ||  netns, created by the bridge-CNI     ||    |
                           |  ||  plugin via NAD name=bnk-bgp ;        ||    |
                           |  ||  host-local IPAM 192.168.99.20-250    ||    |
                           |  ||  on /24                               ||    |
                           |  +========================================+     |
                           |                                                 |
                           |  +-------------------------------------------+  |
                           |  | scenario backends  (plain Calico pods —   |  |
                           |  | no NAD attachment, no node pinning)       |  |
                           |  |   nginx        ns=scn-httproute-e2e       |  |
                           |  |   pp-backend   ns=scn-proxy               |  |
                           |  |   ext-backend  ns=scn-extres   (Pool      |  |
                           |  |     member references its Calico podIP)   |  |
                           |  +-------------------------------------------+  |
                           |                                                 |
                           |  DaemonSets in node netns:                      |
                           |    Calico-node     Multus thick                 |
                           |    f5-coremond (if how-to #4 ran)               |
                           +-------------------------------------------------+

BGP session:
  TMM/ZeBOS  AS 65000  =======  net1 <-> net1, L2 over br-bnk-bgp  =======>  FRR  AS 65001
                                                                             listen-range
                                                                             192.168.99.0/24
                                                                             peer-group
                                                                             from-tmm

  TMM ZeBOS advertises (redistribute kernel, at router-bgp scope —
  silently dropped if placed inside address-family ipv4):
    192.168.99.0/24      (net1 connected)
    203.0.113.100/32     Gateway scn-gateway        (http-routing-e2e)
    203.0.113.101/32     Gateway scn-extres-gw      (external-resource-pool)
    203.0.113.102/32     Gateway scn-proxy-gw       (proxy-protocol-l4)

  FRR installs each /32 as a kernel route:
    203.0.113.100/32 via 192.168.99.X dev net1 proto bgp
  so any client in the FRR pod can curl the Gateway addresses
  end-to-end via the NAD bridge, completely bypassing TMM's eth0
  TCP hook. This is what http-routing-e2e and external-resource-pool
  rely on for their data-plane assertions.
```

Key knob: `CNEInstance.spec.advanced.tmm.env TMM_MAPRES_ADDL_VETHS_ON_DP=FALSE`
is set by `bgp-peer-frr`. With this `TRUE` (TMM's default for
demoMode), `mapres` grabs `net1` for the userspace data plane and
flushes its kernel IP — ZeBOS then has nothing to source-bind
to. Flipping it `FALSE` lets `net1` stay a normal Linux interface
with its NAD-assigned IP so the kernel TCP stack handles BGP
traffic ordinarily.

## Scenarios — testing F5 how-tos against the running cluster

After `e2e` brings the cluster up, drive named test scenarios against
it. Each scenario maps to one F5 how-to article (or sub-article) and
exercises a slice of BNK functionality end-to-end: render manifests
into `artifacts/scenarios/<name>/`, apply them, assert reconciled
state, write a JSON+md report under `reports/<timestamp>/scenarios/`.

```bash
kindbnkctl scenario list                            # all known scenarios + rating
kindbnkctl scenario run http-routing --poc ./demo   # apply + verify + report
kindbnkctl scenario run http-routing --dry-run      # render manifests only
kindbnkctl scenario clean http-routing              # delete what was applied
```

Rating is a stable hint about what's testable in the 2-node / demo-TMM
shape:

| Rating | Meaning |
|---|---|
| green | fully testable here |
| amber | partially testable — control-plane verifies, data-plane plumbing missing |
| red   | requires real DPUs / BGP peers / etc.; listed for discoverability, never executed |

Scoring of the [F5 BNK how-tos index](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/):

| # | How-to | Rating | Scenario | Wall time |
|---|---|---|---|---|
| 1 | [Restrict access to sensitive data](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/spk-admin-access-api.html) | 🟢 | [`cwc-admin-access`](internal/scenarios/cwcadminaccess) | 14s |
| 2 | [Components needing cluster-wide access](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/spk-whole-cluster.html) | — | not yet implemented | — |
| 3 | [Set up dynamic routing with BGP](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/spk-zebos-config.html) | 🟢 | [`bgp-peer-frr`](internal/scenarios/bgppeer) | 3m04s |
| 4 | [Set up core file collection](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/spk-coremond.html) | 🟡 | [`core-file-collection`](internal/scenarios/corefiles) | 3m01s |
| 6 | [Configure Token Counting and Enforcement](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/configure-token-counting-and-enforcement.html) | 🟡 | [`ai-token-counting`](internal/scenarios/aitokencount) | 6s |
| 7 | [Semantic AI Model Caching](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/ai-related-features/ai-semantic-caching.html) (sub-article of [AI Traffic Optimization](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/ai-related-features/index.html)) | 🟡 | [`ai-semantic-cache`](internal/scenarios/aisemcache) | 9s |
| 8 | [HTTP traffic steering with Gateway API HTTPRoute](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/Configure-HTTP-traffic-steering-with-Gateway-API-HTTPRoute.html) | 🟢 | [`http-routing-e2e`](internal/scenarios/httproutee2e) | 40s |
| 9 | [Proxy Protocol iRule support for L4 routes](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/proxy-protocol.html) | 🟡 | [`proxy-protocol-l4`](internal/scenarios/proxyprotocol) | 24s |
| 10 | [Load Balance Traffic to External Resources](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/configure-external-resource-load-balancing.html) | 🟢 | [`external-resource-pool`](internal/scenarios/extrespool) | 25s |

Wall times measured on a fresh `e2e` (cluster destroy + redeploy)
running 2026-05-20 on a Linux laptop. The two TMM-restarting
scenarios (`bgp-peer-frr` + `core-file-collection`) dominate at
~3 minutes each; the others are tens of seconds because they
either don't touch TMM or piggyback on the bridge already up.
Cluster bring-up itself (`kindbnkctl e2e`) is **5m00s**:
validate 0s · cluster-up 49s · deploy-prereqs 19s · deploy-flo
23s · deploy-cne 3m27s.

How-tos **#5** ([DOCA Offloads on DPU](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/traffic-offload.html)),
**#11** ([Static Active-Standby Interface Bonding](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/configure-static-active-standby-bonding.html)),
and **#12** ([TMOS DNS Service Integration with CIS](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/configure-tmos-dns-service-integration-with-container-ingress-services.html))
are omitted from the table because they require resources kind
can't provide: DPU silicon (#5), bondable physical NICs (#11),
and a real upstream BIG-IP GTM box (#12). They remain valid BNK
features outside the kindbnkctl shape.

Ratings are assigned only after a scenario is built and run.
Implemented scenarios that pan out land as 🟢; ones that
hit a real architectural barrier on kind+demoMode get 🟡
with the gap documented in the scenario's `Description()`.
Empty cell = scenario not yet built.

`bgp-peer-frr` (green) deploys a real BGP session between an FRR
pod and TMM's ZeBOS daemon, peered over a Multus
NetworkAttachmentDefinition (bridge CNI) on a per-node Linux
bridge. The NAD path bypasses TMM's eth0 TCP hook entirely —
BGP rides net1 in both pods, exchanging prefixes via the bridge.
Six assertions pass: Multus DaemonSet Ready, both pods have net1
in the 192.168.99.0/24 NAD range, ZeBOS sees the neighbor, BGP
session Established, and FRR's BGP table has at least one prefix
learned from TMM (via `redistribute kernel`).

`http-routing-e2e` (green) — depends on `bgp-peer-frr` for the
NAD plumbing. Applies a GatewayClass + Gateway (static
spec.addresses=203.0.113.100) + HTTPRoute + nginx backend.
TMM's ZeBOS (via `redistribute kernel`) advertises 203.0.113.100/32
into BGP; FRR installs the kernel route via net1; the verify
step execs 5 curls from inside the FRR pod, which already has
the route. All 6 assertions pass including the 5×curl. Path:
FRR socket → FRR kernel route → net1 → bnk-bgp bridge → TMM net1
→ Gateway listener → nginx. TMM's eth0 TCP hook is completely
bypassed.

Reproduce manually:

```bash
kubectl -n scn-bgp exec deploy/scn-frr -c frr -- \
  curl -sS -H 'Host: kindbnkctl.local' http://203.0.113.100/
# → kindbnkctl-scenario-httproute-e2e-OK
```

`external-resource-pool` (green) — demonstrates how-to #10 (load
balance to non-Service backends) via the BNK `Pool` CR. HTTPRoute
`backendRefs` points at a `Pool {group:k8s.f5net.com, kind:Pool}`
instead of a Service; `Pool.spec.members` lists endpoints by
IP+port. On kind, the "external" backend is an nginx pod attached
to the bnk-bgp NAD (same bridge TMM uses), with its NAD IP
auto-discovered and rendered into the Pool CR. Gateway address
is 203.0.113.101 to avoid collision with `http-routing-e2e`.

`cwc-admin-access` (green) — implements how-to #1 (restrict access
to sensitive data). Demonstrates BNK's dual-gate access control
on the CWC admin API: mTLS at the TLS layer + bearer token at
the HTTP layer. Both materials are produced by the deploy-flo
phase already (cwc-license-client-certs Secret + cwc-auth-token
Secret in f5-cne-core); the scenario just replicates them into
its own namespace, spawns a curl probe pod, and runs three
requests against https://f5-spk-cwc.f5-cne-core.svc:38081/status:
authenticated (expect 200 + license JSON), no Authorization
header (expect 401 "invalid token format"), bogus token
(expect 401 "invalid token"). Independent of bgp-peer-frr —
this is a pure runtime-access check.

`proxy-protocol-l4` (amber) — implements how-to #9 (PROXY-protocol
iRule on an L4 route). Six control-plane assertions pass: the
new BNK CRs reconcile correctly (`F5BigCneIrule` Programmed,
`L4Route` Accepted, `BNKNetPolicy` ResolvedRefs True), TMM
proxies the TCP traffic, FRR learns the Gateway IP via BGP. The
data-plane PROXY-header assertion (`[bonus]`) fails on this BNK
2.3 build — TMM accepts the L4 connection and forwards it, but
the iRule's `TCP::respond` does not actually inject the PROXY
v1 header before the server-side payload, so nginx's
`listen 80 proxy_protocol` rejects the connection with
"broken header". The scenario remains useful as a complete
demonstration of the CR wiring; lifting it to green would
require figuring out whether BNK 2.3's iRule TCL subset
supports `TCP::respond` on L4Route flows or whether a different
iRule shape is needed.

`core-file-collection` (amber) — implements how-to #4 (set up
core file collection). One-line CNEInstance.spec.coreCollection.
enabled=true flip; FLO auto-creates a CoreMond CR + DaemonSet
in f5-cne-core and adds kernel-cores / f5-core-store /
tmm-core volumes to the TMM Deployment template. The scenario
asserts the CR exists, the DaemonSet has a desired replica
count, and the TMM template carries the new volumes. The
how-to's "kill -11 to force a crash" verification step is
intentionally NOT automated — crashing TMM mid-scenario
destabilises the cluster, and the follow-up "did a core file
land in /var/crash" check needs a privileged node-level read
we'd rather not bake in. Operators can run the kill manually
after the scenario and inspect the kind worker container's
filesystem to confirm capture.

## Testing

```bash
make test    # Go unit tests (poc, deploy, cluster, scenarios)
make smoke   # unit tests + Layer A CLI smoke (no cluster required, ~5s)
```

`make smoke` is the gate to run before pushing — it covers the
non-cluster-dependent surface area in one shot.

## Design references

- **[dpubnkctl](https://github.com/mwiget/dpubnkctl)** — the
  bare-metal + DPU sister tool. kindbnkctl is a copy-fork:
  `internal/poc`, `internal/cluster`, `internal/cli` are rewritten
  for the kind path; `internal/bnkforge`, `internal/deploy` are
  forked verbatim with minor adjustments (local kubectl/helm
  instead of containerized).
- **[f5-bnk-udf](https://github.com/f5devcentral/f5-bnk-udf/tree/v2.2.0)**
  (branch `v2.2.0`) — the inspiration for the BNK-on-host shape:
  `advanced.demoMode.enabled: true` + node label + nodeSelector,
  ZeBOS dynamic-routing ConfigMap pattern, multi-worker
  topology. Same CNEInstance recipe family; kindbnkctl adapts it
  to a two-node kind cluster with Multus NADs replacing the
  macvlan-on-bare-metal approach used in f5-bnk-udf.
