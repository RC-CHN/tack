# Tack management development baseline

This directory contains Tack's independent control, node and protocol modules. `tack-control` is hosted by the operator, `tack` is the operator CLI, and `tack-node` runs on customer machines. There is no Web UI. The node is a foreground process: deployment paths, service installation and boot persistence are entirely external.

The accepted requirements and remaining stages are in [the design](../docs/tack-design.md). Use the repository [deployment skill](../skills/tack-deploy/SKILL.md) to adapt installation to a customer machine. The current binaries are a development baseline, not the obfuscated, packed or hardware-key-protected commercial release.

## Implemented behavior

- Separate Node and administrator HTTP handlers and listeners; the administrator listener is restricted to literal loopback addresses.
- Per-node credentials, explicit node selection, durable task queue, cancellation, bounded final stdout/stderr and exit status.
- Bounded active queue, disk-backed completed history, paginated task summaries and reserved result capacity.
- HTTPS certificate validation, optional private CA, no redirects; plaintext development access requires explicit literal-loopback opt-in.
- Per-task execution intent journal, replay suppression and result retry. Interrupted execution becomes `unknown` on restart rather than being rerun.
- Linux process-group cancellation and Windows suspended process creation followed by Job Object assignment before execution.
- `exec`, `posix-sh`, `portable-sh`, `bash`, `zsh`, `powershell`, `cmd` modes. BusyBox is configured externally; its binaries are not bundled yet.
- A single running task per Node, with an independent result uploader and bounded output backlog.
- Version 2 HTTPS long polling: default application heartbeat is 30 minutes with ±50% jitter; queued commands and cancellations wake the waiting connection immediately.
- Durable, offset-based output upload with digest verification and retry; separate control/upload connection pools.
- Private `tack metrics` output for link failures, request durations, process resources, state writes and output backlog.

Remaining work includes certificate enrollment/rotation, tenants and RBAC, online revocation, streaming output, operator-controlled history deletion, Chisel process supervision, native old-platform validation and the protected release pipeline. No automatic changes are made to existing Chisel configuration.

## Build

Run from the repository root with a compatible Go toolchain:

```sh
mkdir -p build/tack
(cd management/control && go build -trimpath -o ../../build/tack/tack-control ./cmd/tack-control)
(cd management/control && go build -trimpath -o ../../build/tack/tack ./cmd/tack)
(cd management/node && go build -trimpath -o ../../build/tack/tack-node ./cmd/tack-node)
```

The controller requires Go 1.25 or newer. Node and protocol declare Go 1.23 and use only the standard library. Legacy builds must explicitly select the compatible toolchain, e.g. `GOTOOLCHAIN=go1.23.12 CGO_ENABLED=0`; setting `go 1.23` alone does not select an old runtime. Root Chisel has its own separate toolchain and dependencies.

Tack modules use `tack.local/management/...` with relative `replace` directives. This is a local module namespace, not a download address; build from this checkout. Chisel's upstream module path remains attached to the tunnel component.

## Renaming an existing development deployment

Replace the old management executables with `tack-control`, `tack-node` and `tack`, and update the external startup definitions to their selected paths. Keep the same configuration files, node identities and state directories. The new binaries read existing configuration and journals and retain the version 1 endpoint `/v1/agent/sync` and `agent.lock`. Version 2 writes additional state fields; see the upgrade limits below before rolling binaries back. Do not delete or move the journal merely to rename the software.

New provisioning uses `-node-config` and `-node-ca`. The CLI still accepts `-agent-config` and `-agent-ca` as deprecated aliases. The deployment skill is now `tack-deploy`. The tunnel executable remains `chisel`.

## Provision and start the Controller

These paths are examples for a regular server. Create directories with restricted ownership using the chosen deployment account. Do not apply them unchanged to a NAS or router. The target node directories supplied to `provision` are only written into configuration, not created on the node.

```sh
umask 077
mkdir -p ./operator-config ./operator-state
./build/tack/tack provision \
  -config ./operator-config/controller.json \
  -node-config ./operator-config/customer-a-node-01.json \
  -node customer-a-node-01 \
  -server https://control.example.com:8443 \
  -state-dir /srv/tack-node/state \
  -work-dir /srv/tack-node/work
```

