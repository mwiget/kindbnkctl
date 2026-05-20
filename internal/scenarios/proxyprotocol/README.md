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
data-plane assertion fails on this BNK 2.3 build.

### Root cause (investigated 2026-05-20)

Hypothesis 2 + 3 from earlier — *"BNKNetPolicy iRule
attachment may be HTTP-only"* — turned out to be **wrong**.
The full chain works fine for L4Route:

- `F5BigCneIrule` reconciles (`status.conditions[Programmed]
  CR config sent to all grpc endpoints`).
- `BNKNetPolicy` reconciles (`ResolvedRefs True`,
  ancestorRef = Gateway, descendantRef = F5BigCneIrule).
- The CNE controller pushes the iRule body to TMM via gRPC
  AND adds `"irules_reference": ["scn-proxy-pp-prepend"]` to
  the virtual_server config for the L4 listener.
- TMM audit-logs `action: CREATE; UUID: scn-proxy-pp-prepend;
  event: declTmm.irule; Error: No error`.
- The iRule **fires** on connections — we patched it with
  `log tmm.local0.info "PP_IRULE: ..."` statements and TMM
  emits both `<CLIENT_ACCEPTED>` and `<SERVER_CONNECTED>` log
  lines with the correct client/local IPs and ports.

Hypothesis 1 — *"TCP::respond semantics differ on BNK L4Route
vs BIG-IP TMOS"* — is the actual finding. Concretely:

- **`TCP::respond` in `SERVER_CONNECTED` is a silent no-op
  on BNK 2.3 L4Route listeners.** The iRule reaches the line
  and Tcl reports no error, but nothing is injected.
- We probed by patching the iRule to `TCP::respond
  "INVESTIGATION_MARKER_LINE\r\n"`. Neither the client
  (FRR's `nc`) nor the server (nginx) sees the bytes —
  nginx still logs `broken header: "GET / HTTP/1.1" while
  reading PROXY protocol`, and the client receives nothing.
  The bytes are dropped on the floor.
- The F5 TMOS workaround `serverside { TCP::respond
  $proxyhdr }` is **rejected by the F5 validation webhook**:
  `admission webhook "f5validate.f5net.com" denied the
  request: braces are required around the expression`. So
  the documented-elsewhere "force server-side context" form
  isn't available here either.

The CR-wiring half is genuinely useful as a demonstration
of how BNK's three iRule-related CRs slot together. The
data-plane gap is a BNK-build limitation (iRule TCL subset
on L4Route flows), not a configuration error in this
scenario. Lifting to green requires F5 to either implement
`TCP::respond` injection for L4Route or document an
alternative pattern.

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
