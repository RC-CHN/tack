"""Pinned protection tools and validation shared by release/benchmark builders."""
import hashlib
import os
from pathlib import Path
import struct
import subprocess

GARBLE = {"go1.23.12": ("legacy", "v0.14.2"), "go1.26.3": ("modern", "v0.17.0")}
UPX_VERSION = "5.0.2"
OBFUSCATION_SCOPE = "tack.local/*,github.com/jpillora/chisel"
# Official release asset digests, not checksums supplied by the downloaded file.
UPX_ASSETS = {
    ("linux", "amd64"): ("amd64_linux.tar.xz", "ddc2654063fe4dc80d95b420788494e4db078ebb01a650692d623b5a9906e61e"),
    ("linux", "arm64"): ("arm64_linux.tar.xz", "100310f74eb6f67694d1d0377f1c729b6a49238ce8c4de21ea2e7d3406186f8b"),
    ("windows", "amd64"): ("win64.zip", "1cff6d6ee01c404d4a1ae56003056bd4cfa28d0a5cea1d0950daaf872a4facc9"),
}


def sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def run(*args, env=None, cwd=None):
    return subprocess.check_output(args, env=env, cwd=cwd, text=True).strip()


def build_env(toolchain, goos, goarch, arm=""):
    # Ambient GOFLAGS must never silently introduce race instrumentation, tags,
    # alternate architectures or a different obfuscation scope into the record.
    return dict(os.environ, GOTOOLCHAIN=toolchain, CGO_ENABLED="0", GOOS=goos,
                GOARCH=goarch, GOARM=arm or "5", GOAMD64="v1", GOMIPS="softfloat",
                GOMIPS64="softfloat", GOFLAGS="", GOEXPERIMENT="", GOWORK="off",
                GOGARBLE=OBFUSCATION_SCOPE)


def tool(path, expected, command):
    path = Path(path).resolve()
    actual = run(str(path), command).splitlines()[0]
    if actual != expected:
        raise RuntimeError(f"unexpected tool version: {path}: {actual!r}, expected {expected!r}")
    return {"version": actual, "sha256": sha256(path)}


def verify_target(path, goos, arch):
    """Read executable headers even after Go metadata has been removed."""
    data = Path(path).read_bytes()
    try:
        if goos == "linux":
            bits, endian, machine = {
                "amd64": (2, 1, 62), "386": (1, 1, 3), "arm": (1, 1, 40),
                "arm64": (2, 1, 183), "mips": (1, 2, 8), "mipsle": (1, 1, 8),
                "mips64": (2, 2, 8), "mips64le": (2, 1, 8),
            }[arch]
            valid = (data[:4] == b"\x7fELF" and data[4:6] == bytes((bits, endian))
                     and struct.unpack_from("<H" if endian == 1 else ">H", data, 18)[0] == machine)
        elif goos == "windows":
            offset = struct.unpack_from("<I", data, 60)[0]
            machine = {"amd64": 0x8664, "386": 0x14c, "arm64": 0xaa64}[arch]
            valid = (data[:2] == b"MZ" and data[offset:offset + 4] == b"PE\0\0"
                     and struct.unpack_from("<H", data, offset + 4)[0] == machine)
        elif goos == "darwin":
            machine = {"amd64": 0x01000007, "arm64": 0x0100000c}[arch]
            valid = data[:4] == b"\xcf\xfa\xed\xfe" and struct.unpack_from("<I", data, 4)[0] == machine
        else:
            valid = False
    except (IndexError, KeyError, struct.error):
        valid = False
    if not valid:
        raise RuntimeError(f"incorrect executable target: {path}: {goos}/{arch}")


def version_output(binary, component, version):
    flag = "--version" if component == "chisel" else "-version"
    result = subprocess.run([str(binary), flag], text=True, capture_output=True, timeout=15)
    if result.returncode or result.stdout.strip() != version:
        raise RuntimeError(f"version smoke test failed: {result.returncode}: {result.stdout} {result.stderr}")
    return result.stdout.strip()


def protect(binary, upx, native, component, version, goos, arch):
    """Only known unsupported formats may fall back; corruption is fatal."""
    packed = binary.with_name(binary.stem + "-packed" + binary.suffix)
    flags = ["--no-env", "-9"]
    # PE import tables may otherwise be rewritten by UPX. Until that transform
    # has native semantic validation, require its byte-preserving mode.
    if goos == "windows":
        flags.append("--exact")
    result = subprocess.run([str(upx)] + flags + ["-q", "-o", str(packed), str(binary)],
                            capture_output=True, text=True, timeout=300)
    report = {"tool_version": UPX_VERSION, "flags": flags, "attempted": True,
              "input_sha256": sha256(binary), "input_bytes": binary.stat().st_size}
    if result.returncode:
        # A compiler failure, broken UPX installation or corrupt output is never
        # converted into a successful unprotected release.
        reason = (result.stdout + "\n" + result.stderr).strip()
        unsupported = ("UnknownExecutableFormatException", "NotCompressibleException",
                       "CantPackException: cannot pack", "CantPackException: unknown format",
                       "CantPackException: option '--exact' does not work with this file",
                       "CantPackException: win64/arm64 is not yet supported",
                       "CantPackException: macOS is currently not supported (try --force-macos)")
        if not any(marker in reason for marker in unsupported):
            raise RuntimeError(f"UPX failed unexpectedly: {reason}")
        report.update(status="exception", reason=reason, validation="obfuscated executable header")
        packed.unlink(missing_ok=True)
        return report
    verify_target(packed, goos, arch)
    subprocess.run([str(upx), "--no-env", "-t", str(packed)], check=True, capture_output=True, timeout=60)
    # UPX's internal checksum is supplemented by a byte-for-byte round trip.
    restored = binary.with_name(binary.stem + "-restored" + binary.suffix)
    subprocess.run([str(upx), "--no-env", "-d", "-q", "-o", str(restored), str(packed)],
                   check=True, capture_output=True, timeout=60)
    if sha256(restored) != sha256(binary):
        raise RuntimeError("UPX decompression changed executable bytes")
    restored.unlink()
    if native:
        try:
            version_output(packed, component, version)
        except (RuntimeError, subprocess.TimeoutExpired) as exc:
            # The design allows a recorded platform exception after an actual
            # execution failure, but the original must still pass its smoke test.
            version_output(binary, component, version)
            report.update(status="exception", reason=str(exc), validation="native obfuscated version smoke")
            packed.unlink()
            return report
    packed.replace(binary)
    report.update(status="packed", validation="upx-test and exact decompression round trip")
    return report
