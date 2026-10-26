#!/usr/bin/env python3
"""Install pinned Garble/UPX into an explicit directory; never change system tools."""
import argparse
import hashlib
import io
import os
from pathlib import Path
import platform
import subprocess
import tarfile
import urllib.request
import zipfile

from release_support import GARBLE, UPX_ASSETS, UPX_VERSION, tool


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--without-upx", action="store_true", help="obfuscation-only host, e.g. macOS")
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    suffix = ".exe" if os.name == "nt" else ""
    for go, (directory, version) in GARBLE.items():
        dest = output / directory
        dest.mkdir(exist_ok=True)
        binary = dest / ("garble" + suffix)
        if not binary.exists():
            env = dict(os.environ, GOTOOLCHAIN=go, GOBIN=str(dest), GOFLAGS="", GOWORK="off",
                       CGO_ENABLED="0", GOOS=platform.system().lower(),
                       GOARCH={"x86_64": "amd64", "AMD64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()])
            subprocess.run(["go", "install", "mvdan.cc/garble@" + version], env=env, check=True)
        tool(binary, "mvdan.cc/garble " + version, "version")
    if not args.without_upx:
        host = (platform.system().lower(), {"x86_64": "amd64", "AMD64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()])
        archive, digest = UPX_ASSETS[host]
        binary = output / ("upx" + suffix)
        if not binary.exists():
            name = "upx-" + UPX_VERSION + "-" + archive
            url = f"https://github.com/upx/upx/releases/download/v{UPX_VERSION}/{name}"
            with urllib.request.urlopen(url, timeout=120) as response:
                data = response.read(32 * 1024 * 1024 + 1)
            if hashlib.sha256(data).hexdigest() != digest:
                raise RuntimeError("UPX release archive SHA-256 mismatch")
            # Extract only the named regular executable, never arbitrary archive paths.
            if name.endswith(".zip"):
                with zipfile.ZipFile(io.BytesIO(data)) as package:
                    contents = package.read(name[:-4] + "/upx.exe")
            else:
                with tarfile.open(fileobj=io.BytesIO(data), mode="r:xz") as package:
                    member = package.getmember(name[:-7] + "/upx")
                    if not member.isfile():
                        raise RuntimeError("UPX executable is not a regular file")
                    contents = package.extractfile(member).read()
            with binary.open("xb") as out:
                out.write(contents)
            binary.chmod(0o755)
        tool(binary, "upx " + UPX_VERSION, "--version")
    print(f"Verified pinned build tools in {output}")


if __name__ == "__main__":
    main()
