#!/usr/bin/env python3
"""Linux localhost resource stress against separate Node/Controller processes.

Requires Python 3, OpenSSL, and tack-control/tack-node binaries.
Creates disposable state, uses real TLS, and never contacts remote nodes.
Reports observed peaks, not a resource ceiling. See --help for workload choices.
"""
import argparse
import base64
import collections
import concurrent.futures
import hashlib
import http.client
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time

MIB = 1024 * 1024
HELPER = '''import json, os, resource, sys
marker, label, count = sys.argv[1:]
with open(marker, "a") as f:
    f.write(label + "\\n")
for fd, char in ((1, b"A"), (2, b"B")):
    remaining = int(count)
    chunk = char * 65536
    while remaining:
        n = os.write(fd, chunk[:min(remaining, len(chunk))])
        remaining -= n
usage = resource.getrusage(resource.RUSAGE_SELF)
status = dict(line.split(":", 1) for line in open("/proc/self/status").read().splitlines())
with open(marker + ".resources", "a") as f:
    f.write(json.dumps({"label": label, "hwm_bytes": int(status["VmHWM"].split()[0]) * 1024,
                       "pre_exec_inclusive_hwm_bytes": usage.ru_maxrss * 1024,
                       "cpu_seconds": usage.ru_utime + usage.ru_stime}) + "\\n")
'''


def write_json(path, data):
    path.write_text(json.dumps(data, indent=2) + "\n")
    path.chmod(0o600)


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def proc_sample(pid):
    root = Path("/proc") / str(pid)
    status = dict(line.split(":", 1) for line in (root / "status").read_text().splitlines())
    fields = (root / "stat").read_text().rsplit(") ", 1)[1].split()
    io = dict(line.split(":", 1) for line in (root / "io").read_text().splitlines())
    return {
        "rss_bytes": int(status.get("VmRSS", "0").split()[0]) * 1024,
        "hwm_bytes": int(status.get("VmHWM", "0").split()[0]) * 1024,
        "fds": len(list((root / "fd").iterdir())),
        "threads": int(status["Threads"]),
        "cpu_seconds": (int(fields[11]) + int(fields[12])) / os.sysconf("SC_CLK_TCK"),
        "write_bytes": int(io["write_bytes"]),
    }


def children_rss(pid):
    total = 0
    for entry in (Path("/proc") / str(pid) / "task").glob("*/children"):
        try:
            for child in entry.read_text().split():
                total += proc_sample(int(child))["rss_bytes"] + children_rss(int(child))
        except (OSError, KeyError, ValueError):
            pass  # A command can exit between discovery and sampling.
    return total


def disk_sample(path):
    logical = allocated = files = 0
    for root, dirs, names in os.walk(path):
        for name in ["."] + names:
            try:
                s = (Path(root) / name).stat()
                allocated += s.st_blocks * 512
                if name != ".":
                    logical += s.st_size
                    files += 1
            except FileNotFoundError:
                pass  # Atomic replacement / retention during the sample.
    return {"logical_bytes": logical, "allocated_bytes": allocated, "files": files}


class Monitor:
    def __init__(self):
        self.processes = {}
        self.directories = {}
        self.summary = {}
        self.disk = {}
        self.samples = []
        self.phase = "startup"
        self.error = None
        self.stop = threading.Event()
        self.lock = threading.Lock()
        self.thread = threading.Thread(target=self.run, daemon=True)
        self.thread.start()

    def register(self, name, process, path):
        with self.lock:
            self.processes[name] = process
            self.directories[name] = path

    def run(self):
        previous = {}
        last_slow = 0
        while not self.stop.wait(0.05):
            now = time.monotonic()
            slow = now - last_slow >= 1
            live = {}
            with self.lock:
                processes = list(self.processes.items())
                directories = list(self.directories.items())
            for name, process in processes:
                try:
                    value = proc_sample(process.pid)
                except (OSError, KeyError, ValueError):
                    continue
                live[name] = value
                high = self.summary.setdefault(name, {})
                for key, item in value.items():
                    high[key] = max(high.get(key, 0), item)
                if slow:
                    child = children_rss(process.pid)
                    high["sampled_children_rss_bytes"] = max(high.get("sampled_children_rss_bytes", 0), child)
                    old_time, old_cpu = previous.get(name, (process.started_monotonic, 0))
                    if now - old_time >= 1:
                        percent = 100 * (value["cpu_seconds"] - old_cpu) / (now - old_time)
                        high["cpu_core_percent_1s_max"] = max(high.get("cpu_core_percent_1s_max", 0), percent)
                        previous[name] = now, value["cpu_seconds"]
            if sum(s["rss_bytes"] for s in live.values()) > 4 * 1024 * MIB:
                self.error = "4 GiB aggregate RSS test safety limit exceeded"
            # Disk is sampled at 50 ms as well; brief atomic-write overlap can
            # still be missed, so this is not a filesystem-space upper bound.
            current_disk = {}
            for name, path in directories:
                current_disk[name] = disk_sample(path)
                high = self.disk.setdefault(name, {})
                for key, value in current_disk[name].items():
                    high[key] = max(high.get(key, 0), value)
            # Restarted nodes share a directory; count each path once here.
            unique = {str(path): current_disk[name]["allocated_bytes"] for name, path in directories}
            if sum(unique.values()) > 4 * 1024 * MIB:
                self.error = "4 GiB allocated test-state safety limit exceeded"
            if slow:
                self.samples.append({"monotonic": now, "phase": self.phase, "processes": live, "disk": current_disk})
                last_slow = now


