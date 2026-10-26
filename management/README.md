# Tack management development baseline

This directory contains Tack's independent control, node and protocol modules. `tack-control` is hosted by the operator, `tack` is the operator CLI, and `tack-node` runs on customer machines. There is no Web UI. The node is a foreground process: deployment paths, service installation and boot persistence are entirely external.

The accepted requirements and remaining stages are in [the design](../docs/tack-design.md). Use the repository [deployment skill](../skills/tack-deploy/SKILL.md) to adapt installation to a customer machine. The release builder obfuscates and attempts packing of the unified client; hardware-backed credentials and signed commercial releases remain incomplete.

## Implemented behavior

- One customer executable and foreground process combining the management worker and Chisel client; see [configuration and compatibility](../docs/unified-node.md).
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
- Persistent submission identities, queue start deadlines, structured execution/cleanup errors, decoded CLI output and bounded `wait --check`.
- Scoped operator credentials with online rotation/revocation, caller correlation and filtered task pages; see [task operations](../docs/task-operations.md).
- Separate management and embedded tunnel status, including update times and backlog reasons.
- Private `tack metrics` output for link failures, request durations, process resources, state writes and output backlog.

Remaining work includes certificate enrollment/rotation, tenant isolation, online Node credential revocation, streaming output, operator-controlled history deletion, centrally updated tunnel configuration and native old-platform validation. Tunnel configuration is currently read at Node startup; no existing standalone Chisel installation is changed automatically.

## Build

Run from the repository root with a compatible Go toolchain:

```sh
mkdir -p build/tack
(cd management/control && go build -trimpath -o ../../build/tack/tack-control ./cmd/tack-control)
(cd management/control && go build -trimpath -o ../../build/tack/tack ./cmd/tack)
GOTOOLCHAIN=go1.26.3 CGO_ENABLED=0 go build -trimpath -o build/tack/tack-node ./cmd/tack-node
```

The unified Node is built from the root module. Releases provide a Go 1.26.3 Modern build and a Go 1.23.12 Linux Legacy build using the pinned compatibility adapter; both contain command execution and Chisel. See [artifact selection, supported boundaries and rollback](../docs/legacy-compatibility.md). The Legacy amd64 artifact has passed functional tests on a CentOS 6 kernel in QEMU; other devices need their own acceptance checks. Building `management/node/cmd/tack-node` still produces a management-only development worker that rejects tunnel configuration and must not replace the unified release.

Tack modules use `tack.local/management/...` with relative `replace` directives. This is a local module namespace, not a download address; build from this checkout. Chisel's upstream module path remains attached to the tunnel component.

## Renaming an existing development deployment

Replace the old management executables with `tack-control`, `tack-node` and `tack`, and update the external startup definitions to their selected paths. Keep the same configuration files, node identities and state directories. The new binaries read existing configuration and journals and retain the version 1 endpoint `/v1/agent/sync` and `agent.lock`. Protocol v3 writes a versioned Node journal and replay watermark; see the upgrade limits below before rolling binaries back. Do not delete or move the journal merely to rename the software.

New provisioning uses `-node-config` and `-node-ca`. The CLI still accepts `-agent-config` and `-agent-ca` as deprecated aliases. The deployment skill is now `tack-deploy`. `chisel` remains available for the hosted tunnel server and standalone compatibility use; the customer client is embedded in `tack-node`.

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
| `tunnel` | Embedded Chisel client configuration; `tack provision -tunnel-config FILE` copies a node-specific tunnel JSON object into the generated Node config. See [the unified guide](../docs/unified-node.md). Omit only for management-only operation. |
| `state_dir` | Exclusive state directory for one node; contains identity, lock, task journal, replay checkpoint and pending `results/` files |
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

`get` / `wait` decode stdout/stderr by default; `--format json` returns decoded strings, `--format json-raw` retains protocol Base64, and `--stdout-file` / `--stderr-file` save exact bytes. See [task operations](../docs/task-operations.md) for exit statuses, safe submission retries and restricted credentials. `stdout_truncated` and `stderr_truncated` indicate omitted output. Each stream keeps its first ceil(limit/2) bytes and a circular tail of floor(limit/2) bytes; when truncated these are concatenated without an inserted text marker. Excess output is drained so the process does not block. Logs are returned after execution in this stage. A terminal task with `result_pending: true` has committed its exit metadata but is still transferring output; wait for that flag to clear before treating output as complete. `metadata_truncated` marks bounded diagnostic error/executable text (8192/4096 UTF-8 bytes respectively). The exit code is absent if the process never started or its final status is unknown.

