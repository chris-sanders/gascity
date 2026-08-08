# Image provenance — how to rebuild the k8sfix-verify4 images

The live dev2 deployment runs:
- `registry.kagent.svc.cluster.local:5000/gc-controller:k8sfix-verify4`
- `registry.kagent.svc.cluster.local:5000/gc-agent:k8sfix-verify4`

These are NOT reproducible from a registry tag alone (registry PVC dies with
dev2). Rebuild recipe:

## Source
- gascity base commit: **459b7822c173** (upstream at build time)
- POC branch: **build-3872fix** (= fork `chris-sanders/gascity:feat/k8s-graphv2-poc-fixes`), tip `73c03e750`
- On top of base, the 9 commits (see COMMITS.txt); the 5 upstream-worthy ones are PRs #6-#10 on the fork.

## IMPORTANT: an UNCOMMITTED patch is baked into k8sfix-verify4
The controller image includes a reconciler fix that was NEVER committed — it
existed only as a working-tree diff in the build checkout. Preserved here as:
- `upstream-fixes/UNCOMMITTED-reconciler-draft16-patchAC.patch`
This is the create-race / duplicate-pending-create fix (Draft 16, Patch A + C):
it defers the "belongs to another session" rollback while a pool worker's async
k8s Start is still in flight, and rolls back a DUPLICATE pending-create mint
(leaving the live worker running) instead of killing the good worker. Without
it, pool workers churn perpetually and never claim work. It is the reworked form
of PR-candidate #8 (the naive Stop(name) version is unsafe — see SOL review).
APPLY THIS PATCH before building, on top of the 9-commit branch.

## Build (from the gascity checkout at build-3872fix + the uncommitted patch)
```
# 1. gc/bd/br binaries (static)
CGO_ENABLED=0 go build -o build/gc ./cmd/gc
# (bd 1.1.0, br: from the beads build, copied into the image context)
# 2. images (Dockerfiles committed under k8s/):
#    Dockerfile.base -> Dockerfile.agent (adds codex 0.144.1 + Go toolchain) -> Dockerfile.controller
#    Build in-cluster via kaniko/builder pod (see k8s/builder-pod.yaml, builder-controller-pod.yaml);
#    a full local Go build has OOM'd, so use the capped in-cluster builder then push to the registry.
# 3. tag both gc-controller and gc-agent as the target tag and push to
#    registry.kagent.svc.cluster.local:5000
```

## Agent image extras (codex reviewer lane)
The agent image also bundles codex 0.144.1 (static binary) + Go 1.26.5 + gcc +
claude, so the reviewer (codex/gpt-5.6-sol) and coder (claude) both run and can
build/test. See Dockerfile.agent.

## Verdict
Rebuildable, but only because the source commit + the uncommitted patch are now
preserved in this repo. The clean-room path is: check out fork
feat/k8s-graphv2-poc-fixes, apply UNCOMMITTED-reconciler-draft16-patchAC.patch,
build binaries, build images via the committed Dockerfiles, push to the registry
under a fresh tag, and point the manifests/city.toml at that tag.
