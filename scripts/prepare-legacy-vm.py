#!/usr/bin/env python3
"""Fetch checksum-pinned old-kernel test fixtures; never install them on the host."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import tempfile

LOCK = Path(__file__).resolve().parents[1] / "compat/legacy/vm-fixtures.json"


def sha(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def prepare(directory, download=False):
    lock = json.loads(LOCK.read_text())
    directory.mkdir(parents=True, exist_ok=True)
    for name, item in lock["downloads"].items():
        dest = directory / name
        if not dest.exists():
            if not download:
                raise ValueError(f"missing {dest}; use --download to fetch pinned fixtures")
            partial = dest.with_suffix(".partial")
            subprocess.run(["curl", "--fail", "--location", "--proto", "=https", "--retry", "3",
                            "--connect-timeout", "20", "--max-time", "180", "--output", str(partial), item["url"]], check=True)
            if sha(partial) != item["sha256"]:
                raise ValueError("fixture checksum mismatch: " + name)
            partial.replace(dest)
        if sha(dest) != item["sha256"]:
            raise ValueError("fixture checksum mismatch: " + name)
    with tempfile.TemporaryDirectory(prefix="tack-kernel-extract-") as temp:
        unpack = subprocess.Popen(["busybox", "rpm2cpio", str(directory / "kernel.rpm")], stdout=subprocess.PIPE)
        try:
            subprocess.run(["cpio", "--extract", "--make-directories", "--no-absolute-filenames", "--no-preserve-owner",
                            *("./" + member for member in lock["rpm_members"])], stdin=unpack.stdout, cwd=temp, check=True,
                           stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            unpack.stdout.close()
            if unpack.wait(timeout=10):
                raise RuntimeError("RPM decoding failed")
        finally:
            if unpack.poll() is None:
                unpack.kill()
                unpack.wait()
        for member, item in lock["rpm_members"].items():
            source = Path(temp) / member
            if sha(source) != item["sha256"]:
                raise ValueError("extracted fixture checksum mismatch: " + member)
            shutil.copyfile(source, directory / item["name"])
    return lock


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("--download", action="store_true")
    args = parser.parse_args()
    prepare(args.directory.resolve(), args.download)
    print("Verified pinned kernel, network module and BusyBox test fixtures")
