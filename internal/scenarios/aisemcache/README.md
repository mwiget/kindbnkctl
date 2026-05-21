# `ai-semantic-cache` — `k8s.f5.com/ai` semantic-cache + SSE annotations

F5 how-to: [Semantic AI Model Caching](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/ai-related-features/ai-semantic-caching.html)
&nbsp;·&nbsp; Rating: 🟢
&nbsp;·&nbsp; Depends on: [`bgp-peer-frr`](../bgppeer)
&nbsp;·&nbsp; Wall time: **~22s** (with bgp-peer-frr running already)

Two annotations together enable BNK's semantic caching. On the
Gateway:

```yaml
k8s.f5.com/ai: |
  semantic_cache=enabled,
  semantic_cache_ip_port=<modelcache IP>:<port>,
  semantic_cache_recv_timeout=1000
```

On the HTTPRoute:

```yaml
k8s.f5.com/sse-enabled: "true"
```

(SSE pairs with semantic caching because LLM completions commonly
stream via Server-Sent Events.)

On a cache **HIT**, TMM returns the cached response straight from
the configured CodeFuse-ModelCache endpoint. On **MISS**, the
request continues to the HTTPRoute's `backendRefs` and the
response is stored in the cache.

## How TMM exposes the iRule's behavior (investigation 2026-05-20)

The auto-generated iRule logs the events we need to assert
data-plane wiring:

```
Rule scn-semcache-gateway-scn-semcache-semantic-cache
  <CLIENT_ACCEPTED>:
  Client initialized with modelcache_server=10.96.31.232:5050,
  modelcache_recv_timeout=1000
Rule scn-semcache-gateway-scn-semcache-semantic-cache
  <HTTP_REQUEST>:
  SEMANTIC_CACHE_IRULE: HTTP_REQUEST triggered
  /v1/chat/completions client=192.168.99.47 method=POST
```

The CLIENT_ACCEPTED log line proves the iRule read the
modelcache endpoint from the `k8s.f5.com/ai` annotation
correctly. The HTTP_REQUEST log line proves the iRule fires
on each incoming completion request and would have queried
the cache if a real one were running.

The scenario sends 3 POSTs with identical bodies and asserts:

1. All 3 return the stub-llm marker body (cache-miss path
   completes — i.e. the iRule doesn't break the request flow
   even when the cache responds with garbage).
2. TMM logs contain both `Client initialized with
   modelcache_server=` and `SEMANTIC_CACHE_IRULE: HTTP_REQUEST
   triggered`.

## What's still left for a "real" deployment

The cache HIT path requires a real CodeFuse-ModelCache speaking
its protobuf protocol. We can't verify HIT-vs-MISS behavior
without it. What we DO verify is that BNK's side of the feature
is plumbed correctly: the iRule attaches to the listener, fires
on every request, reads the annotation, and lets the
non-matching path fall through to the LLM backend.

## Manifests

| File | What it is |
|---|---|
| [`01-namespace.yaml`](manifests/01-namespace.yaml) | `scn-semcache` |
| [`02-bnkgateway.yaml`](manifests/02-bnkgateway.yaml) | F5BnkGateway IP pool for 203.0.113.104 |
| [`03-stubs.yaml`](manifests/03-stubs.yaml) | stub-llm (nginx) + stub-modelcache (socat TCP listener on :5050) Deployments + Services |
| [`04-gateway.yaml.tmpl`](manifests/04-gateway.yaml.tmpl) | text/template — Gateway with `spec.addresses=[203.0.113.104]` and the `k8s.f5.com/ai` annotation; `{{.CacheIP}}` is the stub-modelcache Service ClusterIP, filled in at apply time |
| [`05-httproute.yaml`](manifests/05-httproute.yaml) | HTTPRoute hostname `semcache.kindbnkctl.local`, path `/v1/chat/completions` → stub-llm; carries `k8s.f5.com/sse-enabled: "true"` annotation |

## How to run

```bash
kindbnkctl scenario run ai-semantic-cache --poc <pocdir>
```

## Verification (6/6)

```
✓ stub-llm Deployment Available
✓ stub-modelcache Deployment Available
✓ Gateway Programmed=True
✓ HTTPRoute Accepted=True
✓ k8s.f5.com/ai semantic-cache annotation present on Gateway
✓ k8s.f5.com/sse-enabled annotation present on HTTPRoute
```

## Cleanup

`kindbnkctl scenario clean ai-semantic-cache` deletes the
`scn-semcache` namespace.
