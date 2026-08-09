#!/usr/bin/env python3
"""Validate HarmonyNetBridge release metadata and extract release notes."""

from __future__ import annotations

import argparse
from datetime import date
from pathlib import Path
import re
import subprocess
import sys

from ohos_signing_secret import SigningBundleError, load_json5


PROJECT_ROOT = Path(__file__).resolve().parent.parent
CHANGELOG = PROJECT_ROOT / "CHANGELOG.md"
APP_PROFILE = PROJECT_ROOT / "harmony/HarmonyNetBridge/AppScope/app.json5"
PACKAGE_PROFILE = PROJECT_ROOT / "harmony/HarmonyNetBridge/oh-package.json5"
BUILD_PROFILE = PROJECT_ROOT / "harmony/HarmonyNetBridge/build-profile.json5"
SEMVER = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+$")
CHANGELOG_HEADING = re.compile(
    r"^## \[(?P<version>[^]]+)](?: - (?P<date>[0-9]{4}-[0-9]{2}-[0-9]{2}))?\s*$",
    re.MULTILINE,
)
SIGNING_SUFFIXES = {".p12", ".p7b", ".pfx", ".jks", ".keystore"}


class ReleaseError(RuntimeError):
    """Raised when release metadata does not satisfy the publishing contract."""


def release_notes(version: str) -> str:
    try:
        changelog = CHANGELOG.read_text(encoding="utf-8")
    except OSError as error:
        raise ReleaseError(f"Unable to read {CHANGELOG}: {error}") from error
    matches = list(CHANGELOG_HEADING.finditer(changelog))
    target_indexes = [index for index, match in enumerate(matches) if match.group("version") == version]
    if len(target_indexes) != 1:
        raise ReleaseError(f"CHANGELOG.md must contain exactly one section for version {version}.")
    index = target_indexes[0]
    match = matches[index]
    release_date = match.group("date")
    if release_date is None:
        raise ReleaseError(f"CHANGELOG.md version {version} must include a YYYY-MM-DD date.")
    try:
        date.fromisoformat(release_date)
    except ValueError as error:
        raise ReleaseError(f"CHANGELOG.md version {version} has an invalid date.") from error
    end = matches[index + 1].start() if index + 1 < len(matches) else len(changelog)
    notes = changelog[match.end() : end].strip()
    if not notes or not re.search(r"^[-*] ", notes, re.MULTILINE):
        raise ReleaseError(f"CHANGELOG.md version {version} has no release-note bullets.")
    return notes + "\n"


def tracked_signing_material() -> list[str]:
    completed = subprocess.run(
        ["git", "-C", str(PROJECT_ROOT), "ls-files", "-z"],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if completed.returncode != 0:
        raise ReleaseError("git ls-files failed while checking signing material.")
    tracked = completed.stdout.decode("utf-8", errors="replace").split("\0")
    return sorted(path for path in tracked if Path(path).suffix.lower() in SIGNING_SUFFIXES)


def validate(version: str) -> None:
    if not SEMVER.fullmatch(version):
        raise ReleaseError("Release version must be a stable x.y.z version without a leading v.")
    try:
        app_profile = load_json5(APP_PROFILE)
        package_profile = load_json5(PACKAGE_PROFILE)
        build_profile = load_json5(BUILD_PROFILE)
    except SigningBundleError as error:
        raise ReleaseError(str(error)) from error

    try:
        app_version = app_profile["app"]["versionName"]
        version_code = app_profile["app"]["versionCode"]
        package_version = package_profile["version"]
        signing_configs = build_profile["app"]["signingConfigs"]
    except (KeyError, TypeError) as error:
        raise ReleaseError("A required release metadata field is missing.") from error
    if app_version != version:
        raise ReleaseError(f"AppScope versionName is {app_version!r}, expected {version!r}.")
    if package_version != version:
        raise ReleaseError(f"oh-package version is {package_version!r}, expected {version!r}.")
    if not isinstance(version_code, int) or version_code <= 0:
        raise ReleaseError("AppScope versionCode must be a positive integer.")
    if signing_configs != []:
        raise ReleaseError("The committed build-profile.json5 must keep signingConfigs empty.")
    tracked_material = tracked_signing_material()
    if tracked_material:
        raise ReleaseError("Signing key/profile material is tracked by Git: " + ", ".join(tracked_material))
    release_notes(version)


def command_validate(args: argparse.Namespace) -> None:
    validate(args.version)
    print(f"Release metadata for {args.version} is consistent and signing-safe.")


def command_notes(args: argparse.Namespace) -> None:
    notes = release_notes(args.version)
    args.output.write_text(notes, encoding="utf-8")
    print(f"Wrote release notes for {args.version} to {args.output}.")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    validate_parser = subparsers.add_parser("validate")
    validate_parser.add_argument("version")
    validate_parser.set_defaults(handler=command_validate)

    notes_parser = subparsers.add_parser("notes")
    notes_parser.add_argument("version")
    notes_parser.add_argument("output", type=Path)
    notes_parser.set_defaults(handler=command_notes)
    return parser


def main() -> int:
    args = build_parser().parse_args()
    try:
        args.handler(args)
    except ReleaseError as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
