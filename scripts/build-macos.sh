#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output_dir="${project_root}/bin"
output_file="${output_dir}/harmony-netbridge"
commit="unknown"
go_bin="${GO_BIN:-go}"

go_is_supported() {
  local version_output

  version_output="$("$1" version 2>/dev/null)" || return 1
  if [[ "${version_output}" =~ go([0-9]+)\.([0-9]+) ]]; then
    ((BASH_REMATCH[1] > 1 || (BASH_REMATCH[1] == 1 && BASH_REMATCH[2] >= 24)))
    return
  fi
  return 1
}

if ! command -v "${go_bin}" >/dev/null 2>&1; then
  echo "Go executable not found: ${go_bin}" >&2
  exit 1
fi
go_bin="$(command -v "${go_bin}")"

if ! go_is_supported "${go_bin}" && [[ -z "${GO_BIN:-}" ]] && command -v brew >/dev/null 2>&1; then
  for formula in go go@1.24; do
    brew_prefix="$(brew --prefix "${formula}" 2>/dev/null || true)"
    brew_go="${brew_prefix}/bin/go"
    if [[ -n "${brew_prefix}" && -x "${brew_go}" ]] && go_is_supported "${brew_go}"; then
      go_bin="${brew_go}"
      break
    fi
  done
fi

if ! go_is_supported "${go_bin}"; then
  echo "Go 1.24 or newer is required; set GO_BIN to a compatible executable." >&2
  exit 1
fi

if git -C "${project_root}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  commit="$(git -C "${project_root}" rev-parse --short HEAD)"
fi

mkdir -p "${output_dir}"
cd "${project_root}"
GOOS=darwin GOARCH=arm64 "${go_bin}" build \
  -trimpath \
  -ldflags "-X github.com/realskyrin/harmony-netbridge/internal/version.Commit=${commit}" \
  -o "${output_file}" \
  ./cmd/harmony-netbridge

echo "Built ${output_file}"
