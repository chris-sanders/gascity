# Upstream issue/PR drafts (gastownhall/gascity) — DRAFTS, NOT FILED

These are prepared for the owner to review + file. NOTHING here has been posted to
the upstream tracker (no `gh`, no token in this env; filing is an external
side-effect that needs explicit owner go-ahead). All findings reproduced live on
pure-upstream `main` @ `459b782` (current main `4fbbd1e` is only 5 commits ahead,
none touching these paths; `edge` == rolling main). Each draft says whether it's a
NEW issue, a COMMENT on an existing issue, or validation of an open PR.

Verified existing tracker items referenced below:
- #472 (open) `gc init` registers the city as a side effect, blocking `gc start`
- #418 (closed, merged) feat(k8s): rework container images for multi-stage builds
- #413 (closed) fix(k8s): use rig prefix from controller env in initBeadsInPod
- #1436 (open) gc init doesn't seed bd issue_prefix, leaving fresh cities unbootable
- #3872 (open) Session lifecycle: durable-state vs runtime-state divergence family
- #3912 (open) fix(dispatch): fan the control-dispatcher serve loop across rig stores
- #3873 (open) fix(control-dispatcher): route rig-store control beads to resident rig

Environment note to include where relevant: this deployment reaches models ONLY
through an Anthropic/OpenAI-compatible gateway (no first-party provider auth), which
is the shape that surfaces the OIDC/provider-readiness gap (Draft 2).

---

## Draft 1 — COMMENT on #472 (regression): contrib/k8s controller deploy is broken on current main

**Where:** comment on existing open issue #472; also flags a regression of #418's flow.

**Title (if filed new instead):** `contrib/k8s controller deploy fails "gc init: already initialized" on main (regression of #418 staging)`

**Body:**
On current `main` (@459b782), deploying the controller via `contrib/session-scripts/gc-controller-k8s deploy` fails before startup:
```
gc init: already initialized      (controller exits 2, never starts)
```
Root cause: `gc-controller-k8s deploy` copies the city (incl `city.toml`) **into** `/city` (`kubectl cp "$city_path/." "$POD:/city/"`, gc-controller-k8s ~line 372), then the `Dockerfile.controller` CMD runs `gc init --from /tmp/city-src /city` into that now-non-empty `/city`. `CityAlreadyInitializedFS` returns true on `city.toml` alone, so `gc init --from` exits 2 before scaffolding `.gc`.

This is a **regression of the #418 flow**, which staged the city to `/tmp/city-src` (NOT `/city`) — the #418 `Dockerfile.controller` CMD comment even says "which must be clean for init" — and ran `gc unregister /city` before `gc start`. Current main's deploy script copies into `/city` and the CMD dropped the `gc unregister`.

Repro: `kubectl apply -f contrib/k8s/` + `gc-controller-k8s deploy <city>` on a fresh namespace.

