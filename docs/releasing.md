# Release workflow

HarmonyNetBridge publishes from tags named `release-v<version>`. The workflow creates one GitHub Release with exactly these downloadable assets:

- `harmony-netbridge.zip`: an ad-hoc-signed Apple Silicon macOS CLI binary named `harmony-netbridge`.
- `harmony-netbridge.hap.zip`: a release-mode HarmonyOS HAP named `harmony-netbridge.hap`, signed with a device-bound debug Profile for `hdc` installation.

The workflow does not update or dispatch a Homebrew repository.

## One-time GitHub setup

### 1. Add the protected release environment

In **Settings → Environments**, create an environment named `release`. Configure custom deployment branches and tags so only tags matching `release-v*` and the `main` branch can deploy. `main` is needed only for the manual retry path below; the workflow still checks out and validates the requested release tag before using signing material. A required reviewer is optional; enabling one makes each release wait for manual approval.

In **Settings → Actions → General**, allow workflows to create releases with `GITHUB_TOKEN`. The workflow requests `contents: write` only in its final publishing job.

### 2. Use the GitHub-hosted HarmonyOS build runner

The HarmonyOS job runs on the standard GitHub-hosted Apple Silicon `macos-14` image. Public repositories can use standard GitHub-hosted runners without registering or maintaining a build machine.

Each fresh runner installs Temurin Java 21 and the HarmonyOS `26.0.0.621` Command Line Tools through the commit-pinned `ErBWs/setup-ohos` action. The downloaded SDK is cached between workflow runs. No DevEco Studio installation or runner registration is required in GitHub; the first uncached build will take longer while the toolchain is downloaded.

### 3. Upload the HarmonyOS device signing secret

The downloadable HAP is intended for `hdc` installation on registered development devices. A HarmonyOS release Profile is for trusted store distribution and contains no debugging device list; although its cryptographic signature can verify successfully, a development device can reject that HAP with install error `9568322` (`not trusted app source`). Use a debug certificate and debug Profile instead.

Create or obtain all of the following for bundle `io.github.realskyrin.harmonynetbridge` in AppGallery Connect:

- debug keystore (`.p12`), including its store password;
- key alias and key password;
- application debug certificate (`.cer`);
- application debug Profile (`.p7b`) containing every device that should install the GitHub Release HAP;
- the DevEco `material` directory stored beside the `.p12`, which decrypts the ciphertext password fields written by DevEco Studio.

Configure these as the `default` signing configuration in DevEco Studio. Then run this command from the repository root while that local `build-profile.json5` contains the debug configuration:

```bash
python3 scripts/ohos_signing_secret.py upload \
  --build-profile harmony/HarmonyNetBridge/build-profile.json5 \
  --repo realskyrin/harmony-netbridge \
  --environment release
```

The helper verifies the `debug` Profile type, exact bundle name, and a nonempty registered-device list. It packages the configuration and signing files in memory and uploads one environment secret named `OHOS_DEBUG_SIGNING_BUNDLE_BASE64`; it does not print the secret. The encoded bundle must stay below GitHub's 48 KB secret limit.

The workflow still compiles the application with `HNB_BUILD_MODE=release`; `debug` here describes the device-bound installation Profile, not the build optimization mode. Regenerate and upload the secret whenever the set of allowed development devices changes.

Never commit the populated `signingConfigs` block. The repository version of `build-profile.json5` must keep `"signingConfigs": []`; `scripts/release.py validate` enforces this before any release build starts.

## Publish a version

Before creating the tag:

1. Set the same semantic version in `harmony/HarmonyNetBridge/AppScope/app.json5` (`versionName`) and `harmony/HarmonyNetBridge/oh-package.json5` (`version`). Increment `versionCode` for every HarmonyOS update.
2. Move the relevant entries from `[Unreleased]` into a dated `## [x.y.z] - YYYY-MM-DD` section in `CHANGELOG.md`.
3. Run the release metadata check:

   ```bash
   python3 scripts/release.py validate x.y.z
   ```

4. Run the normal project checks and commit the release metadata.
5. Create and push the release tag:

   ```bash
   git tag release-vx.y.z
   git push --atomic origin main release-vx.y.z
   ```

The tag starts both builds in parallel on GitHub-hosted `macos-14` runners. The publishing job runs only after both assets pass their tests, version checks, signature verification, and packaging checks. Release notes are taken from the matching `CHANGELOG.md` section and include both asset SHA-256 values.

If GitHub accepted an existing tag but did not create a workflow run, retry that exact tag without moving or recreating it:

```bash
gh workflow run Release \
  --repo realskyrin/harmony-netbridge \
  --ref main \
  -f tag=release-vx.y.z
```

The manual run executes the workflow definition from `main`, but every build and release step checks out the supplied tag. The validation job fails before signing if the tag name, checked-out commit, version metadata, or changelog do not match.

## Install downloaded assets

macOS:

```bash
unzip harmony-netbridge.zip
install -m 755 harmony-netbridge /usr/local/bin/harmony-netbridge
```

HarmonyOS development device:

```bash
unzip harmony-netbridge.hap.zip
hdc install -r harmony-netbridge.hap
```

The HAP installs only on devices present in its debug Profile. Device policy, Developer Mode, USB debugging, and `hdc` authorization can still control whether sideload installation is allowed.
