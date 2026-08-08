#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
signed_hap="${project_root}/harmony/HarmonyNetBridge/entry/build/default/outputs/default/entry-default-signed.hap"
bundle_name="io.github.realskyrin.harmonynetbridge"
ability_name="EntryAbility"
requested_device="${HNB_DEVICE:-}"

usage() {
  cat <<'EOF'
Usage: ./scripts/run-ohos-app.sh [--device <index-or-target>]

Builds the HarmonyOS App, installs the signed HAP, and opens EntryAbility.
With one connected device, the target is selected automatically. With multiple
devices, rerun with the 1-based index printed by this script.

Environment overrides:
  HNB_HDC       Path to the hdc executable
  HNB_DEVICE    Explicit device index or target
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --device)
      if [[ $# -lt 2 || -z "$2" ]]; then
        echo "--device requires a 1-based index or hdc target." >&2
        usage >&2
        exit 2
      fi
      requested_device="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ "${requested_device}" == *$'\n'* || "${requested_device}" == *$'\r'* ]]; then
  echo "Device selection must be a single index or target." >&2
  exit 2
fi

if [[ -n "${HNB_HDC:-}" ]]; then
  hdc_bin="${HNB_HDC}"
elif command -v hdc >/dev/null 2>&1; then
  hdc_bin="$(command -v hdc)"
elif [[ -x "/Applications/DevEco-Studio.app/Contents/sdk/default/openharmony/toolchains/hdc" ]]; then
  hdc_bin="/Applications/DevEco-Studio.app/Contents/sdk/default/openharmony/toolchains/hdc"
elif [[ -x "/Applications/DevEco Studio.app/Contents/sdk/default/openharmony/toolchains/hdc" ]]; then
  hdc_bin="/Applications/DevEco Studio.app/Contents/sdk/default/openharmony/toolchains/hdc"
else
  echo "hdc was not found. Set HNB_HDC to the hdc executable path." >&2
  exit 1
fi

if [[ ! -x "${hdc_bin}" ]]; then
  echo "hdc is not executable: ${hdc_bin}" >&2
  exit 1
fi

targets_file="$(mktemp /tmp/hnb-ohos-targets.XXXXXX)"
trap 'rm -f "${targets_file}"' EXIT

if ! "${hdc_bin}" list targets | awk '
  {
    sub(/\r$/, "")
    gsub(/^[[:space:]]+|[[:space:]]+$/, "")
    if ($0 != "" && $0 != "[Empty]" && $0 != "Empty") {
      print
    }
  }
' > "${targets_file}"; then
  echo "Failed to query HarmonyOS devices with hdc." >&2
  exit 1
fi

device_count="$(awk 'END { print NR + 0 }' "${targets_file}")"
if [[ "${device_count}" -eq 0 ]]; then
  echo "No connected HarmonyOS device was found. Connect and authorize a device, then retry." >&2
  exit 1
fi

print_devices() {
  awk '
    {
      target = $0
      if (length(target) > 8) {
        target = substr(target, 1, 4) "..." substr(target, length(target) - 3)
      }
      printf "  %d. %s\n", NR, target
    }
  ' "${targets_file}"
}

selected_device=""
if [[ -z "${requested_device}" ]]; then
  if [[ "${device_count}" -ne 1 ]]; then
    echo "Multiple HarmonyOS devices are connected. Select one explicitly:" >&2
    print_devices >&2
    echo "Rerun: ./scripts/run-ohos-app.sh --device <index>" >&2
    exit 2
  fi
  selected_device="$(sed -n '1p' "${targets_file}")"
  echo "Using the only connected HarmonyOS device."
elif [[ "${requested_device}" =~ ^[0-9]+$ ]]; then
  if [[ "${requested_device}" =~ ^0 ]]; then
    echo "Device index must be a 1-based number without leading zeroes." >&2
    print_devices >&2
    exit 2
  fi
  selected_device="$(awk -v index="${requested_device}" 'NR == index { print; exit }' "${targets_file}")"
  if [[ -z "${selected_device}" ]]; then
    echo "Device index is out of range: ${requested_device}" >&2
    print_devices >&2
    exit 2
  fi
  echo "Using HarmonyOS device #${requested_device}."
else
  selected_device="$(awk -v target="${requested_device}" '$0 == target { print; exit }' "${targets_file}")"
  if [[ -z "${selected_device}" ]]; then
    echo "The requested HarmonyOS device is not connected." >&2
    print_devices >&2
    exit 2
  fi
  echo "Using the explicitly selected HarmonyOS device."
fi

"${project_root}/scripts/build-harmony.sh"

if [[ ! -f "${signed_hap}" ]]; then
  echo "A signed HAP is required for device installation: ${signed_hap}" >&2
  echo "Configure a matching personal debug signature in DevEco Studio, then retry." >&2
  exit 1
fi

"${hdc_bin}" -t "${selected_device}" install -r "${signed_hap}"
"${hdc_bin}" -t "${selected_device}" shell aa start -a "${ability_name}" -b "${bundle_name}"

echo "HarmonyNetBridge is installed and open on the selected device. Please inspect the App UI and behavior."
