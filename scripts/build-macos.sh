#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output_dir="${project_root}/bin"
output_file="${output_dir}/harmony-netbridge"
commit="unknown"

if git -C "${project_root}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  commit="$(git -C "${project_root}" rev-parse --short HEAD)"
fi

mkdir -p "${output_dir}"
cd "${project_root}"
GOOS=darwin GOARCH=arm64 go build \
  -trimpath \
  -ldflags "-X github.com/realskyrin/harmony-netbridge/internal/version.Commit=${commit}" \
  -o "${output_file}" \
  ./cmd/harmony-netbridge

echo "Built ${output_file}"
