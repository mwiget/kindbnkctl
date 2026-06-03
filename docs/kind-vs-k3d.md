# kind vs k3d backend

`kindbnkctl` can drive its two-node cluster with either **kind** (the
default) or **k3d** (k3s-in-docker). The backend is selected by the
binary's own name — install the binary as `kindbnkctl` and symlink
`k3dbnkctl → kindbnkctl`; invoking the symlink flips the backend to k3d.
Everything downstream (Calico, the `app=f5-tmm` worker label, the BNK
deploy pipeline, scenarios) is identical across backends.

```bash
make install                       # installs ~/.local/bin/kindbnkctl
ln -sf kindbnkctl ~/.local/bin/k3dbnkctl

kindbnkctl cluster up --poc demo …  # kind
k3dbnkctl cluster up --poc demo …   # k3d (same binary, same PoC dir)
```

Both backends produce the same shape: one combined control-plane+worker
node and one worker node, **Calico** as the CNI (kind's default CNI is
disabled via `disableDefaultCNI: true`; k3d's bundled flannel +
network-policy + traefik + servicelb + metrics-server are all disabled
in `templates/k3d.yaml.tmpl`), pinned to k8s **v1.30.8**.

## Measurements

Measured 2026-06-01 on macOS Docker Desktop (10 CPUs / 15.6 GiB to the
Docker VM). `cluster up` = create cluster → apply Calico → wait for
`calico-kube-controllers` Available → label worker → fetch kubeconfig.

| Metric | kind | k3d |
|---|---|---|
| `cluster up`, cold node image | 102 s | 98 s |
| `cluster up`, warm node image | **102 s** | **84 s** |
| idle node-container memory | **~1.3 GiB** | **~1.1 GiB** |
| &nbsp;&nbsp;· control-plane / server | 1007 MiB | 692 MiB |
| &nbsp;&nbsp;· worker / agent | 318 MiB | 410 MiB |
| system pods at idle | 12 | 5 |
| node image | `kindest/node:v1.30.8` | `rancher/k3s:v1.30.8-k3s1` |

### What the numbers say

- **Bring-up time:** warm, k3d is ~18 % faster (84 s vs 102 s). Both
  are bounded by the same Calico rollout + `kubectl wait`, which is why
  the gap is modest rather than dramatic — the cluster-create step
  itself is where k3d wins; the shared Calico wait dilutes it. (The
  cold-image k3d run still came in at 98 s *including* a fresh
  `rancher/k3s` pull, so warm is the fair comparison.)
- **Idle footprint:** k3d is ~200 MiB lighter and runs 5 pods vs 12.
  k3s collapses apiserver + etcd (sqlite) + scheduler +
  controller-manager + kube-proxy into a single server process, so
  those don't appear as separate pods; CoreDNS runs a single replica.
- **Scheduling difference that matters for BNK:** kind taints the
  control-plane node `node-role.kubernetes.io/control-plane:NoSchedule`,
  so every F5 pod lands on the worker. k3d's server node is
  **schedulable** (no such taint), so F5 pods can spread across both
  nodes. This doesn't change the verdict that full BNK 2.3 doesn't fit
  this host — the sum of pod `requests` (~20 GiB / ~12 cores, see
  [Minimum host resources](../README.md#minimum-host-resources)) still
  exceeds the 15.6 GiB VM — but k3d will schedule *further* before
  hitting the wall because the load isn't pinned to one node.

### Scenario parity

The full green scenario suite has been run end-to-end on the k3d backend
(committed under `examples/reports-k3d/`) and diffed against the kind
reference report (`examples/reports/scenarios/`):

- **11/11 green scenarios pass** on k3d (run 2026-06-02, 10m19s),
  matching kind's 11/11 *at that time*. (`grpc-loadbalance` was
  subsequently re-rated green, so the current suite is 12 green and the
  kind reference report is now 12/12 — the k3d report under
  `examples/reports-k3d/` predates that and still shows 11. The new
  scenario rides a backend-agnostic L4Route path, so 12/12 parity is
  expected; re-run `scenario run --all` on k3d to reconfirm.)
- **Every assertion is identical** in description and pass/fail across
  both backends — no assertion passes on kind but fails, skips, or
  relaxes on k3d. This includes the data-plane-heavy paths (BGP peering,
  HTTPRoute steering, L4 TCP/UDP, semantic cache + token-counting iRules)
  that depend on the Multus NAD / FRR / ZeBOS plumbing.
- The only deltas are environment noise in observed values — pod IPs and
  pod-CIDR-derived router IDs, replica-hash pod names, timestamps, BGP
  next-hop table indices, and the license asset ID / expiry — all
  expected to vary between two independent deployments.

So the backends are at full **scenario parity**; kind stays the
reference only by convention (it's where the committed reference report
is regenerated), not because of any behavioral gap.

### Verdict

For the demo-TMM shape, k3d is a modest win: faster cluster create and a
lighter idle control plane, with no change to the BNK deploy path and
verified scenario parity (above). It is kept as an *option* (the
`k3dbnkctl` symlink), not the default — kind remains the reference
backend the committed reference report is regenerated against.

## Reproducing

```bash
# from the repo root, with a `demo` PoC already init'd:
demo/measure.sh kindbnkctl kind
demo/measure.sh k3dbnkctl k3d
# raw logs + stats land under demo/compare/
```
