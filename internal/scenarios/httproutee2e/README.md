# `http-routing-e2e` — HTTPRoute end-to-end with real curl through TMM

F5 how-to: [HTTP traffic steering with Gateway API HTTPRoute](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/Configure-HTTP-traffic-steering-with-Gateway-API-HTTPRoute.html)
&nbsp;·&nbsp; Rating: 🟢
&nbsp;·&nbsp; Depends on: [`bgp-peer-frr`](../bgppeer)
&nbsp;·&nbsp; Wall time: **~21s** (no TMM restart; piggybacks on the existing NAD)

Builds on the BGP plumbing established by `bgp-peer-frr`. Applies
a Gateway with static `spec.addresses=[203.0.113.100]` plus an
HTTPRoute pointing at an nginx backend. TMM's ZeBOS sees the
Gateway IP as a kernel route and BGP-advertises it; FRR receives
it and installs:

```
203.0.113.100/32 via 192.168.99.X dev net1 proto bgp
```

The verify step execs **5 curls from inside the FRR pod**. FRR
is already on the NAD bridge with the BGP-learned route, so it's
a free, ready-made test client. The data path is:

```
FRR curl
  → FRR kernel route via net1
  → br-bnk-bgp Linux bridge
  → TMM net1
  → Gateway listener at 203.0.113.100
  → HTTPRoute → nginx Service → nginx pod
  → reverse path back to FRR
```

TMM's eth0 TCP hook is **completely bypassed** — packets reach
TMM via `net1`, a normal Linux interface in the pod netns.

## Manifests

| File | What it is |
|---|---|
| [`01-gatewayclass.yaml`](manifests/01-gatewayclass.yaml) | Cluster-wide `bnk-gatewayclass` (idempotent across scenarios) |
| [`02-namespace.yaml`](manifests/02-namespace.yaml) | `scn-httproute-e2e` |
| [`03-bnkgateway.yaml`](manifests/03-bnkgateway.yaml) | `F5BnkGateway` IP pool (203.0.113.100-200) |
| [`04-backend.yaml`](manifests/04-backend.yaml) | nginx Deployment + Service + ConfigMap (marker body) — plain Calico pod, no NAD |
| [`05-gateway.yaml`](manifests/05-gateway.yaml) | Gateway with `spec.addresses=203.0.113.100`, HTTP listener |
| [`06-httproute.yaml`](manifests/06-httproute.yaml) | HTTPRoute hostname `kindbnkctl.local`, backendRefs → nginx Service |

## How to run

```bash
kindbnkctl scenario run bgp-peer-frr   --poc <pocdir>   # if not running yet
kindbnkctl scenario run http-routing-e2e --poc <pocdir>
```

The scenario short-circuits with a clear error if the
`scn-bgp/scn-frr` pod isn't Running:

```
dependency missing: run `kindbnkctl scenario run bgp-peer-frr` first
```

## Verification

```
✓ nginx Deployment Available
✓ Gateway Programmed=True
✓ HTTPRoute Accepted=True
✓ FRR BGP table has 203.0.113.100/32 advertised by TMM
✓ FRR kernel route 203.0.113.100/32 installed via net1
✓ 5/5 end-to-end curls via Gateway return nginx marker body
```

Reproduce the curl manually:

```bash
kubectl -n scn-bgp exec deploy/scn-frr -c frr -- \
  curl -sS -H 'Host: kindbnkctl.local' http://203.0.113.100/
# → kindbnkctl-scenario-httproute-e2e-OK
```

## Cleanup

`kindbnkctl scenario clean http-routing-e2e` deletes the
`scn-httproute-e2e` namespace. GatewayClass stays cluster-wide
(reused by other scenarios).

## Gotcha worth knowing

Older versions of kindbnkctl shipped a separate `http-routing`
scenario; if its `scn-httproute` namespace is still around from a
prior run, two Gateways will compete for 203.0.113.100 and TMM
picks whichever was applied first. Symptom: 5×curl returns
`kindbnkctl-scenario-httproute-OK` (the OLD marker) instead of
the `-e2e-OK` one. Fix: `kubectl delete namespace scn-httproute`
and rerun.
