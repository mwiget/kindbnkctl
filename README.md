# kindbnkctl

![BNK](https://img.shields.io/badge/BNK-2.3.0-0a3a5c)
![Kubernetes](https://img.shields.io/badge/Kubernetes-1.30.8-326ce5?logo=kubernetes&logoColor=white)
![kind](https://img.shields.io/badge/kind-v0.26%2B-1f6feb)
![Go](https://img.shields.io/github/go-mod/go-version/mwiget/kindbnkctl)
![License](https://img.shields.io/github/license/mwiget/kindbnkctl)
![Last commit](https://img.shields.io/github/last-commit/mwiget/kindbnkctl)

Single-binary CLI that deploys F5 BIG-IP Next for Kubernetes (BNK) 2.3.0
on a two-node [kind](https://kind.sigs.k8s.io/) cluster — one combined
control-plane + worker, one worker dedicated to TMM running in demo
mode (virtio inside the pod netns; no DPU, no SR-IOV, no Multus).

Aimed at low-spec corporate laptops where dpubnkctl's bare-metal +
DPU pipeline is overkill. Same poc.yaml-driven, resume-safe shape;
much shorter pipeline.

## What this tool does

Drives a BNK deployment in three phases:

1. **cluster up** — `kind create cluster`, install Calico (acts as a
   simulator for larger SR-IOV deployments), create internal + external
   docker bridge networks and attach both to every kind node container,
   label the worker for TMM, fetch kubeconfig.
2. **deploy prereqs** — namespaces, FAR pull secret, cert-manager.
3. **deploy flo + cne** — FLO from the release-manifest chart at
   `repo.f5.com`, License CR with the operator's JWT, CNEInstance with
   `advanced.demoMode.enabled: true` and TMM pinned via `nodeSelector:
   app=f5-tmm`.

Symmetric **`destroy`** unwinds it: bnk-forge unregister → `kind
delete cluster` → docker network rm.

## Pinned versions

| Component | Version |
|---|---|
| BNK | 2.3.0 |
| CNE release manifest | 2.3.0-3.2598.3-0.0.170 |
| Kubernetes (kind node image) | 1.30.8 (kind v0.26 ships this) |
| Calico | v3.28.2 |
| cert-manager | v1.16.2 |
| FLO chart | resolved at deploy time from the release manifest |

## Minimum host resources

First-measurement floor from a verified end-to-end run on linux/amd64
(kindbnkctl init → e2e → CNEInstance.Available=True, all 16 components
green, TMM 6/6 Running, License Active):

| Baseline | With bnk-forge |
|---|---|
| **4 cores** | **5 cores** |
| **6 GB RAM** | **8 GB RAM** |
| **~8 GB free disk** | **~10 GB free disk** |

Where the memory goes (measured at steady state):

| Component | Working set | CPU |
|---|---|---|
| TMM pod (worker)        | ~1.17 GB | ~100m |
| kube-apiserver          | ~900 MB  | ~150m |
| All other F5 pods (20)  | ~1.0 GB  | ~470m |
| Calico + coredns + etcd + kube-* | ~700 MB | ~150m |
| Kernel / runtime per node | ~500 MB × 2 | — |
| **Total cluster**       | **~4.5 GB pod RSS + ~1 GB overhead** | **~900m steady, peaks ~1.2c during TMM init** |

Disk: 1.4 GB (kindest/node image) + ~2.4 GB (F5 container images pulled
to the worker) + ~0.5 GB (cert-manager, alpine/k8s tooling, manifests)
+ ~2 GB headroom for kind cluster state and logs.

macOS Docker Desktop runs the cluster inside a Linux VM — add ~2 GB
to the baseline to cover that VM's own overhead. Same applies to
Rancher Desktop / Colima.

`kindbnkctl doctor` reports the host's actual CPU count and warns
when it falls below `MinBaseline`. Override the constants in
`internal/version/version.go` if your environment is tighter or
fatter than the defaults.

## bnk-forge integration

If a local [bnk-forge](https://github.com/sp-prod-field/bnk-forge)
clone exists at `~/git/bnk-forge` (or `$KINDBNKCTL_BNK_FORGE_PATH`)
when `kindbnkctl init` runs, the new PoC's `bnk_forge:` block is
pre-filled and enabled. On `cluster up`, kindbnkctl best-effort
registers the kind cluster with bnk-forge — if the local bnk-forge
stack isn't running, the auto-hook logs a clean skip and continues.

**`kindbnkctl` never installs or starts bnk-forge for you.** If it's
configured but not running, bring it up manually (`cd ~/git/bnk-forge
&& make deploy`) then `kindbnkctl bnk-forge launch` to register
after the fact.

## Requirements

| Tool | Why |
|---|---|
| **Docker** or **Podman** | kind runs Kubernetes nodes as containers; FLO + cert-gen also shell into an `alpine/k8s:1.31.5` container at deploy time |
| **kind** | cluster bring-up |
| **kubectl** | cluster reads/writes (apply, wait, label) |
| **helm** | cert-manager + FLO install, release-manifest pull |
| **git** *(optional)* | `init` git-inits the PoC repo (skippable with `--no-git`) |

Verify after install:

```bash
kindbnkctl doctor
```

What customers supply themselves, dropped into `keys/` of the PoC repo
(delivered through F5's normal channels):

- FAR tarball — image-pull credentials for `repo.f5.com`
- JWT — TEEM activation token

## Quick start

```bash
# 1. Create a fresh PoC repo. Auto-detects ~/git/bnk-forge.
kindbnkctl init demo --customer "Acme"
cd demo

# 2. Drop the operator-supplied files into keys/.
cp /path/to/f5-far-auth-key.tgz keys/
cp /path/to/license.jwt          keys/.jwt

# 3. Confirm poc.yaml is clean.
kindbnkctl validate

# 4. Run the pipeline (~10–20 min with a warm docker cache).
kindbnkctl e2e --yolo --confirm-cluster demo

# 5. Tear down (symmetric):
kindbnkctl destroy --yolo --confirm-cluster demo
```

## Per-phase invocation

If you'd rather drive the phases one at a time for diagnostics:

```bash
kindbnkctl cluster up      --yolo --confirm-cluster demo
kindbnkctl deploy prereqs  --yolo --confirm-deploy  demo
kindbnkctl deploy flo      --yolo --confirm-deploy  demo
kindbnkctl deploy cne      --yolo --confirm-deploy  demo
```

Every phase is idempotent and gated by `--yolo` plus a typo-guard.

## Repo layout (the binary itself)

```
cmd/kindbnkctl/        main entrypoint
internal/cli/          cobra commands (init, validate, doctor, cluster,
                       deploy, destroy, e2e, bnk-forge, version)
internal/poc/          poc.yaml schema + I/O
internal/cluster/      kind + docker wrappers
internal/deploy/       cert-manager, FLO, License CR, CWC cert-gen
internal/bnkforge/     bnk-forge HTTP client (copy-fork of dpubnkctl)
internal/embedded/     go:embed AGENTS.md, CLAUDE.md, templates/
internal/version/      build-stamped + BNK 2.3.0 pins + min-spec floor
```

## Repo layout (a PoC created by `kindbnkctl init`)

```
poc.yaml         declarative state — source of truth
AGENTS.md        operator + agent guide
CLAUDE.md        @AGENTS.md include
journal/         append-only markdown log
artifacts/       rendered kind.yaml, kubeconfig, helm values, CWC certs
keys/            gitignored — FAR tgz + JWT live here
.gitignore       excludes all secret material
```

## Scenarios — testing F5 how-tos against the running cluster

After `e2e` brings the cluster up, drive named test scenarios against
it. Each scenario maps to one F5 how-to article (or sub-article) and
exercises a slice of BNK functionality end-to-end: render manifests
into `artifacts/scenarios/<name>/`, apply them, assert reconciled
state, write a JSON+md report under `reports/<timestamp>/scenarios/`.

```bash
kindbnkctl scenario list                            # all known scenarios + rating
kindbnkctl scenario run http-routing --poc ./demo   # apply + verify + report
kindbnkctl scenario run http-routing --dry-run      # render manifests only
kindbnkctl scenario clean http-routing              # delete what was applied
```

Rating is a stable hint about what's testable in the 2-node / demo-TMM
shape:

| Rating | Meaning |
|---|---|
| green | fully testable here |
| amber | partially testable — control-plane verifies, data-plane plumbing missing |
| red   | requires real DPUs / BGP peers / etc.; listed for discoverability, never executed |

Scoring of the [F5 BNK how-tos index](https://clouddocs.f5.com/bigip-next-for-kubernetes/latest/how-tos/):

| # | How-to | Rating | Status |
|---|---|---|---|
| 1 | Restrict access to sensitive data | 🟢 green | not yet implemented |
| 2 | Components needing cluster-wide access | 🟢 green | not yet implemented |
| 3 | Set up dynamic routing with BGP | 🔴 red | needs real BGP peer |
| 4 | Set up core file collection | 🟡 amber | not yet implemented |
| 5 | Configure DOCA Offloads on DPU | 🔴 red | needs DPU |
| 6 | Configure Token Counting and Enforcement | 🟢 green | not yet implemented |
| 7 | Configure AI Traffic Optimization Features | 🟡 amber | not yet implemented |
| **8** | **HTTP traffic steering with Gateway API HTTPRoute** | **🟡 amber** | **implemented (`http-routing`)** |
| 9 | Proxy Protocol iRule support for L4 routes | 🟡 amber | not yet implemented |
| 10 | Load Balance Traffic to External Resources | 🟢 green | not yet implemented |
| 11 | Static Active-Standby Interface Bonding | 🔴 red | needs real NICs |
| 12 | TMOS DNS Service Integration with CIS | 🟡 amber | not yet implemented |

The implemented `http-routing` scenario is rated amber because TMM's
demoMode listener can't be reached from outside the pod netns on
kind — control-plane reconciliation (GatewayClass + Gateway +
HTTPRoute + Listener Programmed) is fully verified, but the
end-to-end curl is out of scope until a data-plane plumbing
scenario lands.

## Testing

```bash
make test    # Go unit tests (poc, deploy, cluster, scenarios)
make smoke   # unit tests + Layer A CLI smoke (no cluster required, ~5s)
```

`make smoke` is the gate to run before pushing — it covers the
non-cluster-dependent surface area in one shot.

## Design references

- **dpubnkctl** — the bare-metal + DPU sister tool. kindbnkctl is a
  copy-fork: `internal/poc`, `internal/cluster`, `internal/cli` are
  rewritten for the kind path; `internal/bnkforge`, `internal/deploy`
  are forked verbatim with minor adjustments (local kubectl/helm
  instead of containerized).
- **f5-bnk-udf** — the inspiration for the BNK-on-host shape:
  `advanced.demoMode.enabled: true` + node label + nodeSelector. Same
  CNEInstance recipe, minus Multus / SR-IOV / dynamicRouting.
