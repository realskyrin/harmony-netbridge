#!/usr/bin/env python3
"""Manage the release-only HarmonyOS signing bundle used by GitHub Actions."""

from __future__ import annotations

import argparse
import base64
import copy
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
from typing import Any


SECRET_NAME = "OHOS_SIGNING_BUNDLE_BASE64"
SECRET_SIZE_LIMIT = 48 * 1024
MAX_ARCHIVE_MEMBERS = 128
MAX_EXTRACTED_BYTES = 4 * 1024 * 1024
DEFAULT_BUILD_PROFILE = Path("harmony/HarmonyNetBridge/build-profile.json5")
DEFAULT_HAP_SIGN_TOOL = Path(
    "/Applications/DevEco-Studio.app/Contents/sdk/default/openharmony/"
    "toolchains/lib/hap-sign-tool.jar"
)
DEFAULT_JAVA = Path("/Applications/DevEco-Studio.app/Contents/jbr/Contents/Home/bin/java")
REQUIRED_MATERIAL_KEYS = (
    "certpath",
    "storePassword",
    "keyAlias",
    "keyPassword",
    "profile",
    "signAlg",
    "storeFile",
)
ARCHIVE_FILES = {
    "certpath": "signing/signing.cer",
    "profile": "signing/signing.p7b",
    "storeFile": "signing/signing.p12",
}


class SigningBundleError(RuntimeError):
    """Raised when signing material is incomplete, unsafe, or unsuitable."""


def _strip_json5_comments(text: str) -> str:
    result: list[str] = []
    index = 0
    quote = ""
    escaped = False
    while index < len(text):
        current = text[index]
        following = text[index + 1] if index + 1 < len(text) else ""
        if quote:
            result.append(current)
            if escaped:
                escaped = False
            elif current == "\\":
                escaped = True
            elif current == quote:
                quote = ""
            index += 1
            continue
        if current in ('"', "'"):
            quote = current
            result.append(current)
            index += 1
            continue
        if current == "/" and following == "/":
            index += 2
            while index < len(text) and text[index] not in "\r\n":
                index += 1
            continue
        if current == "/" and following == "*":
            index += 2
            while index + 1 < len(text) and text[index : index + 2] != "*/":
                index += 1
            index += 2
            continue
        result.append(current)
        index += 1
    return "".join(result)


def load_json5(path: Path) -> dict[str, Any]:
    try:
        text = _strip_json5_comments(path.read_text(encoding="utf-8"))
        text = re.sub(r",(?=\s*[}\]])", "", text)
        data = json.loads(text)
    except (OSError, json.JSONDecodeError) as error:
        raise SigningBundleError(f"Unable to read supported JSON5 from {path}: {error}") from error
    if not isinstance(data, dict):
        raise SigningBundleError(f"Expected a JSON object in {path}.")
    return data