Provisioning creates a random administrator credential only when the Controller configuration is new, and a unique credential for the new node. It refuses duplicate IDs or an existing Node configuration output file. Transfer only that node's generated Node configuration privately. The Controller configuration contains administrative and other node credentials and must never be copied to a customer machine.

Supply an existing certificate valid for the advertised hostname:

```sh
./build/tack/tack-control \
  -config ./operator-config/controller.json \
  -state ./operator-state/tasks.json \
  -listen 0.0.0.0:8443 \
  -admin-listen 127.0.0.1:8081 \
  -tls-cert /path/to/fullchain.pem \
  -tls-key /path/to/private-key.pem
```

Only the Node listener is customer-facing. Keep the administrator listener private; operate `tack` locally on the control server or through an administrator SSH forwarding arrangement. If using a reverse proxy, publish only the Node listener and apply the long-poll timeouts below. Restart the Controller after provisioning another node; tasks remain in its state file. Remove a node's entry and restart to revoke its management credential. Already running local tasks continue until cancellation is received or their local timeout expires.

## Start a customer Node

The deployment operator selects and prepares directories before starting the process. Both `state_dir` and `work_dir` must be absolute, existing writable directories. The Node never chooses an alternative directory, installs itself, registers a service, changes mount options or adds a startup entry.

```sh
/chosen/bin/tack-node -config /chosen/config/node.json
```

Additional configuration fields:

| Field | Meaning |
| --- | --- |
| `state_dir` | Exclusive state directory for one node; contains identity, lock and task journal |
| `work_dir` | Task-specific temporary directories; may be on a different writable volume |
| `state_persistence` | `durable` (default) or `volatile`, declared by the deployment operator |
| `ca_file` | Optional node-side PEM CA bundle for a private issuer |
| `busybox` | Optional absolute path to the separately provisioned BusyBox executable |
| `heartbeat_seconds` | 2–86400; default 1800, each idle wait randomized between 50% and 150% of this value |
| `lease_seconds` | Default `max(90, 5 × heartbeat_seconds)`, thus 9000 seconds by default; must cover the maximum heartbeat plus 30 seconds, maximum 604800 |
| `poll_seconds` | 1–10, default 2; retained for v1 fallback and spacing repeated cancellation responses; does not determine v2 task delivery |
| `max_pending_results` | Default 8, range 1–128; stops accepting new tasks when this many results await complete acknowledgement |
| `max_pending_result_bytes` | Default 8388608, range 2097152–134217728; retained raw stdout/stderr bytes, reserving another maximum-sized result before accepting work |
| `upload_bytes_per_second` | Default 0 (unlimited), maximum 1073741824; raw output payload pacing, with one initial burst of up to 64 KiB |

Declare volatile storage honestly: replay suppression cannot survive loss of that storage. Do not share a state directory between nodes, or run multiple Node instances with the same identity. Existing journal state is bound to its node ID. Files are created with restricted POSIX modes; Windows ACLs and protecting the parent directories are responsibilities of deployment.

## Heartbeats and immediate commands

The application heartbeat updates node status and metrics over an outbound HTTPS connection. It is independent of command scheduling: the controller holds a long poll while idle and wakes it when a task or cancellation arrives. No inbound customer-machine port is required. With the default configuration, idle refreshes occur every 15–45 minutes. Task transitions and completed uploads also refresh the control connection immediately.

For explicit settings, add `"heartbeat_seconds": 1800` and optionally `"lease_seconds": 9000` to node configuration. A transport failure starts reconnect attempts with exponential backoff (1–30 second base, ±50% jitter), independently of the heartbeat interval. A lease is a stale-status/identity-conflict tolerance, not proof of a currently working connection: `nodes` exposes both `last_seen` and `lease_expires_at`; `online` means the lease has not expired. Another instance with the same node ID is rejected until expiry. The instance ID is persisted with the node identity, so an ordinary restart using the same state directory does not wait for the lease. Never clone that state directory to a second running machine.

