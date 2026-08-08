# START HERE — stand up the Gas City Kubernetes POC

Point a fresh agent at **this file** to rebuild the POC. It ran end-to-end on a
Kubernetes cluster (an agent authored a PR, a second agent reviewed it on the
PR) and a clean teardown+rebuild was verified reproducible.

## Read these, in order
1. **`BRING-UP.md`** (in this `poc-notes/` dir) — THE clean-rebuild recipe:
   namespace → secrets → dolt → **fresh-DB init (§3a, easy to miss)** → city
   configmap → controller → verify. Follow it top to bottom.
2. **`IMAGE-PROVENANCE.md`** — the container images (`gc-controller`/`gc-agent`
   `:k8sfix-verify4`) are NOT recoverable from a registry after teardown; rebuild
   them from source. Source = this fork's branch **`feat/k8s-graphv2-poc-fixes`**
   (base upstream `459b7822`) PLUS **`patches/UNCOMMITTED-reconciler-draft16-patchAC.patch`**
   (a create-race fix that was never a commit — apply it before building, or pool
   workers churn forever).
3. **`CONFIG-REQUIREMENTS.md`** — every config line explained + which are
   intended knobs vs. workarounds (useful when something behaves oddly).
4. **`k8s-manifests-README.md`** — which k8s manifests are authoritative vs. stale.

## Where the full artifacts live (TWO repos)
- **This fork (`chris-sanders/gascity`), branch `feat/k8s-graphv2-poc-fixes`,
  `poc-notes/`** — the docs above + the uncommitted patch + backup config files.
- **`twinlabs/kagent` (gitea), branch `feat/gas-city-poc`, `deploy/gas-city-poc/`** —
  the CANONICAL, complete kit: the same docs PLUS the full **`city/` pack tree**
  (rigwork coder+reviewer agents, formulas, core pack — the actual workload
  config the controller loads) and all **`k8s/` manifests**
  (`controller-deployment.LIVE.yaml`, `dolt-*.yaml`, `controller-rbac.yaml`,
  Dockerfiles). **You need the kagent repo for the `city/` tree and manifests** —
  the fork only carries the notes. BRING-UP.md references `k8s/...` and `city/...`
  paths that are in the kagent `deploy/gas-city-poc/` dir.

## One-line summary of the rebuild
Get `twinlabs/kagent:feat/gas-city-poc` → `deploy/gas-city-poc/`; build images
from fork `feat/k8s-graphv2-poc-fixes` + the uncommitted patch (IMAGE-PROVENANCE.md);
then run BRING-UP.md against a cluster with an in-cluster registry + a reachable
`duo-shim` in ns `kagent`.

## The upstream contribution (separate from bring-up)
The 5 code fixes are PRs #6-#10 on this fork (base `upstream-main`), with umbrella
issue #5 and design issues #11/#12. Those are for proposing to upstream
`gastownhall/gascity`, not for standing up the POC.