Workaround (deployment-layer): stage the city to a separate dir and `gc init --from <src> /city` into an EMPTY `/city`, then `gc unregister /city` before `gc start` (i.e. restore the #418 behavior). This is #472's "right fix in gc init itself" but the immediate contrib/k8s regression should also be fixed.

---

## Draft 2 — NEW issue: k8s worker init blocked by provider-readiness for gateway (non-first-party) auth

**Title:** `k8s worker initCityInPod runs provider-readiness preflight that cannot pass with a custom ANTHROPIC_BASE_URL gateway (no --skip-provider-readiness, no env override)`

**Body:**
`internal/runtime/k8s/provider.go` `initCityInPod` runs the worker pod's in-pod init as:
```go
[]string{"env", "GC_DOLT=skip", "gc", "init", "--from", "/tmp/city-src", "/workspace"}
```
with **no** `--skip-provider-readiness` and **no** env/config override. So `finalizeInit` runs the provider-readiness preflight, which for the `claude` provider (`probeClaude`, internal/api/handler_provider_readiness.go) requires first-party OAuth: `LoggedIn && AuthMethod ∈ {claude.ai, oauth_token} && APIProvider == "firstParty"`, and **explicitly rejects API-key / alternate-provider auth**.

Deployments that reach Anthropic through a compatible **gateway/proxy** (custom `ANTHROPIC_BASE_URL` + key) can therefore NEVER pass this probe — there is no first-party token to present. Result: every worker aborts:
```
gc init: city created, but startup is blocked by provider readiness
- Claude Code: needs authentication
```
The controller's own init already passes `--no-start --skip-provider-readiness`; the in-pod worker init does not, and offers no knob.

Requested: make `initCityInPod` pass `--no-start --skip-provider-readiness` (mirroring the controller), or honor a `GC_SKIP_PROVIDER_READINESS` env. Rationale: a worker pod inheriting projected `GC_DOLT_*`/provider env does not need the interactive login preflight the controller already performed; and gateway deployments legitimately have no first-party auth. (This POC carries a 1-line downstream patch adding both flags; happy to submit a PR.)

---

## Draft 3 — NEW issue (or comment on #413/#1436): GC_BEADS_PREFIX not projected for CITY-scope k8s workers

**Title:** `k8s: initBeadsInPod fails "missing projected GC_BEADS_PREFIX" for city-scope pool workers (producer never set in template_resolve)`

**Body:**
`internal/runtime/k8s/provider.go` `initBeadsInPod` reads `cfg.Env["GC_BEADS_PREFIX"]` and errors `missing projected GC_BEADS_PREFIX` when empty. For **city-scope** agents, `cmd/gc/template_resolve.go` (step-8 agent env, ~line 275-296) sets `GC_BEADS_SCOPE_ROOT` but **never sets `GC_BEADS_PREFIX`** — grep confirms 0 producers for the city scope on main. #413/#1432 added the producer for **rig** agents (`rig.EffectivePrefix()`), but the city-scope path was left uncovered. Related: #1436 (gc init doesn't seed issue_prefix).

Symptom on a stock `contrib/k8s` city-scope `gc sling coder`: worker warns `missing projected GC_BEADS_PREFIX`; even with the prefix supplied, the worker's native bd store resolves the city/HQ scope to a **different Dolt database** than the controller's exec:gc-beads-k8s store (see Draft 4), so `gc hook --claim` returns `no_work` and the bead never gets claimed.

Requested: set `GC_BEADS_PREFIX` from `EffectiveHQPrefix(cfg)` for city-scope agents in `template_resolve.go` (mirroring the rig path), so `initBeadsInPod` and the worker's claim resolve the HQ store.

---

## Draft 4 — NEW issue: k8s controller vs worker disagree on the HQ Dolt database name

**Title:** `k8s: controller (exec:gc-beads-k8s) HQ store lands in DB=<prefix> while worker (native bd) resolves DB="hq" → split store, worker sees no work`

**Body:**
On a stock `contrib/k8s` city-scope deploy, the controller and worker end up on **different Dolt databases** for the same HQ scope:
- `defaultScopeDoltDatabase(cityPath, cityPath, prefix)` returns `"hq"` for the city scope (`cmd/gc/beads_provider_lifecycle.go:468`). The worker's native-bd `initBeadsInPod` therefore defaults `/workspace/.beads` to `dolt_database="hq"`.
- BUT `contrib/beads-scripts/gc-beads-k8s` `init` (lines ~281-293) runs `bd init --server ... -p "$prefix"` with **no `--database`** — it ignores the optional `$3` database arg that `initBeadsForDir` passes — so the controller's HQ store lands in DB == `<prefix>` (e.g. `ci` for a city named "city"), not `hq`. `GC_DOLT_DATABASE=hq` on the controller is NOT honored via the exec path.
- Net: controller writes the routed bead to DB `<prefix>`; worker looks in DB `hq` → `database "hq" not found` → `gc hook --claim` = `no_work` → bead never claimed.

Additionally the worker's bd-init derived `issue_prefix` from the pod workdir basename (`wo` from `/workspace`) rather than the intended prefix, compounding the mismatch.

Requested: (a) `gc-beads-k8s` `init` should forward the `$3` database arg as `--database` (mirroring the `init <dir> <prefix> [doltDatabase]` contract `initBeadsForDir` uses); and (b) `initBeadsInPod` should pass `--database` (the canonical HQ db) so controller and worker converge on ONE (db, prefix). Deployment-layer workaround today: pin `[workspace].prefix` and bootstrap the worker store explicitly with `bd init --server --database <hqdb> -p <prefix>`.

---

## Draft 5 — NEW issue: k8s provider drops the startup prompt; stock coder nudge can't drive autonomous work

**Title:** `k8s session provider delivers only the nudge (drops PromptSuffix); stock contrib/k8s coder nudge is too vague to drive claim→work→close`

**Body:**
The k8s session provider launches the agent with only `cfg.Command` and delivers the startup prompt via the post-spawn nudge channel; the rendered `PromptSuffix` used by the tmux/local adapters is not applied. Upstream's `contrib/k8s/agents/coder/agent.toml` ships only `nudge = "Check your hook for work assignments."` (and a vague `prompt.template.md`), so a woken Claude does not autonomously run the `gc hook --claim → do work → gc bd close → gc runtime drain-ack` loop — it just idles.

Workaround that works (and is arguably the intended pattern): set `prompt_mode = "none"` on the provider and point the agent at the canonical `internal/bootstrap/packs/core/assets/prompts/pool-worker.md` prompt — `promptDelivery` (cmd/gc/prompt_delivery.go) then prepends the rendered prompt to the nudge, which the k8s provider delivers. With that, the city-scope loop closes zero-touch.

Requested: either ship an actionable nudge/prompt in the `contrib/k8s` coder example (reference pool-worker.md), and/or route the startup prompt through the retried nudge queue in the k8s provider so `ready_delay_ms`/first-turn delivery is reliable. At minimum, document that k8s agents need an actionable prompt via `prompt_mode="none"` + pool-worker.md.

---

## Draft 6 — NEW issue: upstream contrib/k8s example ships an unrecognized `isolation` key that breaks `[[rigs]]` registration

**Title:** `contrib/k8s/example-city.toml ships workspace.isolation = "none" — an unrecognized key that makes rig registration refuse to rewrite city.toml`

**Body:**
`contrib/k8s/example-city.toml:28` sets `isolation = "none"` under `[workspace]`, but `config.Workspace` (internal/config/config.go) has **no `Isolation` field** — the key is unrecognized. This is harmless until a `[[rigs]]` block is added: the rig-path migration wants to rewrite `/city/city.toml` and fails closed:
```
refusing to rewrite /city/city.toml: it contains keys this gc binary does not recognize and the rewrite would silently drop them: workspace.isolation (upgrade gc or remove the keys, then retry)
```
So following the shipped example + adding a rig bricks the controller. Requested: remove `isolation = "none"` from the example (or add the field back to the Workspace struct / allowlist it in the rewrite validator).

---

## Draft 7 — NEW issue: init_controller_beads rig bootstrap uses `gc bd --rig` which is refused under exec:gc-beads-k8s

**Title:** `contrib/session-scripts/gc-controller-k8s init_controller_beads runs "gc bd --rig <rig> init" which fails under GC_BEADS=exec:gc-beads-k8s ("only supported for bd-backed providers")`

**Body:**
`gc-controller-k8s` `init_controller_beads` (line ~217) bootstraps each rig scope via:
```
gc bd --rig '<rig>' init -p '<prefix>' --skip-hooks
```
run inside the controller pod, which sets `GC_BEADS=exec:gc-beads-k8s`. But `gc bd` refuses non-bd-contract providers (`cmd/gc/cmd_bd.go` ~222-239):
```
gc bd: only supported for bd-backed beads providers (resolved "exec:gc-beads-k8s" ...)
```
So the rig-beads bootstrap step of upstream's own k8s deploy cannot run in the exec-beads topology → the rig scope is never initialized in the runner → `gc sling <rig>/<agent>` can't resolve the rig store.

Requested: `init_controller_beads` should not use `gc bd` under a non-bd exec provider; instead invoke the provider's own `init` op (e.g. `GC_STORE_ROOT=<rig> gc-beads-k8s init <rig> <prefix>`), or `gc-beads-k8s init` should accept + forward the database arg (see Draft 4) so the rig store is created deterministically in the runner at `/workspace/rigs/<rig>/.beads`.

---

## Draft 8 — VALIDATION comment on #3872 / #3912 / #3873 (rig-store dispatch): reproduced on real k8s

**Where:** comment on #3872 (and/or the fix PRs #3912/#3873), offering live-k8s validation.

**Body:**
Reproduced the rig-store dispatch gap on real k8s (pure-upstream `contrib/k8s`, main @459b782). After deployment-layer fixes get the city-scope loop fully green (worker claims + closes zero-touch), a **rig-scoped** pool sling stalls at dispatch:
- `gc sling testrig/rigcoder <tr-bead>` stamps `gc.routed_to = testrig/rigcoder` correctly, and the rig (`tr`) store resolves via `gc-beads-k8s` once bootstrapped in the runner.
- BUT the controller reconcile loop only logs `beads cache: reconciled rig=(no-prefix) beads=N` and NEVER emits `poolDesired: <rig-agent>` / a rig session — no rig worker pod spawns. The scale-check does not compute pool demand from the rig store.

This matches #3872's rig-store control-dispatch family; the city-scope pool works, the rig-scope pool does not scale. Happy to validate #3912 (fan control-dispatcher serve loop across rig stores) + #3873 (route rig-store control beads to resident rig dispatcher) on this live cluster — this deploy is exactly the environment those PRs target. Let me know what evidence would help land them.

---

## Draft 9 — NEW issue: exec beads store loses cache prefix identity (no IDPrefix) → rig scale-check blind

**Title:** `internal/beads/exec.Store has no IDPrefix() → NewCachingStore caches exec-backed rig stores as "(no-prefix)", breaking rig-scoped pool scale-check`

**Body:**
`beads.NewCachingStore` (internal/beads/caching_store.go:234-248) derives its cache prefix only from a `*BdStore` or a backing implementing `IDPrefix() string`. `internal/beads/exec.Store` implements neither, so an `exec:`-backed store (e.g. `exec:gc-beads-k8s` for a k8s rig scope) caches with an empty prefix — the reconciler logs `beads cache: reconciled rig=(no-prefix)`. Consequence: the controller's rig-scoped default scale-check cannot associate a routed rig bead (`gc.routed_to=<rig>/<agent>`) with the rig pool, so `gc sling <rig>/<agent> <bead>` never produces `poolDesired`/a worker (direct rig-pool dispatch is dead on k8s).

Fix (verified working on a live cluster): add `func (s *Store) IDPrefix() string` to `internal/beads/exec.Store` returning the trimmed `GC_BEADS_PREFIX` from its env. Then `NewCachingStore` keys the exec rig cache by prefix, the reconciler logs `rig=<prefix>`, and rig-scoped scale-check counts routed demand. Minimal 3-line method; PR-ready.

---

## Draft 10 — NEW issue: gc-beads-k8s `update` drops `--type`/`--status` → graph.v2 step activation stuck at type=gate

**Title:** `contrib/beads-scripts/gc-beads-k8s update op drops --type/--status → graph.v2 workflow steps stay type=gate (never activate/dispatch)`

**Body:**
graph.v2 workflow activation restores a step's deferred type via `Store.Update{Type/Status}`. But the `gc-beads-k8s` shim's `update` case forwards title/description/etc and NOT `--type`/`--status` to `bd`, so the type-restore silently no-ops: the first work step stays `issue_type=gate` (ready-excluded) forever, no worker is dispatched, and the workflow never advances (finalize control bead never becomes ready). Confirmed live: a rig graph.v2 formula materialized correctly (root + steps + control beads) but the work step stayed `gate` until the shim was patched, after which it activated to `type=task` and dispatched. Mirrors the shim's `create` op which already passes `--type`.

Fix (verified): in the `update` case, parse `.type`/`.status` from the JSON input and append `--type`/`--status` to the `bd` args when present (patches/gc-k8s-shim-update-type.patch). Scope-independent; PR-ready.

---

## Draft 11 — NEW issue: k8s resident rig control-dispatcher pod dies at init (rig .git staging perms + no store bootstrap)

**Title:** `k8s: a resident rig-scoped control-dispatcher session never stays serving — initCityInPod fails on rig .git staging perms + its rig .beads is never bootstrapped`

**Body:**
With #3873/#3912 applied, routing a rig graph.v2 workflow's control beads to a resident `<rig>/core.control-dispatcher` (`[[named_session]] dir=<rig> mode="always"`) is correct, but that dispatcher runs as a k8s SESSION POD and fails to stay serving:
1. `initCityInPod` stages the controller's `/city/rigs/<rig>` (root-owned `.git` from the controller-side clone) into the pod and `gc init --from` fails: `creating .../.git/objects/pack/*.idx: permission denied` (pod runs as gcagent uid 1000). A controller-side `chown -R 1000:1000 /city/rigs/<rig>` mitigates once but is fragile.
2. The dispatcher pod's `--serve` loop then resolves its own workspace `.beads` to the city HQ database (`database "hq" not found`) because the pod's rig store is never bootstrapped — the core control-dispatcher agent has a `start_command` but no `session_setup`, and `session_setup` runs post-launch anyway (too late for the init-phase failure) + `initBeadsInPod` warns `missing projected GC_BEADS_PREFIX`.
Net: the resident rig dispatcher can't serve its rig store's control beads, so a rig-scoped graph.v2 workflow stalls at its gate/finalize control step.

Suggested direction: for a scripted control-dispatcher session the k8s provider should not stage the rig `.git` checkout (it only needs the rig beads store, not the git tree), and should project the rig `GC_BEADS_PREFIX` + bootstrap the rig `.beads` at init time (or expose a pre_start hook that runs before the tmux/serve start). Reported for guidance; we can provide a live repro.

---

## Draft 12 — UPDATE to Draft 8: with #3912/#3873 + the fixes above, rig CONTROL routing works; direct rig-pool PR loop GREEN

**Where:** follow-up on #3872/#3912/#3873 (supersedes the "rig pool never scales" framing in Draft 8, which was a separate exec-cache-prefix bug — see Draft 9).

**Body:**
Update after integrating #3873 + #3912 on real k8s (main @459b782) plus the exec IDPrefix fix (Draft 9) and shim update-type fix (Draft 10):
- The prior "rig pool never scales / rig=(no-prefix)" symptom was NOT #3872 — it was the exec-store cache-prefix bug (Draft 9). With that fixed, **direct rig-pool dispatch is fully GREEN**: `gc sling <rig>/<pack>.<agent>` scales a worker that clones the rig, opens a real PR, and closes the bead (verified: 3 parallel PRs on a Gitea test repo, beads closed zero-touch).
- For the graph.v2 WORKFLOW path, #3912/#3873's control-bead ROUTING is confirmed correct (gate + workflow-finalize route to the resident `<rig>/core.control-dispatcher`), and with the shim update-type fix the work step activates. The remaining gap to fully validate #3912/#3873 end-to-end is the resident-dispatcher pod init (Draft 11), not the dispatch logic itself.
Happy to post the full trace; this cluster is a ready validation environment for #3912/#3873.

---

## Draft 13 — NEW issue: k8s provider sets a per-bead WorkingDir that nothing creates → pool/workflow step workers fail chdir at startup

**Title:** `k8s: pod WorkingDir set to a per-bead trigger workdir (<rig>/<beadID>-<slug>) that is never created → container "chdir to cwd ... no such file or directory"`

**Body:**
For a graph.v2 workflow (or pool-triggered) step, `poolTriggerWorkDir` (cmd/gc/build_desired_state.go:3005-3025) sets the worker's WorkDir to a per-bead path `<base>/<beadID>-<slug>` where base is the rig root. The k8s provider then sets the pod spec `WorkingDir: podWorkDir` (internal/runtime/k8s/pod.go — `projectedPodWorkDir`) to that per-bead path. But nothing creates that directory: the `ws` EmptyDir mounts only `/workspace`, `stageFiles`/`copyDirToPod` silently skips it (no controller-side source dir exists — no `git worktree add` ran), and the `stage` init container only waits for `.gc-ready`. The container runtime's `chdir` to a non-existent WorkingDir fails BEFORE the entrypoint runs, so `pre_start`/`session_setup` cannot repair it:
```
died immediately after startup: ... OCI runtime exec failed: chdir to cwd ("/workspace/rigs/<rig>/<bead>-<slug>") set in config.json failed: no such file or directory
```
Fix (verified on a live cluster): have the k8s `stage` init container `mkdir -p "$podWorkDir"` before waiting for `.gc-ready` (it already mounts the ws volume and always completes before the main container). Minimal, provider-local. This is a general invariant: any configured `WorkingDir` must exist before the main container starts.

---

## Draft 14 — NEW issue (compound, k8s graph.v2 step-worker store setup): worker claim needs city+rig store identity adoption + lifecycle ordering

**Title:** `k8s graph.v2 step worker: gc hook --claim can't claim routed work — city/rig store identity + initBeadsInPod race`

**Body (for guidance; several sub-issues):**
A graph.v2 rig-pool step worker on k8s cannot claim its routed work bead. Root causes found:
1. `gc hook --claim` for a rig agent reads workDir=rig root + BEADS_DIR=$RIG_ROOT/.beads (cmd_hook.go:335) and FEDERATES into the CITY store (appendCityHookStore). The in-pod city store `/workspace/.beads` is created by initCityInPod at db `hq` (defaultScopeDoltDatabase→"hq") with no project_id, while the controller HQ store is db `<prefix>` (e.g. ci) with a project_id → `native_store_unavailable gate=identity_match "metadata project_id is missing" scope=/workspace` errors the whole claim → no_work drain.
2. `bd init --server --database <db>` ABORTS when the dolt db already has data; the correct way to create a local `.beads` cache that ADOPTS the existing db identity is a minimal metadata.json + `gc dolt-state ensure-project-id` (dolt_project_id.go:142-235 adopts L3→L2/L1).
3. `initBeadsInPod` (provider.go:254) races the pod entrypoint `pre_start` (both keyed off the `.gc-workspace-ready` touch), and with GC_BEADS_PREFIX set it can create a conflicting store. Making it a no-op (unset GC_BEADS_PREFIX for the worker) + aligning stores in session_setup is required.
4. Lifecycle: `process_names=["claude"]` + a 3s postStartSettle liveness (provider.go:279-296) expects the agent process alive quickly; a worker whose store-setup (ensure-project-id x2 + clone) isn't done in that window (or is gated behind a marker) reads as "died immediately after startup". This is the same class as the long-observed `stale_async_start`.
Suggested direction: initCityInPod should create the pod city `.beads` at the ACTUAL city db (not hardcode "hq") and adopt identity; provide a deterministic pre-tmux hook (not racing initBeadsInPod) for scoped store setup; and relax the startup liveness for agents whose first-turn setup is legitimately longer.

---

## Draft 15 — NEW issue: k8s pool worker pod orphaned when pending-create is rolled back ("live runtime belongs to another session")

**Title:** `k8s: pending-create rollback ("live runtime belongs to another session") closes the session bead but leaks the running pod`

**Body:**
`session_reconciler.go` rolls back a pending-create when the live runtime under the session name has a different identity token than the pending session expects (`runningSessionMatchesPendingCreateInfo` false), logging "live runtime belongs to another session" and calling `attemptRollbackPendingCreate`. That closes the session bead but never stops the runtime. On k8s a pool worker runs a `sleep infinity` pod (tmux + agent) that outlives the rolled-back create → an orphaned, unnudged pod that idles forever and is never reaped (the controller marks the session stopped, but the pod keeps Running). Over repeated pool create-races this accumulates dead pods on the node.

Fix (verified live): call `Provider.Stop(name)` at the rollback site before `attemptRollbackPendingCreate`. `Provider.Stop` deletes pods by the `gc-session` label; pool-instance session names are unique so it targets only the orphan. Minimal, provider-agnostic (uses the existing runtime.Provider interface). patches/gc-reconciler-stop-mismatched-rollback.patch.

Related: the drain-ack stop path (queueDrainAckAsyncStop → Provider.Stop) already reaps drained workers correctly, but the drain-ack supervisor-poke fails harmlessly in k8s worker pods (no supervisor socket) so reaping waits for the next patrol tick rather than firing immediately — a latency nit worth noting but not a leak.

---

## Patch classification (README guardrail #5 — each temporary, with removal condition)

All live in deploy/gas-city-poc/patches/ + on build branch /workspace/.gcbuild-3872 (build-3872fix). Images: stock-459b782-3872fix5 (controller, adds mkdir-workdir + rollback-stop) / -3872fix2 (agent).

| Patch file | What | Layer | Draft | Removal condition |
|---|---|---|---|---|
| gc-k8s-init-skip-provider-readiness.patch | initCityInPod in-pod `gc init` += `--no-start --skip-provider-readiness` | gc SOURCE (owner-approved) | 2 | upstream makes in-pod init skip readiness / honor an env for gateway auth |
| gc-exec-store-idprefix.patch | exec.Store.IDPrefix() from GC_BEADS_PREFIX | gc SOURCE (minimal) | 9 | upstream adds IDPrefix() to exec store |
| gc-k8s-shim-update-type.patch | gc-beads-k8s `update` forwards --type/--status | SHIM script (baked in controller image) | 10 | upstream shim forwards --type/--status |
| gc-3872-rig-dispatch-3873-3912.patch | cherry-pick of PRs #3873 + #3912 | gc SOURCE (upstream-authored) | 8/12 | upstream merges #3912 + #3873 |
| gc-k8s-initcontainer-mkdir-workdir.patch | stage init container `mkdir -p $podWorkDir` before main container (per-bead WorkingDir) | gc SOURCE (owner-approved) | 13 | upstream creates a configured WorkingDir before container start |
| gc-reconciler-stop-mismatched-rollback.patch | sp.Stop(name) on pending-create rollback ("belongs to another session") so orphaned k8s pods reap | gc SOURCE (owner-approved) | 15 | upstream stops the mismatched runtime on rollback |
| gc-k8s-session-fixes.patch | (legacy path-C consolidated patch — NOT used in Phase 0/1) | — | — | superseded; do not apply |
| gc-reaper-processnames.patch | (legacy, dropped) | — | — | ignore unless reaper nil-processNames resurfaces |

Deployment-layer (chart/config, NOT patches — no removal needed, but upstream-reportable as gaps): #418-bridge controller init (Draft 1), HQ-db=hq / prefix pin (Draft 4), city-scope GC_BEADS_PREFIX injection (Draft 3), pool-worker prompt + prompt_mode=none (Draft 5), drop `isolation` key (Draft 6), rig-store bootstrap in runner + rig-bound-pack agent (Draft 7 + rig recipe), rig worker identity/path alignment (Helm — HANDOFF).

## Filing order suggestion (for the owner)
1. #472 comment (Draft 1) — quick, anchors the regression.
2. #3872/#3912/#3873 validation + update (Drafts 8 + 12) — offers to help land in-flight fixes; corrects the "pool never scales" framing.
3. Small, clear, PR-ready SOURCE fixes: Draft 9 (exec IDPrefix), Draft 10 (shim update-type), Draft 2 (OIDC gateway skip-readiness).
4. Deploy/packaging bugs: Draft 6 (isolation, trivial), Draft 4 (HQ db split), Draft 3 (GC_BEADS_PREFIX city scope), Draft 7 (init_controller_beads rig), Draft 5 (prompt delivery), Draft 11 (resident dispatcher pod init).
Several (Draft 3/4/7/9/10) share the "k8s beads scoping/identity" theme and could be ONE umbrella issue "k8s rig-scoped beads store handling is incomplete" with the sub-items, if upstream prefers.
