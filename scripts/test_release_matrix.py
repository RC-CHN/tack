import copy
import importlib.util
import json
from pathlib import Path
import shutil
import struct
import tempfile
import unittest
from unittest import mock

from release_matrix import MATRIX_ID, archive_name, targets, validate_matrix
from release_support import sha256


def load(name):
    spec = importlib.util.spec_from_file_location(name.replace("-", "_"), Path(__file__).with_name(name + ".py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


builder = load("build-release")
merger = load("merge-release")


def executable_header(target):
    """Synthetic, never-executed headers let integrity checks cover every target."""
    data = bytearray(128)
    arch, os = target["arch"], target["os"]
    if os == "linux":
        bits, endian, machine = {
            "amd64": (2, 1, 62), "386": (1, 1, 3), "arm": (1, 1, 40), "arm64": (2, 1, 183),
            "mips": (1, 2, 8), "mipsle": (1, 1, 8), "mips64": (2, 2, 8), "mips64le": (2, 1, 8),
        }[arch]
        data[:6] = b"\x7fELF" + bytes((bits, endian))
        struct.pack_into("<H" if endian == 1 else ">H", data, 18, machine)
    elif os == "windows":
        data[:2], data[64:68] = b"MZ", b"PE\0\0"
        struct.pack_into("<I", data, 60, 64)
        struct.pack_into("<H", data, 68, {"amd64": 0x8664, "386": 0x14c, "arm64": 0xaa64}[arch])
    else:
        data[:4] = b"\xcf\xfa\xed\xfe"
        struct.pack_into("<I", data, 4, {"amd64": 0x01000007, "arm64": 0x0100000c}[arch])
    return bytes(data)


class MatrixTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.shards = self.root / "shards"
        self.shards.mkdir()
        self.output = self.root / "release"
        self.commit, self.version = "a" * 40, "0.1.2"
        self.base = {"format": "tack-release-v2", "version": self.version, "commit": self.commit,
                     "source_date_epoch": 1700000000, "source_dirty": False, "source_sha256": "b" * 64,
                     "native_only": False, "node_runtime": "both", "requested_protection": "packed",
                     "matrix_id": MATRIX_ID, "release_scope": "shard"}

    def write_manifest(self, directory, manifest):
        (directory / "manifest.json").write_text(json.dumps(manifest))
        files = sorted(p for p in directory.iterdir() if p.name != "SHA256SUMS")
        (directory / "SHA256SUMS").write_text("".join(f"{sha256(path)}  {path.name}\n" for path in files))

    def fixture(self):
        for target in targets():
            directory = self.shards / target["id"]
            directory.mkdir()
            binary = executable_header(target)
            executable = target["component"] + (".exe" if target["os"] == "windows" else "")
            path = directory / executable
            path.write_bytes(binary)
            record = {"format": "tack-build-v1", "component": target["component"], "version": self.version,
                      "source_commit": self.commit, "source_dirty": False, "source_sha256": self.base["source_sha256"],
                      "go_version": target["toolchain"], "os": target["os"], "arch": target["arch"], "arm": target["arm"],
                      "runtime_profile": target["runtime"], "race": False, "cgo_enabled": False,
                      "sha256": sha256(path), "bytes": len(binary), "protection": "packed", "packing": {"status": "packed"}}
            path.unlink()
            if target["component"] == "tack-node":
                record["capabilities"] = ["management", "embedded-chisel"]
            if target["component"] == "tack-control":
                record["capabilities"] = ["management", "embedded-chisel-server"]
            if target["runtime"] == "legacy":
                record.update(protection="obfuscated", packing={"status": "exception"},
                              legacy_adapter={"format": "tack-legacy-adapters-v1"})
            name = archive_name(target, self.version)
            builder.archive(directory / name, {executable: (binary, 0o755), "LICENSE": (b"license", 0o644),
                            "README.md": (b"guide", 0o644), "BUILD.json": (json.dumps(record).encode(), 0o644)},
                            self.base["source_date_epoch"])
            manifest = {**self.base, "shard": target["id"], "artifacts": [{"file": name, "component": target["component"],
                        "os": target["os"], "arch": target["label"], "toolchain": target["toolchain"],
                        "archive_sha256": sha256(directory / name), "build": record}]}
            self.write_manifest(directory, manifest)

    def change(self, update):
        directory = self.shards / targets()[-1]["id"]
        manifest = json.loads((directory / "manifest.json").read_text())
        update(manifest)
        self.write_manifest(directory, manifest)

    def merge(self):
        return merger.merge(self.shards, self.output, self.commit, self.version)

    def test_matrix_keeps_all_supported_targets_without_standalone_tunnel(self):
        matrix = targets()
        self.assertEqual(len(matrix), 33)
        self.assertEqual(len({t["id"] for t in matrix}), 33)
        self.assertEqual({component: sum(t["component"] == component for t in matrix)
                          for component in {t["component"] for t in matrix}},
                         {"tack-node": 23, "tack-control": 3, "tack": 7})
        self.assertEqual(sum(t["runtime"] == "legacy" for t in matrix), 10)
        self.assertEqual({t["arm"] for t in matrix if t["arch"] == "arm"}, {"5", "6", "7"})

    def test_complete_merge_checks_bytes_and_does_not_execute_inputs(self):
        self.fixture()
        with mock.patch.object(merger.verifier, "version_output") as execute, mock.patch("builtins.print"):
            manifest = self.merge()
            execute.assert_not_called()
        self.assertEqual(len(manifest["artifacts"]), 33)
        self.assertEqual(len(list(self.output.iterdir())), 35)
        self.assertNotIn("shard", manifest)
        validate_matrix(manifest, complete=True)
        for entry in manifest["artifacts"]:
            self.assertEqual(sha256(self.output / entry["file"]), entry["archive_sha256"])
        with mock.patch("builtins.print"), self.assertRaisesRegex(ValueError, "output must be empty"):
            self.merge()

    def test_missing_or_duplicate_shard_cannot_publish(self):
        self.fixture()
        first = self.shards / targets()[0]["id"]
        duplicate = self.shards / "duplicate"
        shutil.copytree(first, duplicate)
        with mock.patch("builtins.print"), self.assertRaisesRegex(ValueError, "duplicate"):
            self.merge()
        shutil.rmtree(duplicate)
        shutil.rmtree(first)
        with mock.patch("builtins.print"), self.assertRaisesRegex(ValueError, "missing release shards"):
            self.merge()
        self.assertFalse(self.output.exists())

    def test_foreign_dirty_or_inconsistent_shards_are_rejected(self):
        self.fixture()
        path = self.shards / targets()[-1]["id"] / "manifest.json"
        original = json.loads(path.read_text())
        for key, value in (("commit", "c" * 40), ("version", "0.1.3"), ("source_sha256", "d" * 64),
                           ("source_date_epoch", 1700000001), ("source_dirty", True), ("native_only", True),
                           ("node_runtime", "modern"), ("requested_protection", "plain"),
                           ("release_scope", "smoke"), ("shard", "unexpected")):
            with self.subTest(key=key), mock.patch("builtins.print"):
                manifest = {**copy.deepcopy(original), key: value}
                self.write_manifest(path.parent, manifest)
                with self.assertRaises(ValueError):
                    self.merge()
                self.assertFalse(self.output.exists())
        self.write_manifest(path.parent, original)

    def test_corruption_is_rejected_before_output_or_execution(self):
        self.fixture()
        directory = self.shards / targets()[0]["id"]
        archive = next(directory.glob("*.tar.gz"))
        archive.write_bytes(archive.read_bytes() + b"corruption")
        with mock.patch.object(merger.verifier, "version_output") as execute, mock.patch("builtins.print"):
            with self.assertRaisesRegex(ValueError, "checksum mismatch"):
                self.merge()
            execute.assert_not_called()
        self.assertFalse(self.output.exists())

    def test_one_shard_and_relabelled_partial_release_cannot_pass_publication(self):
        self.fixture()
        manifest = json.loads((self.shards / targets()[0]["id"] / "manifest.json").read_text())
        with self.assertRaises(ValueError):
            validate_matrix(manifest, complete=True)
        manifest.pop("shard")
        manifest["release_scope"] = "complete"
        with self.assertRaisesRegex(ValueError, "missing release targets"):
            validate_matrix(manifest, complete=True)

    def test_capabilities_and_protection_cannot_be_downgraded(self):
        self.fixture()
        control = next(t for t in targets() if t["component"] == "tack-control")
        original = json.loads((self.shards / control["id"] / "manifest.json").read_text())
        for field, value in (("capabilities", ["management"]), ("protection", "plain"),
                             ("packing", {"status": "disabled"}), ("arch", "386")):
            with self.subTest(field=field):
                manifest = copy.deepcopy(original)
                manifest["artifacts"][0]["build"][field] = value
                with self.assertRaises(ValueError):
                    validate_matrix(manifest)


if __name__ == "__main__":
    unittest.main()
