#!/usr/bin/env python3
"""Validate every exact build shard and assemble one complete release without rebuilding."""
import argparse
import importlib.util
import json
from pathlib import Path
import shutil

from release_matrix import targets, validate_matrix
from release_support import sha256

spec = importlib.util.spec_from_file_location("verify_release", Path(__file__).with_name("verify-release.py"))
verifier = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verifier)


def merge(shards, output, commit, version):
    directories = sorted(shards.iterdir())
    if not directories or any(not path.is_dir() or path.is_symlink() for path in directories):
        raise ValueError("expected separate release shard directories")
    expected = {target["id"] for target in targets()}
    seen, artifacts, common = set(), [], None
    fields = ("format", "version", "commit", "source_date_epoch", "source_dirty", "source_sha256",
              "native_only", "node_runtime", "requested_protection", "matrix_id")
    for directory in directories:
        manifest = json.loads((directory / "manifest.json").read_text())
        validate_matrix(manifest)
        shard = manifest.get("shard")
        if (manifest["release_scope"] != "shard" or shard not in expected or shard in seen
                or manifest["version"] != version or manifest["commit"] != commit
                or manifest["source_dirty"] is not False or manifest["native_only"] is not False
                or manifest["node_runtime"] != "both" or manifest["requested_protection"] != "packed"):
            raise ValueError("invalid, duplicate or foreign release shard: " + directory.name)
        current = {key: manifest[key] for key in fields}
        if common is None:
            common = current
        elif current != common:
            raise ValueError("release shards have different source or build provenance")
        seen.add(shard)
        # Archive hashes, embedded build records and executable headers are
        # checked before any file is accepted. Functional gates run afterwards.
        verifier.verify(directory, commit=commit, execute_native=False)
        artifacts.extend((directory, entry) for entry in manifest["artifacts"])
    if seen != expected:
        raise ValueError("missing release shards: " + ", ".join(sorted(expected - seen)))
    manifest = {**common, "release_scope": "complete",
                "artifacts": [entry for _, entry in sorted(artifacts, key=lambda pair: pair[1]["file"])]}
    validate_matrix(manifest, complete=True)
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        raise ValueError("output must be empty; existing release files are never overwritten")
    for directory, entry in artifacts:
        shutil.copyfile(directory / entry["file"], output / entry["file"])
    (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    checksums = [f"{sha256(path)}  {path.name}\n" for path in sorted(output.iterdir())]
    (output / "SHA256SUMS").write_text("".join(checksums))
    verifier.verify(output, commit=commit, require_complete=True, execute_native=False)
    return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--shards", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--version", required=True)
    args = parser.parse_args()
    manifest = merge(args.shards.resolve(), args.output.resolve(), args.commit, args.version)
    print(f"Merged and verified {len(manifest['artifacts'])} archives at {manifest['commit']}")


if __name__ == "__main__":
    main()
