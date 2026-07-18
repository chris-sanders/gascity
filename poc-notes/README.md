# POC notes — k8s + graph.v2 workflow fixes

Working notes for the Gas City on Kubernetes POC that produced this branch
(`feat/k8s-graphv2-poc-fixes`, based on upstream `main` @ `459b782`). This is a
**fork-only work-in-progress** shared for review before polishing individual fixes
into focused upstream submissions.

## Result

A rig-scoped graph.v2 WORKFLOW runs end-to-end, zero-touch, on real Kubernetes:
workflow root → resident `<rig>/core.control-dispatcher` serves control beads →
work step activates (gate→task) → rig pool worker claims + opens a PR → step closes
→ workflow-finalize served → root closes. No worker pods linger afterward.

## Files

- `UPSTREAM-ISSUES-DRAFT.md` — the full set of draft issue/PR write-ups (Drafts 1–15),
  each classified as a NEW issue, a COMMENT on an existing issue, or validation of an
  open PR, with the patch-classification table (temporary / removal condition) at the
  end. **Nothing here has been filed upstream.**
- `patches/00NN-*.patch` — the individual commits as `git format-patch` output, for
  splitting/polishing into upstream PRs. `0001`–`0002`+`0004` are the upstream-authored
  #3873/#3912 cherry-picks (not ours to submit); `0003`,`0005`–`0008` are our candidate
  contributions.

## Commit → draft map

| Commit / patch | Draft | Ours? |
|---|---|---|
| `0003` k8s skip provider readiness for pod init | 2 | yes |
| `0005` exec.Store.IDPrefix() | 9 | yes |
| `0006` gc-beads-k8s update forwards --type/--status | 10 | yes |
| `0007` stage init container mkdir WorkingDir | 13 | yes |
| `0008` reconciler stop mismatched runtime on rollback | 15 | yes |
| `0001` #3873 route rig-store control beads | 8/12 | upstream cherry-pick |
| `0002` #3912 fan serve loop across rig stores | 8/12 | upstream cherry-pick |
| `0004` residency test fixtures for #3873/#3912 | 8/12 | supports the cherry-picks |

## Not included here

The deployment-layer fixes (config/recipe, not source patches) — HQ-db/prefix
alignment, city-scope `GC_BEADS_PREFIX` injection, the `#418` empty-`/city` init
bridge, dropping the unrecognized `isolation` key, rig-store identity adoption via
`gc dolt-state ensure-project-id`, the resident-dispatcher rig `.beads` bootstrap,
and per-bead worker store topology — live in the separate POC deploy repo. They are
upstream-reportable as gaps (see the drafts) but need no source change.
