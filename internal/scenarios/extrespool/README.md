# `external-resource-pool` — BNK Pool CR as an HTTPRoute backend

F5 how-to: [Configure Load Balance Traffic to External Resources](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/configure-external-resource-load-balancing.html)
&nbsp;·&nbsp; Rating: 🟢
&nbsp;·&nbsp; Depends on: [`bgp-peer-frr`](../bgppeer)
&nbsp;·&nbsp; Wall time: **~14s** (no TMM restart)

The novelty over `http-routing-e2e` is the BNK
**`Pool`** CR (`group: k8s.f5net.com, kind: Pool`). HTTPRoute's
`backendRefs` points at the Pool, not at a Service.
`Pool.spec.members` lists endpoints by `address` + `port` —
production BNK uses this for VMs / bare-metal / other clusters
that aren't in the local Service registry.

On kind we simulate "external" with a plain Calico nginx pod;
the Pool member references its `status.podIP`. TMM reaches it
via normal cross-pod Kubernetes networking, exactly what BNK
does in production when a Pool member is any reachable IP.
(Earlier iterations attached the backend to the bnk-bgp NAD,
but that just coupled the backend to the same node as TMM
without adding capability.)

## Manifests

| File | What it is |
|---|---|
| [`01-namespace.yaml`](manifests/01-namespace.yaml) | `scn-extres` |
| [`02-bnkgateway.yaml`](manifests/02-bnkgateway.yaml) | `F5BnkGateway` IP pool for 203.0.113.101 (a separate /32 from `http-routing-e2e`'s .100 so they don't collide) |
| [`03-backend.yaml`](manifests/03-backend.yaml) | nginx Deployment + Service + ConfigMap (marker body) — plain Calico pod |
| [`04-gateway.yaml`](manifests/04-gateway.yaml) | Gateway with `spec.addresses=203.0.113.101`, HTTP listener |
| [`05-pool.yaml.tmpl`](manifests/05-pool.yaml.tmpl) | text/template — Pool CR with `{{.BackendIP}}` filled at apply time from `ext-backend`'s `status.podIP` |
| [`06-httproute.yaml`](manifests/06-httproute.yaml) | HTTPRoute hostname `extres.kindbnkctl.local`, `backendRefs` → Pool CR |

## How to run

```bash
kindbnkctl scenario run bgp-peer-frr           --poc <pocdir>   # if not running
kindbnkctl scenario run external-resource-pool --poc <pocdir>
```

## Verification

```
✓ ext-backend Deployment Available
✓ Gateway Programmed=True
✓ HTTPRoute Accepted=True
✓ Pool CR has at least one member entry on :80
✓ FRR BGP table has 203.0.113.101/32 advertised by TMM
✓ 5/5 curls via Gateway return ext-backend marker body
```

Reproduce manually:

```bash
kubectl -n scn-bgp exec deploy/scn-frr -c frr -- \
  curl -sS -H 'Host: extres.kindbnkctl.local' http://203.0.113.101/
# → kindbnkctl-scenario-extres-pool-OK
```

## Cleanup

`kindbnkctl scenario clean external-resource-pool` deletes the
`scn-extres` namespace. GatewayClass + `bnk-bgp` NAD stay
cluster-wide (reused by other scenarios).
