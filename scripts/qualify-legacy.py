#!/usr/bin/env python3
"""Qualify exact Modern/Legacy archives: recovery, resource bounds and old kernel."""
import argparse
import os
from pathlib import Path
import platform
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--fixture-dir", type=Path, required=True)
    parser.add_argument("--download", action="store_true", help="allow checksum-pinned kernel fixture downloads")
    args = parser.parse_args()
    if platform.system() != "Linux" or platform.machine() not in ("x86_64", "amd64"):
        parser.error("old-kernel qualification currently requires a Linux amd64 build host")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, GOTOOLCHAIN="go1.26.3", GOMAXPROCS="2")

    def run(script, *flags):
        subprocess.run([sys.executable, "-B", str(ROOT / "scripts" / script), *map(str, flags)],
                       cwd=ROOT, env=env, check=True, timeout=600)

    for runtime in ("modern", "legacy"):
        run("verify-release.py", args.artifacts.resolve(), "--node-runtime", runtime, "--extract-native", output / runtime)
    run("smoke-unified.py", "--bin-dir", output / "legacy", "--report", output / "unified.json")
    for initial, final in (("modern", "legacy"), ("legacy", "modern")):
        run("stress-management.py", "--bin-dir", output / final, "--initial-node-bin", output / initial / "tack-node",
            "--profile", "defaults", "--http", "1", "--idle-seconds", "0", "--timeout", "180",
            "--output", output / f"state-{initial}-to-{final}.json")
    run("smoke-legacy-kernel.py", "--bin-dir", output / "legacy", "--fixture-dir", args.fixture_dir.resolve(),
        "--report", output / "kernel.json", *(["--download"] if args.download else []))
    for http in ("0", "1"):
        resource_env = dict(env, TACK_NODE_BINARY=str(output / "legacy/tack-node"),
                            TACK_NODE_BUILD_RECORD=str(output / "legacy/tack-node.BUILD.json"),
                            TACK_RESOURCE_HTTP2=http, TACK_RESOURCE_SECONDS="5", TACK_RESOURCE_MAX_OUTPUT="1",
                            TACK_RESOURCE_REPORT=str(output / f"footprint-{http}.json"),
                            TACK_SEVERE_SECONDS="120", TACK_SEVERE_REQUIRE_COMPLETE="1",
                            TACK_SEVERE_REPORT=str(output / f"severe-{http}.json"))
        with (output / f"resources-{http}.log").open("w") as log:
            subprocess.run(["go", "test", "-run", "^TestNode(LowFootprint|SevereLink)$", "-count=1", "-timeout=8m", "-v", "."],
                           cwd=ROOT / "management/integration", env=resource_env, check=True, timeout=500,
                           stdout=log, stderr=subprocess.STDOUT)
    print("Exact Legacy artifact passed kernel, state migration, resources and intermittent-link gates")


if __name__ == "__main__":
    main()