class Workload:
    def __init__(self, args, base):
        self.args, self.base = args, base
        self.monitor = Monitor()
        self.processes = []
        self.nodes = []
        self.admin_port, self.node_port = free_port(), free_port()
        self.admin_token = secrets.token_hex(32)
        self.tls_env = dict(os.environ)
        # Do not accidentally inherit tuning from the shell running the test.
        for key in ("GOMEMLIMIT", "GOGC", "GOMAXPROCS", "GODEBUG"):
            self.tls_env.pop(key, None)
        if args.http == "1":
            self.tls_env["GODEBUG"] = "http2client=0,http2server=0"
        self.report = {"profile": args.profile, "http": args.http,
                       "http_mode": "HTTP/1.1 forced" if args.http == "1" else "HTTP/2 enabled with normal TLS negotiation",
                       "driver_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                       "started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                       "platform": os.uname()._asdict() if hasattr(os.uname(), "_asdict") else list(os.uname()),
                       "cpu_affinity_count": len(os.sched_getaffinity(0)),
                       "sampling_seconds": 0.05, "cpu_window_seconds": 1,
                       "binary_sha256": {}, "checkpoints": {}, "checks": {}}
        for binary in ("tack-node", "tack-control"):
            path = args.bin_dir / binary
            self.report["binary_sha256"][binary] = hashlib.sha256(path.read_bytes()).hexdigest()
        if args.initial_node_bin:
            self.report["binary_sha256"]["initial-tack-node"] = hashlib.sha256(args.initial_node_bin.read_bytes()).hexdigest()
        self.started = time.monotonic()

    def check(self):
        if self.monitor.error:
            raise RuntimeError(self.monitor.error)
        for p in self.processes:
            if getattr(p, "expected_exit", False):
                continue
            if p.poll() is not None:
                raise RuntimeError(f"test process exited unexpectedly: {p.args[0]} status={p.returncode}")
        if time.monotonic() - self.started > self.args.timeout:
            raise TimeoutError("workload deadline exceeded")

    def pause(self, seconds):
        until = time.monotonic() + seconds
        while time.monotonic() < until:
            self.check()
            time.sleep(min(0.1, max(0, until - time.monotonic())))

    def phase(self, name):
        self.monitor.phase = name
        print(f"{self.args.profile}: {name}", flush=True)

    def launch(self, binary, flags, name, directory):
        executable = self.args.bin_dir / binary
        if binary == "tack-node" and self.args.initial_node_bin and not name.endswith("-restart"):
            executable = self.args.initial_node_bin
        p = subprocess.Popen([str(executable)] + flags,
                             env=self.tls_env, stdout=subprocess.DEVNULL,
                             stderr=subprocess.DEVNULL, start_new_session=True)
        p.started_monotonic = time.monotonic()
        self.processes.append(p)
        self.monitor.register(name, p, directory)
        return p

    def setup(self):
        (self.base / "helper.py").write_text(HELPER)
        cert, key = self.base / "cert.pem", self.base / "key.pem"
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                        "-keyout", str(key), "-out", str(cert), "-days", "1",
                        "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1"],
                       check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        count = 8 if self.args.profile == "fanout" else 1
        credentials = [{"id": f"stress-{i}", "token": secrets.token_hex(32)} for i in range(count)]
        control = {"admin_token": self.admin_token, "nodes": credentials}
        if self.args.profile == "fanout":
            control.update(max_history_tasks=64, max_history_bytes=256 * MIB)
        write_json(self.base / "control.json", control)
        cdir = self.base / "controller"
        cdir.mkdir()
        self.launch("tack-control", ["-config", str(self.base / "control.json"), "-state", str(cdir / "tasks.json"),
                    "-listen", f"127.0.0.1:{self.node_port}", "-admin-listen", f"127.0.0.1:{self.admin_port}",
                    "-tls-cert", str(cert), "-tls-key", str(key), "-log-file", str(cdir / "runtime.log")], "controller", cdir)
        for _ in range(100):
            try:
                if self.request("GET", "/health")[0] == 200:
                    break
            except OSError:
                self.pause(0.1)
        else:
            raise RuntimeError("controller did not start")
        for credential in credentials:
            root = self.base / credential["id"]
            root.mkdir()
            for directory in ("state", "work"):
                (root / directory).mkdir()
            cfg = {"server": f"https://127.0.0.1:{self.node_port}", "node_id": credential["id"],
                   "token": credential["token"], "ca_file": str(cert),
                   "state_dir": str(root / "state"), "work_dir": str(root / "work")}
            if self.args.profile != "fanout":
                cfg["upload_bytes_per_second"] = 1
            if self.args.profile == "max-backlog":
                cfg.update(max_pending_results=128, max_pending_result_bytes=128 * MIB)
            config_path = self.base / (credential["id"] + ".json")
            write_json(config_path, cfg)
            flags = ["-config", str(config_path), "-log-file", str(root / "runtime.log")]
            proc = self.launch("tack-node", flags, credential["id"], root)
            self.nodes.append({"id": credential["id"], "root": root, "cfg": cfg,
                               "config_path": config_path, "flags": flags, "process": proc})
        self.report["workload"] = {"nodes": count, "history_tasks": control.get("max_history_tasks", 100000),
                                   "history_bytes": control.get("max_history_bytes", 1024*MIB),
                                   "pending_results": self.nodes[0]["cfg"].get("max_pending_results", 8),
                                   "pending_bytes": self.nodes[0]["cfg"].get("max_pending_result_bytes", 8*MIB),
                                   "generated_bytes_per_stream": 8*MIB,
                                   "output_limit_per_stream": 65536 if self.args.profile == "defaults" else MIB}
        deadline = time.monotonic() + 30
        while True:
            code, data = self.request("GET", "/v1/nodes")
            if code == 200 and len(data) == count:
                break
            if time.monotonic() > deadline:
                raise RuntimeError("nodes did not register")
            self.pause(0.1)
        self.phase("idle-baseline")
        self.pause(5)
        self.checkpoint("baseline")

    def request(self, method, path, payload=None, decode=True):
        conn = http.client.HTTPConnection("127.0.0.1", self.admin_port, timeout=30)
        try:
            body = json.dumps(payload).encode() if payload is not None else None
            conn.request(method, path, body=body, headers={"Authorization": "Bearer " + self.admin_token,
                                                         "Content-Type": "application/json"})
            response = conn.getresponse()
            if decode:
                data = response.read()
                return response.status, json.loads(data) if data else None
            while response.read(65536):
                pass
            return response.status, None
        finally:
            conn.close()

    def checkpoint(self, name):
        snapshot = {"processes": {}, "disk": {}, "journals": {}}
        for key, process in self.monitor.processes.items():
            if process.poll() is None:
                snapshot["processes"][key] = proc_sample(process.pid)
        for key, path in self.monitor.directories.items():
            snapshot["disk"][key] = disk_sample(path)
        for node in self.nodes:
            journal = node["root"] / "state" / "journal.json"
            if journal.exists():
                snapshot["journals"][node["id"]] = journal.stat().st_size
        snapshot["metrics"] = self.request("GET", "/v1/metrics")[1]
        self.report["checkpoints"][name] = snapshot

    def submit(self, node, label):
        limit = self.report["workload"]["output_limit_per_stream"]
        payload = {"node_id": node["id"], "command": {
            "mode": "exec", "program": sys.executable,
            "args": [str(self.base / "helper.py"), str(node["root"] / "executions"), label, str(8*MIB)],
            "timeout_seconds": 120, "output_limit": limit}}
        while True:
            self.check()
            code, result = self.request("POST", "/v1/tasks", payload)
            if code == 503:
                self.pause(0.1)
                continue
            if code != 201:
                raise RuntimeError(f"submit status {code}: {result}")
            return result["id"]

    def wait_result(self, task_id):
        while True:
            self.check()
            code, task = self.request("GET", "/v1/tasks/" + task_id)
            if code == 503:
                self.pause(0.05)
                continue
            if code != 200:
                raise RuntimeError(f"get status {code}")
            if task["state"] in ("succeeded", "failed", "timed_out", "unknown", "canceled") and not task.get("result_pending"):
                if task["state"] != "succeeded" or task["result"].get("exit_code") != 0:
                    raise RuntimeError(f"unsuccessful command: {task['state']}")
                for stream, char in (("stdout", b"A"), ("stderr", b"B")):
                    value = base64.b64decode(task["result"][stream], validate=True)
                    if value != char * self.report["workload"]["output_limit_per_stream"] or not task["result"][stream+"_truncated"]:
                        raise RuntimeError("retained output did not match expected bytes/truncation")
                return
            self.pause(0.05)

    def execution_labels(self, node):
        marker = node["root"] / "executions"
        return marker.read_text().splitlines() if marker.exists() else []

    def stop_process(self, process):
        process.expected_exit = True
        process.send_signal(signal.SIGTERM)
        try:
            process.wait(15)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
            raise RuntimeError("process required forced termination")
        if process.returncode != 0:
            raise RuntimeError(f"process shutdown status {process.returncode}")

    def backlog(self):
        node = self.nodes[0]
        capacity = {"defaults": 8, "large-output": 4, "max-backlog": 64}[self.args.profile]
        total = capacity + 2
        self.phase("fill-backlog-at-1-byte-per-second")
        ids = [self.submit(node, str(i)) for i in range(total)]
        while len(self.execution_labels(node)) < capacity:
            self.pause(0.2)
        # Wait until the last command's output is persisted and advertised.
        while True:
            _, metrics = self.request("GET", "/v1/metrics")
            if metrics["nodes"][0]["metrics"]["pending_results"] == capacity:
                break
            self.pause(0.2)
        self.phase("hold-saturated-backlog")
        self.pause(10)
        if len(self.execution_labels(node)) != capacity:
            raise RuntimeError("node executed beyond pending-result budget")
        self.checkpoint("saturated")
        self.report["checks"]["backpressure_at_results"] = capacity
        self.phase("restart-and-drain-preserved-results")
        self.stop_process(node["process"])
        node["cfg"]["upload_bytes_per_second"] = 0
        write_json(node["config_path"], node["cfg"])
        node["process"] = self.launch("tack-node", node["flags"], node["id"]+"-restart", node["root"])
        for i, task_id in enumerate(ids):
            self.wait_result(task_id)
            if (i+1) % 8 == 0:
                print(f"{self.args.profile}: verified {i+1}/{total} results", flush=True)
        labels = self.execution_labels(node)
        if collections.Counter(labels) != collections.Counter(str(i) for i in range(total)):
            raise RuntimeError("missing or repeated command execution")
        self.report["checks"].update(verified_results=total, executions=total, duplicate_executions=0)

    def fanout(self):
        recent = collections.deque(maxlen=16)
        lock = threading.Lock()
        stop = threading.Event()
        statuses = collections.Counter()
        self.phase("eight-nodes-and-sixteen-history-readers")

        def reader(index):
            while not stop.is_set():
                with lock:
                    task_id = recent[index % len(recent)] if recent else None
                if task_id:
                    status, _ = self.request("GET", "/v1/tasks/"+task_id, decode=False)
                    with lock:
                        statuses[str(status)] += 1
                    if status not in (200, 404, 503):
                        raise RuntimeError(f"history read status {status}")
                stop.wait(0.05)

        def worker(node):
            for i in range(self.args.tasks_per_node):
                task_id = self.submit(node, str(i))
                self.wait_result(task_id)
                with lock:
                    recent.append(task_id)
                if (i+1) % 8 == 0:
                    print(f"fanout: {node['id']} verified {i+1}/{self.args.tasks_per_node}", flush=True)

        with concurrent.futures.ThreadPoolExecutor(max_workers=24) as pool:
            readers = [pool.submit(reader, i) for i in range(16)]
            try:
                jobs = [pool.submit(worker, node) for node in self.nodes]
                for job in concurrent.futures.as_completed(jobs):
                    job.result()
            finally:
                stop.set()
            for job in readers:
                job.result()
        self.report["history_read_statuses"] = dict(statuses)
        for node in self.nodes:
            if collections.Counter(self.execution_labels(node)) != collections.Counter(str(i) for i in range(self.args.tasks_per_node)):
                raise RuntimeError("missing or repeated command execution")
        total = len(self.nodes)*self.args.tasks_per_node
        self.report["checks"].update(verified_results=total, executions=total, duplicate_executions=0)

    def finish(self):
        self.phase("drained-idle")
        while True:
            self.check()
            journals = [json.loads((n["root"] / "state" / "journal.json").read_text()) for n in self.nodes]
            if all(not j["records"] for j in journals):
                break
            self.pause(0.2)
        self.checkpoint("drained")
        self.pause(self.args.idle_seconds)
        self.checkpoint("after_idle")
        if any(list((n["root"] / "work").glob("tack-*/task-*")) for n in self.nodes):
            raise RuntimeError("task workspaces remain")
        history = list((self.base / "controller" / "tasks.json.history").rglob("*.json"))
        self.report["checks"]["history_files_after_drain"] = len(history)
        if self.args.profile == "fanout" and len(history) > 64:
            raise RuntimeError("history retention count exceeded")
        self.report["checks"].update(empty_journals=True, empty_task_workspaces=True)
        if any(list((n["root"] / "state" / "results").glob("*")) for n in self.nodes):
            raise RuntimeError("acknowledged result files remain after drain")
        self.report["checks"]["empty_result_spools"] = True
        child_records = [json.loads(line) for node in self.nodes
                         for line in (node["root"] / "executions.resources").read_text().splitlines()]
        self.report["command_children"] = {"count": len(child_records),
                                            "max_individual_hwm_bytes": max(r["hwm_bytes"] for r in child_records),
                                            "hwm_method": "post-exec /proc/self/status VmHWM",
                                            "max_pre_exec_inclusive_hwm_bytes": max(r["pre_exec_inclusive_hwm_bytes"] for r in child_records),
                                            "total_cpu_seconds": sum(r["cpu_seconds"] for r in child_records)}

    def cleanup(self):
        errors = []
        for p in reversed(self.processes):
            if p.poll() is None:
                try:
                    self.stop_process(p)
                except Exception as e:
                    errors.append(str(e))
        self.monitor.stop.set()
        self.monitor.thread.join(5)
        self.report.update(elapsed_seconds=time.monotonic()-self.started,
                           process_high_water=self.monitor.summary, disk_high_water=self.monitor.disk,
                           samples=self.monitor.samples, shutdown_errors=errors)
        if errors:
            self.report["passed"] = False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bin-dir", type=Path, required=True)
    parser.add_argument("--initial-node-bin", type=Path, help="backlog migration test: start with this older Node, then restart with --bin-dir/tack-node")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--profile", choices=("defaults", "large-output", "max-backlog", "fanout"), required=True)
    parser.add_argument("--http", choices=("1", "2"), default="2")
    parser.add_argument("--tasks-per-node", type=int, default=32)
    parser.add_argument("--idle-seconds", type=int, default=30)
    parser.add_argument("--timeout", type=int, default=900)
    args = parser.parse_args()
    if args.initial_node_bin and args.profile == "fanout":
        parser.error("initial-node-bin requires a backlog profile with a restart")
    if not 1 <= args.tasks_per_node <= 128 or not 0 <= args.idle_seconds <= 600:
        parser.error("tasks-per-node must be 1..128 and idle-seconds 0..600")
    if sys.platform != "linux" or shutil.disk_usage(tempfile.gettempdir()).free < 4*1024*MIB:
        parser.error("requires Linux /proc and at least 4 GiB of temporary free space")
    args.bin_dir = args.bin_dir.resolve()
    if args.initial_node_bin:
        args.initial_node_bin = args.initial_node_bin.resolve()
    os.umask(0o077)
    with tempfile.TemporaryDirectory(prefix="tack-stress-") as tmp:
        workload = Workload(args, Path(tmp))
        try:
            workload.setup()
            if args.profile == "fanout":
                workload.fanout()
            else:
                workload.backlog()
            workload.finish()
            workload.report["passed"] = True
        except Exception as e:
            workload.report.update(passed=False, error=str(e))
            # Logs have no command output or credentials; retain only on failure.
            workload.report["diagnostic_logs"] = {str(p.relative_to(tmp)): p.read_text(errors="replace")[-8192:]
                                                   for p in Path(tmp).rglob("runtime.log")}
            print(f"FAILED: {e}", file=sys.stderr, flush=True)
        finally:
            workload.cleanup()
            write_json(args.output, workload.report)
        print(json.dumps({"passed": workload.report["passed"], "report": str(args.output),
                          "process_high_water": workload.report["process_high_water"]}), flush=True)
        return 0 if workload.report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
