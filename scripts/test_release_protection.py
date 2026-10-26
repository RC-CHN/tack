import importlib.util
from pathlib import Path
import struct
import tempfile
import unittest
from unittest import mock
import subprocess

from release_support import OBFUSCATION_SCOPE, build_env, protect, verify_target


class ProtectionTests(unittest.TestCase):
    def test_target_header_rejects_wrong_architecture_and_truncation(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "node"
            header = bytearray(64)
            header[:6] = b"\x7fELF\x02\x01"
            struct.pack_into("<H", header, 18, 62)
            binary.write_bytes(header)
            verify_target(binary, "linux", "amd64")
            for os, arch in (("linux", "arm64"), ("linux", "386"), ("windows", "amd64")):
                with self.assertRaises(RuntimeError):
                    verify_target(binary, os, arch)
            binary.write_bytes(b"\x7fELF")
            with self.assertRaises(RuntimeError):
                verify_target(binary, "linux", "amd64")

    def test_ambient_flags_cannot_invalidate_build_record(self):
        with mock.patch.dict("os.environ", {"GOFLAGS": "-race", "GOGARBLE": "none", "CGO_ENABLED": "1", "GOARM64": "v9.0", "GOAMD64": "v4"}):
            env = build_env("go1.23.12", "linux", "arm", "6")
        self.assertEqual((env["GOFLAGS"], env["GOGARBLE"], env["CGO_ENABLED"], env["GOARM"]), ("", OBFUSCATION_SCOPE, "0", "6"))
        self.assertEqual((env["GOAMD64"], env["GOARM64"], env["GO386"]), ("v1", "v8.0", "sse2"))

    def test_only_known_packer_rejections_allow_recorded_fallback(self):
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "node"
            binary.write_bytes(b"original")
            with mock.patch("release_support.subprocess.run") as run:
                run.return_value = subprocess.CompletedProcess([], 1, "", "UnknownExecutableFormatException")
                result = protect(binary, Path("upx"), False, "tack-node", "1.0.0", "windows", "arm64")
                self.assertEqual(result["status"], "exception")
                self.assertEqual(binary.read_bytes(), b"original")
                for error in ("segmentation fault", "checksum error", "permission denied"):
                    run.return_value = subprocess.CompletedProcess([], 1, "", error)
                    with self.assertRaises(RuntimeError):
                        protect(binary, Path("upx"), False, "tack-node", "1.0.0", "linux", "amd64")

    def test_archive_tampering_is_rejected_before_execution(self):
        spec = importlib.util.spec_from_file_location("verify_release", Path(__file__).with_name("verify-release.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "manifest.json").write_text("{}")
            (root / "SHA256SUMS").write_text("0" * 64 + "  manifest.json\n")
            with mock.patch.object(module, "version_output") as execute:
                with self.assertRaisesRegex(ValueError, "checksum mismatch"):
                    module.verify(root)
                execute.assert_not_called()


if __name__ == "__main__":
    unittest.main()
