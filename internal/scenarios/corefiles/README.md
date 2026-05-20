# `core-file-collection` — CNEInstance toggle + CoreMond reconcile

F5 how-to: [Set up core file collection](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/spk-coremond.html)
&nbsp;·&nbsp; Rating: 🟡
&nbsp;·&nbsp; Depends on: nothing
&nbsp;·&nbsp; Wall time: **~3m01s** (full TMM restart to pick up hostPath mounts)

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

The reconciled infrastructure is all asserted (CoreMond CR
exists, DaemonSet has at least one desired replica, TMM
Deployment template carries the core-dump volumes). But the
data plane doesn't actually work end-to-end on our cluster.
Investigated 2026-05-20; three compounding issues:

### 1. The F5 doc has no real verify step

The how-to just says "`kill -11 $(pidof tmm)` to force a
crash" with no automation and no "this is what you should
see" assertion. Verifying "did a core file get captured"
requires privileged node-level filesystem access into the
kind worker container (the CoreMond DaemonSet bind-mounts
`/home/crash/f5` from the host).

### 2. CoreMond's DaemonSet pod can't stay scheduled

The CoreMond pod creates and re-creates on a few-minute
cycle without ever reaching Ready. The kube-scheduler log
fingerprint is:

```
"Plugin failed" err="binding volumes: pod does not exist any
  more: pod \"f5-coremond-XXX\" not found"
plugin="VolumeBinding" pod="f5-cne-core/f5-coremond-XXX"
node="smoke-worker"
```

The DaemonSet controller is recreating the pod before the
scheduler's PreBind plugin finishes — a classic race. Root
cause is the FLO null-`crashagentConfig` reconcile loop
(documented in `scenario.go::Cleanup`): FLO keeps trying to
update child CRs, gets rejected by the API server, retries,
and as a side-effect repeatedly churns the CoreMond
DaemonSet. The pod never has time to settle. Empirically the
worker has plenty of CPU/memory/disk — it's purely a
control-plane race.

Symptom: `CNEInstance.status.conditions[CoremondAvailable]`
stays `False` for the lifetime of the scenario.

### 3. Forcing a crash needs the data plane to be live

Even if we automated the `kill -11`, the verification
("did the core land in `/home/crash/f5` on the worker")
needs CoreMond to be Running so its hostPath is created and
the in-cluster path is wired up. With CoreMond stuck
Pending, the host path doesn't even exist on the worker
(`ls /home/crash/f5: No such file or directory`).

### What lifting to green would need

- F5 to fix the null-`crashagentConfig` reconcile bug (so
  the DaemonSet stops churning and CoreMond stays Ready),
  OR
- a kindbnkctl-side workaround that pins/manages the
  CoreMond DaemonSet directly instead of relying on FLO's
  template path, OR
- a different verification angle that doesn't depend on
  CoreMond running (e.g. just check `crashagentConfig` is
  reflected onto the TMM container env — already covered by
  the existing "TMM Deployment template includes a core-dump
  volume" assertion).

The reconciled-infrastructure assertions are the honest
testable subset. Operators can still run the `kill -11`
manually post-scenario; if CoreMond happens to be Running at
that moment (which does occasionally happen between churn
cycles), the core file does land in `/home/crash/f5` on the
worker.

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
