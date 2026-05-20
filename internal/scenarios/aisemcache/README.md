# `ai-semantic-cache` — `k8s.f5.com/ai` semantic-cache + SSE annotations

F5 how-to: [Semantic AI Model Caching](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/ai-related-features/ai-semantic-caching.html)
&nbsp;·&nbsp; Rating: 🟡
&nbsp;·&nbsp; Depends on: nothing
&nbsp;·&nbsp; Wall time: **~9s**

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

## Why amber

The control-plane wiring works on kind. The data plane needs a
real **CodeFuse-ModelCache** (vector storage + embedding model
+ working ML stack) and a real LLM backend — out of scope here.

What the scenario stands up:

- a stub LLM nginx returning a fixed OpenAI-style JSON
- a stub TCP listener at port 5050 to stand in for the
  ModelCache endpoint (TMM dials it on each request, gets a
  clean TCP accept but no useful protocol response → every
  request falls through to the stub LLM as "cache miss")
- Gateway + HTTPRoute with both annotations

Verifies that the annotations reconcile cleanly and survive on
the live objects.

Lifting this to green would require deploying real
CodeFuse-ModelCache + its vector backend, a real LLM (NIM/vLLM)
as the cache-miss path, then sending two identical prompts and
observing one HIT + one MISS in TMM's data-plane telemetry.

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