A reverse proxy's upstream response/read timeout must exceed the longest idle wait plus processing margin: use at least 2800 seconds with the default heartbeat. Configure any load balancer maximum request duration/idle timeout consistently. The controller extends each authenticated long poll's waiting deadline and switches to a 20-second write deadline when sending its response; request headers and control bodies retain short read deadlines. Upload requests have a separate 120-second body deadline and a 135-second client timeout. If an intermediary cannot keep the required connection open, lower `heartbeat_seconds` to fit it.

These are application intervals; the OS/Go TCP keepalive is separate. A silently lost NAT mapping can delay delivery until detected. Real network and device validation is required before claiming a command latency guarantee.

## Execute on one node

Create a task file on the control server:

```json
{
  "node_id": "customer-a-node-01",
  "command": {
    "mode": "posix-sh",
    "script": "printf 'management connection works\\n'",
    "timeout_seconds": 15,
    "output_limit": 65536
  }
}
```

Use the private administrator listener:

```sh
./build/tack/tack metrics -server http://127.0.0.1:8081 -allow-insecure-loopback -config ./operator-config/controller.json
./build/tack/tack nodes -server http://127.0.0.1:8081 -allow-insecure-loopback -config ./operator-config/controller.json
./build/tack/tack submit -server http://127.0.0.1:8081 -allow-insecure-loopback -config ./operator-config/controller.json -file task.json
./build/tack/tack get -server http://127.0.0.1:8081 -allow-insecure-loopback -config ./operator-config/controller.json -id TASK_ID
./build/tack/tack cancel -server http://127.0.0.1:8081 -allow-insecure-loopback -config ./operator-config/controller.json -id TASK_ID
```

JSON stdout/stderr are base64 byte strings. Decode according to the command's output encoding. `stdout_truncated` and `stderr_truncated` indicate retained output limits; excess output is drained so the process does not block. Logs are returned after execution in this stage. A terminal task with `result_pending: true` has committed its exit metadata but is still transferring output; wait for that flag to clear before treating output as complete. `metadata_truncated` marks bounded diagnostic error/executable text (8192/4096 UTF-8 bytes respectively). The exit code is absent if the process never started or its final status is unknown.

`exec` uses `program` and `args`; script modes use `script`. `dir` sets the task working directory, otherwise a temporary task directory is used. `env` overlays inherited Node environment, so do not inject operator credentials or unrelated secrets into the Node environment. Every task runs as the Node's account. Explicit Bash/Zsh/PowerShell/CMD requests never fall back to another language. A failed script is never rerun automatically under BusyBox.

The Node listener offers `POST /v2/node/sync` and `POST /v2/node/results`, retaining `POST /v1/agent/sync` for old nodes. The private administrator listener offers `GET /v1/nodes`, `GET /v1/metrics`, `GET/POST /v1/tasks`, `GET /v1/tasks/{id}`, `POST /v1/tasks/{id}/cancel`. `GET /health` advertises protocol version 2. New nodes negotiate at startup and use v1 polling with old controllers; restart a node to renegotiate after a controller upgrade. A new `submit` always creates a new task; a lost submit response must be reconciled using `tasks` before manually retrying.

## Output recovery and metrics

The control connection sends terminal metadata plus output lengths and a SHA-256 digest of raw `stdout || stderr`. A distinct uploader probes the controller's durable offset, sends chunks of up to 64 KiB, and retries after transport/HTTP failures. It slices immutable runner output directly, allocating at most one chunk when crossing the stdout/stderr boundary instead of duplicating the full output. Duplicate chunks must match stored bytes. Final acknowledgement requires digest verification and persistence of the complete result. The node retains full output until that final acknowledgement; receipt of metadata alone never discards output. Node or controller restarts resume from stored state without rerunning the command. Empty output follows the same final-acknowledgement rule.

One uploader runs alongside one executor. Result limits stop new work while an upload backlog exists, leaving excess tasks queued; metrics expose the reason. Upload pacing applies to raw payload bytes, not TLS/JSON overhead, and is independent of the control connection. This is application-level isolation, not a reserved network lane: shared NIC bandwidth, filesystem latency and controller snapshot writes can still delay control traffic.

