# `bgp-peer-frr` — BGP between TMM and an FRR peer over a Multus NAD

F5 how-to: [Set up dynamic routing with BGP](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/spk-zebos-config.html)
&nbsp;·&nbsp; Rating: 🟢
&nbsp;·&nbsp; Wall time: **~3m19s** (TMM rollover + passwd.conf inject + wait-for-net1-annotation + BGP convergence)

Foundation scenario. Stands up everything other data-plane
scenarios depend on:

- Multus thick CNI on both kind nodes (if not already there).
- The `bridge` CNI plugin binary in `/opt/cni/bin/` on each kind
  node (downloaded from the upstream
  [containernetworking/plugins v1.5.1](https://github.com/containernetworking/plugins)
  tarball — kind's base image doesn't ship it).
- A `NetworkAttachmentDefinition` named `bnk-bgp` (in `default`
  + `scn-bgp` namespaces) backed by a `br-bnk-bgp` Linux bridge
  on the worker, with host-local IPAM in `192.168.99.0/24`.
- An `scn-frr` Deployment in the `scn-bgp` namespace running
  [frrouting/frr:9.1.0](https://hub.docker.com/r/frrouting/frr).
  `nodeAffinity` to the f5-tmm-labelled node so both peers land
  on the same Linux bridge (bridge CNI is per-node).
- ZeBOS BGP configuration on TMM via the
  `f5-tmm-dynamic-routing-template` ConfigMap. Peer FRR over
  `net1` with `update-source net1`, `redistribute kernel` at
  `router-bgp` scope (silently dropped if placed inside
  `address-family ipv4`).
- CNEInstance patched with:
  - `spec.networkAttachments=["bnk-bgp"]` — FLO adds the Multus
    annotation to TMM's pod template.
  - `spec.advanced.tmm.env TMM_MAPRES_ADDL_VETHS_ON_DP=FALSE` —
    load-bearing. With this `TRUE` (TMM demoMode default),
    `mapres` claims `net1` for the userspace data plane and
    flushes its kernel IP, so ZeBOS has no source-bind. Flipping
    it `FALSE` keeps `net1` a normal Linux interface.

After CNEInstance patch + TMM restart, the scenario writes
`/config/zebos/rd0/passwd.conf` into the new TMM pod via
`kubectl exec` (gate for bfd_watcher to imish-load the config),
with a retry loop targeting the newest pod (RollingUpdate is
patched to Recreate by `deploy cne`, but the retry stays
defensive for any future schedule churn).

## Manifests

| File | What it is |
|---|---|
| [`01-namespace.yaml`](manifests/01-namespace.yaml) | `scn-bgp` namespace |
| [`02-nad.yaml`](manifests/02-nad.yaml) | `bnk-bgp` NAD (in `default` + `scn-bgp`) — bridge CNI on `br-bnk-bgp`, host-local IPAM |
| [`03-frr-config.yaml`](manifests/03-frr-config.yaml) | ConfigMap with `daemons` + `frr.conf` (BGP AS 65001, listen-range 192.168.99.0/24, peer-group `from-tmm`) |
| [`04-frr.yaml`](manifests/04-frr.yaml) | FRR Deployment + Service, NAD annotation on net1, nodeAffinity to f5-tmm |
| [`05-zebos-template.yaml.tmpl`](manifests/05-zebos-template.yaml.tmpl) | text/template — patched into `f5-tmm-dynamic-routing-template` ConfigMap; `{{.FRRNetIP}}` is filled at apply time |

## How to run

```bash
kindbnkctl scenario run bgp-peer-frr --poc <pocdir>
```

## Verification (all 6 must pass)

```
✓ Multus DaemonSet Ready
✓ FRR pod has net1 on the bnk-bgp bridge (192.168.99.0/24)
✓ TMM pod has net1 on the bnk-bgp bridge
✓ ZeBOS in TMM sees neighbor on bnk-bgp bridge
✓ BGP session Established between FRR and TMM/ZeBOS
✓ FRR BGP table has at least one prefix learned from TMM
```

FRR's `show bgp summary` reports Established as a timer
(`00:02:13`) in the State column rather than the literal word
"Established"; the verify helper accepts the timer form or
explicit string.

## Cleanup

`kindbnkctl scenario clean bgp-peer-frr` reverts:

- `CNEInstance.spec.networkAttachments` → `[]`
- `f5-tmm-dynamic-routing-template` ConfigMap → empty `ZebOS.conf`
- TMM restarted to drop net1 + bgpd config
- `scn-bgp` namespace deleted (takes FRR with it)
- `bnk-bgp` NAD in `default` deleted

Multus stays installed (cluster-wide; reverting it would impact
other workloads).

## Why this scenario looks long

Three CR types orchestrated, one CNEInstance patch, one TMM
rollover, one ConfigMap rewrite, and one passwd.conf injection
into the live TMM pod. The complexity is in the BNK-side glue —
the Mermaid sketch in [README.md "Network topology"](../../../README.md#network-topology)
shows what ends up wired together.
