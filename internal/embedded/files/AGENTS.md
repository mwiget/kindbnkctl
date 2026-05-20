# kindbnkctl PoC operations guide

This PoC repo deploys F5 BIG-IP Next for Kubernetes (BNK) 2.3.0 on a
two-node kind cluster — one combined control-plane + worker, one
worker dedicated to TMM. TMM runs in demo mode (virtio inside the pod
netns); no DPU, no SR-IOV, no Multus.

## Pipeline

```
validate  →  cluster up  →  deploy prereqs  →  deploy flo  →  deploy cne
```

End-to-end with one command:

```
kindbnkctl e2e --yolo --confirm-cluster <poc-name>
```

The full run typically takes 10–20 minutes on a laptop with a warm
Docker cache.

## Customer-supplied files

Drop these into `keys/` (gitignored) before running anything:

- `keys/f5-far-auth-key.tgz` — FAR image-pull credentials for repo.f5.com
- `keys/.jwt` — TEEM activation token

Both come from F5's normal license-portal channels.

## What's different from dpubnkctl

| Phase | dpubnkctl (bare-metal + DPU) | kindbnkctl (kind + demo TMM) |
|---|---|---|
| discover | probes hosts + DPUs over SSH | **dropped** |
| provision | BFB-flashes DPUs | **dropped** |
| host network | netplan VLAN sub-ifs | **dropped** |
| cluster up | kubespray over SSH | `kind create cluster` |
| deploy network | Multus / SR-IOV / NAD / F5SPKVlan | **dropped** |
| deploy flo | same | same |
| deploy cne | CNEInstance + License | same, demo-mode flipped on |

## bnk-forge

If `~/git/bnk-forge` (or `$KINDBNKCTL_BNK_FORGE_PATH`) exists when
`kindbnkctl init` runs, the PoC's `bnk_forge:` block gets pre-filled
and `cluster up` auto-registers the kind cluster with bnk-forge. If
the local bnk-forge stack isn't running, registration is skipped —
deployment never blocks on it.

`kindbnkctl` will not install or start bnk-forge for you.
