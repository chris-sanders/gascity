# Public bootstrap images

`chris-sanders/gascity` is the sole public source and build authority for the
bootstrap runtime images. The `Gas City public runtime images` workflow accepts
only an exact public-fork SHA, verifies that it descends from upstream base
`6ba434271c5f76bc150f694ebeef9d713be16b3a`, builds `linux/amd64`, and emits
digest-only evidence. It never publishes `latest`.

The workflow publishes credential-free images at:

* `ghcr.io/chris-sanders/gascity-runtime-base`
* `ghcr.io/chris-sanders/gascity-runtime-agent`
* `ghcr.io/chris-sanders/gascity-runtime-controller`

The agent pins Codex CLI `0.153.4` and its npm archive SHA-512. Runtime
credentials belong in deployment-local Secrets and must never be added here.

The Kubernetes provider accepts deployment-neutral Secret projections through
`GC_K8S_SECRET_ENV` and `GC_K8S_SECRET_MOUNTS`. Each is a JSON array: an env
entry has `name`, `secret`, `key`, and optional `optional`; a mount has
`secret`, `mount_path`, and optional `optional`. The unset env default preserves
`GITHUB_TOKEN` from `git-credentials[token]`; the unset mount default preserves
the optional `claude-credentials` mount at `/tmp/claude-secret` for non-Codex
workers. An explicit empty array disables that projection. Values are always
resolved by Kubernetes from Secret references; they are never copied into
config, argv, logs, or images.
