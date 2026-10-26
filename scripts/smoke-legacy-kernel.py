#!/usr/bin/env python3
"""Exercise a final unified Node on a real 2.6.32 kernel in an isolated QEMU VM.

This is a BusyBox initramfs, not a full CentOS installation. No root/TAP device is
needed; the guest can reach only two explicit loopback TLS fixture endpoints.
"""
import argparse
import base64
import gzip
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import stat
import subprocess
import tempfile
import time

spec = importlib.util.spec_from_file_location("vm_fixtures", Path(__file__).with_name("prepare-legacy-vm.py"))
fixtures = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixtures)


def initramfs(files):
    data = bytearray()

    def entry(name, contents=b"", mode=stat.S_IFREG | 0o644, major=0, minor=0):
        fields = [1, mode, 0, 0, 1, 0, len(contents), 0, 0, major, minor, len(name.encode())+1, 0]
        data.extend(b"070701" + b"".join(f"{n:08x}".encode() for n in fields) + name.encode() + b"\0")
        data.extend(b"\0" * (-len(data) % 4))
        data.extend(contents)
        data.extend(b"\0" * (-len(data) % 4))

    for name in ("bin", "dev", "proc", "sys", "tmp", "state", "work"):
        entry(name, mode=stat.S_IFDIR | 0o755)
    entry("dev/console", mode=stat.S_IFCHR | 0o600, major=5, minor=1)
    entry("dev/null", mode=stat.S_IFCHR | 0o666, major=1, minor=3)
    for name, (contents, executable) in files.items():
        entry(name, contents, stat.S_IFREG | (0o755 if executable else 0o600))
    entry("TRAILER!!!")
    return gzip.compress(data, mtime=0)


