#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
harmony_root="${project_root}/harmony/HarmonyNetBridge"
ohpm_bin="${HNB_OHPM:-/Applications/DevEco-Studio.app/Contents/tools/ohpm/bin/ohpm}"
hvigor_bin="${HNB_HVIGORW:-/Applications/DevEco-Studio.app/Contents/tools/hvigor/bin/hvigorw}"
node_home="${HNB_NODE_HOME:-/Applications/DevEco-Studio.app/Contents/tools/node}"
result_file="${harmony_root}/entry/.test/default/intermediates/test/coverage_data/test_result.txt"

if [[ ! -x "${node_home}/bin/node" ]]; then
  echo "DevEco Node.js runtime not found: ${node_home}/bin/node" >&2
  exit 1
fi

cd "${harmony_root}"
"${ohpm_bin}" install
NODE_HOME="${node_home}" "${hvigor_bin}" \
  --mode module \
  -p product=default \
  -p module=entry@default \
  -p buildMode=debug \
  test \
  --no-daemon

if [[ ! -f "${result_file}" ]]; then
  echo "Hypium result file was not generated: ${result_file}" >&2
  exit 1
fi

summary="$(tail -n 1 "${result_file}")"
case "${summary}" in
  *"Failure: 0, Error: 0"*)
    echo "${summary}"
    ;;
  *)
    sed -n '1,240p' "${result_file}" >&2
    exit 1
    ;;
esac
