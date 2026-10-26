#!/usr/bin/env python3
"""Measure integrated shaping and TLS profiles in actual Node and Chisel processes."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import threading

from release_support import build_env

ROOT = Path(__file__).resolve().parents[1]
GO = "go1.26.3"


def qualify(output, checks, runs, modes=2, chisel=False):
    """A successful test process alone does not prove transfer completion."""
    result = {"passed": all(c["exit_code"] == 0 for c in checks), "reports": []}
    for check in checks:
        name = check["name"]
        if not name.startswith(("footprint-", "severe-", "chisel-")):
            continue
        record = {"name": name, "passed": False}
        try:
            raw = json.loads((output / (name + ".json")).read_text())
            record["fixture_passed"] = raw.get("passed") is True
            record["passed"] = record["fixture_passed"]
            if name.startswith(("severe-", "chisel-")):
                record["completed"] = raw.get("completed") is True
                record["passed"] = record["passed"] and record["completed"]
        except (OSError, ValueError) as error:
            record["error"] = str(error)
        result["reports"].append(record)
        result["passed"] = result["passed"] and record["passed"]
    result["passed"] = result["passed"] and len(result["reports"]) == runs * modes * (3 if chisel else 2)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--runs", type=int, default=3)
    parser.add_argument("--jobs", type=int, default=1, choices=(1, 2, 3), help="concurrent mode workers; process resources remain isolated")
    parser.add_argument("--browser-profile", choices=("chrome-133", "personalized-v1"), default="chrome-133")
    parser.add_argument("--phase-seconds", type=int, default=5)
    parser.add_argument("--severe-seconds", type=int, default=75)
    args = parser.parse_args()
    if sys.platform != "linux" or not 1 <= args.runs <= 10:
        parser.error("Linux process fixtures required; runs must be 1..10")
    if not 3 <= args.phase_seconds <= 86400 or not 20 <= args.severe_seconds <= 600:
        parser.error("phase-seconds must be 3..86400 and severe-seconds 20..600")
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=True)
    if any(out.iterdir()):
        parser.error("output directory must be empty")
    env = build_env(GO, "linux", subprocess.check_output(["go", "env", "GOHOSTARCH"], text=True).strip(), "")
    # Control runtime parallelism for the process fixtures; no artificial memory cap.
    env.update(GOMAXPROCS="2", GOGC="100")
    for key in list(env):
        if key.startswith("TACK_"):
            env.pop(key)
    evidence = {"format": "tack-obfuscation-integrated-v1", "started_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "go": GO, "runs": args.runs, "mode_workers": args.jobs, "builds": {}, "checks": [],
                "browser_profile": args.browser_profile,
                "scope": f"Actual Node HTTPS and Chisel WSS: native, native + bounded-v1, {args.browser_profile} + bounded-v1; HTTP/1.1, direct TLS"}
    evidence["commit"] = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    evidence["dirty"] = bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT))
    paths = subprocess.check_output(["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"], cwd=ROOT).decode().split("\0")
    sources = {path: hashlib.sha256((ROOT / path).read_bytes()).hexdigest()
               for path in sorted(set(paths)) if path and (ROOT / path).is_file()
               and not path.startswith("management/validation/")
               and (Path(path).suffix in (".go", ".mod", ".sum", ".py") or path == ".github/workflows/tack.yml")}
    (out / "source-sha256.json").write_text(json.dumps(sources, indent=2) + "\n")

    lock = threading.Lock()

    def save():
        (out / "summary.json").write_text(json.dumps(evidence, indent=2) + "\n")

    def run(name, command, cwd=ROOT, extra=None, required=False):
        current = env.copy()
        if extra:
            current.update(extra)
        start = time.monotonic()
        with (out / (name + ".log")).open("w") as log:
            result = subprocess.run(command, cwd=cwd, env=current, stdout=log, stderr=subprocess.STDOUT)
        with lock:
            evidence["checks"].append({"name": name, "command": command, "cwd": str(cwd.relative_to(ROOT)),
                                       "exit_code": result.returncode, "seconds": time.monotonic() - start})
            save()
        print(f"{name}: exit={result.returncode}", flush=True)
        if required and result.returncode:
            raise RuntimeError(f"{name} failed; inspect {out / (name + '.log')}")

    for name, module, package, tags in [
        ("node-native", "management/node", "./cmd/tack-node", []),
        ("node-browser", "transport/browser", "./cmd/tack-node", []),
        ("chisel-native", ".", ".", []),
        ("chisel-browser", ".", ".", ["-tags", "browser"]),
        ("tack-control", "management/control", "./cmd/tack-control", []),
        ("tack", "management/control", "./cmd/tack", []),
    ]:
        binary = out / name
        run("build-" + name, ["go", "build", *tags, "-trimpath", "-ldflags=-s -w", "-o", str(binary), package], ROOT / module, required=True)
        evidence["builds"][name] = {"bytes": binary.stat().st_size,
                                      "sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
                                      "go_version_m": subprocess.check_output(["go", "version", "-m", str(binary)], env=env, text=True)}
        save()
    smoke = out / "smoke-bin"
    smoke.mkdir()
    for name, binary in {"tack": "tack", "tack-control": "tack-control", "tack-node": "node-browser", "chisel": "chisel-browser"}.items():
        (smoke / name).symlink_to(out / binary)
    run("cli-obfuscation-smoke", [sys.executable, str(ROOT / "scripts/smoke-protected.py"),
                                 "--bin-dir", str(smoke), "--report", str(out / "cli-smoke.json"), "--obfuscation", "--tls-profile", args.browser_profile], required=True)
    fixture = out / "integration.test"
    run("build-fixture", ["go", "test", "-c", "-o", str(fixture), "."], ROOT / "management/integration", required=True)
    chisel_fixture = out / "chisel.test"
    run("build-chisel-fixture", ["go", "test", "-c", "-o", str(chisel_fixture), "./test/e2e"], required=True)
    run("tls-and-padding-wire", ["go", "test", "-count=1", "-run", "^(TestTLSWireExperiment|TestEarlyPaddingTLSExperiment)$", "-v", "."],
        ROOT / "transport/browser", {"TACK_TLS_REPORT": str(out / "tls-wire.json"), "TACK_PADDING_REPORT": str(out / "padding-wire.json")})
    run("padding-allocations", ["go", "test", "-run", "^$", "-bench", "BenchmarkEarlyPadding", "-benchmem", "-count=3", "."], ROOT / "management/protocol/shaping")
    if args.browser_profile == "personalized-v1":
        run("template-allocations", ["go", "test", "-run", "^$", "-bench", "BenchmarkPersonalizedSpec$", "-benchmem", "-count=3", "."], ROOT / "transport/browser")
    for sample in range(args.runs):
        modes = [("native", "native", "off", "node-native", "chisel-native"),
                 ("shaped", "native", "bounded-v1", "node-native", "chisel-native"),
                 ("shaped-browser", args.browser_profile, "bounded-v1", "node-browser", "chisel-browser")]
        if sample % 2:
            modes.reverse()
        def measure(mode_config):
            mode, profile, shape, binary, chisel_binary = mode_config
            common = {"TACK_NODE_BINARY": str(out / binary), "TACK_TLS_PROFILE": profile, "TACK_TRAFFIC_SHAPING": shape, "TACK_RESOURCE_HTTP2": "0"}
            for kind, test, params in [
                ("footprint", "TestNodeLowFootprint", {"TACK_RESOURCE_SECONDS": str(args.phase_seconds), "TACK_RESOURCE_MAX_OUTPUT": "1"}),
                ("severe", "TestNodeSevereLink", {"TACK_SEVERE_SECONDS": str(args.severe_seconds), "TACK_SEVERE_REQUIRE_COMPLETE": "1"}),
            ]:
                label = f"{kind}-{mode}-{sample+1}"
                report_key = "TACK_RESOURCE_REPORT" if kind == "footprint" else "TACK_SEVERE_REPORT"
                run(label, [str(fixture), "-test.run=^" + test + "$", "-test.v", "-test.timeout=8m"],
                    ROOT / "management/integration", {**common, **params, report_key: str(out / (label + ".json"))})
            label = f"chisel-{mode}-{sample+1}"
            run(label, [str(chisel_fixture), "-test.run=^TestObfuscationProcess$", "-test.v", "-test.timeout=4m"],
                ROOT, {**common, "TACK_CHISEL_BINARY": str(out / chisel_binary), "TACK_CHISEL_REPORT": str(out / (label + ".json"))})
        with ThreadPoolExecutor(max_workers=args.jobs) as executor:
            list(executor.map(measure, modes))
    evidence["qualification"] = qualify(out, evidence["checks"], args.runs, modes=3, chisel=True)
    evidence["passed"] = evidence["qualification"]["passed"]
    save()
    return 0 if evidence["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