`exec` uses `program` and `args`; script modes use `script`. `dir` sets the task working directory, otherwise a temporary task directory is used. `env` overlays inherited Node environment, so do not inject operator credentials or unrelated secrets into the Node environment. Every task runs as the Node's account. Explicit Bash/Zsh/PowerShell/CMD requests never fall back to another language. A failed script is never rerun automatically under BusyBox.

The Node listener offers `POST /v4/node/sync` and `POST /v2/node/results`, retaining the v1–v3 synchronization endpoints for old nodes. The private administrator listener offers `GET /v1/nodes`, `GET /v1/metrics`, `GET/POST /v1/tasks`, `GET /v1/tasks/{id}`, `POST /v1/tasks/{id}/cancel`. `GET /health` advertises protocol version 4. New nodes negotiate at startup and use v1 polling with old controllers; restart a node to renegotiate after a controller upgrade. The CLI persists `request_id` in the task file before sending it; retry the same file after a lost response. Matching requests return the existing task; changed parameters conflict; retired identities never create new tasks. Direct legacy API submissions without an identity remain non-idempotent.

## Output recovery and metrics

The control connection sends terminal metadata plus output lengths and a SHA-256 digest of raw `stdout || stderr`. In protocol v3 sessions, the Node saves each nonempty result once in an immutable `results/<task-id>.out` file and retains only metadata in its journal. A distinct uploader probes the controller's durable offset and reads chunks of up to 64 KiB from that file using one reusable buffer. Legacy v1/v2 sessions retain the inline-output path. Duplicate chunks must match stored bytes. Final acknowledgement requires digest verification and persistence of the complete result. The Node flushes that acknowledgement to its journal before deleting the result file; receipt of metadata alone never discards output. Node or controller restarts resume from stored state without rerunning the command. Empty output follows the same final-acknowledgement rule.

One uploader runs alongside one executor. Result limits stop new work while an upload backlog exists, leaving excess tasks queued; metrics expose the reason. Upload pacing applies to raw payload bytes, not TLS/JSON overhead, and is independent of the control connection. This is application-level isolation, not a reserved network lane: shared NIC bandwidth, filesystem latency and controller snapshot writes can still delay control traffic.

`tack metrics` returns JSON only on the private administrator API. It includes separate control/upload request counts, failures and failure classes, durations, consecutive failures and last success; intentional canceled requests have a separate counter. Long-poll duration includes idle waiting and is **not network RTT**. Per-task `timing` measures queue delay, observed dispatch-to-running, cancellation-to-terminal and result-transfer time using the controller clock. Inspect timestamps to distinguish unavailable observations from zero-duration ones. Tiny jobs may finish before a running report is observed.

Process metrics include Go allocated heap (not RSS), goroutine count, cumulative process CPU time where supported, state JSON write attempts/failures/payload bytes/time and node output backlog. JSON counters include journal/snapshot writes, not raw Node result files, upload staging writes or physical disk/flash write amplification. Counters reset at process restart. Node metrics are self-reported snapshots, normally refreshed on the heartbeat/task transitions; they are not trusted for authentication or authorization. Metrics contain no tokens or task output.

Existing configuration and v1 wire clients remain usable. Controller storage remains v2 and the wire protocol remains v3. Node journal storage v4 references separate result files and reads existing v3 journals one record at a time during migration. Older Nodes reject v4 state; a new Node with a replay checkpoint also refuses a v1/v2 Controller. Back up the complete state directory before upgrade and do not perform an in-place binary-only downgrade. Restoring a snapshot predating executed tasks can lose evidence; preserve the journal, `results/` and replay checkpoint and reconcile task history. A lost v1 result acknowledgement is reconciled when a node switches to v2.

## Diagnostic logs and crash cleanup

Node and Controller remain foreground programs. Routine diagnostic logging is disabled by default. Fatal startup/runtime errors still go to stderr and exit unsuccessfully. For troubleshooting, select `-log-stderr` with bounded supervisor capture, or `-log-file /chosen/path/runtime.log -log-max-bytes 4194304 -log-files 4`; these destinations are mutually exclusive. The rotating option keeps at most four files including the current file (16 MiB by default). The path and its numeric backup names are reserved for this logger. Smaller limits on restart remove excess/oversized old diagnostic files. Avoid unbounded shell append redirection. Task results, acknowledgement records and replay checkpoints remain durable regardless of this logging choice.

The Node owns only `work_dir/tack-<persistent-instance-id>/task-*` beneath the supplied work path. It removes leftover owned workspaces at startup and before the next command; unrecognized content or failed cleanup is reported. Existing customer paths and old unowned `task-*` directories are not swept. Commands may themselves write outside this workspace, so filesystem/container quotas are a deployment decision when a hard limit on command-created files is required.

