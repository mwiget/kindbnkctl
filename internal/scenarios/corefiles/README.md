# `core-file-collection` — CNEInstance toggle + CoreMond reconcile

F5 how-to: [Set up core file collection](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/spk-coremond.html)
&nbsp;·&nbsp; Rating: 🟡
&nbsp;·&nbsp; Depends on: nothing

The how-to is a single CNEInstance toggle:

```yaml
spec:
  coreCollection:
    enabled: true
```

FLO responds by:

- creating a `CoreMond` CR (`coremonds.k8s.f5.com/f5-coremond`
  in `f5-cne-core`) — operator doesn't author this manifest
- creating a `f5-coremond` DaemonSet
- adding `kernel-cores`, `f5-core-store`, `tmm-core` volumes
  (with mounts) to the TMM Deployment template, so any
  kernel-side core dumps survive pod restarts.

## Why amber

The reconciled infrastructure is all asserted. What's **not**
asserted (hence amber):

- that a forced TMM crash actually deposits a core file at the
  expected host path. F5's how-to suggests
  `kubectl exec -n default <tmm-pod> -c f5-tmm -- kill -11 <tmm-pid>`
  for this. We don't automate it because:
  - crashing TMM mid-scenario leaves the cluster in a state
    that confuses other scenarios + the runtime cluster's own
    monitoring loops;
  - the follow-up "did the file land in /var/crash" check needs
    a privileged node-level read into the kind worker container,
    which is out of scope for a self-contained scenario.

Operators can run the kill manually after the scenario completes
and inspect `/var/crash` on the kind worker.

## Manifests

| File | What it is |
|---|---|
| [`01-cneinstance-patch.yaml`](manifests/01-cneinstance-patch.yaml) | Placeholder for audit — the actual patch is a JSON-merge applied via `kubectl patch` in `scenario.go` (we need to merge into the live object, not replace it) |

## How to run

```bash
kindbnkctl scenario run core-file-collection --poc <pocdir>
```

Apply is more lenient than other scenarios: the `rollout status`
wait for f5-tmm is best-effort (3 min, logs WARN on timeout)
rather than gating. Verify reads the Deployment template
directly, so it can prove the change landed even if the new pod
hasn't finished rolling out — useful when the worker is under
resource pressure from other scenarios.

## Verification

Required:

```
✓ FLO auto-created a CoreMond CR
✓ CoreMond DaemonSet exists with at least one desired replica
✓ TMM Deployment template includes a core-dump volume
```

Bonus (sometimes fails on a heavily-loaded worker; non-fatal):

```
✗ [bonus] CNEInstance condition (Coremond|CoreMon)Available=True
```

FLO surfaces this condition only once the CoreMond pod schedules
and stays Ready, which can lag.

## Cleanup

`kindbnkctl scenario clean core-file-collection` reverts:

- `CNEInstance.spec.coreCollection.enabled` → `false`
- TMM restarted so the new volumes drop out of the pod spec

FLO then garbage-collects the CoreMond CR + DaemonSet on its own.