`tack metrics` returns JSON only on the private administrator API. It includes separate control/upload request counts, failures and failure classes, durations, consecutive failures and last success; intentional canceled requests have a separate counter. Long-poll duration includes idle waiting and is **not network RTT**. Per-task `timing` measures queue delay, observed dispatch-to-running, cancellation-to-terminal and result-transfer time using the controller clock. Inspect timestamps to distinguish unavailable observations from zero-duration ones. Tiny jobs may finish before a running report is observed.

Process metrics include Go allocated heap (not RSS), goroutine count, cumulative process CPU time where supported, state JSON write attempts/failures/payload bytes/time and node output backlog. JSON counters include journal/snapshot writes, not upload staging writes or physical disk/flash write amplification. Counters reset at process restart. Node metrics are self-reported snapshots, normally refreshed on the heartbeat/task transitions; they are not trusted for authentication or authorization. Metrics contain no tokens or task output.

Existing configuration and v1 wire clients remain usable. New state includes persisted instance IDs, metadata acknowledgements, manifests and timings. Older binaries using strict JSON decoders cannot read all new state fields: back up state before upgrade and do not perform an in-place binary-only downgrade. Restoring a snapshot predating executed tasks can lose evidence; preserve the node journal and reconcile task history. A lost v1 result acknowledgement is reconciled when a node switches to v2.

## Current operational limits

The Controller is single-process. Its `-state` file holds only the bounded active queue; completed tasks are individual JSON files in `<state-file>.history` and are read on demand. Back up the state file **together with** `.history` and `.uploads` while the Controller is stopped. Partial output lives in `.uploads` until the completed history entry and active-state replacement are durable. Completed staging files are removed before final acknowledgement; a failed cleanup is retried on probe and known completed leftovers are removed at startup. Pending or unrecognized files are preserved.

Existing full snapshots migrate one task at a time at startup. The original snapshot is replaced only after historical entries have been flushed; interrupted migration can be retried. The new `storage_version` field intentionally makes older strict-decoding controllers reject the new state. Downgrades require a consistent pre-upgrade backup, not just replacing the binary.

Optional Controller configuration limits:

| Field | Default | Behavior |
| --- | --- | --- |
| `max_active_tasks` | 256 | Maximum queued, executing or result-pending tasks |
| `max_active_bytes` | 16777216 | Command bytes plus reserved metadata in the active queue |
| `max_history_tasks` | 100000 | Completed history plus slots reserved for accepted work |
| `max_history_bytes` | 1073741824 | History plus conservative reservations for accepted output/staging |

Capacity exhaustion returns HTTP 503 for new submissions. Existing work can still complete and return results. These are application data bounds, not filesystem free-space guarantees; actual disk-full errors retain pending data and suppress successful acknowledgement. Existing history is never automatically deleted. Raise capacity deliberately when needed; do not remove history records behind the running Controller, since they are also needed to reconcile retries. History enumeration scans directory entries with bounded memory and becomes slower as disk history grows.

`tack tasks -limit 100 [-after CURSOR]` returns `{ "tasks": [...], "next_cursor": "..." }` via `/v2/tasks`; ordering is by opaque task ID, not chronological. Pages contain summaries, and `tack get -id ID` retrieves the original command and complete result. Maximum page size is 200. The older `/v1/tasks` route returns the first bounded summary page and exposes `X-Next-Cursor`; use the v2 route for new clients. Pagination is not a transactionally frozen snapshot during concurrent submissions.

The Node retains task tombstones, caps the journal at 10,000 entries, and stops accepting new work at that boundary. Do not delete the journal to bypass replay protection; journal rollover with replay-safe reconciliation remains a later stage. After final Controller acknowledgement the Node reduces each record to a small identity/state tombstone, discarding output and the original command, arguments and environment. Startup also compacts older acknowledged records without removing replay identities. If an old acknowledged task is replayed after a Controller restore, the Node reports `unknown` and refuses to execute it again.