Atomic state writes use one fixed `.tack-pending` name per destination. Before cleanup, a Node recovers complete pending journal results and monotonic checkpoints; conflicting/unreadable results are preserved with an error. Partial uncommitted files and recognized obsolete atomic-write files are reclaimed only after the recovered journal is durable. The Controller reclaims incomplete archive temporaries and known completed upload staging; pending or unrecognized uploads stay protected. Transient failure writing a completed Node result is retried while the process retains the result and pauses new execution. No retention policy can make unpersisted RAM survive forced termination or a failed disk.

## Current operational limits

The Controller is single-process. Its `-state` file holds the bounded active queue and durable replay/request watermarks; completed tasks are individual JSON files in `<state-file>.history` and are read on demand. Back up the state file **together with** `.history`, `.uploads` and `.operators.json` while the Controller is stopped. Partial output lives in `.uploads` until the completed history entry and active-state replacement are durable. Completed staging files are removed before final acknowledgement; a failed cleanup is retried on probe and known completed leftovers are removed at startup. Pending or unrecognized files are preserved; orphan staging files count against admission capacity. Completed results move into `.history/released` only after a Node receipt confirms it received the final acknowledgement. Never delete this directory independently of the rest of the state.

Existing full snapshots migrate one task at a time at startup. The original snapshot is replaced only after historical entries have been flushed; interrupted migration can be retried. The new `storage_version` field intentionally makes older strict-decoding controllers reject the new state. Downgrades require a consistent pre-upgrade backup, not just replacing the binary.

Optional Controller configuration limits:

| Field | Default | Behavior |
| --- | --- | --- |
| `max_concurrent_uploads` | 8 | Simultaneous authenticated result requests (1–64); excess receives 503 before body allocation |
| `max_active_tasks` | 256 | Maximum queued, executing or result-pending tasks |
| `max_active_bytes` | 16777216 | Command bytes plus reserved metadata in the active queue |
| `max_history_tasks` | 100000 | Completed history plus accepted work; reclaim oldest receipt-confirmed history when needed |
| `max_history_bytes` | 1073741824 | History, protected orphan uploads and conservative reservations for accepted output/staging |
| `history_retention_seconds` | 2592000 (30 days) | Maximum age of receipt-confirmed history; pending results never expire |

Each node has at most two simultaneous control requests, allowing a canceled poll to finish while its replacement starts. The administrator API allows four concurrent mutations and eight history reads; status/metrics retain a separate path. These admission bounds reject excess requests before decoding their bodies. Node status strings are individually bounded.

Before accepting a task, the Controller automatically deletes the oldest receipt-confirmed results needed to fit the count/byte budgets. Expired confirmed results are also removed at startup and by the hosted process every minute. Unstarted canceled or expired tasks are immediately eligible. A directory scan buffers at most 128 eviction candidates. Only a full active queue or capacity occupied by unconfirmed/protected data causes HTTP 503; accepted work can still drain. These are application data bounds, not filesystem free-space guarantees: atomic replacements, directory entries and filesystem allocation need bounded additional headroom. Unknown upload data is preserved for reconciliation, never silently expired. Do not remove state behind a running process.

`tack tasks -limit 100 [-after CURSOR]` returns `{ "tasks": [...], "next_cursor": "..." }` via `/v2/tasks`; ordering is by opaque task ID, not chronological. Pages contain summaries, and `tack get -id ID` retrieves the original command and complete result. Maximum page size is 200. The older `/v1/tasks` route returns the first bounded summary page and exposes `X-Next-Cursor`; use the v2 route for new clients. Pagination is not a transactionally frozen snapshot during concurrent submissions.

With protocol v3, the Controller allocates increasing task sequences per node. The Node flushes a monotonic replay watermark before execution intent. After final output acknowledgement it reduces the journal entry to a receipt, and removes that entry after the Controller durably accepts the receipt. Completed work therefore does not accumulate per-task tombstones. Sequence zero or a sequence behind the watermark without a live journal entry is reported as `unknown` and never executed. A crash between the watermark and execution can refuse an unstarted command; this is at-most-once execution, not a promise that every command runs. The bounded pending-result/receipt queue still applies when acknowledgements cannot be delivered. Old v1/v2 sessions retain their original tombstone limit and need upgrading for rolling retention.