def write_json5(path: Path, data: dict[str, Any]) -> None:
    temporary = path.with_name(f".{path.name}.tmp")
    temporary.write_text(
        json.dumps(data, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    temporary.chmod(0o600)
    os.replace(temporary, path)


def resolve_hap_sign_tool(value: str | None) -> Path:
    configured = value or os.environ.get("HNB_HAP_SIGN_TOOL")
    tool = Path(configured).expanduser() if configured else DEFAULT_HAP_SIGN_TOOL
    if not tool.is_file():
        raise SigningBundleError(
            "hap-sign-tool.jar was not found. Install DevEco Studio or pass "
            "--hap-sign-tool."
        )
    return tool.resolve()


def resolve_java() -> str:
    configured = os.environ.get("HNB_JAVA")
    if configured:
        candidate = Path(configured).expanduser()
        if not candidate.is_file() or not os.access(candidate, os.X_OK):
            raise SigningBundleError(f"Configured Java executable is not usable: {candidate}")
        return str(candidate.resolve())
    if DEFAULT_JAVA.is_file() and os.access(DEFAULT_JAVA, os.X_OK):
        return str(DEFAULT_JAVA)
    java = shutil.which("java")
    if java is None:
        raise SigningBundleError("Java was not found; install DevEco Studio or set HNB_JAVA.")
    return java


def verify_profile(profile: Path, hap_sign_tool: Path, expected_type: str) -> None:
    java = resolve_java()
    with tempfile.TemporaryDirectory(prefix="hnb-profile-") as directory:
        output = Path(directory) / "profile.json"
        completed = subprocess.run(
            [
                java,
                "-jar",
                str(hap_sign_tool),
                "verify-profile",
                "-inFile",
                str(profile),
                "-outFile",
                str(output),
            ],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        if completed.returncode != 0 or not output.is_file():
            raise SigningBundleError("The HarmonyOS profile could not be verified.")
        try:
            result = json.loads(output.read_text(encoding="utf-8"))
            profile_type = result["content"]["type"]
        except (OSError, json.JSONDecodeError, KeyError, TypeError) as error:
            raise SigningBundleError("The verified HarmonyOS profile has no readable type.") from error
    if profile_type != expected_type:
        raise SigningBundleError(
            f"The HarmonyOS profile type is {profile_type!r}; {expected_type!r} is required."
        )


def _selected_signing_config(build_profile: Path) -> tuple[dict[str, Any], dict[str, Path]]:
    data = load_json5(build_profile)
    try:
        app = data["app"]
        products = app["products"]
        configs = app["signingConfigs"]
    except (KeyError, TypeError) as error:
        raise SigningBundleError("The build profile has no app signing configuration.") from error
    if not isinstance(products, list) or not isinstance(configs, list):
        raise SigningBundleError("The build profile signing configuration is malformed.")
    product = next(
        (item for item in products if isinstance(item, dict) and item.get("name") == "default"),
        None,
    )
    if not product or not isinstance(product.get("signingConfig"), str):
        raise SigningBundleError("The default product has no signingConfig name.")
    config_name = product["signingConfig"]
    config = next(
        (item for item in configs if isinstance(item, dict) and item.get("name") == config_name),
        None,
    )
    if config is None:
        raise SigningBundleError(
            f"No signingConfigs entry matches the default product name {config_name!r}."
        )
    material = config.get("material")
    if not isinstance(material, dict):
        raise SigningBundleError("The selected signing configuration has no material object.")
    for key in REQUIRED_MATERIAL_KEYS:
        if not isinstance(material.get(key), str) or not material[key]:
            raise SigningBundleError(f"Signing material field {key!r} is missing.")
    if material["signAlg"] != "SHA256withECDSA":
        raise SigningBundleError("Only SHA256withECDSA signing material is supported.")

    resolved: dict[str, Path] = {}
    for key, suffix in (("storeFile", ".p12"), ("certpath", ".cer"), ("profile", ".p7b")):
        candidate = Path(material[key]).expanduser()
        if not candidate.is_absolute():
            candidate = build_profile.resolve().parent / candidate
        candidate = candidate.resolve()
        if candidate.suffix.lower() != suffix or not candidate.is_file():
            raise SigningBundleError(f"Signing material field {key!r} does not name a {suffix} file.")
        resolved[key] = candidate

    material_directory = resolved["storeFile"].parent / "material"
    if not material_directory.is_dir():
        raise SigningBundleError(
            "The DevEco password-decryption material directory beside the .p12 file is missing."
        )
    resolved["materialDirectory"] = material_directory.resolve()
    return copy.deepcopy(config), resolved


def _add_bytes(archive: tarfile.TarFile, name: str, content: bytes, mode: int = 0o600) -> None:
    info = tarfile.TarInfo(name)
    info.size = len(content)
    info.mode = mode
    info.mtime = 0
    info.uid = 0
    info.gid = 0
    info.uname = ""
    info.gname = ""
    archive.addfile(info, io.BytesIO(content))


def create_bundle(build_profile: Path, hap_sign_tool: Path, expected_type: str = "release") -> bytes:
    config, paths = _selected_signing_config(build_profile)
    verify_profile(paths["profile"], hap_sign_tool, expected_type)

    source_material = config["material"]
    manifest_material = {
        "certpath": ARCHIVE_FILES["certpath"],
        "storePassword": source_material["storePassword"],
        "keyAlias": source_material["keyAlias"],
        "keyPassword": source_material["keyPassword"],
        "profile": ARCHIVE_FILES["profile"],
        "signAlg": source_material["signAlg"],
        "storeFile": ARCHIVE_FILES["storeFile"],
    }
    manifest = {
        "schemaVersion": 1,
        "profileType": expected_type,
        "signingConfig": {
            "name": config["name"],
            "type": config.get("type", "HarmonyOS"),
            "material": manifest_material,
        },
    }

    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w:gz", format=tarfile.PAX_FORMAT) as archive:
        _add_bytes(
            archive,
            "manifest.json",
            json.dumps(manifest, ensure_ascii=False, sort_keys=True).encode("utf-8"),
        )
        for key, archive_name in ARCHIVE_FILES.items():
            _add_bytes(archive, archive_name, paths[key].read_bytes())
        material_directory = paths["materialDirectory"]
        material_files = sorted(path for path in material_directory.rglob("*") if path.is_file())
        if not material_files:
            raise SigningBundleError("The DevEco password-decryption material directory is empty.")
        for source in material_files:
            if source.is_symlink():
                raise SigningBundleError("Signing material symlinks are not supported.")
            relative = source.relative_to(material_directory)
            _add_bytes(archive, f"signing/material/{relative.as_posix()}", source.read_bytes())

    encoded = base64.b64encode(buffer.getvalue())
    if len(encoded) > SECRET_SIZE_LIMIT:
        raise SigningBundleError(
            f"The encoded signing bundle is {len(encoded)} bytes; GitHub Secrets allow at most "
            f"{SECRET_SIZE_LIMIT} bytes."
        )
    return encoded


def _safe_archive_name(name: str) -> bool:
    path = PurePosixPath(name)
    if path.is_absolute() or ".." in path.parts:
        return False
    if name in {"manifest.json", *ARCHIVE_FILES.values()}:
        return True
    return name.startswith("signing/material/") and len(path.parts) > 2


def _extract_bundle(encoded: bytes, destination: Path) -> dict[str, Any]:
    if not encoded:
        raise SigningBundleError(f"{SECRET_NAME} is empty.")
    if len(encoded) > SECRET_SIZE_LIMIT:
        raise SigningBundleError(f"{SECRET_NAME} exceeds the GitHub Secrets size limit.")
    try:
        payload = base64.b64decode(encoded, validate=True)
    except ValueError as error:
        raise SigningBundleError(f"{SECRET_NAME} is not valid Base64.") from error

    destination = destination.resolve()
    destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    if destination.exists():
        raise SigningBundleError(f"Signing output directory already exists: {destination}")
    stage = Path(tempfile.mkdtemp(prefix=f".{destination.name}.", dir=destination.parent))
    try:
        try:
            with tarfile.open(fileobj=io.BytesIO(payload), mode="r:gz") as archive:
                members = archive.getmembers()
                names = {member.name for member in members}
                if len(members) > MAX_ARCHIVE_MEMBERS or len(names) != len(members):
                    raise SigningBundleError("The signing bundle has too many or duplicate entries.")
                extracted_bytes = sum(member.size for member in members if member.isfile())
                if extracted_bytes > MAX_EXTRACTED_BYTES:
                    raise SigningBundleError("The signing bundle expands beyond the safety limit.")
                required = {"manifest.json", *ARCHIVE_FILES.values()}
                if not required.issubset(names):
                    raise SigningBundleError("The signing bundle is missing required files.")
                for member in members:
                    if not _safe_archive_name(member.name) or not (member.isfile() or member.isdir()):
                        raise SigningBundleError("The signing bundle contains an unsafe archive entry.")
                    target = stage.joinpath(*PurePosixPath(member.name).parts)
                    if member.isdir():
                        target.mkdir(parents=True, exist_ok=True, mode=0o700)
                        continue
                    source = archive.extractfile(member)
                    if source is None:
                        raise SigningBundleError("The signing bundle contains an unreadable file.")
                    target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                    target.write_bytes(source.read())
                    target.chmod(0o600)
        except tarfile.TarError as error:
            raise SigningBundleError("The signing bundle is not a valid gzip tar archive.") from error
        try:
            manifest = json.loads((stage / "manifest.json").read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as error:
            raise SigningBundleError("The signing bundle manifest is invalid.") from error
        stage.rename(destination)
        return manifest
    except Exception:
        shutil.rmtree(stage, ignore_errors=True)
        raise


def install_bundle(
    encoded: bytes,
    destination: Path,
    build_profile: Path,
    hap_sign_tool: Path,
    expected_type: str = "release",
) -> None:
    destination = destination.resolve()
    destination_existed = destination.exists()
    try:
        manifest = _extract_bundle(encoded.strip(), destination)
        try:
            if manifest["schemaVersion"] != 1 or manifest["profileType"] != expected_type:
                raise SigningBundleError("The signing bundle schema or profile type is not supported.")
            config = manifest["signingConfig"]
            material = config["material"]
        except (KeyError, TypeError) as error:
            raise SigningBundleError("The signing bundle manifest is incomplete.") from error
        if not isinstance(config, dict) or not isinstance(material, dict):
            raise SigningBundleError("The signing bundle manifest has malformed signing data.")
        for key in REQUIRED_MATERIAL_KEYS:
            if not isinstance(material.get(key), str) or not material[key]:
                raise SigningBundleError(f"The signing bundle field {key!r} is missing.")

        signing_root = destination.resolve() / "signing"
        profile = signing_root / "signing.p7b"
        verify_profile(profile, hap_sign_tool, expected_type)

        installed_material = copy.deepcopy(material)
        installed_material["certpath"] = str(signing_root / "signing.cer")
        installed_material["profile"] = str(profile)
        installed_material["storeFile"] = str(signing_root / "signing.p12")
        installed_config = {
            "name": config.get("name"),
            "type": config.get("type", "HarmonyOS"),
            "material": installed_material,
        }
        if not isinstance(installed_config["name"], str) or not installed_config["name"]:
            raise SigningBundleError("The signing bundle has no signing configuration name.")

        build_data = load_json5(build_profile)
        try:
            products = build_data["app"]["products"]
            product = next(item for item in products if item.get("name") == "default")
        except (KeyError, TypeError, StopIteration) as error:
            raise SigningBundleError("The target build profile has no default product.") from error
        if product.get("signingConfig") != installed_config["name"]:
            raise SigningBundleError(
                "The signing bundle name does not match the default product signingConfig."
            )
        build_data["app"]["signingConfigs"] = [installed_config]
        write_json5(build_profile, build_data)
    except Exception:
        if not destination_existed and destination.exists():
            shutil.rmtree(destination, ignore_errors=True)
        raise


def verify_hap(hap: Path, hap_sign_tool: Path, expected_type: str = "release") -> None:
    if not hap.is_file():
        raise SigningBundleError(f"Signed HAP not found: {hap}")
    java = resolve_java()
    with tempfile.TemporaryDirectory(prefix="hnb-hap-") as directory:
        certificate = Path(directory) / "certificate.cer"
        profile = Path(directory) / "profile.p7b"
        completed = subprocess.run(
            [
                java,
                "-jar",
                str(hap_sign_tool),
                "verify-app",
                "-inFile",
                str(hap),
                "-outCertChain",
                str(certificate),
                "-outProfile",
                str(profile),
            ],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        if completed.returncode != 0 or not certificate.is_file() or not profile.is_file():
            raise SigningBundleError("The signed HAP failed hap-sign-tool verification.")
        verify_profile(profile, hap_sign_tool, expected_type)


def sanitize_build_profile(build_profile: Path) -> None:
    data = load_json5(build_profile)
    try:
        data["app"]["signingConfigs"] = []
    except (KeyError, TypeError) as error:
        raise SigningBundleError("The target build profile has no app object.") from error
    write_json5(build_profile, data)


def cleanup_signing(destination: Path, allowed_root: Path, build_profile: Path) -> None:
    sanitize_build_profile(build_profile)
    root = allowed_root.resolve()
    target = destination.resolve()
    if target == root or root not in target.parents:
        raise SigningBundleError("Refusing to remove signing data outside the allowed temporary root.")
    if target.exists():
        shutil.rmtree(target)


def command_upload(args: argparse.Namespace) -> None:
    build_profile = args.build_profile.resolve()
    tool = resolve_hap_sign_tool(args.hap_sign_tool)
    encoded = create_bundle(build_profile, tool, expected_type="release")
    gh = shutil.which("gh")
    if gh is None:
        raise SigningBundleError("GitHub CLI is required to upload the signing secret.")
    command = [
        gh,
        "secret",
        "set",
        SECRET_NAME,
        "--repo",
        args.repo,
        "--env",
        args.environment,
    ]
    completed = subprocess.run(command, input=encoded + b"\n", check=False)
    if completed.returncode != 0:
        raise SigningBundleError("GitHub CLI could not upload the environment secret.")
    print(f"Uploaded {SECRET_NAME} to the {args.environment!r} environment for {args.repo}.")


def command_install(args: argparse.Namespace) -> None:
    tool = resolve_hap_sign_tool(args.hap_sign_tool)
    install_bundle(
        sys.stdin.buffer.read(),
        args.output_dir,
        args.build_profile,
        tool,
        expected_type="release",
    )
    print("Installed a verified release signing configuration for this build.")


def command_verify_hap(args: argparse.Namespace) -> None:
    tool = resolve_hap_sign_tool(args.hap_sign_tool)
    verify_hap(args.hap.resolve(), tool, expected_type="release")
    print("Verified the signed HAP and its embedded release profile.")


def command_cleanup(args: argparse.Namespace) -> None:
    cleanup_signing(args.output_dir, args.allowed_root, args.build_profile)
    print("Removed temporary signing material and sanitized the build profile.")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    upload = subparsers.add_parser("upload", help="Verify and upload a release signing bundle.")
    upload.add_argument("--build-profile", type=Path, default=DEFAULT_BUILD_PROFILE)
    upload.add_argument("--hap-sign-tool")
    upload.add_argument("--repo", default="realskyrin/harmony-netbridge")
    upload.add_argument("--environment", default="release")
    upload.set_defaults(handler=command_upload)

    install = subparsers.add_parser("install", help="Install a bundle read from standard input.")
    install.add_argument("--build-profile", type=Path, default=DEFAULT_BUILD_PROFILE)
    install.add_argument("--output-dir", type=Path, required=True)
    install.add_argument("--hap-sign-tool")
    install.set_defaults(handler=command_install)

    verify = subparsers.add_parser("verify-hap", help="Verify a release-signed HAP.")
    verify.add_argument("--hap", type=Path, required=True)
    verify.add_argument("--hap-sign-tool")
    verify.set_defaults(handler=command_verify_hap)

    cleanup = subparsers.add_parser("cleanup", help="Remove temporary material and signing config.")
    cleanup.add_argument("--build-profile", type=Path, default=DEFAULT_BUILD_PROFILE)
    cleanup.add_argument("--output-dir", type=Path, required=True)
    cleanup.add_argument("--allowed-root", type=Path, required=True)
    cleanup.set_defaults(handler=command_cleanup)
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    try:
        args.handler(args)
    except SigningBundleError as error:
        print(f"error: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
