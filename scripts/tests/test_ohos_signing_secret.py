from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock


SCRIPT = Path(__file__).resolve().parents[1] / "ohos_signing_secret.py"
SPEC = importlib.util.spec_from_file_location("hnb_ohos_signing_secret", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
signing = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(signing)


class VerifyProfileTests(unittest.TestCase):
    def verify(self, content: dict[str, object]) -> dict[str, object]:
        def fake_run(command: list[str], **_: object) -> subprocess.CompletedProcess[str]:
            output = Path(command[command.index("-outFile") + 1])
            output.write_text(json.dumps({"content": content}), encoding="utf-8")
            return subprocess.CompletedProcess(command, 0)

        with tempfile.TemporaryDirectory(prefix="hnb-signing-test-") as directory:
            profile = Path(directory) / "profile.p7b"
            with (
                mock.patch.object(signing, "resolve_java", return_value="/fake/java"),
                mock.patch.object(signing.subprocess, "run", side_effect=fake_run),
            ):
                return signing.verify_profile(
                    profile,
                    Path("/fake/hap-sign-tool.jar"),
                    signing.EXPECTED_PROFILE_TYPE,
                    signing.EXPECTED_BUNDLE_NAME,
                    require_registered_devices=True,
                )

    @staticmethod
    def profile_content(
        *,
        profile_type: str = "debug",
        bundle_name: str | None = None,
        device_ids: list[str] | None = None,
    ) -> dict[str, object]:
        return {
            "type": profile_type,
            "bundle-info": {
                "bundle-name": bundle_name or signing.EXPECTED_BUNDLE_NAME,
            },
            "debug-info": {
                "device-ids": ["registered-device"] if device_ids is None else device_ids,
            },
        }

    def test_accepts_matching_debug_profile_with_registered_device(self) -> None:
        content = self.profile_content()

        self.assertEqual(self.verify(content), content)

    def test_rejects_release_profile_even_when_signature_is_valid(self) -> None:
        with self.assertRaisesRegex(signing.SigningBundleError, "'debug' is required"):
            self.verify(self.profile_content(profile_type="release", device_ids=[]))

    def test_rejects_profile_for_another_bundle(self) -> None:
        with self.assertRaisesRegex(signing.SigningBundleError, "does not match bundle"):
            self.verify(self.profile_content(bundle_name="com.example.other"))

    def test_rejects_debug_profile_without_registered_devices(self) -> None:
        with self.assertRaisesRegex(signing.SigningBundleError, "no registered devices"):
            self.verify(self.profile_content(device_ids=[]))


if __name__ == "__main__":
    unittest.main()