Short network failures do not cancel tasks. Shutdown requests cancel the local task and save its result when possible; an unexpected restart converts unfinished intent to `unknown`. Process-group cleanup does not cover programs deliberately escaping the group. Job Object behavior requires Windows native validation. Filesystem crash durability, flash limits, device drivers and boot persistence require target-system testing.

Current node tokens are readable by the Node's account. No software-only scheme can promise secrecy from root or an attacker controlling the Node process. They authorize only this node's synchronization, not administrative APIs or server shell access. Production still requires the identity, network isolation and credential lifecycle work described in the design.

## Checks

```sh
for module in protocol control node integration; do
  (cd "management/$module" && go test -race ./... && go vet ./...)
done
```

## Resource lifecycle and endurance tests

The Node owns its control and upload transports. Shutdown cancels pending dials, closes owned sockets (including HTTP/2 connections whose canceled streams have not become idle yet), waits for workers, and closes the idle pools. A `Node` instance has one `Run` lifetime; library callers construct a new instance to restart. Response bodies are closed on success, HTTP failure and decode failure. Request/backoff timers are stopped or canceled with their owner. Task workspace cleanup failures are reported in the task's error text instead of being silently ignored.

Controller response serialization/socket writes and administrator request-body reads occur outside the task queue mutex. A slow peer does not hold that mutex while waiting for the network. Durable mutations are serialized separately, and the memory/read lock is released during persistence: status and already committed data remain readable while a disk write is slow. New durable mutations still wait for storage; the active snapshot is bounded and completed history is not rewritten on every task transition.

Ordinary tests include 24 repeated shutdowns for each HTTP version with both control and upload requests blocked, pending-dial cancellation, failed intent writes, final-output commit failure and slow peer tests. The opt-in soak first executes a finite batch of real bounded-output tasks, checks their journal and temporary-directory cleanup, then holds task history constant while repeatedly aborting connections and returning HTTP 503. This distinguishes retained historical data from resources accumulating on every reconnect. Samples include two-GC live heap, goroutines and, on Linux, RSS and file descriptors. Node and controller share the test process, so the figures are combined, not a per-agent memory footprint.

Run from `management/integration`:

```sh
TACK_SOAK_SECONDS=180 TACK_SOAK_TASKS=64 TACK_SOAK_REPORT=/tmp/tack-soak-http1.json \
  go test -race -run '^TestManagementResourceSoak$' -count=1 -timeout=8m -v .
TACK_SOAK_SECONDS=180 TACK_SOAK_TASKS=64 TACK_SOAK_HTTP2=1 TACK_SOAK_REPORT=/tmp/tack-soak-http2.json \
  go test -race -run '^TestManagementResourceSoak$' -count=1 -timeout=8m -v .
```

For an overnight reconnect soak, set `TACK_SOAK_SECONDS=86400` and `-timeout=25h`. Increase the task batch separately using `TACK_SOAK_TASKS` (1–1000); this is still a fixed-history reconnect test, not an unlimited history-growth benchmark. Reports contain no node credentials. The coarse gates allow up to 8 MiB of post-GC heap growth and 8 extra goroutines/FDs during the live soak; the shutdown test additionally requires **every owned server-side connection and request handler to close** after every cycle. Short passing runs cannot rule out smaller or slower leaks.

Controller history is now disk-backed with admission limits; tests cover memory growth, pagination, migration, and final-ack recovery. Reaching the configured history capacity or the Node's 10,000-entry journal limit stops new work, preserving records; do not delete the Node journal to reclaim space. Native Windows handle counts, CentOS 6/embedded behavior, disk-full/power-loss recovery and 24–72 hour device soaks are separate acceptance work.

The separate management CI covers these modules, a Go 1.23.12 Node build/test job and an architecture compilation matrix. Cross-compilation is recorded separately from native OS/kernel/device testing.

See the [validation record](VALIDATION.md) for checks actually run in this workspace and remaining native-platform validation.

## Release artifacts

See [release procedure](../docs/releases.md) for the pinned toolchains, artifact matrix, archive checks and draft-to-published process. `tack version`, `tack-control -version` and `tack-node -version` report the stamped version. The initial Tack release is a candidate build; it does not claim native legacy-device validation or commercial binary protection.
