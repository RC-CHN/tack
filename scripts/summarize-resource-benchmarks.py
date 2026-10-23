#!/usr/bin/env python3
"""Summarize raw comparison evidence; never infer a pass from missing reports."""
import json
import pathlib
import re
import statistics
import sys


def stats(values):
    return {"median": statistics.median(values), "min": min(values),
            "max": max(values), "samples": len(values)}


def benchmarks(directory):
    collected = {}
    for path in sorted(directory.glob("*-bench.txt")):
        text = path.read_text()
        if not text.rstrip().endswith("PASS"):
            raise ValueError(f"incomplete benchmark: {path}")
        for line in text.splitlines():
            if not line.startswith("Benchmark"):
                continue
            parts = line.split()
            name = re.sub(r"-\d+$", "", parts[0])
            if int(parts[1]) <= 0 or len(parts[2:]) % 2:
                raise ValueError(f"invalid benchmark line: {line}")
            metrics = collected.setdefault(name, {})
            for value, unit in zip(parts[2::2], parts[3::2]):
                metrics.setdefault(unit, []).append(float(value))
    return {name: {unit: stats(values) for unit, values in metrics.items()}
            for name, metrics in collected.items()}


def footprint(directory):
    if (directory / "exit-status.txt").read_text().strip() != "0":
        raise ValueError("footprint comparison did not pass")
    manifest = json.loads((directory / "manifest.json").read_text())
    repeats = manifest["repeats_per_http_version"]
    output = {}
    for h2 in (0, 1):
        for label in ("before", "after"):
            runs = []
            for index in range(1, repeats + 1):
                path = directory / f"{label}-h2-{h2}-round-{index}.json"
                report = json.loads(path.read_text())
                if not report["passed"] or report["http2"] != bool(h2):
                    raise ValueError(f"failed/mismatched footprint: {path}")
                if (report["seconds_per_phase"] != manifest["seconds_per_phase"]
                        or report["output_bytes_per_task"] != manifest["output_bytes_per_task"]):
                    raise ValueError(f"mismatched workload: {path}")
                values = {"recovery_ms": report["recovery_to_three_results_ms"],
                          "shutdown_ms": report["shutdown_ms"]}
                phases = report["phases"]
                if [p["name"] for p in phases] != ["idle", "upload_blocked", "offline", "recovered_idle"]:
                    raise ValueError(f"incomplete phases: {path}")
                values["peak_mib"] = max(max(s["process"]["rss_bytes"], s["process"]["peak_rss_bytes"])
                                         for p in phases for s in p["samples"]) / 1048576
                for phase in phases:
                    name = phase["name"]
                    start, end = phase["samples"][0], phase["samples"][-1]
                    p, q = start["process"], end["process"]
                    values[name + "/rss_end_mib"] = q["rss_bytes"] / 1048576
                    values[name + "/cpu_s"] = (q["cpu_ticks"] - p["cpu_ticks"]) / report["clock_ticks_per_second"]
                    for field in ("read_syscall_bytes", "storage_write_bytes", "write_syscall_bytes"):
                        values[name + "/" + field] = q[field] - p[field]
                    values[name + "/fds_end"] = q["file_descriptors"]
                    values[name + "/logs_bytes"] = end["diagnostic_bytes"]
                    values[name + "/upload_failures"] = end["upload_failures"] - start["upload_failures"]
                    network = ("upstream_stream_bytes", "downstream_stream_bytes", "accepted_connections")
                    for field in network:
                        values[name + "/" + field] = end["network"][field] - start["network"][field]
                runs.append(values)
            output[f"http{2 if h2 else 1}/{label}"] = {key: stats([r[key] for r in runs]) for key in runs[0]}
    return output


def main():
    directory = pathlib.Path(sys.argv[1])
    before, after = benchmarks(directory / "before"), benchmarks(directory / "after")
    shared = sorted(before.keys() & after.keys())
    if not shared:
        raise ValueError("no matched benchmarks")
    paired = {}
    for name in shared:
        for unit in before[name].keys() & after[name].keys():
            if min(before[name][unit]["samples"], after[name][unit]["samples"]) < 5:
                raise ValueError(f"fewer than five repetitions: {name}/{unit}")
        paired[name] = {"before": before[name], "after": after[name]}
    report = {"format": "tack-resource-optimization-v1", "benchmarks": paired,
              "unpaired_benchmarks": sorted(before.keys() ^ after.keys()),
              "footprint": footprint(directory / "process")}
    print(json.dumps(report, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
