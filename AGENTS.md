# Repository agent instructions

## HarmonyOS App changes

- After every change that affects HarmonyOS App functionality, UI, interaction, state, or runtime behavior, you must run `./scripts/run-ohos-app.sh` before handing the work back to the user.
- This run is required in addition to relevant static checks and tests. A successful compile, unit test, or HAP build does not replace launching the updated App for user inspection.
- The script builds the App, installs the signed HAP, and opens `EntryAbility`. After it succeeds, tell the user that the updated App is open and ready for their manual check; do not claim that the behavior itself has been verified unless it was actually exercised.
- If multiple HarmonyOS devices are connected, the script prints one unified 1-based list and exits. Never choose a device silently. Ask the user for the target index, then rerun `./scripts/run-ohos-app.sh --device <index>`.
- If the script cannot run because no device is connected, signing is unavailable, or the device is not authorized, report that concrete blocker and distinguish it from any static checks that did pass.

## macOS binary changes

- After every task that changes source code, build inputs, or runtime behavior included in the macOS executable, you must run `./scripts/build-macos.sh` before handing the work back to the user.
- This build is required in addition to relevant static checks and tests. It regenerates `bin/harmony-netbridge`; after it succeeds, tell the user that the macOS binary was rebuilt. If it cannot run, report the concrete blocker and distinguish it from any other checks that did pass.
