# Fresh full-stack integration manifest

This branch is an integration candidate, not a claim that its local adaptation commits are upstream PR heads. It is based on the pinned upstream commit below and preserves each selected public contribution as an actual second parent.

- Candidate worktree: `/home/chris/src/worktrees/gascity-full-integration-fresh-20261017`
- Candidate branch: `gascity/full-upstream-pr-stack-20261017`
- Validation checkout: Sprite `gascity-full-20261017-validation2`, `/home/sprite/gascity-full-integration-validation/src`
- Frozen historical candidate: `/home/chris/src/worktrees/gascity-carry6-carry9-integration`, branch `gascity/full-upstream-pr-stack-20261016-r2`, HEAD `d49d861989a2978d1794d06b311912a464defebb`

## Pinned base and observed public refs

- Base: `3c4cf446b497c52de11bf769ccbb9fb682d78122`, verified as `upstream/main` in the prior immutable report after the original integration. At `2026-10-17T15:00:54Z`, public `refs/heads/main` was `a2c9a5a112dbf3933b972ec786f15e28094d32d5`; the pinned base remains its ancestor. Later main commits are intentionally not imported into this candidate.
- The read-only public PR snapshot below was queried at `2026-10-17T15:00:54Z`; exact head and current state are recorded below. No author branch was changed.

