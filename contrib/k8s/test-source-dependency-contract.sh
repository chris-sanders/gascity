#!/usr/bin/env bash
# Deterministic tests for the Stage 14 source/dependency coherence gate.
set -euo pipefail

root=$(cd "$(dirname "$0")" && pwd)
validator="$root/verify-source-dependency-contract.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

write_fixture() {
  local dir=$1 bd_version=$2 bd_current_version=$3 bd_ref=$4 docker_version=$5 docker_ref=$6
  mkdir -p "$dir/contrib/k8s"
  cat >"$dir/deps.env" <<EOF
BD_VERSION=$bd_version
BD_CURRENT_VERSION=$bd_current_version
BD_CURRENT_REF=$bd_ref
EOF
  cat >"$dir/go.mod" <<EOF
module example.invalid/gascity

require github.com/steveyegge/beads $bd_current_version
EOF
  cat >"$dir/contrib/k8s/Dockerfile.agent" <<EOF
FROM example.invalid/base
ARG BD_VERSION=$docker_version
ARG BD_SOURCE_REF=$docker_ref
RUN curl "https://github.com/gastownhall/beads/archive/\${BD_SOURCE_REF}.tar.gz" \\
    && grep -Fq "Version = \\"\${bd_version}\\"" cmd/bd/version.go
EOF
}

run_valid() {
  local dir=$1 expected=$2
  (
    cd "$dir"
    "$validator" \
      --source-class upstream-topic \
      --source-sha af596339dacee2ac472b041fec546b15f2665e33 \
      --upstream-base-sha af596339dacee2ac472b041fec546b15f2665e33 \
      --output evidence.json
  )
  jq -e --arg expected "$expected" \
    '.status == "pass" and .source_contract.deps_env.bd_version == $expected and .source_contract.relationships.go_mod_matches_bd_current_version and .source_contract.relationships.dockerfile_matches_bd_current_ref' \
    "$dir/evidence.json" >/dev/null \
    || fail "valid source contract did not produce passing evidence"
}

write_fixture "$tmp/rc" \
  v1.3.0-rc.2 v1.3.0-rc.2 c185735c38e25569277eae798ce363ecba9859e8 \
  v1.3.0-rc.2 c185735c38e25569277eae798ce363ecba9859e8
run_valid "$tmp/rc" v1.3.0-rc.2

write_fixture "$tmp/integration" \
  v1.1.0 v1.1.1-0.20260805093327-bf97b73749ac \
  bf97b73749ac3ef2fca2365b54537ac041ad4293 \
  v1.1.0 bf97b73749ac3ef2fca2365b54537ac041ad4293
run_valid "$tmp/integration" v1.1.0

write_fixture "$tmp/mixed" \
  v1.3.0-rc.2 v1.3.0-rc.2 c185735c38e25569277eae798ce363ecba9859e8 \
  v1.1.0 bf97b73749ac3ef2fca2365b54537ac041ad4293
if (cd "$tmp/mixed" && "$validator" \
      --source-class upstream-topic \
      --source-sha af596339dacee2ac472b041fec546b15f2665e33 \
      --upstream-base-sha af596339dacee2ac472b041fec546b15f2665e33 \
      --output evidence.json); then
  fail "reviewed source af596... plus Beads v1.1.0 packaging was accepted"
fi

echo "PASS: source dependency contract rejects the reviewed mixed packaging"
