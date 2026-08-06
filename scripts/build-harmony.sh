#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
harmony_root="${project_root}/harmony/HarmonyNetBridge"
ohpm_bin="${HNB_OHPM:-/Applications/DevEco-Studio.app/Contents/tools/ohpm/bin/ohpm}"
hvigor_bin="${HNB_HVIGORW:-/Applications/DevEco-Studio.app/Contents/tools/hvigor/bin/hvigorw}"

cd "${harmony_root}"
"${ohpm_bin}" install
"${hvigor_bin}" \
  --mode module \
  -p product=default \
  -p module=entry@default \
  -p buildMode=debug \
  assembleHap \
  --no-daemon

signed_hap="${harmony_root}/entry/build/default/outputs/default/entry-default-signed.hap"
unsigned_hap="${harmony_root}/entry/build/default/outputs/default/entry-default-unsigned.hap"
if [[ -f "${signed_hap}" ]]; then
  echo "Built ${signed_hap}"
else
  echo "Built ${unsigned_hap}"
fi
