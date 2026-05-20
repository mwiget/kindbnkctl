# `ai-token-counting` — `k8s.f5.com/ai-token-counting` Gateway annotation

F5 how-to: [Configure Token Counting and Enforcement](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/configure-token-counting-and-enforcement.html)
&nbsp;·&nbsp; Rating: 🟡
&nbsp;·&nbsp; Depends on: nothing
&nbsp;·&nbsp; Wall time: **~6s**

Demonstrates BNK's AI token-counting feature. The mechanism is a
single annotation on `Gateway.spec.infrastructure.annotations`:

```yaml
k8s.f5.com/ai-token-counting: |
  token_counting=enabled,
  user_id_source=api_key,
  user_id_header=Authorization,
  fallback_to_ip=true,
  hsl_pool=hsl-logging-pool
```

No dedicated BNK CR is introduced. TMM reads the annotation,
parses incoming OpenAI-style `/v1/chat/completions` responses,
counts per-user tokens, and (if `enabled`) enforces quotas.

## Why amber

The F5 doc itself doesn't ship:

- a runnable LLM backend
- a curl/kubectl verification command
- any expected response to assert against

The scenario stands up a stub nginx that returns a single fixed
OpenAI-style JSON (`usage.prompt_tokens=42 / completion_tokens=11`)
and verifies the control-plane wiring is correct. Token counting
itself is a TMM data-plane feature; with no varying token output
and no HSL receiver to introspect, we can't validate the math.

Lifting this to green would require:

- a real LLM backend that emits varied token counts, OR a stub
  backend that varies `usage` based on prompt size
- an HSL log receiver to capture per-user counters
- F5-side tooling to introspect TMM's user_id buckets

## Manifests

| File | What it is |
|---|---|
| [`01-namespace.yaml`](manifests/01-namespace.yaml) | `scn-tokencount` |
| [`02-bnkgateway.yaml`](manifests/02-bnkgateway.yaml) | F5BnkGateway IP pool for 203.0.113.103 |
| [`03-backend.yaml`](manifests/03-backend.yaml) | Stub LLM (nginx returning fixed OpenAI-style JSON) + Service |
| [`04-gateway.yaml`](manifests/04-gateway.yaml) | Gateway with `spec.addresses=[203.0.113.103]`, HTTP listener on :8000, and the verbatim `k8s.f5.com/ai-token-counting` annotation under `spec.infrastructure.annotations` |
| [`05-httproute.yaml`](manifests/05-httproute.yaml) | HTTPRoute hostname `tokencount.kindbnkctl.local`, path `/v1/chat/completions` → stub-llm |

## How to run

```bash
kindbnkctl scenario run ai-token-counting --poc <pocdir>
```

## Verification (4/4)

```
✓ stub-llm Deployment Available
✓ Gateway Programmed=True
✓ HTTPRoute Accepted=True
✓ k8s.f5.com/ai-token-counting annotation present on Gateway
```

## Cleanup

`kindbnkctl scenario clean ai-token-counting` deletes the
`scn-tokencount` namespace.
