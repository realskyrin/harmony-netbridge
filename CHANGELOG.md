# Changelog

All notable changes to **HarmonyNetBridge** will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Publish the HarmonyOS asset as `harmony-netbridge.hap.zip` and sign its release-mode HAP with a device-bound debug Profile so registered development devices can install it through `hdc`.

## [0.4.0] - 2026-08-09

### Added

- Add the macOS USB bridge daemon with gVisor-based IPv4 TCP, UDP, and split-DNS forwarding.
- Add the HarmonyOS NEXT VPN app with per-application whitelist and blacklist routing.
- Add automatic bridge reconnection and exact `hdc rport` recovery after USB reconnects.
- Add an isolated mitmweb proxy mode with HTTP diagnostics and a local CA handoff flow.
- Add signed HarmonyOS HAP and Apple Silicon macOS binary packaging for GitHub Releases.

### Changed

- Productize the HarmonyOS home and Settings surfaces around connection, routing, diagnostics, and certificate actions.
- Bound VPN shutdown and resource cleanup so the App converges instead of remaining in a stopping state.
- Build both release assets on ephemeral GitHub-hosted Apple Silicon macOS runners.
