# gRPC Routing Investigation — grpc-loadbalance (🟡 amber)

## Problem

`kindbnkctl scenario run grpc-loadbalance` passes all control-plane assertions but the data-plane test fails:

```
rpc error: code = Internal desc = stream terminated by RST_STREAM with error code: INTERNAL_ERROR
```

Cleartext gRPC traffic through the **GRPCRoute** Gateway is corrupted. Direct-to-backend gRPC works fine.

## Setup

- Gateway listener: `protocol: HTTP`, port `50051`
- Backend: `moul/grpcbin` on port `9000`
- Route: `GRPCRoute` → `Service` (grpcbin)
- FRR pod on Multus NAD bridge, BGP-learned Gateway IP `203.0.113.108`
- Verification: `grpcurl -plaintext` from FRR pod through Gateway IP

The listener/route config matches the F5 BNK GRPCRoute CRD reference
([source](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/custom-resource-definitions/bnk-gateway-api-grpcroute.html))
verbatim — `protocol: HTTP` port `50051`, a single rule, a single
`backendRefs`, no hostnames/matches/filters (all documented as
unsupported). So this is **not** a misconfiguration relative to the
docs; it's a genuine data-plane gap.

## Investigation

### TMM audit log analysis

TMM creates these profiles for both HTTP and HTTPS listeners:

```
scn-grpc-...-https-443-profile-http
scn-grpc-...-https-443-cl-profile-http2
scn-grpc-...-https-443-srv-profile-http2
scn-grpc-...-https-443-profile-httpcompression
scn-grpc-...-https-443-profile-httprouter
scn-grpc-...-https-443-profile-json
scn-grpc-...-https-443-profile-sbi         (HTTPS only)
scn-grpc-...-https-443-vs-profile-clientssl (HTTPS only)
```

The HTTP/json/httprouter profile chain applies to ALL L7 listener types.
These profiles perform content transformations that corrupt HTTP/2 binary frames.

### Approaches tested

| Approach | Result | Notes |
|---|---|---|
| GRPCRoute, Gateway `protocol: HTTP` port 50051 | ❌ RST_STREAM | Original config. `profile-http` + `profile-json` + `profile-httprouter` chain |
| GRPCRoute, Gateway `protocol: HTTPS` port 443 with TLS | ❌ RST_STREAM | HTTPS listener also gets `profile-http` + `profile-json` + `profile-httprouter`. Verified via TMM audit logs |
| `appProtocol: kubernetes.io/h2c` on Service | ❌ No effect on GRPCRoute | K8s hint for controller only; doesn't disable the client-side profile chain. (On an L4Route it is *worse* — see below) |
| Adding `profile-sbi` | ❌ No effect | SBI profile present but HTTP/json/httprouter chain still applied |
| curl TLS handshake verification | ✅ Works | TLS terminates correctly, HTTP/2 ALPN negotiation succeeds, RST comes later |

### Decisive experiment — L4Route (TCP) bypass

To isolate *which* layer corrupts the stream, the same `grpcbin:9000`
backend was bound to a raw **L4Route (TCP)** on a separate listener
(`203.0.113.109:50052`), bypassing the L7 HTTP/HTTP2/json/httprouter
profile chain entirely, and `grpcurl`'d from the same FRR pod:

| Path | `pvaAccelerationMode` | Result |
|---|---|---|
| GRPCRoute (L7 HTTP/2 virtual server) | n/a (L7) | ❌ RST_STREAM |
| L4Route (raw TCP) | `disabled` | ✅ reflection `list` + unary call succeed |
| L4Route (raw TCP) | `full/assisted` (accel ON) | ✅ still succeed |

Both an `ls`-equivalent reflection `list` and a real unary
`grpcbin.GRPCBin/Index` call returned correctly through the Gateway over
the L4 path.

Two findings fall out of this:

1. **PVA acceleration is ruled out.** gRPC over the L4 path works with
   `pvaAccelerationMode: full/assisted` (the default — acceleration ON),
   so disabling PVA changes nothing for gRPC. The fix that resolved the
   [`proxy-protocol-l4`](../internal/scenarios/proxyprotocol/README.md)
   amber (`pvaAccelerationMode: disabled`) does **not** transfer here.
   `pvaAccelerationMode` doesn't even exist on the GRPCRoute CRD — it is
   an `L4Route`-only field.
