# `core-file-collection` — CNEInstance toggle + CoreMond reconcile

F5 how-to: [Set up core file collection](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/spk-coremond.html)
&nbsp;·&nbsp; Rating: 🟢
&nbsp;·&nbsp; Depends on: nothing
&nbsp;·&nbsp; Wall time: **~3m01s** (TMM rollover dominates)

The how-to is a single CNEInstance toggle, plus one kind-specific
knob we have to set alongside it (see below):

```yaml
spec:
  coreCollection:
    enabled: true
  advanced:
    coremon:
      hostPath: true       # critical on kind — see below
```

FLO responds by:

- creating a `CoreMond` CR (`coremonds.k8s.f5.com/f5-coremond`
  in `f5-cne-core`) — operator doesn't author this manifest
- creating a `f5-coremond` DaemonSet
- adding `kernel-cores`, `f5-core-store`, `tmm-core` volumes
  (with mounts) to the TMM Deployment template, so any
  kernel-side core dumps survive pod restarts.

## The kind-specific `hostPath: true` knob

The default CoreMond CR sets `spec.persistence.enabled: true,
accessMode: ReadWriteMany` and creates a PVC named
`coremond-pvc` for the core-dump destination. **kind's default
StorageClass (`rancher.io/local-path` / NodePath) only supports
ReadWriteOnce**, so the PVC stays `Pending` forever.

What that looks like without the workaround:

```
$ kubectl get events -n f5-cne-core
Warning ProvisioningFailed pvc/coremond-pvc
  failed to provision volume with StorageClass "standard":
  NodePath only supports ReadWriteOnce and ReadWriteOncePod (1.22+)

Warning FailedScheduling pod/f5-coremond-XXX
  running PreBind plugin "VolumeBinding": binding volumes:
  pod does not exist any more: pod "f5-coremond-XXX" not found
```

Chain of consequence: PVC Pending → scheduler's `VolumeBinding`
PreBind plugin can't bind → DaemonSet controller times out and
recreates the pod → race between scheduler-bind and
controller-delete-recreate → the pod never reaches Ready →
`CNEInstance.status.conditions[CoremondAvailable]` stays `False`.

Setting `advanced.coremon.hostPath: true` bypasses the PVC
entirely — CoreMond bind-mounts `/home/crash/f5` from the
worker host directly. The DaemonSet pod schedules cleanly in
~30s and CoremondAvailable flips True.

This matches F5's own how-to mention that "Coremond supports
storing core files directly on a host directory, eliminating
the need for ReadWriteMany volumes" — they just don't surface
that the knob is essential on kind-like clusters.

## What's NOT verified

The "`kill -11 $(pidof tmm)` to force a crash" step F5
mentions still isn't automated. Reasons:

- Crashing TMM disrupts any other concurrent scenarios — they
  all rely on TMM being responsive (BGP-dependent scenarios
  in particular).
- The reconciled-infrastructure + CoremondAvailable=True
  assertions already prove the feature is plumbed correctly;
  what's left is "does a kernel core dump actually get
  captured and processed by CoreMond", which is a TMM-internal
  data-plane concern that the F5 doc doesn't suggest verifying
  beyond eyeballing the file.

Operators who want to verify capture end-to-end can run, after
the scenario completes:

```bash
TMM=$(kubectl -n default get pod -l app=f5-tmm \
        --field-selector=status.phase=Running \
        -o jsonpath='{.items[0].metadata.name}')
kubectl -n default exec $TMM -c f5-tmm -- sh -c '
  kill -11 $(pidof tmm) 2>/dev/null || pgrep -f tmm | xargs kill -11
'
sleep 60
docker exec smoke-worker ls -la /home/crash/f5
```

## Manifests

| File | What it is |
|---|---|
| [`01-cneinstance-patch.yaml`](manifests/01-cneinstance-patch.yaml) | Placeholder for audit — the actual patch is a JSON-merge applied via `kubectl patch` in `scenario.go` (we need to merge into the live object, not replace it) |

## How to run

```bash
kindbnkctl scenario run core-file-collection --poc <pocdir>
```

## Verification (4/4 required)

```
✓ FLO auto-created a CoreMond CR
✓ CoreMond DaemonSet exists with at least one desired replica
✓ TMM Deployment template includes a core-dump volume
✓ CNEInstance condition (Coremond|CoreMon)Available=True
```

## Cleanup

`kindbnkctl scenario clean core-file-collection` reverts both
flags on CNEInstance and restarts TMM to drop the hostPath
mounts. FLO garbage-collects the CoreMond CR + DaemonSet.

## Investigation history

Earlier iterations of this scenario stayed amber because the
CoremondAvailable bonus assertion was unreliable. Live tracing
(2026-05-20) found the PVC-provisioning chain described above.
The `advanced.coremon.hostPath: true` workaround was hiding in
plain sight on the CoreMond CRD schema.

The earlier "FLO null-`crashagentConfig` reconcile loop"
diagnosis was orthogonal — that loop is still real and noisy,
but it doesn't actually block this scenario once
`hostPath: true` is set (the PVC is what blocked it, not FLO).
