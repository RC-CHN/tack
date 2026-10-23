# Platform deployment decisions

Node does not discover mount layouts, pick directories, register a service or repair a boot entry. The deployment operator makes these choices and supplies its config. Keep executable, config, state, work and logs independently configurable.

## Linux discovery

Read `/proc/mounts` or `findmnt` if available, `uname`, current user information, filesystem capacity and the active init process. Minimal systems may offer these through BusyBox; do not assume Python, Bash, systemctl or package installation. Match the ELF architecture, endianness, CPU baseline and kernel with the artifact's validation record. A static binary can still require unsupported kernel calls or CPU instructions.

Inspect the selected mount's `ro/rw` and `noexec` options. Node and BusyBox binaries need executable storage. Task scripts are read by an explicit interpreter, so their workspace need not itself permit direct execution. State needs writes, renames and an exclusive process lock. Test these as the intended account in the selected directory, cleaning only the probe's own files.

## Choosing startup

| Environment | Candidate approach | Conditions to verify |
| --- | --- | --- |
| systemd host | Explicit named service with absolute ExecStart | Run account, state/work access, mount dependencies, restart backoff and log retention |
| SysV host such as CentOS 6 | LSB/init script or existing supported supervisor | PID ownership, start/stop semantics, networking/mount order; command helpers may vary |
| OpenWrt | Named procd service | Real overlay space, boot persistence, respawn limits and firmware retention configuration |
| NAS | Vendor-supported package/service or documented startup hook | Root may be read-only, data volumes may appear late, firmware upgrades may replace system paths |
| Minimal embedded system | Existing init/supervisor or explicit external wrapper | Avoid assuming systemd, Bash or writable `/etc`; bound retries and output |
| Windows | Existing approved service wrapper or visible scheduled task | Account privileges, startup trigger, ACLs, executable quoting, storage readiness and stop behavior |

The current Node is not a Windows SCM service binary. Do not register it directly with `sc create` and claim service support. Use an approved wrapper or a named startup task appropriate to the host, or leave it in foreground mode until a suitable mechanism is available. Do not invent a hidden startup mechanism.

## Read-only roots and overlay

An example NAS layout may place binaries/config on a persistent data volume, runtime work on tmpfs, and task state on a durable writable directory. This is only a candidate: verify whether the volume is mounted when the startup hook runs. An external wrapper can wait with a bounded timeout for the volume, create runtime directories and exec the Node; never replace the device's init process.

On routers, avoid frequent journal writes to a tiny flash overlay when a suitable persistent external volume exists. If only tmpfs is practical, set `state_persistence` to `volatile`, explain that power loss removes task replay records, and do not automatically replay old work after rediscovery. Firmware upgrade preservation requires device-specific configuration and cannot be inferred from ordinary reboot behavior.

Node storage v4 writes each nonempty result once under the selected state directory's `results/` and rewrites only a small metadata journal on state transitions. Measure workload and capacity before choosing a constrained overlay; filesystem metadata, atomic replacement and recovery still need headroom. Protocol v3 compacts receipt-confirmed task entries behind a durable replay watermark; unconfirmed results remain protected. Set pending-result and Controller retention budgets for the target volume. Routine Node/Controller diagnostic logging is off by default; enable rotating files or bounded stderr capture when troubleshooting. Node-owned temporary workspaces are recovered independently of deployment paths; command-created files still require filesystem/container quotas if their disk usage must be forcibly capped.

## Startup artifact shape

Use explicit placeholders such as `NODE_EXE`, `CONFIG_FILE`, `STATE_DIR`, `WORK_DIR`, `RUN_ACCOUNT`, `STARTUP_NAME`, `REQUIRED_MOUNT` in the deployment record. Render actual paths into the selected service format; do not interpolate untrusted paths into shell source. Keep credentials in the protected config, not unit command lines or task arguments. The runtime invocation is only:

```text
NODE_EXE -config CONFIG_FILE
```

Any mount waiting, directory preparation, privilege selection, restart or boot registration belongs to the external supervisor/wrapper. Check foreground execution before installing startup entries and preserve rollback instructions.