2. **The corruption is in the L7 profile chain, now proven by bypass.**
   Removing the HTTP/json/httprouter chain (by switching to L4) is the
   single change that makes gRPC work. This positively confirms the
   audit-log hypothesis rather than merely inferring it.

#### Caveat: `appProtocol: h2c` blocks the L4Route bind

The grpcbin Service in the scenario carries `appProtocol:
kubernetes.io/h2c`. An L4Route refuses to bind to it:

```
status.conditions: ResolvedRefs=False
  reason:  UnsupportedProtocol
  message: Referenced resource does not support this protocol or appProtocol
```

The L4 path requires a **plain-TCP Service** (no `appProtocol`) pointing
at the same pods. So `appProtocol: h2c` is not merely a no-op on the
data plane — on an L4Route it is actively rejected.

### What TMM logs show

For HTTPS listener on port 443:
- Virtual server created: `scn-grpc-scn-grpc-gateway-203.0.113.108-https-443-vs`
- Profiles: `clientssl` + `http` + `http2` + `httprouter` + `json` + `sbi` + `tcp`
- Pool member `10.244.157.167:9000` marked Up
- Connection error on gRPC flow: `not SSL` (when client sends cleartext to HTTPS listener)

## Root cause

BNK 2.3.0 FLO applies the L7 `profile-http` + `profile-json` +
`profile-httprouter` chain to every GRPCRoute virtual server (HTTP and
HTTPS alike), and that chain corrupts gRPC's HTTP/2 binary framing.
There is no raw HTTP/2 passthrough mode for GRPCRoute listeners. This is
now confirmed empirically: the identical backend carries gRPC correctly
the moment the L7 chain is removed (L4Route), independent of PVA
acceleration.

## Workaround in this shape

gRPC **can** be load-balanced end-to-end in the 2-node demo-TMM shape
today — by routing it as L4 instead of L7:

- Bind the gRPC backend to an **`L4Route` (protocol: TCP)** on a TCP
  listener (the same pattern as the green
  [`tcp-l4-loadbalance`](../internal/scenarios/tcpl4lb/) scenario —
  L4Route TCP at `pvaAccelerationMode: full/assisted`).
- Use a **plain-TCP Service** (drop `appProtocol: h2c`) so the L4Route
  resolves its `backendRefs`.

The trade-off: an L4Route is opaque TCP load balancing — no
gRPC/HTTP-2-aware features (per-method routing, weighting on gRPC
semantics, header matches). It is connection-level LB that happens to
carry HTTP/2 intact. That is why the scenario stays **amber for
GRPCRoute proper**: the Gateway API `GRPCRoute` CRD does not yet work on
the data plane in this shape, even though a raw-L4 substitute does.

## What's needed for green (GRPCRoute)

1. **FLO enhancement**: detect GRPCRoute and skip the HTTP/json/httprouter
   transformation chain, OR provide a raw HTTP/2 passthrough mode.
2. **OR**: a BNK profile-override path exposed through Gateway API CRDs or
   annotations.
3. **OR**: a dedicated GRPCRoute listener protocol that maps to passthrough.

## Remaining control-plane assertions

All pass:
- grpcbin Deployment Available
- Gateway Programmed=True
- GRPCRoute Accepted=True
- FRR BGP table has Gateway IP advertised by TMM
- Direct grpcurl-to-backend Service returns grpcbin.GRPCBin (proves backend healthy)

## References

- BNK Gateway API docs: [Configure HTTP traffic steering with Gateway API HTTPRoute](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/Configure-HTTP-traffic-steering-with-Gateway-API-HTTPRoute.html)
- BNK GRPCRoute CRD: [BNK Gateway API GRPCRoute](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/custom-resource-definitions/bnk-gateway-api-grpcroute.html)
- Related L4Route scenarios: [`tcp-l4-loadbalance`](../internal/scenarios/tcpl4lb/), [`proxy-protocol-l4`](../internal/scenarios/proxyprotocol/README.md)
- PR documenting findings: https://github.com/mwiget/kindbnkctl/pull/2