def port():
    with socket.socket() as stream:
        stream.bind(("127.0.0.1", 0))
        return stream.getsockname()[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bin-dir", type=Path, required=True, help="Legacy Node plus modern tack, tack-control and chisel")
    parser.add_argument("--fixture-dir", type=Path, required=True)
    parser.add_argument("--download", action="store_true")
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    fixture = args.fixture_dir.resolve()
    lock = fixtures.prepare(fixture, args.download)
    binaries = {name: str((args.bin_dir / name).resolve()) for name in ("tack", "tack-node", "tack-control", "chisel")}
    report = {"passed": False, "kernel_release": lock["kernel_release"], "fixture_lock_sha256": fixtures.sha(fixtures.LOCK),
              "binary_sha256": {name: fixtures.sha(path) for name, path in binaries.items()},
              "scope": "CentOS 6 kernel with BusyBox initramfs; not full vendor OS/persistence qualification"}
    args.report.parent.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, GOMAXPROCS="2")
    processes = []
    serial = b""
    try:
        with tempfile.TemporaryDirectory(prefix="tack-legacy-functional-") as temp:
            root = Path(temp)
            with (root / "servers.log").open("wb") as servers, (root / "serial.log").open("wb") as console:
                def launch(argv, **kwargs):
                    proc = subprocess.Popen(list(map(str, argv)), env=env, **kwargs)
                    processes.append(proc)
                    return proc

                def cli(*flags):
                    return subprocess.check_output([binaries["tack"], *map(str, flags)], env=env, stderr=subprocess.PIPE, timeout=20)

                def wait(check, timeout=90):
                    deadline = time.monotonic() + timeout
                    while time.monotonic() < deadline:
                        if any(p.poll() is not None for p in processes):
                            raise RuntimeError("fixture exited unexpectedly")
                        try:
                            value = check()
                            if value:
                                return value
                        except (OSError, subprocess.SubprocessError):
                            pass
                        time.sleep(0.2)
                    raise RuntimeError("old-kernel functional check timed out")

                try:
                    cert, key = root / "ca.pem", root / "server.key"
                    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-sha256", "-nodes", "-days", "1",
                                    "-subj", "/CN=legacy-fixture", "-addext", "subjectAltName=IP:127.0.0.1,IP:10.0.2.100",
                                    "-keyout", str(key), "-out", str(cert)], check=True, timeout=30, stdout=servers, stderr=servers)
                    subprocess.run([binaries["chisel"], "server", "--keygen", str(root / "ssh.key")],
                                   env=env, check=True, timeout=20, stdout=servers, stderr=servers)
                    control, admin, tunnel, forward = port(), port(), port(), port()
                    credential = "legacy:" + secrets.token_hex(16)
                    (root / "users.json").write_text(json.dumps({credential: ["R:127.0.0.1:" + str(forward)]}))
                    launch([binaries["chisel"], "server", "--reverse", "--host", "127.0.0.1", "--port", tunnel,
                            "--keyfile", root / "ssh.key", "--authfile", root / "users.json",
                            "--tls-cert", cert, "--tls-key", key], stdout=servers, stderr=servers)

                    def fingerprint():
                        match = re.search(r"Fingerprint (\S+)", (root / "servers.log").read_text(errors="replace"))
                        return match.group(1) if match else None

                    fp = wait(fingerprint)
                    (root / "tunnel.json").write_text(json.dumps({"server": f"https://10.0.2.100:{tunnel}",
                        "auth": credential, "fingerprint": fp, "ca_file": "/ca.pem", "keep_alive_seconds": 0,
                        "remotes": [f"R:127.0.0.1:{forward}:127.0.0.1:7777"]}))
                    cli("provision", "-config", root / "control.json", "-node-config", root / "node.json", "-node", "legacy-vm",
                        "-server", f"https://10.0.2.100:{control}", "-state-dir", "/state", "-work-dir", "/work",
                        "-state-persistence", "volatile", "-node-ca", "/ca.pem", "-tunnel-config", root / "tunnel.json")
                    launch([binaries["tack-control"], "-config", root / "control.json", "-state", root / "tasks.json",
                            "-listen", f"127.0.0.1:{control}", "-admin-listen", f"127.0.0.1:{admin}",
                            "-tls-cert", cert, "-tls-key", key], stdout=servers, stderr=servers)
                    init = b'''#!/bin/busybox sh
/bin/busybox --install -s /bin
export PATH=/bin GOMAXPROCS=2
mount -t proc proc /proc
mount -t sysfs sys /sys
mount -t devtmpfs devtmpfs /dev
insmod /e1000.ko || poweroff -f
ifconfig lo 127.0.0.1 up
ifconfig eth0 10.0.2.15 netmask 255.255.255.0 up
route add default gw 10.0.2.2
echo GUEST_KERNEL=$(uname -r)
tcpsvd 127.0.0.1 7777 /bin/cat >/tmp/echo.log 2>&1 &
/tack-node -config /node.json >/tmp/node.log 2>&1 &
node_pid=$!
echo NODE_PID=$node_pid
read stop
kill -TERM "$node_pid"
wait "$node_pid"
echo NODE_EXIT=$?
echo NODE_LOG_BYTES=$(wc -c </tmp/node.log)
cat /tmp/node.log
echo GUEST_DONE
poweroff -f
'''
                    ramdisk = root / "init.cpio.gz"
                    ramdisk.write_bytes(initramfs({"init": (init, True), "bin/busybox": ((fixture / "busybox").read_bytes(), True),
                        "e1000.ko": ((fixture / "e1000.ko").read_bytes(), False), "tack-node": (Path(binaries["tack-node"]).read_bytes(), True),
                        "ca.pem": (cert.read_bytes(), False), "node.json": ((root / "node.json").read_bytes(), False)}))
                    nc = shutil.which("nc")
                    if not nc or any(ch.isspace() or ch in ",;" for ch in nc):
                        raise RuntimeError("netcat must be installed at a simple executable path")
                    network = "user,model=e1000,restrict=on" + "".join(
                        f",guestfwd=tcp:10.0.2.100:{p}-cmd:{nc} 127.0.0.1 {p}" for p in (control, tunnel))
                    vm = launch(["qemu-system-x86_64", "-machine", "pc,accel=tcg", "-cpu", "qemu64", "-m", "384", "-smp", "2",
                                 "-nodefaults", "-display", "none", "-serial", "stdio", "-monitor", "none", "-no-reboot",
                                 "-nic", network, "-kernel", fixture / "kernel", "-initrd", ramdisk,
                                 "-append", "console=ttyS0 panic=1 quiet rdinit=/init"], stdin=subprocess.PIPE, stdout=console, stderr=console)
                    common = ["-server", f"http://127.0.0.1:{admin}", "-config", root / "control.json", "-allow-insecure-loopback"]
                    wait(lambda: json.loads(cli("nodes", *common)))
                    payload = bytes(range(256)) * 256

                    def transfer():
                        with socket.create_connection(("127.0.0.1", forward), timeout=5) as stream:
                            stream.sendall(payload)
                            received = bytearray()
                            while len(received) < len(payload):
                                chunk = stream.recv(65536)
                                if not chunk:
                                    return False
                                received.extend(chunk)
                            return received == payload

                    wait(transfer)
                    (root / "task.json").write_text(json.dumps({"node_id": "legacy-vm", "command": {"mode": "posix-sh",
                        "script": "uname -r; printf legacy-shell-ok; readlink /proc/$PPID/exe", "timeout_seconds": 30, "output_limit": 4096}}))
                    submitted = json.loads(cli("submit", *common, "-file", root / "task.json"))

                    def result():
                        task = json.loads(cli("get", *common, "--format", "json-raw", "-id", submitted["id"]))
                        if task["state"] in ("failed", "unknown", "cancelled"):
                            raise RuntimeError("guest command did not succeed")
                        return task if task["state"] == "succeeded" and not task.get("result_pending") else None

                    task = wait(result)
                    stdout = base64.b64decode(task["result"]["stdout"]).decode()
                    if stdout != lock["kernel_release"] + "\nlegacy-shell-ok/tack-node\n":
                        raise RuntimeError("unexpected guest output: " + repr(stdout))
                    vm.stdin.write(b"STOP\n")
                    vm.stdin.flush()
                    vm.wait(timeout=30)
                    processes.remove(vm)
                    serial = (root / "serial.log").read_bytes()
                    if vm.returncode or b"NODE_EXIT=0" not in serial or b"NODE_LOG_BYTES=0" not in serial or b"GUEST_DONE" not in serial:
                        raise RuntimeError("guest Node did not exit cleanly and quietly")
                    report.update(passed=True, tasks_completed=1, verified_echo_bytes=len(payload), command_stdout=stdout,
                                  graceful_shutdown=True, default_diagnostics_bytes=0, qemu_network="restricted explicit loopback forwards")
                finally:
                    for proc in reversed(processes):
                        if proc.poll() is None:
                            proc.terminate()
                            try:
                                proc.wait(timeout=8)
                            except subprocess.TimeoutExpired:
                                proc.kill()
                                proc.wait()
                    serial = (root / "serial.log").read_bytes()
                    if not report["passed"]:
                        print(serial.decode(errors="replace")[-5000:])
                        print((root / "servers.log").read_text(errors="replace")[-3000:])
    finally:
        args.report.write_text(json.dumps(report, indent=2) + "\n")
        args.report.with_suffix(".serial.log").write_bytes(serial)
    print("Final unified Legacy Node passed real 2.6.32-kernel TLS, shell, tunnel and shutdown checks")


if __name__ == "__main__":
    main()
