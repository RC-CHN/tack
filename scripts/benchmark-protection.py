#!/usr/bin/env python3
"""Compare actual plain/obfuscated/packed Linux Node release archives."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import statistics
import subprocess
import tarfile
import time

from release_support import sha256

ROOT = Path(__file__).resolve().parents[1]
PROFILES = ("plain", "obfuscated", "packed")


def save(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def extract(directory, dest, profile):
    manifest = json.loads((directory / "manifest.json").read_text())
    entries = [a for a in manifest["artifacts"] if a["component"] == "tack-node"
               and a["os"] == "linux" and a["arch"] == "amd64"]
    if len(entries) != 1:
        raise RuntimeError("comparison requires exactly one Linux amd64 Node archive")
    entry = entries[0]
    archive = directory / entry["file"]
    if sha256(archive) != entry["archive_sha256"]:
        raise RuntimeError("archive checksum mismatch")
    with tarfile.open(archive) as package:
        if len(package.getmembers()) != 4 or set(package.getnames()) != {"tack-node", "BUILD.json", "LICENSE", "README.md"}:
            raise RuntimeError("unexpected release archive members")
        record = json.load(package.extractfile("BUILD.json"))
        data = package.extractfile("tack-node").read()
    if record != entry["build"] or hashlib.sha256(data).hexdigest() != record["sha256"]:
        raise RuntimeError("build record does not match executable")
    if record["protection"] != profile:
        raise RuntimeError(f"{profile} comparison unexpectedly fell back to {record['protection']}")
    dest.mkdir()
    binary = dest / "tack-node"
    binary.write_bytes(data)
    binary.chmod(0o755)
    save(dest / "BUILD.json", record)
    return record, archive.stat().st_size


def startup(binary, version):
    started = time.perf_counter_ns()
    process = subprocess.Popen([str(binary), "-version"], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    _, status, usage = os.wait4(process.pid, 0)
    elapsed = (time.perf_counter_ns() - started) / 1e6
    process.returncode = os.waitstatus_to_exitcode(status)
    stdout, stderr = process.communicate()
    if process.returncode or stdout.decode().strip() != version:
        raise RuntimeError(f"version launch failed: {stdout!r} {stderr!r}")
    # wait4's peak RSS can include the forked Python process before exec. Actual
    # Node RSS is measured independently by the process fixture after exec.
    return {"elapsed_ms": elapsed, "cpu_ms": (usage.ru_utime + usage.ru_stime) * 1000}


def summarize(output, records, archive_sizes, starts):
    summary = {}
    for profile in PROFILES:
        samples = starts[profile]
        times = sorted(s["elapsed_ms"] for s in samples)
        row = {"binary_bytes": records[profile]["bytes"], "archive_bytes": archive_sizes[profile],
               "startup_process_samples": len(times), "startup_median_ms": statistics.median(times),
               "startup_p95_ms": times[min(len(times) - 1, (95 * len(times) + 99) // 100 - 1)],
               "startup_cpu_median_ms": statistics.median(s["cpu_ms"] for s in samples),
               "http": {}}
        for http in (0, 1):
            footprint = [json.loads(p.read_text()) for p in sorted(output.glob(f"{profile}-h{http}-footprint-*.json"))]
            severe = [json.loads(p.read_text()) for p in sorted(output.glob(f"{profile}-h{http}-severe-*.json"))]
            row["http"][str(http)] = {"footprint_runs": len(footprint),
                "footprint_passes": sum(d["passed"] for d in footprint),
                "peak_rss_bytes": max((s["process"]["peak_rss_bytes"] for d in footprint for p in d["phases"] for s in p["samples"]), default=0),
                "severe_runs": len(severe), "severe_completed": sum(d["completed"] for d in severe),
                "severe_elapsed_seconds": [d["elapsed_seconds"] for d in severe],
                "severe_progress_bytes": [d["progress_bytes"] for d in severe],
                "severe_peak_rss_bytes": max((d["max_rss_bytes"] for d in severe), default=0)}
        summary[profile] = row
    save(output / "summary.json", summary)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in PROFILES:
        parser.add_argument("--" + name, required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--seconds", type=int, default=5)
    parser.add_argument("--repeats", type=int, default=3)
    parser.add_argument("--severe-repeats", type=int, default=1)
    parser.add_argument("--severe-seconds", type=int, default=75)
    parser.add_argument("--startup-runs", type=int, default=30)
    args = parser.parse_args()
    if platform.system() != "Linux" or platform.machine() != "x86_64":
        parser.error("this comparison requires native Linux amd64")
    if not (3 <= args.seconds <= 60 and 1 <= args.repeats <= 20 and 1 <= args.severe_repeats <= 20
            and 20 <= args.severe_seconds <= 600 and 3 <= args.startup_runs <= 1000):
        parser.error("invalid comparison duration/sample count")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, GOMAXPROCS="2", GOTOOLCHAIN="go1.26.3", GOFLAGS="",
               GOGC="100", GOMEMLIMIT="off")
    records, archive_sizes, starts = {}, {}, {p: [] for p in PROFILES}
    result = 0
    try:
        for profile in PROFILES:
            records[profile], archive_sizes[profile] = extract(getattr(args, profile), output / profile, profile)
        for key in ("source_sha256", "go_version", "version", "source_commit"):
            if len({r[key] for r in records.values()}) != 1:
                raise RuntimeError("comparison inputs differ: " + key)
        save(output / "manifest.json", {"format": "tack-protection-comparison-v1", "binaries": records,
             "platform": platform.platform(), "fixture_go": subprocess.check_output(["go", "version"], env=env, text=True).strip(),
             "fixture_sources": {str(p.relative_to(ROOT)): sha256(p) for p in sorted((ROOT / "management/integration").glob("*.go"))},
             "seconds_per_phase": args.seconds, "repeats_per_http": args.repeats,
             "severe_repeats_per_http": args.severe_repeats, "severe_budget_seconds": args.severe_seconds,
             "gomaxprocs": 2, "gogc": 100, "gomemlimit": "off",
             "startup_method": "fresh process -version; page cache NOT dropped; profiles rotated each round",
             "resource_method": "separate non-race Node; excludes controller/proxy/command child"})
        subprocess.run(["go", "test", "-c", "-o", str(output / "integration.test"), "."],
                       cwd=ROOT / "management/integration", env=env, check=True)
        for n in range(args.startup_runs):
            order = PROFILES[n % 3:] + PROFILES[:n % 3]
            for profile in order:
                # Apply the same runtime settings to each fresh process.
                previous = {k: os.environ.get(k) for k in ("GOMAXPROCS", "GOGC", "GOMEMLIMIT")}
                os.environ.update({k: env[k] for k in previous})
                try:
                    starts[profile].append(startup(output / profile / "tack-node", records[profile]["version"]))
                finally:
                    for key, value in previous.items():
                        if value is None:
                            os.environ.pop(key, None)
                        else:
                            os.environ[key] = value
        save(output / "startup.json", starts)
        for kind, repeats in (("footprint", args.repeats), ("severe", args.severe_repeats)):
            for http in (0, 1):
                for n in range(repeats):
                    order = PROFILES[n % 3:] + PROFILES[:n % 3]
                    for profile in order:
                        name = f"{profile}-h{http}-{kind}-{n + 1}"
                        test_env = dict(env, TACK_NODE_BINARY=str(output / profile / "tack-node"),
                            TACK_NODE_BUILD_RECORD=str(output / profile / "BUILD.json"), TACK_RESOURCE_HTTP2=str(http),
                            TACK_RESOURCE_MAX_OUTPUT="1", TACK_RESOURCE_SECONDS=str(args.seconds),
                            TACK_RESOURCE_REPORT=str(output / (name + ".json")),
                            TACK_SEVERE_SECONDS=str(args.severe_seconds), TACK_SEVERE_REQUIRE_COMPLETE="1",
                            TACK_SEVERE_REPORT=str(output / (name + ".json")))
                        test = "TestNodeLowFootprint" if kind == "footprint" else "TestNodeSevereLink"
                        with (output / (name + ".log")).open("w") as log:
                            code = subprocess.run([str(output / "integration.test"), "-test.run", "^" + test + "$",
                                                   "-test.count=1", "-test.timeout=8m", "-test.v"],
                                                  env=test_env, stdout=log, stderr=subprocess.STDOUT).returncode
                        result = max(result, int(code != 0))
                        print(name, "PASS" if code == 0 else "FAIL", flush=True)
        summarize(output, records, archive_sizes, starts)
    except BaseException:
        result = 1
        raise
    finally:
        (output / "exit-status.txt").write_text(str(result) + "\n")
        files = sorted(p for p in output.rglob("*") if p.is_file() and p.name != "SHA256SUMS")
        (output / "SHA256SUMS").write_text("".join(f"{sha256(p)}  {p.relative_to(output)}\n" for p in files))
    return result


if __name__ == "__main__":
    raise SystemExit(main())
