#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
check_dir="$(mktemp -d /tmp/hnb-check.XXXXXX)"
trap 'rm -rf "${check_dir}"' EXIT

cd "${project_root}"
go test -race ./...
go vet ./...
GOOS=darwin GOARCH=arm64 go build -trimpath -o "${check_dir}/harmony-netbridge" ./cmd/harmony-netbridge
"${check_dir}/harmony-netbridge" --help | grep -q 'harmony-netbridge .* proxy'
python3 -m unittest discover -s scripts/tests -p 'test_*.py'
"${project_root}/scripts/test-harmony.sh"
"${project_root}/scripts/build-harmony.sh"

hap_file="${project_root}/harmony/HarmonyNetBridge/entry/build/default/outputs/default/entry-default-unsigned.hap"
module_profile="$(unzip -p "${hap_file}" module.json)"
case "${module_profile}" in
  *'"type":"vpn"'*) ;;
  *)
    echo "Built HAP is missing the VPN extension." >&2
    exit 1
    ;;
esac
case "${module_profile}" in
  *'ohos.permission.INTERNET'*) ;;
  *)
    echo "Built HAP is missing the INTERNET permission." >&2
    exit 1
    ;;
esac
case "${module_profile}" in
  *'MANAGE_VPN'*)
    echo "Third-party VPN HAP must not request MANAGE_VPN." >&2
    exit 1
    ;;
esac

echo "HarmonyNetBridge Phase 4 static and host checks passed."
