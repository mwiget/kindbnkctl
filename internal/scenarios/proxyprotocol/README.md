# `proxy-protocol-l4` — F5BigCneIrule + L4Route + BNKNetPolicy

F5 how-to: [Proxy Protocol iRule support for L4 routes](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/proxy-protocol.html)
&nbsp;·&nbsp; Rating: 🟡
&nbsp;·&nbsp; Depends on: [`bgp-peer-frr`](../bgppeer)
&nbsp;·&nbsp; Wall time: **~24s** (no TMM restart)

Demonstrates BNK's PROXY-protocol iRule pattern on a TCP route.
Three new BNK CRs come together:

- **`F5BigCneIrule`** — the iRule TCL script. On `CLIENT_ACCEPTED`
  captures `[IP::client_addr]` + `[TCP::client_port]`; on
  `SERVER_CONNECTED` prepends a PROXY v1 line to the server-side
  payload via `TCP::respond`.
- **`L4Route`** (`gateway.k8s.f5net.com/v1`) — TCP-protocol route
  binding a Gateway listener to a backend Service (analogous to
  HTTPRoute but for raw L4).
- **`BNKNetPolicy`** (`gateway.k8s.f5net.com/v1alpha1`) — wires
  the iRule (`extensionRefs`) to the Gateway listener
  (`targetRefs`) so the iRule fires on that listener's traffic.

The nginx backend has `listen 80 proxy_protocol` configured so
it parses the PROXY header and exposes the original client
address as `$proxy_protocol_addr` — the response body echoes
that value, making end-to-end PROXY plumbing easy to assert.

## Why amber, not green

All six control-plane assertions pass. The `[bonus]`
data-plane assertion fails on this BNK 2.3 build:

- TMM accepts the L4 connection and proxies it through.
- The iRule's `TCP::respond` does not actually inject the
  PROXY v1 header before the server-side payload.
- nginx logs `broken header: "GET / HTTP/1.1" while reading
  PROXY protocol` and returns "Empty reply from server".

Untested hypotheses for why:

1. `TCP::respond` semantics may differ on BNK L4Route vs
   BIG-IP TMOS; might require `TCP::collect` / `TCP::release`
   or a different event.
2. `BNKNetPolicy` iRule attachment may be HTTP-only in this
   build, not honored for L4Route TCP listeners.
3. `sectionName` routing on `BNKNetPolicy` might not propagate
   to TMM's iRule processor for non-HTTP listeners.

The CR-wiring half is still a useful demonstration of how the
new BNK CRs slot together. Until BNK fixes (or documents
differently), the data-plane curl stays as a `[bonus]`
non-fatal assertion.

## Manifests

| File | What it is |
|---|---|
| [`01-namespace.yaml`](manifests/01-namespace.yaml) | `scn-proxy` |
| [`02-bnkgateway.yaml`](manifests/02-bnkgateway.yaml) | `F5BnkGateway` IP pool for 203.0.113.102 |
| [`03-backend.yaml`](manifests/03-backend.yaml) | nginx Deployment + Service + ConfigMap; `listen 80 proxy_protocol` so it parses the PROXY header (and rejects plain HTTP) |
| [`04-gateway.yaml`](manifests/04-gateway.yaml) | Gateway with TCP listener on port 8000 (matching the F5 doc), `allowedRoutes.kinds = L4Route` |
| [`05-irule.yaml`](manifests/05-irule.yaml) | `F5BigCneIrule` with the PROXY v1 iRule TCL |
| [`06-l4route.yaml`](manifests/06-l4route.yaml) | `L4Route` binding the listener to the backend Service |
| [`07-bnknetpolicy.yaml`](manifests/07-bnknetpolicy.yaml) | `BNKNetPolicy` linking iRule → Gateway listener (kind=Gateway is the only allowed `targetRefs.kind`; sectionName=tcp-listener) |

## How to run

```bash
kindbnkctl scenario run bgp-peer-frr     --poc <pocdir>   # if not running
kindbnkctl scenario run proxy-protocol-l4 --poc <pocdir>
```

## Verification

Required (6/6 must pass):

```
✓ pp-backend Deployment Available
✓ Gateway Programmed=True
✓ L4Route Accepted=True
✓ F5BigCneIrule pp-prepend exists
✓ BnkNetPolicy scn-proxy-attach exists
✓ FRR BGP table has 203.0.113.102/32 advertised by TMM
```

Bonus (will fail on BNK 2.3 — see "Why amber" above):

```
✗ [bonus] 5/5 L4 curls carry PROXY header parsed by nginx
```

Reproduce the bonus check manually:

```bash
kubectl -n scn-bgp exec deploy/scn-frr -c frr -- \
  curl -sS --fail http://203.0.113.102:8000/
# → currently: curl: (52) Empty reply from server
# nginx logs (kubectl logs -n scn-proxy deploy/pp-backend) show:
# "broken header: 'GET / HTTP/1.1' while reading PROXY protocol"
```

## Cleanup

`kindbnkctl scenario clean proxy-protocol-l4` deletes the
`scn-proxy` namespace.
