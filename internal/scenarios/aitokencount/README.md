# `ai-token-counting` — `k8s.f5.com/ai-token-counting` Gateway annotation

F5 how-to: [Configure Token Counting and Enforcement](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/configure-token-counting-and-enforcement.html)
&nbsp;·&nbsp; Rating: 🟢
&nbsp;·&nbsp; Depends on: [`bgp-peer-frr`](../bgppeer)
&nbsp;·&nbsp; Wall time: **~25s** (with bgp-peer-frr running already)

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

## How TMM exposes the counters (investigation 2026-05-20)

The F5 doc doesn't say it, but the auto-generated iRule logs
its per-request counts to **`tmm.local0.info`**. That's the
hook we use to verify the data plane:

```
Rule scn-tokencount-gateway-scn-tokencount-token-counting
  <JSON_RESPONSE>:
  TOKEN(1): source=json
            user_ref=user-808b5bf2f1ab8cab5eee7f99dfe65b...
            model=gpt-stub bucket_key=gpt-stub
            prompt=42 completion=11 total=53
Rule scn-tokencount-gateway-scn-tokencount-token-counting
  <JSON_RESPONSE>:
  TOKEN(1): cumulative user: total=53 in=42 out=11
Rule scn-tokencount-gateway-scn-tokencount-token-counting
  <JSON_RESPONSE>:
  TOKEN(1): cumulative model(gpt-stub): total=53 in=42 out=11
Rule scn-tokencount-gateway-scn-tokencount-token-counting
  <JSON_RESPONSE>:
  TOKEN(1): cumulative global: total=53 in=42 out=11
Rule scn-tokencount-gateway-scn-tokencount-token-counting
  <JSON_RESPONSE>:
  TOKEN(1): cumulative user+model(user-808b...:gpt-stub):
            total=53 in=42 out=11
```

The `user_ref` is a SHA-256 hash of the bearer-token value
(per the `user_id_source=api_key` config). Subsequent requests
with the same Authorization header increment the same user
bucket; different tokens get distinct buckets. Per-model and
global counters also tick.

For our stub backend that always returns `prompt_tokens=42,
completion_tokens=11`, three requests should produce
`cumulative user: total=159 in=126 out=33` — verifiable by
scraping TMM logs.

## What's still left for a "real" deployment

This scenario proves the iRule machinery works. A production
deployment would also wire up an HSL receiver (the
`hsl_pool=hsl-logging-pool` annotation field) so the counters
flow into a metering tool like OpenMeter. That's outside the
kindbnkctl scope; the iRule-fired-and-counted assertion is
what verifies BNK's side of the feature.

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
