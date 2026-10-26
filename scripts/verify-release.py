#!/usr/bin/env python3
"""Verify exact release archives and optionally extract the native Node for tests."""
import argparse
import hashlib
import json
from pathlib import Path
import platform
import tarfile
import tempfile
import zipfile

from release_support import sha256, verify_target, version_output


def verify(directory, commit=None, extract_node=None, require_packed_node=False, extract_native=None):
    checks = {}
    for line in (directory / "SHA256SUMS").read_text().splitlines():
        digest, name = line.split("  ", 1)
        if Path(name).name != name or name in checks or len(digest) != 64:
            raise ValueError("invalid checksum entry")
        checks[name] = digest
    if {p.name for p in directory.iterdir()} != set(checks) | {"SHA256SUMS"}:
        raise ValueError("unexpected/missing release files")
    for name, digest in checks.items():
        if sha256(directory / name) != digest:
            raise ValueError("checksum mismatch: " + name)
    manifest = json.loads((directory / "manifest.json").read_text())
    if manifest["format"] != "tack-release-v2" or (commit and manifest["commit"] != commit):
        raise ValueError("unexpected release format/commit")
    entries = manifest["artifacts"]
    names = [a["file"] for a in entries]
    if len(set(names)) != len(names) or set(names) | {"manifest.json"} != set(checks):
        raise ValueError("manifest does not match uploaded files")
    host = (platform.system().lower(), {"x86_64": "amd64", "AMD64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine()))
    extracted = False
    if extract_native:
        extract_native.mkdir(parents=True, exist_ok=False)
    with tempfile.TemporaryDirectory(prefix="tack-verify-") as temp:
        for entry in entries:
            record = entry["build"]
            if entry["component"] not in ("tack-node", "tack", "tack-control", "chisel"):
                raise ValueError("unknown release component")
            label = "armv" + record["arm"] if record["arch"] == "arm" else record["arch"]
            if record["arch"].startswith("mips"):
                label += "-softfloat"
            if entry["arch"] != label:
                raise ValueError("target label does not match build record")
            executable = entry["component"] + (".exe" if entry["os"] == "windows" else "")
            members = {executable, "LICENSE", "README.md", "BUILD.json"}
            archive = directory / entry["file"]
            if archive.suffix == ".zip":
                with zipfile.ZipFile(archive) as package:
                    if len(package.namelist()) != 4 or set(package.namelist()) != members or package.testzip() is not None:
                        raise ValueError("invalid ZIP members/CRC")
                    contents = {n: package.read(n) for n in members}
            else:
                with tarfile.open(archive) as package:
                    if (len(package.getmembers()) != 4 or set(package.getnames()) != members
                            or not all(m.isfile() for m in package.getmembers())):
                        raise ValueError("invalid tar members")
                    contents = {n: package.extractfile(n).read() for n in members}
            if (record != json.loads(contents["BUILD.json"]) or sha256(archive) != entry["archive_sha256"]
                    or record["format"] != "tack-build-v1" or record["component"] != entry["component"]
                    or record["source_commit"] != manifest["commit"] or record["version"] != manifest["version"]
                    or record["go_version"] != entry["toolchain"] or record["os"] != entry["os"]
                    or record["source_sha256"] != manifest["source_sha256"]
                    or record["source_dirty"] != manifest["source_dirty"]
                    or record["protection"] not in ("plain", "obfuscated", "packed")
                    or record["bytes"] != len(contents[executable])
                    or record["race"] is not False or record["cgo_enabled"] is not False
                    or hashlib.sha256(contents[executable]).hexdigest() != record["sha256"]):
                raise ValueError("archive/build record mismatch: " + entry["file"])
            binary = Path(temp) / executable
            binary.write_bytes(contents[executable])
            binary.chmod(0o755)
            verify_target(binary, record["os"], record["arch"])
            if (record["os"], record["arch"]) == host:
                if require_packed_node and entry["component"] == "tack-node" and record["protection"] != "packed":
                    raise ValueError("native Node unexpectedly fell back from packing")
                version_output(binary, entry["component"], manifest["version"])
                if extract_native:
                    target = extract_native / executable
                    target.write_bytes(contents[executable])
                    target.chmod(0o755)
                    (extract_native / (entry["component"] + ".BUILD.json")).write_bytes(contents["BUILD.json"])
                if entry["component"] == "tack-node" and extract_node:
                    extract_node.mkdir(parents=True, exist_ok=False)
                    for name in (executable, "BUILD.json"):
                        (extract_node / name).write_bytes(contents[name])
                    (extract_node / executable).chmod(0o755)
                    extracted = True
            print("Verified", entry["file"], record["protection"], flush=True)
    if extract_node and not extracted:
        raise ValueError("native Node was not present")
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("--commit")
    parser.add_argument("--extract-node", type=Path)
    parser.add_argument("--require-packed-node", action="store_true")
    parser.add_argument("--extract-native", type=Path)
    args = parser.parse_args()
    manifest = verify(args.directory.resolve(), args.commit, args.extract_node, args.require_packed_node, args.extract_native)
    print(f"Verified {len(manifest['artifacts'])} archives at {manifest['commit']}")


if __name__ == "__main__":
    main()
