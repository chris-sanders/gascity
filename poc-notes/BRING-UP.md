# Gas City on Kubernetes (dev2) — clean bring-up from this repo

Reproduces the gas-city-poc deployment from committed artifacts. Assumes a
Kubernetes cluster + an in-cluster image registry reachable at
`registry.kagent.svc.cluster.local:5000` (on dev2 this was openebs-hostpath,
single node — it does NOT survive a full cluster teardown, so step 1 rebuilds
images from source).

All artifacts referenced below live under `deploy/gas-city-poc/` in this repo.

---

## 0. Prerequbuild inputs
- Namespace: `gc`
- Registry: `registry.kagent.svc.cluster.local:5000`
- gascity source: fork `chris-sanders/gascity`, branch `feat/k8s-graphv2-poc-fixes`
  (base upstream `459b7822`). See `IMAGE-PROVENANCE.md`.

## 1. Build & push images (see IMAGE-PROVENANCE.md for the full recipe)
The images are NOT recoverable from a registry tag after teardown — rebuild:
1. Check out fork `feat/k8s-graphv2-poc-fixes`.
2. **Apply `upstream-fixes/UNCOMMITTED-reconciler-draft16-patchAC.patch`** (the
   create-race fix baked into the working image but never committed — without it
   pool workers churn and never claim work).
3. Build `gc` (`CGO_ENABLED=0 go build -o build/gc ./cmd/gc`) + bundle `bd` 1.1.0 / `br`.
4. Build images via the committed Dockerfiles (`k8s/Dockerfile.base` →
   `Dockerfile.agent` (adds codex 0.144.1 + Go toolchain + claude) →
   `Dockerfile.controller`). Use the in-cluster builder pods
   (`k8s/builder-pod.yaml`, `k8s/builder-controller-pod.yaml`) — a full local Go
   build has OOM'd.
5. Tag + push `gc-controller:<TAG>` and `gc-agent:<TAG>` to the registry.
6. Set `<TAG>` consistently in the controller Deployment (`GC_K8S_IMAGE` env +
   the container image) below. The last known-good tag was `k8sfix-verify4`.

## 2. Namespace + secrets
```
kubectl create namespace gc
```
Recreate these secrets (values come from your credential store, NOT this repo):
| Secret | Keys | Purpose |
|---|---|---|
| `git-credentials` | `token` | GitHub PAT for the rig repo (chris-sanders/gascity) — clone/PR |
| `claude-credentials` | `.claude.json`, `settings.json` | claude coder config/onboarding |
| `github-credentials` | `token` | GitHub API |
| `gitea-credentials` | `token` | (optional) gitea |
| `git-credentials-gitea-backup` | `token` | (optional) backup remote |

Note: the codex reviewer needs NO secret — it uses the duo-shim
(`OPENAI_API_KEY=dummy-key-duo-shim`) and its `~/.codex/config.toml` is seeded in
the reviewer agent's pre_start (see `city/packs/rigwork/agents/reviewer/agent.toml`).
The duo-shim itself lives in ns `kagent` (separate deploy; must be reachable at
`duo-shim.kagent.svc.cluster.local`).

## 3. Dolt (beads store)
```
kubectl -n gc apply -f k8s/dolt-service.yaml
kubectl -n gc apply -f k8s/dolt-statefulset.yaml
kubectl -n gc rollout status statefulset/dolt --timeout=180s
# create the databases (NOTE: DOLT_CLI_PASSWORD="" is required or dolt prompts):
kubectl -n gc exec dolt-0 -- sh -c 'export DOLT_CLI_PASSWORD=""; dolt --host=dolt.gc.svc.cluster.local --port=3307 --user=root --no-tls sql -q "CREATE DATABASE IF NOT EXISTS \`ci\`; CREATE DATABASE IF NOT EXISTS \`tr\`"'
```

### 3a. FRESH-DATABASE init (CRITICAL — required on a brand-new dolt, e.g. after teardown)
On a from-scratch dolt DB, the controller's `gc start` runs `bd init`, which
creates a `metadata` table but leaves it UNCOMMITTED in the dolt working set;
bd 1.1.0's schema migration then aborts with:
`schema migration: pending schema migrations alter pre-existing dirty tables: metadata ... run 'bd dolt commit' (gastownhall/beads#4566)`
and the controller crashloops. (Databases migrated under an older bd do not hit
this — only fresh ones do.) Fix: let the controller run once (it dirties the
metadata table), then COMMIT the working set and restart:
```
# after the controller has crashed once on a fresh DB:
kubectl -n gc exec dolt-0 -- sh -c 'export DOLT_CLI_PASSWORD=""; for db in ci tr; do dolt --host=dolt.gc.svc.cluster.local --port=3307 --user=root --no-tls sql -q "USE \`$db\`; CALL DOLT_COMMIT(\"-A\",\"-m\",\"commit gc-init metadata working set (beads#4566)\");"; done'
kubectl -n gc rollout restart deployment/gc-controller
# controller now reaches "City started". (Proper upstream fix: bd should commit
# its own init working set before migrating — candidate patch / beads#4566.)
```

## 4. City config → configmap
The controller reads its city from the `gc-city-src` configmap (a tar of the
`city/` tree). Rebuild it from the committed tree:
```
cd deploy/gas-city-poc/city && tar -czf /tmp/city.tgz .
kubectl -n gc create configmap gc-city-src --from-file=city.tgz=/tmp/city.tgz --dry-run=client -o yaml | kubectl apply -f -
```
`city/city.toml` already carries the working config: `[dolt]` external endpoint,
`[providers.codex]` (`ready_delay_ms`, `path_check=bd`, `args_append`
`--dangerously-bypass-hook-trust`, `prompt_mode=none`), the HQ-scoped
control-dispatcher patch with its store-bootstrap `pre_start_append`, and the
rigwork pack (coder + reviewer agents, formulas). See `CONFIG-REQUIREMENTS.md`
for what each line is and whether it's an intended knob or a workaround.

## 5. Controller
Apply the controller Deployment (rbac + config + deployment). The authoritative
spec incl. the working startup script (direct-bd store bootstrap, external-dolt
init, rig clone) and the correct image tag is
`k8s/controller-deployment.LIVE.yaml` — set its image/`GC_K8S_IMAGE` to your
`<TAG>` from step 1.
```
kubectl -n gc apply -f k8s/controller-rbac.yaml
kubectl -n gc apply -f k8s/controller-deployment.LIVE.yaml
```
The controller's startup script (embedded in that manifest) bootstraps
`/city/.beads` + `/city/rigs/testrig/.beads` metadata against dolt and clones
the rig — no manual store init needed.

## 6. Verify
```
kubectl -n gc get pods                    # controller + dolt + (on demand) agents
kubectl -n gc exec deploy/gc-controller -- sh -c 'cd /city && bd where'
# end-to-end: sling a formula and watch it flow author -> PR -> Sol review:
kubectl -n gc exec deploy/gc-controller -- sh -c 'cd /city && gc --rig testrig sling testrig/rigwork.coder gascity-dev-idprefix --formula'
```
Expected: graph-apply materializes the full graph (not degenerate), a coder pod
claims the author step, clones + implements + builds + opens a PR.

## Teardown
```
kubectl delete namespace gc     # removes controller, dolt (+ its PVC/data), configmaps, secrets, agent pods
```
Releasing the whole dev2 cluster additionally destroys the registry (images) —
recover via step 1.
