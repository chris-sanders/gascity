# Gas City Kubernetes provider — config requirements & workarounds

This documents **the actual configuration needed to run the k8s provider
end-to-end**, and — crucially — separates *intended configuration knobs* from
*config-level workarounds that only exist because a code path is missing or
wrong*. The five patches (PRs #6–#10) fix the code-level bugs; this file covers
what remains in **config**, and asks upstream, per line: **is this the intended
way, or should it become a patch?**

Context: this ran a full loop on a live cluster (agent authors a PR, a second
agent reviews it on the PR) against GitLab-Duo-backed models via an OpenAI/
Anthropic-compatible proxy ("duo-shim"), with a shared remote Dolt beads store.

---

## 1. The classification table (the actual question)

| Config | Where | What it does | Verdict |
|---|---|---|---|
| `[dolt] host/port` | city.toml | Pin the shared external Dolt endpoint | **Intended knob** |
| `[session] provider="k8s"`, `[session.k8s] namespace` | city.toml | Select the k8s runtime | **Intended knob** |
| `[providers.*].env` duo-shim base URLs + dummy key | city.toml | Point CLIs at the compatible proxy | **Intended knob** (standard custom-endpoint config) |
| rig/pack `imports`, `prefix`, `session_template` | city.toml | Normal topology | **Intended knob** |
| **`GC_BEADS=bd` (direct-bd store), not `exec:gc-beads-k8s`** | controller env | Use a graph-apply-capable store | **Workaround → see issue #11.** Direct-bd is arguably the *right* in-cluster topology, but choosing it is currently forced because the exec store lacks graph-apply. Upstream should make this an explicit, documented topology choice (not a silent requirement to avoid a degenerate graph). |
| **`.beads/config.yaml` shipped in the city template** (canonical external endpoint) | city template | Make the city resolve external Dolt on `gc init --from` | **Was a workaround → now patched by PR #10.** Once #10 lands, `gc init --from` honors `--dolt-*`/`GC_DOLT_*` and this template file is no longer needed. |
| **`ready_delay_ms = 15000`** | `[providers.codex]` | Delay the first-prompt nudge until the codex TUI has rendered | **Workaround → see issue #12.** A hardcoded, machine-tunable sleep papering over a TUI-readiness race. Upstream should key delivery off a readiness signal, not a magic delay. |
| **`prompt_mode = "none"`** on codex | `[providers.codex]` | Deliver prompt via nudge (send-keys) | **Forced, not chosen.** `arg` mode is a no-op on the k8s runtime (the pod launch never appends a positional prompt argv). So codex on k8s *must* use nudge delivery — which is what surfaces the submit-key problem in #12. Upstream may want the k8s launch to honor arg-mode, OR to document that k8s is nudge-only. **Open question.** |
| **`path_check = "bd"`** on codex | `[providers.codex]` | Make the controller's pool-scale readiness probe check `bd` (present on the controller) instead of `codex` (only in the agent pod) | **Workaround, no issue yet.** This is effectively lying to the readiness probe. The real problem: the controller runs a provider-in-PATH check against *its own* PATH, but for the k8s runtime the provider binary lives in the *agent pod*. Upstream should either probe in-pod or skip the check for pod-runtime agents. **Candidate patch.** |
| **`args_append = ["--dangerously-bypass-hook-trust"]`** on codex | `[providers.codex]` | Stop the codex TUI blocking on a hook-trust prompt | **Workaround, no issue yet.** gc installs `.codex/hooks.json` into every codex workdir, so codex prompts to trust its own gc-installed hooks. Forcing operators to add a "dangerously" flag is a poor default. Upstream should auto-trust gc-managed hooks (they're gc's own), or not require the flag for gc-installed hooks. **Candidate patch.** |
| **codex `~/.codex/config.toml` seeded in pre_start** (custom `duo-shim` provider, `requires_openai_auth=false`, `wire_api="responses"`) | reviewer pre_start | Make codex use API-key auth against the proxy and skip ChatGPT onboarding | **Partly intended, partly workaround.** Defining a custom model provider is legitimate codex config; but having to hand-seed it in a shell pre_start (rather than gc deriving codex config from `[providers.codex]` like it does the env) is a gap. **Candidate: gc should materialize codex provider config the way it wires env.** |
| **codex `[projects."<dir>"] trust_level="trusted"` seeded in pre_start** | reviewer pre_start | Skip codex's directory-trust prompt for the (dynamic per-bead) workdir | **Workaround, relates to #12/hook-trust family.** Same class as the claude `hasTrustDialogAccepted` seeding. Upstream should let the k8s runtime declare its workdirs trusted for headless agents. **Candidate patch.** |
| **`CODEX_HOME` + `XDG_{CACHE,DATA,STATE,RUNTIME}` pinned to gcagent-writable paths** | reviewer `[env]` + pre_start chown | Let codex (running as the pod's non-root user) write its runtime state | **Workaround, no issue yet.** The default codex home/XDG dirs weren't writable by the pod user, so codex failed to init its app-server. Upstream k8s runtime should provision a writable codex home for the agent user. **Candidate patch.** |
| **`codex-autosubmit.sh` watcher in reviewer pre_start** (polls tmux, sends Esc+Enter) | reviewer pre_start | Submit the codex first prompt (buffered paste; Enter=newline) | **Pure band-aid → this IS issue #12.** Replace entirely with a declarative `nudge_submit_keys=["Escape","Enter"]` provider field. |
| **claude `hasTrustDialogAccepted` / `hasClaudeMdExternalIncludesApproved` seeded** + ancestor `CLAUDE.md` neutralized in pre_start | coder pre_start | Skip claude's folder-trust + external-import prompts headlessly | **Workaround, no issue yet.** Same family as codex trust: headless agents need a way to declare trust without a per-provider shell hack. **Candidate: a runtime "headless/trusted workdir" contract covering all TUI providers.** |
| **dispatcher `pre_start_append` store-bootstrap `align()`** (writes `metadata.json` + `gc dolt-state ensure-project-id` for each store on every start) | `[[patches.agent]]` control-dispatcher | Bootstrap the controller-local `.beads` metadata for HQ + rig stores | **Biggest band-aid, no issue yet.** A large inline shell blob every operator would have to copy, re-implementing store bootstrapping that should be a gc code path (the controller should ensure its scoped stores' metadata/identity itself). **Strong candidate patch — arguably the most important config→code conversion here.** |
| **control-dispatcher HQ scope** (`env_remove GC_BEADS_PREFIX`, `mode="always"`) | `[[patches.agent]]` + `[[named_session]]` | Make the dispatcher serve the HQ store so graph.v2 roots (minted HQ-side) are processed | **Likely intended, but worth confirming.** This is legitimate topology config, but the *need* for it (roots mint HQ-side; dispatcher must be HQ-scoped) is subtle enough that upstream may want a doc or a sane default. **Open question.** |

---

## 2. Summary for maintainers

**Already becoming patches:** exec graph-apply / store topology (**#11**), codex
submit-keys (**#12**), and the five landed fixes (**PRs #6–#10**, one of which —
#10 — removes the `.beads/config.yaml` template workaround).

**Config workarounds with NO issue yet — please advise whether these are the
intended way or want a patch:**
1. `path_check="bd"` — controller provider-readiness probe uses the wrong PATH for pod-runtime agents.
2. `args_append=["--dangerously-bypass-hook-trust"]` — gc-installed hooks shouldn't force a dangerous flag.
3. codex `config.toml` / `CODEX_HOME`+XDG / `[projects]` trust hand-seeding — gc should derive codex config + provision a writable home + declare trusted workdirs for headless agents (a general "headless/trusted TUI provider" contract, also covering the claude trust seeding).
4. **dispatcher store-bootstrap `align()` shell blob** — the controller should ensure its scoped stores' `metadata.json` + project identity itself, not via operator-pasted shell.
5. codex on k8s is effectively **nudge-only** (`arg` mode is a runtime no-op) — intended, or should the k8s launch honor arg-mode?
6. control-dispatcher HQ-scoping — intended topology, or deserves a default/doc?

The question we're really asking: **which of these belong in config, and which
are missing code?** Several "work" today but would make a poor operator
experience as the blessed path.
