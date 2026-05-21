# `cwc-admin-access` — bearer token + mTLS to the CWC admin API

F5 how-to: [Restrict access to sensitive data](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/spk-admin-access-api.html)
&nbsp;·&nbsp; Rating: 🟢
&nbsp;·&nbsp; Depends on: nothing (runs standalone, no `bgp-peer-frr` needed)
&nbsp;·&nbsp; Wall time: **~9s** (the fastest scenario — just deploys a curl probe pod)

Demonstrates BNK's dual-gate access control on the Cluster-Wide
Controller (CWC) admin API at
`https://f5-spk-cwc.f5-cne-core.svc:38081/status`:

- **mTLS** at the TLS layer — CA + client cert + key from the
  `cwc-license-client-certs` Secret in `f5-cne-core`.
- **Bearer token** at the HTTP layer — string in the
  `cwc-auth-token` Secret in `f5-cne-core`.

Both materials are produced by the `deploy flo` phase already
(the `gen_cert.sh` step generates `cwc-license-client-certs`,
CWC's own controller generates `cwc-auth-token`). The scenario
just replicates them into its own namespace and probes the
endpoint.

## Manifests

| File | What it is |
|---|---|
| [`01-namespace.yaml`](manifests/01-namespace.yaml) | `scn-cwcadmin` |
| [`02-probe.yaml`](manifests/02-probe.yaml) | A small `curlimages/curl` Deployment with both Secrets projected as files (mounted at `/certs` and `/token`) |

Replication of `cwc-license-client-certs` + `cwc-auth-token`
from `f5-cne-core` into `scn-cwcadmin` is done in code at apply
time — kubelet doesn't allow cross-namespace Secret mounts, so
the scenario keeps its own copy.

## How to run

```bash
kindbnkctl scenario run cwc-admin-access --poc <pocdir>
```

## Verification (4/4)

```
✓ probe Deployment Available
✓ Authenticated GET /status returns HTTP 200 with license JSON
✓ Unauthenticated GET /status is rejected (no token in header)
✓ Bogus token is rejected (constant-time compare)
```

Reproduce manually:

```bash
kubectl -n scn-cwcadmin exec deploy/probe -- sh -c '
  curl -sS --cacert /certs/ca-root-cert \
       --cert /certs/client-cert \
       --key  /certs/client-key \
       -H "Authorization: Bearer $(cat /token/token)" \
       https://f5-spk-cwc.f5-cne-core.svc.cluster.local:38081/status
'
# → JSON with LicenseDetails + LicenseStatus
```

## Cleanup

`kindbnkctl scenario clean cwc-admin-access` deletes the
`scn-cwcadmin` namespace.

## Documentation drift to know about

The F5 doc still says the SharedComponentNamespace is `f5-utils`;
BNK 2.3 moved everything to `f5-cne-core` (matches
`internal/deploy/cwc_certs.go::SharedComponentNamespace`). The
scenario hardcodes `f5-cne-core`.
