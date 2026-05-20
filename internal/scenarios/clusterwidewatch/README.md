# `cluster-wide-watch` — single controller reconciling a fresh namespace

F5 how-to: [Components needing cluster-wide access](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/spk-whole-cluster.html)
&nbsp;·&nbsp; Rating: 🟢
&nbsp;·&nbsp; Depends on: nothing
&nbsp;·&nbsp; Wall time: **~6s**

The F5 how-to itself is architectural prose — zero YAML, zero
kubectl commands. Two load-bearing claims:

- "A single BIG-IP Next for Kubernetes Controller […] watches
  multiple namespaces."
- "All BIG-IP Next for Kubernetes pods […] run in the same
  namespace."

Our kindbnkctl deploy already runs FLO in that posture:

- `CNEInstance.spec.watchNamespaces = ["All"]`
- `f5-cne-controller` is a single Deployment in `default` (1
  replica)
- `f5-cne-core` namespace hosts the shared CWC + DSSM +
  RabbitMQ + cert-manager + Fluentd

This scenario doesn't reconfigure anything — it stands witness
to the architectural claim by applying a brand-new namespace
+ Gateway + HTTPRoute + nginx backend, and asserting the
existing single controller picks them up.

## Manifests

| File | What it is |
|---|---|
| [`01-namespace.yaml`](manifests/01-namespace.yaml) | `scn-cwatch` |
| [`02-bnkgateway.yaml`](manifests/02-bnkgateway.yaml) | F5BnkGateway IP pool for 203.0.113.105 |
| [`03-backend.yaml`](manifests/03-backend.yaml) | nginx Deployment + Service + ConfigMap |
| [`04-gateway.yaml`](manifests/04-gateway.yaml) | Gateway with `spec.addresses=[203.0.113.105]`, HTTP listener |
| [`05-httproute.yaml`](manifests/05-httproute.yaml) | HTTPRoute hostname `cwatch.kindbnkctl.local`, backendRefs → nginx |

## How to run

```bash
kindbnkctl scenario run cluster-wide-watch --poc <pocdir>
```

## Verification (5/5)

```
✓ CNEInstance.spec.watchNamespaces contains "All"
✓ f5-cne-controller Deployment is a single replica
✓ nginx Deployment in scn-cwatch Available
✓ Gateway in scn-cwatch Programmed=True (cross-namespace reconcile)
✓ HTTPRoute in scn-cwatch Accepted=True
```

The combo — single-replica controller + a brand-new namespace's
Gateway becoming Programmed — is the concrete proof of the
doc's "single controller watching multiple namespaces" claim.

## Cleanup

`kindbnkctl scenario clean cluster-wide-watch` deletes the
`scn-cwatch` namespace.
