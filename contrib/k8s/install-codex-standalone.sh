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
metadata="$tmp_dir/release.json"
checksums="$tmp_dir/$checksums_name"
package_archive="$tmp_dir/$package_name"

metadata_source="openai"
if ! curl -fsSL --retry 3 --retry-all-errors --connect-timeout 15 \
  "${openai_release_root}/${release_version}/release.json" -o "$metadata"; then
  metadata_source="github"
  curl -fsSL --retry 3 --retry-all-errors --connect-timeout 15 \
    -H 'Accept: application/vnd.github+json' \
    -H 'X-GitHub-Api-Version: 2022-11-28' \
    "${github_api_root}/${expected_tag}" -o "$metadata"
fi

metadata_tag="$(jq -r '.tag_name // empty' "$metadata")"
# The OpenAI mirror currently serves release.json without tag_name. Its exact
# version-addressed endpoint is still checked against GitHub's exact release
# tag so the tag invariant remains explicit without trusting a floating ref.
if [[ -z "$metadata_tag" && "$metadata_source" == "openai" ]]; then
  metadata_tag="$(curl -fsSL --retry 3 --retry-all-errors --connect-timeout 15 \
    -H 'Accept: application/vnd.github+json' \
    -H 'X-GitHub-Api-Version: 2022-11-28' \
    "${github_api_root}/${expected_tag}" | jq -er '.tag_name')"
fi
[[ "$metadata_tag" == "$expected_tag" ]] || {
  echo "Codex release metadata tag ${metadata_tag:-<missing>} does not equal ${expected_tag}" >&2
  exit 1
}

asset_digest() {
  local name="$1" value
  value="$(jq -er --arg name "$name" \
    '.assets[] | select(.name == $name) | .digest' "$metadata")"
  [[ "$value" =~ ^sha256:[0-9a-f]{64}$ ]] || {
    echo "Codex release metadata has no SHA-256 digest for ${name}" >&2
    exit 1
  }
  printf '%s\n' "${value#sha256:}"
}

download_and_verify() {
  local name="$1" digest="$2" url="$3" output="$4" actual
  curl -fsSL --retry 3 --retry-all-errors --connect-timeout 15 "$url" -o "$output"
  actual="$(sha256sum "$output" | awk '{print $1}')"
  [[ "$actual" == "$digest" ]] || {
    echo "Codex ${name} SHA-256 mismatch: got ${actual}, want ${digest}" >&2
    exit 1
  }
}

package_digest="$(asset_digest "$package_name")"
checksums_digest="$(asset_digest "$checksums_name")"
if [[ "$metadata_source" == "openai" ]]; then
  asset_root="${openai_release_root}/${release_version}"
else
  asset_root="${github_release_root}/${expected_tag}"
fi
download_and_verify "$checksums_name" "$checksums_digest" \
  "${asset_root}/${checksums_name}" "$checksums"
download_and_verify "$package_name" "$package_digest" \
  "${asset_root}/${package_name}" "$package_archive"

# The release's own package manifest is a second independent checksum source.
grep -F -x "${package_digest}  ${package_name}" "$checksums" >/dev/null || {
  echo "Codex package digest is absent from ${checksums_name}" >&2
  exit 1
}

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
