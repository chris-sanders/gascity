# deploy/gas-city-poc/k8s — which manifests are authoritative

**Read `../BRING-UP.md` first.** It is the canonical clean-rebuild recipe and
names the exact files to use.

## Authoritative (use these)
- `controller-deployment.LIVE.yaml` — the working controller Deployment captured
  from the running dev2 env. Correct image tag (`k8sfix-verify4` — bump to your
  freshly-built tag), correct startup script (direct-bd store bootstrap,
  external-dolt init, rig clone). **This is the controller manifest to apply.**
- `dolt-service.yaml`, `dolt-statefulset.yaml` — beads store.
- `controller-rbac.yaml`, `namespace.yaml` — supporting.
- `Dockerfile.base` / `Dockerfile.agent` / `Dockerfile.controller` — the image
  build chain (see `../IMAGE-PROVENANCE.md` for the source commit + the
  uncommitted reconciler patch that must be applied before building).

## STALE — reference only, DO NOT deploy as-is
These carry outdated image tags / Dockerfile bases from earlier POC iterations.
They are kept for history; using them would deploy the wrong (broken) images.
- `controller-deployment.yaml` — stale tag `gc-controller:v1.3.3-promptfix3` +
  `gc-agent-omni:v7-promptfix3`. Superseded by `controller-deployment.LIVE.yaml`.
- `builder-pod.yaml` — builds the agent image from a stale base
  (`gc-agent:codex3`) and pushes `gc-agent:edge-beadsprefix`. If you rebuild via
  a kaniko builder, update the FROM base and the `--destination` tag to your
  target tag first (per `../IMAGE-PROVENANCE.md`).
- `builder-controller-pod.yaml` — same, for the controller image
  (`edge-reaper-promptfix2` / `edge-beadsprefix`).

When you rebuild, pick ONE consistent target tag, build agent then controller
from it, push, and point `controller-deployment.LIVE.yaml` (image +
`GC_K8S_IMAGE`) at that tag.