| Order | PR | GitHub ref | Head SHA | Current state | Integration merge | First parent | Second parent |
|---:|---|---|---|---|---|---|---|
| 1 | [#7359](https://github.com/gastownhall/gascity/pull/7359) | `refs/pull/7359/head` | `aad534beb802aa4804e5f2ec873e7a880c9ea3bf` | open, clean | `3c5fe43fcb3a4e9d5cea0e511f321a05999ac8b1` | `3c4cf446b497c52de11bf769ccbb9fb682d78122` | `aad534beb802aa4804e5f2ec873e7a880c9ea3bf` |
| 2 | [#7481](https://github.com/gastownhall/gascity/pull/7481) | `refs/pull/7481/head` | `a51a2124df52dc0a086d416436bbd1258b249e49` | open, clean | `1da3551f351c214fc0e539ec0a4f65a99c9d892e` | `3c5fe43fcb3a4e9d5cea0e511f321a05999ac8b1` | `a51a2124df52dc0a086d416436bbd1258b249e49` |
| 3 | [#7369](https://github.com/gastownhall/gascity/pull/7369) | `refs/pull/7369/head` | `4b1e833849eadf0b321ea6c903dd7da2c272bc0c` | open, clean | `5279c2ae6eb732347fced1cabee0316f141dedd2` | `1da3551f351c214fc0e539ec0a4f65a99c9d892e` | `4b1e833849eadf0b321ea6c903dd7da2c272bc0c` |
| 4 | [#7440](https://github.com/gastownhall/gascity/pull/7440) | `refs/pull/7440/head` | `38c4b3bd924fe8b07a3c9cb66120f94938579405` | open, clean | `ef3a3b447564c07d286c55b76d537751d98a60ed` | `5279c2ae6eb732347fced1cabee0316f141dedd2` | `38c4b3bd924fe8b07a3c9cb66120f94938579405` |
| 5 | [#7306](https://github.com/gastownhall/gascity/pull/7306) | `refs/pull/7306/head` | `b692976ed455b5fe4c2d80d71153440525d8f47b` | open, dirty | `d8ba5e899e711e85fb3aae91886b2e11548a24c6` | `ef3a3b447564c07d286c55b76d537751d98a60ed` | `b692976ed455b5fe4c2d80d71153440525d8f47b` |
| 6 | [#7354](https://github.com/gastownhall/gascity/pull/7354) | `refs/pull/7354/head` | `fa65ab13d43e06b0054d556c9d83f36725c5e35a` | open, dirty | `4b0b41469ebd613d3a287465b2599113324e41e5` | `d8ba5e899e711e85fb3aae91886b2e11548a24c6` | `fa65ab13d43e06b0054d556c9d83f36725c5e35a` |
| 7 | [#7568](https://github.com/gastownhall/gascity/pull/7568) | `refs/pull/7568/head` | `7c5195f9dc22c2a76b1d7c8591946642c86dbd2c` | open, clean | `153627fcb8762a4261ac1f9f6ab5d84aa77f0a28` | `4b0b41469ebd613d3a287465b2599113324e41e5` | `7c5195f9dc22c2a76b1d7c8591946642c86dbd2c` |

The #7306 and #7354 merge commits contain local integration conflict resolution. Their author-owned refs remain unchanged. At the recorded snapshot both heads were still dirty, with no author-added conflict-resolution head to integrate. The local merge resolutions therefore need an upstream ownership decision before publication: either the authors resolve their existing PRs, or a separately owned replacement PR must retain explicit attribution and supersession. This phase did not create either replacement because PR operations are out of scope. The #7354 integration retains its resource census, policy, and tests. The #7568 integration retains provider/readiness behavior; the HOME permission correction is a separate local contribution below.

## Proposed contribution layers

These are proposed public layers only. No PR was created or updated, no branch was pushed, and these branch names are plans rather than existing refs.

| Proposed layer | Planned branch | Parent/dependency | Candidate implementation boundary | Status |
|---|---|---|---|---|
| HOME and pod security | `gascity/pr-home-security-20261017` | #7568 integration | `1095fd42c86902eb0b2062f6f7b77d5d5af344e9`, `b868a993e71ac41f645e86ca3b188809556e7c56` | Not opened; source is in integration candidate |
| Metadata, protocol compatibility, transactional identity | `gascity/pr-runtime-identity-20261017` | HOME/security | `6585a40a0c91dbcc5f6ff52f62511d5e4b089d3b`, `255c7295a9bc7766c150a55162aac7e3b3168206`, `e746de2e02f0b211733bfd3d4b0a00a0b28fe031`, `fc947ee2500e6500c68da6ba2dafae69a2b4a7dc`, `2cdfb3c7479f38bfd988e50adb1921576402a2c5`, `a13d9807caab16df7f04f0d6d09fecb0a890f1bd` | Not opened; source is in integration candidate |
| Scoped store attachment | `gascity/pr-scoped-store-20261017` | HOME/security; independent of metadata | `ac0fe8e64c733c361d1d379bca20c74560833929`, `bd60525e99d6c40aa4f677f152defa4dceb0182f` | Not opened; source is in integration candidate |
| Startup delivery and recovery | `gascity/pr-startup-delivery-20261017` | Metadata layer | `7361e1c79b44675bb9f22e2ed5a895cae00beaa5`, `4e23f6aaaf6ce9e87a07917d5502555220c02c92`, `8cc23d519fdc71d17a8a6276759561d62a6b36b7`, `ac0dc1717cdf9ea796c7ed7a71d4bc98f085b49b`, `c558ddcfe51a6d6db1bedbcfd972323af172160c` | Not opened; source is in integration candidate |

The separate census/policy regeneration and provider-ledger cleanup commit is `20e32dc033c11a1d97a847697f8ff90fd3623a66`; `6cb0e8fe6c6f2e7dbffafe13bc2e9d525da6020c` fixes provider-ledger indentation. The three new focused source commits on this candidate are `bd60525e99d6c40aa4f677f152defa4dceb0182f` (store), `a13d9807caab16df7f04f0d6d09fecb0a890f1bd` (identity), and `c558ddcfe51a6d6db1bedbcfd972323af172160c` (startup recovery). These are candidate commits, not upstream PR heads.

The candidate is a single integration branch with these review boundaries represented by commits; it does not materialize separate proposal branch refs. The store layer has no semantic dependency on the metadata layer even though both are present in this candidate's first-parent history.

## Mapping from the eight historical local commits

The prior candidate at `d49d861989a2978d1794d06b311912a464defebb` is frozen historical evidence. Its eight local commits after the #7568 port are mapped semantically here; the new candidate does not claim they are upstream commits or replay them as a single carry.

| Historical commit | Original contribution | New candidate representation |
|---|---|---|
| `360728f6ef1ecaca2c347dbd74b7d427ec04743d` | HOME writable/private root and image identity | HOME/security layer `1095fd42c86902eb0b2062f6f7b77d5d5af344e9`, plus best-effort optional copy `b868a993e71ac41f645e86ca3b188809556e7c56` |
| `efe57a7d73257e771406b18403167ef630d592cd` | Warm runtime identity projection | Metadata layer `6585a40a0c91dbcc5f6ff52f62511d5e4b089d3b` and identity transaction follow-ups |
| `a3a88a78643a08f1a7d925bce4a599d335506030` | Provider-aware startup prompt | Startup delivery layer `7361e1c79b44675bb9f22e2ed5a895cae00beaa5` and recovery follow-ups |
| `881abc78b02b28528ea0b61f16c4915bca953064` | Scoped store attachment | Store layer `ac0fe8e64c733c361d1d379bca20c74560833929` and route/fixture follow-ups |
| `58887687cf4b99f81415648ebdfdcbcb84872c49` | Shared identity metadata contract | Metadata layer `6585a40a0c91dbcc5f6ff52f62511d5e4b089d3b` and protocol correction `fc947ee2500e6500c68da6ba2dafae69a2b4a7dc` |
| `ad5aa9c1b18181de9f12877a73242919fc22677f` | Startup nudge provider metadata tests | Startup delivery/recovery layer and tests, including `7361e1c79b44675bb9f22e2ed5a895cae00beaa5` |
| `22e7a543dca981385247781ea8f37d7b58411f78` | Metadata argv/transport test | Metadata transport tests and protocol correction `fc947ee2500e6500c68da6ba2dafae69a2b4a7dc` |
| `d49d861989a2978d1794d06b311912a464defebb` | Mixed metadata, store, launch, and policy/test changes | Split across HOME/security, metadata, store, and startup layers; census and policy tests are preserved and regenerated as later validation follow-ups |

This semantic mapping is not a patch-equivalence claim. The file accounting and per-commit diffs in the phase receipt are the source of truth for individual paths.

## Validation and release status

The exact final candidate SHA, Sprite command results, and evidence paths belong in the immutable phase receipt. Earlier validation of `d49d861989a2978d1794d06b311912a464defebb` did not pass any test gates and remains **NOT RUN**. Bazel checks and Bazel-backed pre-commit analysis are deferred to upstream PR CI after repeated Sprite runner SIGKILLs; direct Go checks, format checks, and available runtime probes are recorded separately. Unavailable Kubernetes/image fixtures remain **NOT RUN**, not pass.
