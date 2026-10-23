"""Recompute descriptive statistics from the retained raw observations."""
import json
import pathlib
import re
import statistics

ROOT = pathlib.Path(__file__).resolve().parent


def stats(values):
    return {"n": len(values), "min": min(values), "median": statistics.median(values), "max": max(values)}


def transfer(path):
    data = json.loads(path.read_text())
    start, end = data["network_start"], data["network_end"]
    return {
        "file": str(path.relative_to(ROOT)),
        "completed": data["completed"], "passed": data["passed"],
        "seconds": data["elapsed_seconds"], "budget_seconds": data["budget_seconds"],
        "progress_bytes": data["progress_bytes"], "output_bytes": data["output_bytes"],
        "stream_bytes": sum(end[k] - start[k] for k in ("upstream_stream_bytes", "downstream_stream_bytes")),
        "max_rss_mib": data["max_rss_bytes"] / 2**20, "max_fds": data["max_fds"],
        "requests": data["upload_requests"], "outages": data["outages"],
        "shutdown_ms": data["shutdown_ms"], "log_bytes": data["log_bytes"],
    }


def campaign(folder, count):
    result = {}
    for side in ("before", "after"):
        runs = [transfer(p) for p in sorted((ROOT / folder).glob(side + "-*.json"))]
        assert len(runs) == count, (folder, side, len(runs), count)
        result[side] = {
            "runs": runs, "completed": sum(r["completed"] for r in runs),
            "metrics": {k: stats([r[k] for r in runs]) for k in (
                "seconds", "progress_bytes", "stream_bytes", "max_rss_mib", "max_fds", "requests", "shutdown_ms")},
        }
    return result


summary = {"campaigns": {}}
for name, count in (("mixed-h1", 3), ("mixed-h2", 3), ("loss-h1", 1), ("loss-h2", 1)):
    summary["campaigns"][name] = campaign("process/" + name, count)
summary["extended_h2_diagnostic"] = campaign("diagnostic/loss-h2-180", 1)
summary["footprint"] = []
for path in sorted((ROOT / "resource").glob("*.json")):
    data = json.loads(path.read_text())
    phases = []
    for phase in data["phases"]:
        samples = phase["samples"]
        first, last = samples[0], samples[-1]
        phases.append({
            "name": phase["name"],
            "max_rss_mib": max(max(s["process"]["rss_bytes"], s["process"]["peak_rss_bytes"]) for s in samples) / 2**20,
            "cpu_seconds": (last["process"]["cpu_ticks"] - first["process"]["cpu_ticks"]) / data["clock_ticks_per_second"],
            "storage_writes": last["process"]["storage_write_bytes"] - first["process"]["storage_write_bytes"],
            "stream_bytes": sum(last["network"][k] - first["network"][k] for k in ("upstream_stream_bytes", "downstream_stream_bytes")),
            "log_bytes": last["diagnostic_bytes"] - first["diagnostic_bytes"],
            "fds": [min(s["process"]["file_descriptors"] for s in samples), max(s["process"]["file_descriptors"] for s in samples)],
        })
    summary["footprint"].append({"http2": data["http2"], "passed": data["passed"], "phases": phases, "shutdown_ms": data["shutdown_ms"]})
summary["handshake"] = {}
for side in ("before", "after"):
    rows = []
    for line in (ROOT / side / "handshake-bench.txt").read_text().splitlines():
        match = re.match(r"BenchmarkStalledHandshake-\d+\s+(\d+)\s+(\d+) ns/op\s+([\d.]+) watchdog-expirations", line)
        if match:
            rows.append({"iterations": int(match[1]), "ms_per_op": int(match[2]) / 1e6, "watchdog_expirations": float(match[3])})
    assert len(rows) == 3
    summary["handshake"][side] = {"runs": rows, "ms_per_op": stats([r["ms_per_op"] for r in rows])}
(ROOT / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
