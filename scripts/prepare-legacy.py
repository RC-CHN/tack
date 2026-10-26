#!/usr/bin/env python3
"""Prepare pinned dependency adapters for the Go 1.23 unified Node build.

Keeps the modern SSH fixes; it does not downgrade x/crypto. Generated files live
only in bin/legacy-deps. The checked-in modern module and module cache are intact.
"""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import subprocess
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parents[1]
LOCK = ROOT / "compat/legacy/modules.json"
DEST = ROOT / "bin/legacy-deps"


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def module_sum(package):
    """Go's module zip h1 (sumdb/dirhash.Hash1), including module/version paths."""
    lines = []
    for name in sorted(package.namelist()):
        if not name.endswith("/"):
            if "\n" in name:
                raise ValueError("invalid module filename")
            lines.append(hashlib.sha256(package.read(name)).hexdigest() + "  " + name + "\n")
    return "h1:" + base64.b64encode(hashlib.sha256("".join(lines).encode()).digest()).decode()


def replace_exact(path, old, new, count=1):
    text = path.read_text()
    if text.count(old) != count:
        raise RuntimeError(f"upstream adapter input changed: {path.name}")
    path.write_text(text.replace(old, new))


def adapt(stage, modules):
    # These packages use Go 1.23 language/stdlib APIs on the selected build path.
    # Limit the module graph to that path; do not present this as a general fork.
    for module, record in modules.items():
        name = module.rsplit("/", 1)[-1]
        deps = {"crypto": ["sys", "term"], "term": ["sys"]}.get(name, [])
        text = f"module {module}\n\ngo 1.23.0\n"
        for dep in deps:
            version = modules["golang.org/x/" + dep]["version"]
            text += f"\nrequire golang.org/x/{dep} {version}\nreplace golang.org/x/{dep} => ../{dep}\n"
        (stage / name / "go.mod").write_text(text)

    crypto = stage / "crypto"
    for name in ("common.go", "cipher.go", "mac.go", "kex.go"):
        replace_exact(crypto / "ssh" / name, '"crypto/fips140"', '"golang.org/x/crypto/internal/legacyfips"')
    shim = crypto / "internal/legacyfips"
    shim.mkdir()
    (shim / "fips.go").write_text(
        '// Generated Tack Go 1.23 adapter: this runtime has no FIPS 140 mode.\n'
        'package fips140\n\nfunc Enabled() bool { return false }\n')
    # Do not advertise or silently substitute an unimplemented KEX. Its named
    # algorithm remains a constant, but it is absent from supported/default KEXs.
    replace_exact(crypto / "ssh/common.go", '\t\tKeyExchangeMLKEM768X25519,\n', '', count=2)
    replace_exact(crypto / "ssh/kex.go",
                  '\tkexAlgoMap[KeyExchangeMLKEM768X25519] = &mlkem768WithCurve25519sha256{}\n', '')
    replace_exact(crypto / "ssh/mlkem.go", 'package ssh\n', '//go:build go1.24\n\npackage ssh\n')
    # The upstream regressions are retained; only their benchmark loop API
    # needs translation for testing.B before Go 1.24.
    replace_exact(crypto / "ssh/messages_test.go", 'for b.Loop() {', 'for i := 0; i < b.N; i++ {', count=2)


def prepare():
    modules = json.loads(LOCK.read_text())["modules"]
    env = dict(os.environ, GOTOOLCHAIN="go1.26.3", GOWORK="off", GOFLAGS="")
    selected = subprocess.check_output(["go", "list", "-m", "-f", "{{.Path}} {{.Version}}", *modules],
                                       cwd=ROOT, env=env, text=True)
    if dict(line.split() for line in selected.splitlines()) != {m: r["version"] for m, r in modules.items()}:
        raise RuntimeError("modern dependencies changed; review and update the Legacy adapter lock")
    marker = DEST / "manifest.json"
    if marker.is_file():
        previous = json.loads(marker.read_text())
        if (previous.get("generator_sha256") == digest(__file__) and previous.get("lock_sha256") == digest(LOCK)
                and previous.get("upstream") == modules):
            actual = {str(p.relative_to(DEST)): digest(p) for p in sorted(DEST.rglob("*"))
                      if p.is_file() and p != marker}
            if actual == previous.get("files"):
                return previous
    DEST.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="legacy-deps-", dir=DEST.parent) as tmp:
        stage = Path(tmp)
        for module, record in modules.items():
            output = subprocess.check_output(["go", "mod", "download", "-json", module + "@" + record["version"]],
                                             cwd=ROOT, env=env, text=True)
            info = json.loads(output)
            if info.get("Sum") != record["sum"]:
                raise RuntimeError(f"module checksum mismatch: {module}")
            prefix = module + "@" + record["version"] + "/"
            target = stage / module.rsplit("/", 1)[-1]
            with zipfile.ZipFile(info["Zip"]) as package:
                if module_sum(package) != record["sum"]:
                    raise RuntimeError(f"module archive content mismatch: {module}")
                for name in package.namelist():
                    if name.endswith("/"):
                        continue
                    if not name.startswith(prefix):
                        raise ValueError("unexpected module archive prefix")
                    relative = PurePosixPath(name[len(prefix):])
                    if relative.is_absolute() or ".." in relative.parts:
                        raise ValueError("unsafe module archive path")
                    dest = target.joinpath(*relative.parts)
                    dest.parent.mkdir(parents=True, exist_ok=True)
                    dest.write_bytes(package.read(name))
        adapt(stage, modules)
        files = {str(p.relative_to(stage)): digest(p) for p in sorted(stage.rglob("*")) if p.is_file()}
        record = {"format": "tack-legacy-adapters-v1", "generator_sha256": digest(__file__),
                  "lock_sha256": digest(LOCK), "upstream": modules, "files": files,
                  "limitations": ["Go 1.23 runtime", "SSH ML-KEM unavailable", "FIPS 140 unavailable"]}
        (stage / "manifest.json").write_text(json.dumps(record, indent=2) + "\n")
        if DEST.exists():
            marker = DEST / "manifest.json"
            if not marker.is_file() or json.loads(marker.read_text()).get("format") != record["format"]:
                raise RuntimeError("refusing to replace an unrecognized legacy dependency directory")
            shutil.rmtree(DEST)
        shutil.move(str(stage), str(DEST))
    return record


if __name__ == "__main__":
    argparse.ArgumentParser(description=__doc__).parse_args()
    result = prepare()
    print(f"Prepared {len(result['upstream'])} pinned adapters in {DEST}")
