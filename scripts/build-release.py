#!/usr/bin/env python3
"""Build Tack artifacts, verify their contents, then produce a SHA-256 manifest."""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile
import time
import zipfile

ROOT = Path(__file__).resolve().parents[1]
MODERN = "go1.26.3"
LEGACY = "go1.23.12"
NODE_TARGETS = [
    ("linux", "amd64", ""), ("linux", "386", ""),
    ("linux", "arm", "5"), ("linux", "arm", "6"), ("linux", "arm", "7"),
    ("linux", "arm64", ""), ("linux", "mips", ""), ("linux", "mipsle", ""),
    ("linux", "mips64", ""), ("linux", "mips64le", ""),
    ("windows", "amd64", ""), ("windows", "386", ""), ("windows", "arm64", ""),
]
TOOL_TARGETS = [("linux", "amd64", ""), ("linux", "arm64", ""),
                ("darwin", "amd64", ""), ("darwin", "arm64", ""),
                ("windows", "amd64", ""), ("windows", "386", ""), ("windows", "arm64", "")]


def command(*args, cwd=ROOT, env=None):
    return subprocess.check_output(args, cwd=cwd, env=env, text=True).strip()


def archive(path, files, epoch):
    if path.suffix == ".zip":
        stamp = time.gmtime(max(epoch, 315532800))[:6]
        with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as out:
            for name, (data, mode) in files.items():
                info = zipfile.ZipInfo(name, stamp)
                info.external_attr = mode << 16
                info.compress_type = zipfile.ZIP_DEFLATED
                out.writestr(info, data)
        with zipfile.ZipFile(path) as check:
            if check.testzip() is not None or set(check.namelist()) != set(files):
                raise RuntimeError(f"invalid archive: {path}")
            for name, (data, _) in files.items():
                if check.read(name) != data:
                    raise RuntimeError(f"archive changed contents: {path}/{name}")
    else:
        with path.open("wb") as raw, gzip.GzipFile(filename="", fileobj=raw, mode="wb", mtime=epoch) as gz:
            with tarfile.open(fileobj=gz, mode="w") as out:
                for name, (data, mode) in files.items():
                    info = tarfile.TarInfo(name)
                    info.size, info.mode, info.mtime = len(data), mode, epoch
                    out.addfile(info, io.BytesIO(data))
        # Read through the gzip trailer as well as verifying tar member bytes.
        with gzip.open(path, "rb") as check:
            while check.read(1024 * 1024):
                pass
        with tarfile.open(path) as check:
            if set(check.getnames()) != set(files):
                raise RuntimeError(f"invalid archive members: {path}")
            for name, (data, _) in files.items():
                if check.extractfile(name).read() != data:
                    raise RuntimeError(f"archive changed contents: {path}/{name}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--native-only", action="store_true", help="local packaging smoke test only")
    args = parser.parse_args()
    if not re.fullmatch(r"\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?", args.version):
        parser.error("expected a semantic release version without a tag prefix")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        parser.error("output directory must be empty; existing artifacts are never overwritten")
    commit = command("git", "rev-parse", "HEAD")
    epoch = int(command("git", "show", "-s", "--format=%ct", "HEAD"))
    native_os = command("go", "env", "GOHOSTOS", env={**os.environ, "GOTOOLCHAIN": "local"})
    native_arch = command("go", "env", "GOHOSTARCH", env={**os.environ, "GOTOOLCHAIN": "local"})
    components = [
        ("tack-node", "management/node", "./cmd/tack-node", LEGACY, NODE_TARGETS),
        ("tack", "management/control", "./cmd/tack", MODERN, TOOL_TARGETS),
        ("tack-control", "management/control", "./cmd/tack-control", MODERN,
         [("linux", "amd64", ""), ("linux", "arm64", ""), ("windows", "amd64", "")]),
        ("chisel", ".", ".", MODERN, NODE_TARGETS),
    ]
    manifest = {"version": args.version, "commit": commit, "source_date_epoch": epoch,
                "native_only": args.native_only, "artifacts": []}
    with tempfile.TemporaryDirectory(prefix="tack-release-") as temp:
        for component, module, package, toolchain, targets in components:
            for goos, goarch, arm in targets:
                if args.native_only and (goos, goarch) != (native_os, native_arch):
                    continue
                label = goarch if not arm else "armv" + arm
                if goarch.startswith("mips"):
                    label += "-softfloat"
                executable = component + (".exe" if goos == "windows" else "")
                binary = Path(temp) / executable
                env = dict(os.environ, GOTOOLCHAIN=toolchain, CGO_ENABLED="0", GOOS=goos,
                           GOARCH=goarch, GOARM=arm or "5", GOMIPS="softfloat", GOMIPS64="softfloat")
                symbol = ("github.com/jpillora/chisel/share.BuildVersion" if component == "chisel"
                          else "tack.local/management/protocol.BuildVersion")
                subprocess.run(["go", "build", "-trimpath", "-ldflags", f"-s -w -X {symbol}={args.version}",
                                "-o", str(binary), package], cwd=ROOT / module, env=env, check=True)
                info = command("go", "version", "-m", str(binary), env=env)
                if toolchain not in info or f"GOOS={goos}" not in info or f"GOARCH={goarch}" not in info or "CGO_ENABLED=0" not in info:
                    raise RuntimeError(f"unexpected build metadata for {component}/{goos}/{label}")
                if (goos, goarch) == (native_os, native_arch):
                    flag = "--version" if component == "chisel" else "-version"
                    version = command(str(binary), flag)
                    if args.version not in version:
                        raise RuntimeError(f"incorrect version stamp: {version}")
                extension = ".zip" if goos == "windows" else ".tar.gz"
                name = f"{component}_{args.version}_{goos}_{label}{extension}"
                files = {executable: (binary.read_bytes(), 0o755),
                         "LICENSE": ((ROOT / "LICENSE").read_bytes(), 0o644),
                         "README.md": ((ROOT / "management/README.md").read_bytes(), 0o644)}
                archive(output / name, files, epoch)
                manifest["artifacts"].append({"file": name, "component": component,
                                             "os": goos, "arch": label, "toolchain": toolchain})
                print(f"Verified {name}", flush=True)
    if not manifest["artifacts"]:
        raise RuntimeError("no artifacts produced")
    (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    checksums = []
    for file in sorted(output.iterdir()):
        checksums.append(f"{hashlib.sha256(file.read_bytes()).hexdigest()}  {file.name}\n")
    (output / "SHA256SUMS").write_text("".join(checksums))
    print(f"Verified {len(manifest['artifacts'])} archives; commit {commit}", flush=True)


if __name__ == "__main__":
    main()
