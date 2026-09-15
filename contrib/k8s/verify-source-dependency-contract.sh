#!/usr/bin/env bash
# Verify that source-owned Beads pins remain the pins used by the source-owned
# agent image. This is deliberately data-driven: it never names a particular
# Beads release and therefore remains valid when upstream changes its pins.
set -euo pipefail

source_class=${SOURCE_CLASS:-}
source_sha=${SOURCE_SHA:-}
upstream_base_sha=${UPSTREAM_BASE_SHA:-}
output=source-dependency-evidence.json

fail() {
  echo "source dependency contract: $*" >&2
  exit 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --source-class)
      [[ $# -ge 2 ]] || fail "$1 requires a value"
      source_class=$2
      shift 2
      ;;
    --source-sha)
      [[ $# -ge 2 ]] || fail "$1 requires a value"
      source_sha=$2
      shift 2
      ;;
    --upstream-base-sha)
      [[ $# -ge 2 ]] || fail "$1 requires a value"
      upstream_base_sha=$2
      shift 2
      ;;
    --output)
      [[ $# -ge 2 ]] || fail "$1 requires a value"
      output=$2
      shift 2
      ;;
    *)
      fail "unknown argument $1"
      ;;
  esac
done

[[ "$source_class" == upstream-topic || "$source_class" == integration-control ]] \
  || fail "source class must be upstream-topic or integration-control"
[[ "$source_sha" =~ ^[0-9a-f]{40}$ ]] || fail "source SHA must be a full lowercase 40-character SHA"
[[ "$upstream_base_sha" =~ ^[0-9a-f]{40}$ ]] \
  || fail "upstream base SHA must be a full lowercase 40-character SHA"
[[ -r deps.env ]] || fail "deps.env is missing"
[[ -r go.mod ]] || fail "go.mod is missing"
[[ -r contrib/k8s/Dockerfile.agent ]] || fail "source-owned contrib/k8s/Dockerfile.agent is missing"

read_assignment() {
  local file=$1 key=$2 value
  value=$(awk -v key="$key" '
    $0 ~ "^[[:space:]]*" key "[[:space:]]*=" {
      line = $0
      sub("^[[:space:]]*" key "[[:space:]]*=[[:space:]]*", "", line)
      count++
      value = line
    }
    END {
      if (count != 1 || value == "") exit 1
      print value
    }
  ' "$file") || fail "$key must have exactly one non-empty assignment in $file"
  printf '%s' "$value"
}

read_docker_arg() {
  local key=$1 value
  value=$(awk -v key="$key" '
    $0 ~ "^[[:space:]]*ARG[[:space:]]+" key "([[:space:]]|=)" {
      line = $0
      sub("^[[:space:]]*ARG[[:space:]]+" key "[[:space:]]*=[[:space:]]*", "", line)
      count++
      value = line
    }
    END {
      if (count != 1 || value == "") exit 1
      print value
    }
  ' contrib/k8s/Dockerfile.agent) || fail "ARG $key must have exactly one non-empty assignment in source-owned Dockerfile.agent"
  printf '%s' "$value"
}

bd_version=$(read_assignment deps.env BD_VERSION)
bd_current_version=$(read_assignment deps.env BD_CURRENT_VERSION)
bd_current_ref=$(read_assignment deps.env BD_CURRENT_REF)
go_mod_beads_version=$(awk '
  $1 == "github.com/steveyegge/beads" && $2 ~ /^v/ {
    count++
    value = $2
  }
  $1 == "require" && $2 == "github.com/steveyegge/beads" && $3 ~ /^v/ {
    count++
    value = $3
  }
  END {
    if (count != 1 || value == "") exit 1
    print value
  }
' go.mod) || fail "go.mod must declare exactly one github.com/steveyegge/beads version"
docker_bd_version=$(read_docker_arg BD_VERSION)
docker_bd_source_ref=$(read_docker_arg BD_SOURCE_REF)

[[ "$bd_current_ref" =~ ^[0-9a-f]{40}$ ]] || fail "BD_CURRENT_REF is not a full lowercase 40-character SHA"
[[ "$docker_bd_source_ref" =~ ^[0-9a-f]{40}$ ]] \
  || fail "Dockerfile.agent BD_SOURCE_REF is not a full lowercase 40-character SHA"
[[ "$go_mod_beads_version" == "$bd_current_version" ]] \
  || fail "go.mod Beads version $go_mod_beads_version does not match deps.env BD_CURRENT_VERSION $bd_current_version"
[[ "$docker_bd_version" == "$bd_version" ]] \
  || fail "source Dockerfile.agent Beads release $docker_bd_version does not match deps.env BD_VERSION $bd_version"
[[ "$docker_bd_source_ref" == "$bd_current_ref" ]] \
  || fail "source Dockerfile.agent Beads source $docker_bd_source_ref does not match deps.env BD_CURRENT_REF $bd_current_ref"
grep -Fq 'BD_SOURCE_REF}.tar.gz' contrib/k8s/Dockerfile.agent \
  || fail "Dockerfile.agent does not fetch Beads from its declared source ref"
grep -Fq 'grep -Fq "Version = \"${bd_version}\"" cmd/bd/version.go' contrib/k8s/Dockerfile.agent \
  || fail "Dockerfile.agent does not assert the fetched Beads release identity"

jq -n \
  --arg source_class "$source_class" \
  --arg source_sha "$source_sha" \
  --arg upstream_base_sha "$upstream_base_sha" \
  --arg bd_version "$bd_version" \
  --arg bd_current_version "$bd_current_version" \
  --arg bd_current_ref "$bd_current_ref" \
  --arg go_mod_beads_version "$go_mod_beads_version" \
  --arg docker_bd_version "$docker_bd_version" \
  --arg docker_bd_source_ref "$docker_bd_source_ref" \
  '{schema_version:1,status:"pass",source_class:$source_class,source_sha:$source_sha,upstream_base_sha:$upstream_base_sha,source_contract:{deps_env:{bd_version:$bd_version,bd_current_version:$bd_current_version,bd_current_ref:$bd_current_ref},go_mod:{beads_version:$go_mod_beads_version},dockerfile_agent:{bd_version:$docker_bd_version,bd_source_ref:$docker_bd_source_ref},relationships:{go_mod_matches_bd_current_version:true,dockerfile_matches_bd_version:true,dockerfile_matches_bd_current_ref:true}},packaging:{runtime_dockerfiles:"source-owned",stage14_agent_layer:"build-control-additive"}}' \
  >"$output"
chmod 600 "$output"
echo "PASS: source-owned dependency contract is coherent ($source_sha)"
