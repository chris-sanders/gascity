#!/usr/bin/env bash
# Download and verify one exact OpenAI Codex standalone release package.
#
# The OpenAI release service is authoritative when available. GitHub is an
# exact-tag fallback only; neither path accepts a floating or prerelease ref.
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: install-codex-standalone.sh --version X.Y.Z --target x86_64-unknown-linux-musl --destination DIR
EOF
  exit 64
}

version=""
target=""
destination=""
while (($#)); do
  case "$1" in
    --version)
      (($# >= 2)) || usage
      version="$2"
      shift 2
      ;;
    --target)
      (($# >= 2)) || usage
      target="$2"
      shift 2
      ;;
    --destination)
      (($# >= 2)) || usage
      destination="$2"
      shift 2
      ;;
    *)
      usage
      ;;
  esac
done

[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo "Codex version must be stable X.Y.Z: ${version:-<missing>}" >&2
  exit 1
}
[[ "$target" == "x86_64-unknown-linux-musl" ]] || {
  echo "unsupported Codex standalone target: ${target:-<missing>}" >&2
  exit 1
}
[[ -n "$destination" && "$destination" != "/" ]] || {
  echo "Codex standalone destination must be a non-root directory" >&2
  exit 1
}

command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 1; }
command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required" >&2; exit 1; }
command -v tar >/dev/null 2>&1 || { echo "tar is required" >&2; exit 1; }

release_version="$version"
expected_tag="rust-v${release_version}"
package_name="codex-package-${target}.tar.gz"
checksums_name="codex-package_SHA256SUMS"
openai_release_root="${CODEX_OPENAI_RELEASE_ROOT:-https://releases.openai.com/codex/releases}"
github_api_root="${CODEX_GITHUB_API_ROOT:-https://api.github.com/repos/openai/codex/releases/tags}"
github_release_root="${CODEX_GITHUB_RELEASE_ROOT:-https://github.com/openai/codex/releases/download}"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
openai_metadata="$tmp_dir/openai-release.json"
github_metadata="$tmp_dir/github-release.json"

fetch_github_metadata() {
  local output="$1"
  curl -fsSL --retry 3 --retry-all-errors --connect-timeout 15 \
    -H 'Accept: application/vnd.github+json' \
    -H 'X-GitHub-Api-Version: 2022-11-28' \
    "${github_api_root}/${expected_tag}" -o "$output"
}

exact_release_tag() {
  local metadata_path="$1" metadata_tag
  metadata_tag="$(jq -r '.tag_name // empty' "$metadata_path")"
  [[ "$metadata_tag" == "$expected_tag" ]] || {
    echo "Codex release metadata tag ${metadata_tag:-<missing>} does not equal ${expected_tag}" >&2
    return 1
  }
}

asset_digest() {
  local metadata_path="$1" name="$2" value
  value="$(jq -er --arg name "$name" \
    '.assets[]? | select(.name == $name) | .digest // empty' "$metadata_path" 2>/dev/null)" || {
    echo "Codex release metadata has no SHA-256 digest for ${name}" >&2
    return 1
  }
  [[ "$value" =~ ^sha256:[0-9a-f]{64}$ ]] || {
    echo "Codex release metadata has no SHA-256 digest for ${name}" >&2
    return 1
  }
  printf '%s\n' "${value#sha256:}"
}

download_and_verify() {
  local name="$1" digest="$2" url="$3" output="$4" actual
  if ! curl -fsSL --retry 3 --retry-all-errors --connect-timeout 15 "$url" -o "$output"; then
    echo "Codex ${name} download failed from ${url}" >&2
    return 1
  fi
  actual="$(sha256sum "$output" | awk '{print $1}')"
  [[ "$actual" == "$digest" ]] || {
    echo "Codex ${name} SHA-256 mismatch: got ${actual}, want ${digest}" >&2
    return 1
  }
}

verify_release_assets() {
  local metadata_path="$1" asset_root="$2" transaction_dir="$3"
  local checksums="$transaction_dir/$checksums_name"
  local package_archive="$transaction_dir/$package_name"
  local package_digest checksums_digest

  package_digest="$(asset_digest "$metadata_path" "$package_name")" || return 1
  checksums_digest="$(asset_digest "$metadata_path" "$checksums_name")" || return 1
  download_and_verify "$checksums_name" "$checksums_digest" \
    "${asset_root}/${checksums_name}" "$checksums" || return 1
  download_and_verify "$package_name" "$package_digest" \
    "${asset_root}/${package_name}" "$package_archive" || return 1

  # The release's own package manifest is a second independent checksum source.
  awk -v digest="$package_digest" -v name="$package_name" \
    '$1 == digest && ($2 == name || $2 == "*" name) { found = 1 } END { exit !found }' \
    "$checksums" || {
      echo "Codex package digest is absent from ${checksums_name}" >&2
      return 1
    }
}

metadata_source=""
transaction_dir=""
if curl -fsSL --retry 3 --retry-all-errors --connect-timeout 15 \
  "${openai_release_root}/${release_version}/release.json" -o "$openai_metadata"; then
  # The OpenAI mirror may omit tag_name. In that case, validate the
  # version-addressed metadata against the exact GitHub release tag, but do
  # not borrow any GitHub asset digest or asset bytes for this transaction.
  openai_tag="$(jq -r '.tag_name // empty' "$openai_metadata")"
  if [[ -n "$openai_tag" ]]; then
    exact_release_tag "$openai_metadata" || openai_tag="invalid"
  elif fetch_github_metadata "$github_metadata" && exact_release_tag "$github_metadata"; then
    :
  else
    echo "Codex OpenAI metadata could not be tied to exact tag ${expected_tag}" >&2
    openai_tag="invalid"
  fi
  if [[ "$openai_tag" != "invalid" ]]; then
    transaction_dir="$tmp_dir/openai-assets"
    mkdir -p "$transaction_dir"
    if verify_release_assets "$openai_metadata" \
      "${openai_release_root}/${release_version}" "$transaction_dir"; then
      metadata_source="openai"
    fi
  fi
fi

if [[ -z "$metadata_source" ]]; then
  # A metadata success followed by any OpenAI asset download or verification
  # failure enters this one coherent exact-tag GitHub transaction. Its
  # metadata, digests, and bytes cannot be mixed with the OpenAI attempt.
  fetch_github_metadata "$github_metadata" || {
    echo "Codex exact GitHub release metadata could not be fetched" >&2
    exit 1
  }
  exact_release_tag "$github_metadata" || exit 1
  transaction_dir="$tmp_dir/github-assets"
  mkdir -p "$transaction_dir"
  verify_release_assets "$github_metadata" \
    "${github_release_root}/${expected_tag}" "$transaction_dir" || exit 1
  metadata_source="github"
fi

checksums="$transaction_dir/$checksums_name"
package_archive="$transaction_dir/$package_name"

mkdir -p "$destination"
tar --extract --gzip --file "$package_archive" --directory "$destination" \
  --no-same-owner --no-same-permissions
test -f "$destination/codex-package.json"
test -x "$destination/bin/codex"
test -x "$destination/bin/codex-code-mode-host"
test -x "$destination/codex-path/rg"
test -x "$destination/codex-resources/bwrap"
package_version="$(jq -er '.version' "$destination/codex-package.json")"
package_target="$(jq -er '.target' "$destination/codex-package.json")"
[[ "$package_version" == "$release_version" ]] || {
  echo "Codex package version ${package_version} does not equal ${release_version}" >&2
  exit 1
}
[[ "$package_target" == "$target" ]] || {
  echo "Codex package target ${package_target} does not equal ${target}" >&2
  exit 1
}
[[ "$("$destination/bin/codex" --version)" == "codex-cli ${release_version}" ]] || {
  echo "Codex standalone binary version check failed" >&2
  exit 1
}

printf 'verified Codex standalone %s (%s) from %s\n' "$release_version" "$target" "$metadata_source"
