#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
harmony_root="${project_root}/harmony/HarmonyNetBridge"
ohpm_bin="${HNB_OHPM:-/Applications/DevEco-Studio.app/Contents/tools/ohpm/bin/ohpm}"
hvigor_bin="${HNB_HVIGORW:-/Applications/DevEco-Studio.app/Contents/tools/hvigor/bin/hvigorw}"
node_home="${HNB_NODE_HOME:-/Applications/DevEco-Studio.app/Contents/tools/node}"
build_mode="${HNB_BUILD_MODE:-debug}"
require_signed="${HNB_REQUIRE_SIGNED:-0}"
signed_hap="${harmony_root}/entry/build/default/outputs/default/entry-default-signed.hap"
unsigned_hap="${harmony_root}/entry/build/default/outputs/default/entry-default-unsigned.hap"

case "${build_mode}" in
  debug|release) ;;
  *)
    echo "HNB_BUILD_MODE must be debug or release." >&2
    exit 1
    ;;
esac

case "${require_signed}" in
  0|1) ;;
  *)
    echo "HNB_REQUIRE_SIGNED must be 0 or 1." >&2
    exit 1
    ;;
esac

if [[ ! -x "${node_home}/bin/node" ]]; then
  echo "DevEco Node.js runtime not found: ${node_home}/bin/node" >&2
  exit 1
fi

cd "${harmony_root}"
"${ohpm_bin}" install
rm -f "${signed_hap}" "${unsigned_hap}"
NODE_HOME="${node_home}" "${hvigor_bin}" \
  --mode module \
  -p product=default \
  -p module=entry@default \
  -p buildMode="${build_mode}" \
  assembleHap \
  --no-daemon

if [[ -f "${signed_hap}" ]]; then
  echo "Built ${signed_hap}"
elif [[ "${require_signed}" == "1" ]]; then
  echo "A signed HAP was required, but the build produced no signed artifact." >&2
  exit 1
elif [[ -f "${unsigned_hap}" ]]; then
  echo "Built ${unsigned_hap}"
else
  echo "HarmonyOS build completed without the expected HAP artifact." >&2
  exit 1
fi