Upgrade the Controller first, then the CLI and Nodes. Drain the queue before migrating legacy Nodes: the first v3 handshake fences every already allocated sequence before retiring legacy identities, so queued work from before migration may become `unknown`. Check the original side effects before deciding to resubmit. Keep the entire Node state directory together, including `identity.json`, `journal.json`, `replay.json`, `results/` and recovery files; deleting/restoring Node evidence to an older point cannot preserve replay protection. The Node advertises its retained watermark after Controller rollback, and completed archives take precedence over a stale active snapshot. See [retention invariants](../docs/tack-retention.md).

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

Controller history is disk-backed and automatically reclaims confirmed results. Tests cover count/byte/age retention, full legacy-journal migration, lost acknowledgements, recovered temporary results, and stale Controller snapshots after per-task records have been pruned. Never delete a replay checkpoint to reclaim space. Native Windows handle counts, CentOS 6/embedded behavior, disk-full/power-loss recovery and 24–72 hour device soaks are separate acceptance work.

CI covers these modules, the unified Modern/Legacy dependency graphs, architecture builds and final-artifact gates. The Legacy gate adds a real 2.6.32 kernel VM, bidirectional pending-result recovery during runtime replacement, and both HTTP resource/weak-link checks. Cross-compilation remains separate from native OS/kernel/device validation; the old-kernel VM is not vendor storage or reboot qualification.

For **peak-load measurements of separate release processes** on a Linux host,
extract matching `tack-node` and `tack-control` binaries into a private directory
and run the localhost driver from the repository root:

```sh
python3 scripts/stress-management.py --bin-dir /tmp/tack-bin \
  --profile defaults --output /tmp/tack-stress-defaults.json
python3 scripts/stress-management.py --bin-dir /tmp/tack-bin \
  --profile large-output --http 1 --output /tmp/tack-stress-large-output.json
python3 scripts/stress-management.py --bin-dir /tmp/tack-bin \
  --profile max-backlog --output /tmp/tack-stress-max-backlog.json
python3 scripts/stress-management.py --bin-dir /tmp/tack-bin \
  --profile fanout --output /tmp/tack-stress-fanout.json
```

The driver needs Python 3, OpenSSL, `/proc`, at least 4 GiB of temporary free
space, and permission to create loopback listeners. It uses disposable
credentials, real TLS and temporary state, then terminates its processes and
removes that state. Run profiles sequentially on a test host. The maximum
backlog case deliberately exercises substantial memory and disk writes;
do not run it on customer devices or flash media to measure a small agent.
An observed 4 GiB aggregate RSS or allocated-state threshold aborts the run;
this sampling guard is not a kernel-enforced resource limit.

Backlog profiles limit upload to one byte per second, reach the pending-result
budget, hold it for ten seconds, restart with unrestricted upload, and verify
every retained output byte and execution marker. `defaults` keeps the usual
64 KiB per stream; `large-output` uses the maximum 1 MiB per stream with the
default 8 MiB backlog; `max-backlog` raises that backlog to 128 MiB. `fanout`
runs eight Nodes, 16 history readers and 256 commands while reclaiming history
at a 64-task/256-MiB budget. Every command produces 8 MiB on each output stream
to exercise truncation and draining. The driver also checks journal/workspace
and result-file cleanup, history limits and clean shutdown. For an upgrade test,
add `--initial-node-bin /path/to/old/tack-node` to a backlog profile: the driver
fills the old Node's journal and restarts using the Node in `--bin-dir`. Reports
record both binary hashes and each process separately. Diagnostic file logging
is explicitly enabled in these tests so failures retain troubleshooting context.

Reports distinguish each executable's kernel RSS high-water mark, sampled
descriptors/threads, CPU time, approximately one-second CPU peaks, state-space
peaks and cumulative `/proc` write accounting. CPU 100% means one logical CPU.
Commands and the driver are outside the Node's RSS; command helpers report
their own post-exec memory separately. Disk sampling can miss short atomic
replacement overlaps, and `/proc` writes are not physical flash wear. A pending
result byte budget limits retained raw output, **not total process memory**.
These are measured workload peaks, not universal upper bounds or long-term
leak tests. See the validation record for actual observations.

See the [validation record](VALIDATION.md) for checks actually run in this workspace and remaining native-platform validation.

## Release artifacts

See [release procedure](../docs/releases.md) for the pinned toolchains, artifact matrix, archive checks and draft-to-published process. `tack version`, `tack-control -version` and `tack-node -version` report the stamped version. Tack v0.1.1 is the first stable release. Its documented platform qualification and unsigned-binary limits still apply; see the [release notes](https://github.com/RC-CHN/tack/releases/tag/tack-v0.1.1) and [task operation upgrade requirements](../docs/task-operations.md).
