#!/usr/bin/env python3
"""Exercise final artifacts; retain separate-relay fixtures for transport experiments."""
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
import sys
import tempfile
import threading
import time


def port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bin-dir", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--obfuscation", action="store_true", help="exercise chrome-133 + bounded-v1 over verified TLS on both paths")
    parser.add_argument("--tls-profile", choices=("chrome-133", "personalized-v1"), default="chrome-133", help="profile used with --obfuscation")
    args = parser.parse_args()
    # Current release archives contain three unified binaries. Browser transport
    # experiments still supply management-only workers and a separate relay;
    # retain their original path instead of giving those workers tunnel config.
    if not (args.bin_dir / "chisel").is_file():
        flags = (["--tls-profile", args.tls_profile, "--traffic-shaping", "bounded-v1"]
                 if args.obfuscation else [])
        subprocess.run([sys.executable, "-B", str(Path(__file__).with_name("smoke-unified.py")),
                        "--bin-dir", str(args.bin_dir), "--report", str(args.report), *flags], check=True)
        return
    binaries = {n: str((args.bin_dir / n).resolve()) for n in ("tack", "tack-node", "tack-control", "chisel")}
    report = {"passed": False, "binary_sha256": {n: hashlib.sha256(Path(p).read_bytes()).hexdigest() for n, p in binaries.items()}}
    report["transport"] = args.tls_profile+"+bounded-v1" if args.obfuscation else "loopback-http"
    processes = []
    env = dict(os.environ, GOMAXPROCS="2")
    with tempfile.TemporaryDirectory(prefix="tack-protected-smoke-") as directory:
        root = Path(directory)
        with (root / "processes.log").open("w") as log:
            def launch(name, *flags):
                p = subprocess.Popen([binaries[name], *map(str, flags)], env=env, stdout=log, stderr=log)
                processes.append(p)
                return p

            def cli(*flags):
                return subprocess.check_output([binaries["tack"], *map(str, flags)], env=env, stderr=subprocess.PIPE, timeout=15)

            def await_value(check, seconds=20):
                deadline = time.monotonic() + seconds
                while time.monotonic() < deadline:
                    if any(p.poll() is not None for p in processes):
                        raise RuntimeError("protected process exited unexpectedly")
                    try:
                        value = check()
                        if value:
                            return value
                    except (OSError, subprocess.SubprocessError):
                        pass
                    time.sleep(0.1)
                raise RuntimeError("protected artifact smoke test timed out")

            try:
                tls_flags = []
                node_extra = {}
                scheme = "http"
                if args.obfuscation:
                    certificate, key = root / "server.pem", root / "server.key"
                    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-sha256", "-nodes", "-days", "1",
                                    "-subj", "/CN=localhost", "-addext", "subjectAltName=IP:127.0.0.1,DNS:localhost",
                                    "-keyout", str(key), "-out", str(certificate)], check=True,
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
                    tls_flags = ["-tls-cert", certificate, "-tls-key", key, "-traffic-shaping", "bounded-v1"]
                    node_extra = {"tls_profile": args.tls_profile, "traffic_shaping": "bounded-v1", "ca_file": str(certificate)}
                    scheme = "https"
                node_port, admin_port = port(), port()
                state, work = root / "node-state", root / "work"
                state.mkdir(); work.mkdir()
                cli("provision", "-config", root / "control.json", "-node-config", root / "node.json",
                    "-node", "protected-test", "-server", f"{scheme}://127.0.0.1:{node_port}",
                    "-allow-insecure-loopback", "-state-dir", state, "-work-dir", work)
                if node_extra:
                    config = json.loads((root / "node.json").read_text())
                    config.update(node_extra)
                    (root / "node.json").write_text(json.dumps(config))
                launch("tack-control", "-config", root / "control.json", "-state", root / "tasks.json",
                       "-listen", f"127.0.0.1:{node_port}", "-admin-listen", f"127.0.0.1:{admin_port}", *tls_flags)
                launch("tack-node", "-config", root / "node.json")
                common = ["-server", f"http://127.0.0.1:{admin_port}", "-config", root / "control.json", "-allow-insecure-loopback"]
                await_value(lambda: json.loads(cli("nodes", *common)))
                task_file = root / "task.json"
                task_file.write_text(json.dumps({"node_id": "protected-test", "command": {
                    "mode": "posix-sh", "script": "printf protected-artifact-ok", "timeout_seconds": 5, "output_limit": 4096}}))
                task = json.loads(cli("submit", *common, "-file", task_file))

                def completed():
                    result = json.loads(cli("get", *common, "--format", "json-raw", "-id", task["id"]))
                    return result if result["state"] == "succeeded" and not result.get("result_pending") else None

                result = await_value(completed)
                if base64.b64decode(result["result"]["stdout"]) != b"protected-artifact-ok":
                    raise RuntimeError("protected management result corrupted")
                report["management"] = "provision, submit, execute, collect result through actual CLI/Controller/Node"

                class Echo(socketserver.BaseRequestHandler):
                    def handle(self):
                        while data := self.request.recv(65536):
                            self.request.sendall(data)

                with socketserver.ThreadingTCPServer(("127.0.0.1", 0), Echo) as echo:
                    echo.daemon_threads = True
                    thread = threading.Thread(target=echo.serve_forever, daemon=True)
                    thread.start()
                    try:
                        server_port, forward_port = port(), port()
                        auth = "fixture:" + secrets.token_hex(16)
                        server_flags, client_flags = [], []
                        if args.obfuscation:
                            server_flags = ["--tls-cert", certificate, "--tls-key", key, "--traffic-shaping", "bounded-v1"]
                            client_flags = ["--tls-ca", certificate, "--tls-profile", args.tls_profile, "--traffic-shaping", "bounded-v1"]
                            if args.tls_profile == "personalized-v1":
                                client_flags += ["--tls-identity-file", str(root / "chisel-tls-identity.json")]
                        launch("chisel", "server", "--host", "127.0.0.1", "--port", server_port, "--auth", auth, *server_flags)
                        if args.obfuscation:
                            def fingerprint():
                                match = re.search(r"Fingerprint (\S+)", (root / "processes.log").read_text())
                                return match.group(1) if match else None
                            client_flags += ["--fingerprint", await_value(fingerprint)]
                        launch("chisel", "client", "--auth", auth, *client_flags, f"{scheme}://127.0.0.1:{server_port}",
                               f"127.0.0.1:{forward_port}:127.0.0.1:{echo.server_address[1]}")
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

                        await_value(transfer)
                        report["chisel_verified_echo_bytes"] = len(payload)
                    finally:
                        echo.shutdown(); thread.join()
                report["passed"] = True
            finally:
                for process in reversed(processes):
                    if process.poll() is None:
                        process.terminate()
                        try:
                            process.wait(timeout=5)
                        except subprocess.TimeoutExpired:
                            process.kill(); process.wait()
                args.report.write_text(json.dumps(report, indent=2) + "\n")
    print("All four final artifacts passed functional smoke tests")


if __name__ == "__main__":
    main()
