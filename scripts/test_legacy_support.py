import importlib.util
import io
import contextlib
import hashlib
import json
from pathlib import Path
import struct
import tarfile
import tempfile
import unittest
from unittest import mock
import zipfile


def module(name):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(name + ".py"))
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


selector = module("select-node-artifact")
adapter = module("prepare-legacy")
verifier = module("verify-release")


class LegacySupportTests(unittest.TestCase):
    def manifest(self):
        return {"format": "tack-release-v2", "artifacts": [
            {"component": "tack-node", "os": "linux", "arch": "amd64", "file": f"node-{profile}.tar.gz",
             "archive_sha256": "a"*64, "build": {"runtime_profile": profile,
                 "capabilities": ["management", "embedded-chisel"], "go_version": version,
                 "protection": "packed", "validation": "native-version-smoke"}}
            for profile, version in (("modern", "go1.26.3"), ("legacy", "go1.23.12"))]}

    def test_kernel_selects_one_unified_variant(self):
        self.assertEqual(selector.select(self.manifest(), "linux", "x86_64", "2.6.32-754.el6.x86_64")["runtime_profile"], "legacy")
        self.assertEqual(selector.select(self.manifest(), "linux", "amd64", "3.2.0")["runtime_profile"], "modern")
        self.assertEqual(selector.select(self.manifest(), "linux", "amd64", "6.8.0", runtime="legacy")["runtime_profile"], "legacy")

    def test_invalid_or_missing_compatible_artifact_is_not_substituted(self):
        for kwargs in ({"kernel": "2.6.18"}, {"kernel": "2.6.32", "runtime": "modern"}, {"kernel": "unknown"}):
            with self.assertRaises(ValueError):
                selector.select(self.manifest(), "linux", "amd64", **kwargs)
        missing = self.manifest()
        missing["artifacts"].pop()
        with self.assertRaises(ValueError):
            selector.select(missing, "linux", "amd64", "2.6.32")
        with self.assertRaises(ValueError):
            selector.select(self.manifest(), "linux", "mipsel", "3.10.0")
        old_worker = self.manifest()
        old_worker["artifacts"][1]["build"]["capabilities"] = ["management"]
        with self.assertRaises(ValueError):
            selector.select(old_worker, "linux", "amd64", "2.6.32")

    def test_no_claim_of_ancient_windows_support(self):
        with self.assertRaises(ValueError):
            selector.select(self.manifest(), "windows", "amd64", windows_version="6.1")

    def test_architecture_specific_kernel_floors_and_ambiguous_selection(self):
        for arch, kernel in (("armv5l", "2.6.32"), ("mips64el", "4.1.0"), ("mipsel", "2.6.32")):
            with self.assertRaises(ValueError):
                selector.select(self.manifest(), "linux", arch, kernel)
        duplicate = self.manifest()
        duplicate["artifacts"].append(duplicate["artifacts"][0])
        with self.assertRaisesRegex(ValueError, "exactly one"):
            selector.select(duplicate, "linux", "amd64", "6.8.0")

    def release(self, root, profiles=("modern", "legacy")):
        manifest = {"format": "tack-release-v2", "version": "1.0.0", "commit": "test-commit",
                    "source_sha256": "b"*64, "source_dirty": False, "artifacts": []}
        checks = {}
        for profile in profiles:
            header = bytearray(64)
            header[:6] = b"\x7fELF\x02\x01"
            struct.pack_into("<H", header, 18, 62)
            binary = bytes(header) + profile.encode()
            record = {"format": "tack-build-v1", "component": "tack-node", "version": "1.0.0",
                      "source_commit": "test-commit", "source_sha256": "b"*64, "source_dirty": False,
                      "os": "linux", "arch": "amd64", "arm": "", "go_version": "go1.23.12" if profile == "legacy" else "go1.26.3",
                      "runtime_profile": profile, "capabilities": ["management", "embedded-chisel"],
                      "legacy_adapter": {"format": "tack-legacy-adapters-v1"}, "race": False, "cgo_enabled": False,
                      "protection": "obfuscated" if profile == "legacy" else "packed", "bytes": len(binary),
                      "sha256": hashlib.sha256(binary).hexdigest()}
            name = f"node-{profile}.tar.gz"
            with tarfile.open(root / name, "w:gz") as archive:
                for member, data in {"tack-node": binary, "LICENSE": b"fixture", "README.md": b"fixture",
                                     "BUILD.json": json.dumps(record).encode()}.items():
                    info = tarfile.TarInfo(member)
                    info.size = len(data)
                    archive.addfile(info, io.BytesIO(data))
            checks[name] = hashlib.sha256((root / name).read_bytes()).hexdigest()
            manifest["artifacts"].append({"file": name, "component": "tack-node", "os": "linux", "arch": "amd64",
                                          "archive_sha256": checks[name], "toolchain": record["go_version"], "build": record})
        (root / "manifest.json").write_text(json.dumps(manifest))
        checks["manifest.json"] = hashlib.sha256((root / "manifest.json").read_bytes()).hexdigest()
        (root / "SHA256SUMS").write_text("".join(f"{digest}  {name}\n" for name, digest in checks.items()))

    def test_verifier_extracts_selected_runtime_without_overwriting_other_variant(self):
        with tempfile.TemporaryDirectory() as temp, mock.patch.object(verifier.platform, "system", return_value="Linux"), \
                mock.patch.object(verifier.platform, "machine", return_value="x86_64"), \
                mock.patch.object(verifier, "version_output"), contextlib.redirect_stdout(io.StringIO()):
            root = Path(temp)
            archives = root / "archives"
            archives.mkdir()
            self.release(archives)
            for profile in ("modern", "legacy"):
                dest = root / profile
                verifier.verify(archives, extract_native=dest, node_runtime=profile,
                                require_packed_node=profile == "modern")
                self.assertTrue((dest / "tack-node").read_bytes().endswith(profile.encode()))
            with self.assertRaisesRegex(ValueError, "fell back"):
                verifier.verify(archives, node_runtime="legacy", require_packed_node=True)

    def test_missing_legacy_cannot_succeed_as_an_empty_native_extraction(self):
        with tempfile.TemporaryDirectory() as temp, mock.patch.object(verifier.platform, "system", return_value="Linux"), \
                mock.patch.object(verifier.platform, "machine", return_value="x86_64"), \
                mock.patch.object(verifier, "version_output"), contextlib.redirect_stdout(io.StringIO()):
            root = Path(temp)
            archives = root / "archives"
            archives.mkdir()
            self.release(archives, ("modern",))
            with self.assertRaisesRegex(ValueError, "selected native Node"):
                verifier.verify(archives, node_runtime="legacy", extract_native=root / "out")

    def test_module_hash_binds_content_and_names(self):
        def hashed(name, data):
            content = io.BytesIO()
            with zipfile.ZipFile(content, "w") as archive:
                archive.writestr(name, data)
            with zipfile.ZipFile(content) as archive:
                return adapter.module_sum(archive)
        original = hashed("example.org/mod@v1.0.0/file.go", b"original")
        self.assertNotEqual(original, hashed("example.org/mod@v1.0.0/file.go", b"changed"))
        self.assertNotEqual(original, hashed("example.org/mod@v1.0.0/other.go", b"original"))


if __name__ == "__main__":
    unittest.main()
