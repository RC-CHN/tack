#!/usr/bin/env python3
"""Verify one final Node provides shell tasks and a verified-TLS Chisel tunnel."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import socket
import socketserver
import subprocess
import tempfile
import threading
import time


def port():
    with socket.socket() as stream:
        stream.bind(("127.0.0.1", 0))
        return stream.getsockname()[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bin-dir", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    binaries = {name: str((args.bin_dir / name).resolve())
                for name in ("tack", "tack-node", "tack-control", "chisel")}
    report = {"passed": False, "binary_sha256": {
        name: hashlib.sha256(Path(path).read_bytes()).hexdigest() for name, path in binaries.items()}}
    processes = []
    env = dict(os.environ, GOMAXPROCS="2")

    class Echo(socketserver.BaseRequestHandler):
        def handle(self):
            self.request.settimeout(5)
            while data := self.request.recv(65536):
                self.request.sendall(data)

    with tempfile.TemporaryDirectory(prefix="tack-unified-") as directory:
        root = Path(directory)
        with (root / "servers.log").open("w") as server_log, (root / "node.log").open("w") as node_log:
            def launch(name, *flags):
                process = subprocess.Popen([binaries[name], *map(str, flags)], env=env,
                                           stdout=node_log if name == "tack-node" else server_log,
                                           stderr=node_log if name == "tack-node" else server_log)
                processes.append(process)
                return process

            def stop(process):
                graceful = True
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=8)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
                        graceful = False
                processes.remove(process)
                return graceful

            def wait(check, seconds=25):
                deadline = time.monotonic() + seconds
                while time.monotonic() < deadline:
                    if any(p.poll() is not None for p in processes):
                        raise RuntimeError("unified fixture process exited unexpectedly")
                    try:
                        value = check()
                        if value:
                            return value
                    except (OSError, subprocess.SubprocessError):
                        pass
                    time.sleep(0.1)
                raise RuntimeError("unified fixture timed out")

            def cli(*flags):
                return subprocess.check_output([binaries["tack"], *map(str, flags)], env=env,
                                               stderr=subprocess.PIPE, timeout=15)

            try:
                cert, key = root / "server.pem", root / "server.key"
                subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-sha256", "-nodes", "-days", "1",
                                "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1,DNS:localhost",
                                "-keyout", str(key), "-out", str(cert)], check=True, timeout=20,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                subprocess.run([binaries["chisel"], "server", "--keygen", str(root / "ssh.key")],
                               env=env, check=True, timeout=15, stdout=server_log, stderr=server_log)
                control_port, admin_port, tunnel_port, forward_port = port(), port(), port(), port()
                auth = "node:" + secrets.token_hex(16)
                # Credentials are file-based, even in this disposable local fixture.
                (root / "tunnel-users.json").write_text(json.dumps({auth: ["127.0.0.1:.*"]}))
                tunnel_flags = ["server", "--host", "127.0.0.1", "--port", tunnel_port,
                                "--keyfile", root / "ssh.key", "--authfile", root / "tunnel-users.json",
                                "--tls-cert", cert, "--tls-key", key]
                tunnel_server = launch("chisel", *tunnel_flags)

                def fingerprint():
                    match = re.search(r"Fingerprint (\S+)", (root / "servers.log").read_text())
                    return match.group(1) if match else None

                fp = wait(fingerprint)
                state, work = root / "state", root / "work"
                state.mkdir()
                work.mkdir()
                with socketserver.ThreadingTCPServer(("127.0.0.1", 0), Echo) as echo:
                    echo.daemon_threads = True
                    echo_thread = threading.Thread(target=echo.serve_forever, daemon=True)
                    echo_thread.start()
                    try:
                        (root / "tunnel.json").write_text(json.dumps({
                            "server": f"https://127.0.0.1:{tunnel_port}", "auth": auth,
                            "fingerprint": fp, "ca_file": str(cert), "keep_alive_seconds": 0,
                            "remotes": [f"127.0.0.1:{forward_port}:127.0.0.1:{echo.server_address[1]}"]}))
                        cli("provision", "-config", root / "control.json", "-node-config", root / "node.json",
                            "-node", "unified-test", "-server", f"https://127.0.0.1:{control_port}",
                            "-state-dir", state, "-work-dir", work, "-node-ca", cert,
                            "-tunnel-config", root / "tunnel.json")
                        launch("tack-control", "-config", root / "control.json", "-state", root / "tasks.json",
                               "-listen", f"127.0.0.1:{control_port}", "-admin-listen", f"127.0.0.1:{admin_port}",
                               "-tls-cert", cert, "-tls-key", key)
                        node = launch("tack-node", "-config", root / "node.json")
                        common = ["-server", f"http://127.0.0.1:{admin_port}", "-config", root / "control.json", "-allow-insecure-loopback"]
                        wait(lambda: json.loads(cli("nodes", *common)))

                        def task(marker):
                            (root / "task.json").write_text(json.dumps({"node_id": "unified-test", "command": {
                                "mode": "posix-sh", "script": "printf " + marker, "timeout_seconds": 5, "output_limit": 4096}}))
                            submitted = json.loads(cli("submit", *common, "-file", root / "task.json"))

                            def complete():
                                result = json.loads(cli("get", *common, "-id", submitted["id"]))
                                return result if result["state"] == "succeeded" and not result.get("result_pending") else None

                            result = wait(complete)
                            if base64.b64decode(result["result"]["stdout"]) != marker.encode():
                                raise RuntimeError("unified command output corrupted")

                        payload = bytes(range(256)) * 1024

                        def transfer():
                            with socket.create_connection(("127.0.0.1", forward_port), timeout=3) as stream:
                                stream.sendall(payload)
                                received = bytearray()
                                while len(received) < len(payload):
                                    chunk = stream.recv(65536)
                                    if not chunk:
                                        return False
                                    received.extend(chunk)
                                return bytes(received) == payload

                        wait(transfer)
                        task("unified-command-ok")
                        stop(tunnel_server)
                        task("command-during-tunnel-outage-ok")
                        tunnel_server = launch("chisel", *tunnel_flags)
                        wait(transfer)
                        if node.poll() is not None:
                            raise RuntimeError("Node restarted during tunnel recovery")
                        # Task subprocesses have completed; no Chisel client child is needed.
                        proc_tasks = Path(f"/proc/{node.pid}/task")
                        if proc_tasks.exists():
                            if any(p.read_text().strip() for p in proc_tasks.glob("*/children")):
                                raise RuntimeError("client retained an unexpected child process")
                            status = Path(f"/proc/{node.pid}/status").read_text()
                            match = re.search(r"VmHWM:\s+(\d+) kB", status)
                            if match:
                                report["observed_peak_rss_kib"] = int(match.group(1))
                        graceful = stop(node)
                        if not graceful or node.returncode != 0:
                            raise RuntimeError("unified Node failed graceful shutdown")
                        with socket.socket() as released:
                            released.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                            released.bind(("127.0.0.1", forward_port))
                        if (root / "node.log").stat().st_size:
                            raise RuntimeError("default unified Node diagnostics were not quiet")
                        report.update(passed=True, client_processes=1, client_pid=node.pid,
                                      verified_echo_bytes=2*len(payload), tasks_completed=2,
                                      management_during_tunnel_outage=True, tunnel_reconnected=True,
                                      graceful_shutdown=True, listener_released=True, default_diagnostics_bytes=0)
                    finally:
                        echo.shutdown()
                        echo_thread.join(timeout=5)
            finally:
                for process in list(reversed(processes)):
                    stop(process)
                args.report.write_text(json.dumps(report, indent=2) + "\n")
    print("One final Node passed shell, tunnel, outage, reconnect and shutdown checks")


if __name__ == "__main__":
    main()
