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

### Investigation 2026-05-20 — three patterns tried

The full CR chain works fine for L4Route:

- `F5BigCneIrule` reconciles (`status.conditions[Programmed]
  CR config sent to all grpc endpoints`).
- `BNKNetPolicy` reconciles (`ResolvedRefs True`,
  ancestorRef = Gateway, descendantRef = F5BigCneIrule).
- The CNE controller pushes the iRule body to TMM via gRPC
  AND adds `"irules_reference": ["scn-proxy-pp-prepend"]` to
  the virtual_server config for the L4 listener.
- TMM audit-logs `action: CREATE; UUID: scn-proxy-pp-prepend;
  event: declTmm.irule; Error: No error`.
- The iRule **fires** on connections — patched with
  `log tmm.local0.info "PP_IRULE: ..."` statements and TMM
  emits `<CLIENT_ACCEPTED>`, `<SERVER_CONNECTED>`, and
  `<CLIENT_DATA>` log lines with the correct
  client/local IPs and ports.

Three distinct injection patterns tested, all reach the line
and produce no observable wire output:

| Pattern | iRule TCL | Outcome |
|---|---|---|
| Canonical (matches F5 [DevCentral PROXY Initiator](https://community.f5.com/kb/codeshare/proxy-protocol-initiator/280541)) | `when SERVER_CONNECTED { TCP::respond $proxyhdr }` | iRule logs "called". nginx still gets raw `GET / HTTP/1.1`. |
| Explicit serverside context (the F5 TMOS workaround when the canonical fails) | `when SERVER_CONNECTED { serverside { TCP::respond $proxyhdr } }` | iRule logs "done". Wire unchanged. |
| Client-side payload manipulation (TCP::collect + TCP::payload replace + TCP::release in CLIENT_DATA) | `TCP::payload replace 0 0 $proxyhdr` in CLIENT_DATA | iRule logs "released after prepend". `TCP::payload length` returns empty even after `TCP::collect`. nginx wire unchanged. |

In every case nginx logs:
```
broken header: "GET / HTTP/1.1" while reading PROXY protocol
```

confirming the first bytes on the server-side socket are still
the raw HTTP request — the PROXY v1 line never appears.

### Diagnostic correction

The earlier (db42af8) note that `serverside { TCP::respond }`
was *"rejected by the F5 validation webhook"* was wrong. The
real rejection trigger was an em-dash character (`—`) in the
log strings I was using for debugging. The webhook error
`"braces are required around the expression"` is misleading:
it actually fires on any non-ASCII byte in the iRule body.
With ASCII-only strings the `serverside { }` block validates
cleanly — it just still doesn't inject anything.

### Why this likely doesn't work

TMM's L4Route listener uses the auto-generated `profile_bigproto`
profile (visible in the CNE controller gRPC trace), not a
standard TCP profile. The DevCentral PROXY Protocol Initiator
codeshare explicitly notes *"This requires a TCP profile to be
applied, so a 'Standard' Virtual Server will need to be used"*.
BNK's L4Route doesn't seem to offer a switch to use a standard
TCP profile instead of `bigproto` — verified empirically:

- `L4Route.spec` has only `parentRefs`, `protocol`,
  `pvaAccelerationMode`, `pvaDynamicClientPkts`,
  `pvaDynamicServerPkts`, `rules`. No profile knob.
- `F5BnkGateway.spec` has only `ingressConfig`. No profile knob.
- `Pool.spec.members[item]` has only `address`, `port`,
  `priorityGroup`. No `proxy_protocol` toggle.
- `F5BigCneIrule.spec` has only `iRule`, `namespace`, `tenant`.
  No profile pinning.

The auto-generated semantic-cache iRule (which DOES work) only
uses `TCP::respond` for **client-side** HTTP responses via
`HTTP::respond` and `TCP::close`, never for server-side TCP
injection — so the working iRules carefully avoid the broken
opcode.

### Exhaustive empirical results

| Event | Command | Validator | Runtime |
|---|---|---|---|
| `CLIENT_ACCEPTED` | `TCP::respond $hdr` | ✓ accepted | no-op (client got nothing back) |
| `SERVER_CONNECTED` | `TCP::respond $hdr` (default ctx) | ✓ accepted | no-op (nginx sees raw HTTP) |
| `SERVER_CONNECTED` | `serverside { TCP::respond $hdr }` | ✓ accepted | no-op |
| `LB_SELECTED` | `TCP::respond $hdr` | ✓ accepted | no-op |
| `CLIENT_DATA` after `TCP::collect` | `TCP::payload replace 0 0 $hdr` + `TCP::release` | ✓ accepted | `TCP::payload length` returns empty, no injection on wire |
| any | `TCP::send $hdr` | ✗ `undefined procedure: TCP::send` | n/a |
| any | `TCP::write $hdr` | ✗ `undefined procedure: TCP::write` | n/a |

So BNK 2.3's L4Route iRule subset has **no functional TCP-data
injection primitive at all**. The validator accepts `TCP::respond`
but its runtime is a stub. Alternative names (`TCP::send`,
`TCP::write`) don't exist in the dispatch table.

### What lifting to green would require

- F5 to implement `TCP::respond` (or an equivalent) in BNK's
  L4Route data-plane, OR
- BNK to expose a profile-switch on the Gateway listener so
  we can pin a standard TCP profile, OR
- document an alternative pattern we haven't found.

None of those are scenario-side workarounds. Amber stays.

Operators can still poke the data plane manually:
```bash
kubectl -n scn-bgp exec deploy/scn-frr -c frr -- \
  curl -sS --fail --max-time 5 http://203.0.113.102:8000/
# → curl: (52) Empty reply from server
kubectl -n scn-proxy logs deploy/pp-backend --tail=2
# → broken header: "GET / HTTP/1.1" while reading PROXY protocol
```

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
