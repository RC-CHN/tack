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
import importlib.util

from release_support import GARBLE, OBFUSCATION_SCOPE, UPX_VERSION, build_env, protect, sha256, tool, verify_target, version_output

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
    parser.add_argument("--protection", choices=("plain", "obfuscated", "packed"), default="packed")
    parser.add_argument("--tools", type=Path, default=ROOT / "bin/build-tools")
    parser.add_argument("--components", nargs="+", choices=("tack-node", "tack", "tack-control", "chisel"))
    parser.add_argument("--node-runtime", choices=("modern", "legacy", "both"), default="both",
                        help="Node runtime variants; Legacy is an additional Linux single-binary build")
    parser.add_argument("--require-clean", action="store_true", help="required for publication")
    args = parser.parse_args()
    if not re.fullmatch(r"\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?", args.version):
        parser.error("expected a semantic release version without a tag prefix")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        parser.error("output directory must be empty; existing artifacts are never overwritten")
    commit = command("git", "rev-parse", "HEAD")
    dirty = bool(command("git", "status", "--porcelain"))
    if args.require_clean and (dirty or args.native_only or args.components or args.node_runtime != "both"):
        parser.error("publication requires a clean source tree and the complete target matrix")
    sources = command("git", "ls-files", "--cached", "--others", "--exclude-standard").splitlines()
    inputs = {name: sha256(ROOT / name) for name in sorted(set(sources))
              if (ROOT / name).is_file() and not name.startswith("management/validation/")
              and (name.endswith((".go", ".mod", ".sum", ".py", ".sh")) or name.startswith("compat/legacy/"))}
    source_hash = hashlib.sha256(json.dumps(inputs, sort_keys=True).encode()).hexdigest()
    epoch = int(command("git", "show", "-s", "--format=%ct", "HEAD"))
    native_os = command("go", "env", "GOHOSTOS", env={**os.environ, "GOTOOLCHAIN": "local"})
    native_arch = command("go", "env", "GOHOSTARCH", env={**os.environ, "GOTOOLCHAIN": "local"})
    components = [
        ("tack", "management/control", "./cmd/tack", MODERN, TOOL_TARGETS, "modern"),
        ("tack-control", "management/control", "./cmd/tack-control", MODERN,
         [("linux", "amd64", ""), ("linux", "arm64", ""), ("windows", "amd64", "")], "modern"),
        ("chisel", ".", ".", MODERN, NODE_TARGETS, "modern"),
    ]
    if args.node_runtime in ("modern", "both"):
        components.insert(0, ("tack-node", ".", "./cmd/tack-node", MODERN, NODE_TARGETS, "modern"))
    legacy_record = None
    if args.node_runtime in ("legacy", "both") and (not args.components or "tack-node" in args.components):
        if not args.native_only or native_os == "linux":
            spec = importlib.util.spec_from_file_location("tack_prepare_legacy", ROOT / "scripts/prepare-legacy.py")
            adapter = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(adapter)
            prepared = adapter.prepare()
            legacy_record = {k: prepared[k] for k in ("format", "generator_sha256", "lock_sha256", "upstream", "limitations")}
            legacy_record["manifest_sha256"] = sha256(adapter.DEST / "manifest.json")
            components.append(("tack-node", ".", "./cmd/tack-node", LEGACY,
                               [t for t in NODE_TARGETS if t[0] == "linux"], "legacy"))
    manifest = {"format": "tack-release-v2", "version": args.version, "commit": commit,
                "source_date_epoch": epoch, "source_dirty": dirty, "source_sha256": source_hash,
                "native_only": args.native_only, "node_runtime": args.node_runtime,
                "requested_protection": args.protection, "artifacts": []}
    suffix = ".exe" if os.name == "nt" else ""
    tools = args.tools.resolve()
    checked_tools = {}
    if args.protection == "packed":
        checked_tools["upx"] = tool(tools / ("upx" + suffix), "upx " + UPX_VERSION, "--version")
    with tempfile.TemporaryDirectory(prefix="tack-release-") as temp:
        for component, module, package, toolchain, targets, runtime_profile in components:
            if args.components and component not in args.components:
                continue
            garble_dir, garble_version = GARBLE[toolchain]
            garble = tools / garble_dir / ("garble" + suffix)
            if args.protection != "plain" and garble_dir not in checked_tools:
                checked_tools[garble_dir] = tool(garble, "mvdan.cc/garble " + garble_version, "version")
            for goos, goarch, arm in targets:
                if args.native_only and (goos, goarch) != (native_os, native_arch):
                    continue
                label = goarch if not arm else "armv" + arm
                if goarch.startswith("mips"):
                    label += "-softfloat"
                executable = component + (".exe" if goos == "windows" else "")
                binary = Path(temp) / executable
                env = build_env(toolchain, goos, goarch, arm)
                if command("go", "env", "GOVERSION", env=env) != toolchain:
                    raise RuntimeError("Go toolchain does not match the pinned version")
                symbol = ("github.com/jpillora/chisel/share.BuildVersion" if component == "chisel"
                          else "tack.local/management/protocol.BuildVersion")
                flags = ["-trimpath", "-ldflags", f"-s -w -X {symbol}={args.version}"]
                if component == "tack-node":
                    flags[-1] += f" -X github.com/jpillora/chisel/share.BuildVersion={args.version}"
                if runtime_profile == "legacy":
                    flags.extend(["-mod=readonly", "-modfile=go.legacy.mod"])
                    # Garble 0.14's recursive go-list calls do not forward the
                    # command-line modfile flag. Keep every subprocess on the
                    # same module graph via GOFLAGS as well.
                    env["GOFLAGS"] = "-mod=readonly -modfile=go.legacy.mod"
                compiler = ["go"] if args.protection == "plain" else [str(garble), "-literals"]
                subprocess.run(compiler + ["build"] + flags + ["-o", str(binary), package],
                               cwd=ROOT / module, env=env, check=True)
                verify_target(binary, goos, goarch)
                info = command("go", "version", "-m", str(binary), env=env)
                if args.protection == "plain" and (toolchain not in info or f"GOOS={goos}" not in info
                                                   or f"GOARCH={goarch}" not in info or "CGO_ENABLED=0" not in info):
                    raise RuntimeError(f"unexpected build metadata for {component}/{goos}/{label}")
                if args.protection != "plain" and ("GOOS=" in info or "\tmod\t" in info):
                    raise RuntimeError("obfuscated executable still contains Go build metadata")
                native = (goos, goarch) == (native_os, native_arch)
                if native:
                    version_output(binary, component, args.version)
                packing = {"attempted": False, "status": "disabled"}
                actual = args.protection
                if args.protection == "packed":
                    if runtime_profile == "legacy":
                        packing = {"attempted": False, "status": "exception", "tool_version": UPX_VERSION,
                                   "reason": "Legacy kernel qualification: UPX 5.0.2 amd64 loaders exit 139 on CentOS 6 kernel 2.6.32-754; retain obfuscation until each Legacy target's packed loader is qualified"}
                    else:
                        packing = protect(binary, tools / ("upx" + suffix), native,
                                          component, args.version, goos, goarch)
                    if packing["status"] != "packed":
                        actual = "obfuscated"
                native_version = version_output(binary, component, args.version) if native else None
                record = {"format": "tack-build-v1", "component": component, "version": args.version,
                          "source_commit": commit, "source_dirty": dirty, "source_sha256": source_hash,
                          "go_version": toolchain, "os": goos, "arch": goarch, "arm": arm,
                          "mips_float": "softfloat" if goarch.startswith("mips") else None,
                          "cgo_enabled": False, "race": False, "protection": actual,
                          "sha256": sha256(binary), "bytes": binary.stat().st_size,
                          "build_flags": flags, "garble_flags": [] if actual == "plain" else ["-literals"],
                          "garble_seed": "default (derived from build inputs)",
                          "gogarble": OBFUSCATION_SCOPE, "packing": packing,
                          "validation": "native-version-smoke" if native else "compile-and-format",
                          "native_version": native_version,
                          "tools": {"garble": checked_tools.get(garble_dir), "upx": checked_tools.get("upx")}}
                record["runtime_profile"] = runtime_profile
                if component == "tack-node":
                    record["capabilities"] = ["management", "embedded-chisel"]
                if runtime_profile == "legacy":
                    record["legacy_adapter"] = legacy_record
                    record["compatibility"] = {"linux_kernel_floor": {"amd64": "2.6.32", "386": "2.6.32", "arm": "3.1", "mips64le": "4.8"}.get(goarch),
                                               "glibc_required": False, "browser_tls_profiles": False,
                                               "ssh_mlkem": False, "fips140": False,
                                               "old_kernel_validation": "separate functional qualification required"}
                extension = ".zip" if goos == "windows" else ".tar.gz"
                variant = "_legacy" if runtime_profile == "legacy" else ""
                name = f"{component}_{args.version}_{goos}_{label}{variant}{extension}"
                readme = (ROOT / "management/README.md").read_bytes()
                if runtime_profile == "legacy":
                    readme += b"\n\n## Legacy dependency license\n\n"
                    readme += (ROOT / "bin/legacy-deps/crypto/LICENSE").read_bytes()
                files = {executable: (binary.read_bytes(), 0o755),
                         "LICENSE": ((ROOT / "LICENSE").read_bytes(), 0o644),
                         "README.md": (readme, 0o644),
                         "BUILD.json": ((json.dumps(record, indent=2) + "\n").encode(), 0o644)}
                archive(output / name, files, epoch)
                manifest["artifacts"].append({"file": name, "component": component,
                                             "os": goos, "arch": label, "toolchain": toolchain,
                                             "archive_sha256": sha256(output / name), "build": record})
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
