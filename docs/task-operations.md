# Task submission and result handling

This guide describes Tack v0.1.1. Upgrade the Controller and CLI first, then Nodes. Management protocol v4 adds start deadlines, structured execution diagnostics and separate management/tunnel observations. Existing v1–v3 nodes still run ordinary tasks; deadline-bearing tasks wait for a v4 node and expire without execution if that upgrade does not happen in time. Node negotiation happens at startup.

## Submit once, retry safely

Create a task JSON file with `node_id` and `command`, as in the [management guide](../management/README.md). `tack submit --file task.json` locks the file, applies defaults, saves a `request_id` atomically, and only then sends the request. It prints the created or existing task as protocol JSON. Keep that file and retry the same file after a lost response. A deliberate new execution needs a new task file/identity; changing the command while retaining the identity produces a conflict.

The API accepts optional `request_id`, `caller_id` and `start_before` fields alongside `node_id` and `command`. CLI flags `--request-id`, `--caller-id` and `--start-before` are saved into the task file as well. The file must be writable. Direct API callers can obtain an identity with `tack request-id` and persist the complete request before sending it. Legacy API submissions without an identity retain their previous non-idempotent behavior.

Identities have the form `<13-digit Unix milliseconds>-<16..64 character random identifier>`. Use `tack request-id` or the CLI's automatic generation; keep operator and controller clocks synchronized. The Controller scopes each identity to its stable operator ID (`admin` for the administrator), normalizes command defaults, and persists identity, parameters and task in one commit. Matching retries return HTTP 200 with the same task ID and current state; first creation returns 201; changed node, command, caller ID or deadline returns 409 `request_conflict`. Rotating a credential does not change its namespace.

Deduplication remains bounded along with history. Retained tasks can be returned for as long as they exist. For unknown identities, the initial submission window is seven days and future skew is limited to five minutes. Before reclaiming any identified task, the Controller durably advances a conservative retirement timestamp. An absent identity at or before that timestamp returns 410 `request_retired`; it never creates another task. The watermark can also reject unused old identities. It uses constant storage instead of retaining an unlimited tombstone ledger. After 410, reconcile the original outcome using exported records or the target; generating a new identity is a new execution, not a retry. Backup restoration must preserve both history and the matching watermark.

This provides submission idempotency and execution replay suppression, not an exactly-once guarantee for arbitrary external side effects. A crash after execution intent but before a recoverable result remains `unknown`; inspect the target before deciding whether a new execution is appropriate.

## Read and wait

All examples use the private operator listener. The sample variable is a convenience for an interactive POSIX shell; equivalent flags work on Windows.

```sh
TACK_SERVER=http://127.0.0.1:8081
tack get --server "$TACK_SERVER" --allow-insecure-loopback --credential operator-credential.json --id TASK_ID
tack wait --server "$TACK_SERVER" --allow-insecure-loopback --credential operator-credential.json --id TASK_ID --timeout 5m --check
tack get --server "$TACK_SERVER" --allow-insecure-loopback --credential operator-credential.json --id TASK_ID --format json --stdout-file stdout.bin --stderr-file stderr.bin
```

`get` and `wait` default to readable output with state, exit code, pending upload, truncation flags and separate stdout/stderr. `--format json` retains task metadata but decodes the streams into JSON strings; invalid UTF-8 is replaced and marked with `stdout_text_lossy` / `stderr_text_lossy`. Raw files preserve every returned byte, including NULs, non-UTF-8 output and the bounded head/tail when truncated. `--format json-raw` preserves the protocol's Base64 byte fields for existing scripts. Files are written only when a result exists and `result_pending` is false. On wait timeout, available status is printed but output files are not written.

`get` reports query success, even for a failed remote task. `wait` polls until both a terminal state and complete output; its default budget is five minutes, adjustable up to 24 hours. `--poll` defaults to one second (100 ms–30 s). It never sends a cancellation. `cancel` is the explicit operation for stopping work.

| CLI exit status | Meaning |
| --- | --- |
| 0 | Query/wait succeeded; with `wait --check`, remote state is `succeeded` |
| 1 | Local, protocol, credential or query failure |
| 3 | `wait --check` observed a completed remote state other than `succeeded` |
| 4 | Wait budget expired; remote task continues |

A successful command with cleanup trouble still passes `wait --check`. Inspect `result.cleanup` separately. Network/query failure ends the current CLI operation; retry `get`/`wait` for that same task ID. It does not authorize another submit with a new identity.

## Execution, persistence and transfer errors

`result.error_code`, `result.phase` and `result.error` describe execution. An absent exit code means that no reliable process exit status is available. `started_at` is the zero timestamp for pre-start rejection. Cleanup has its own `result.cleanup.error_code`, `error` and `path`; the command outcome and error are never overwritten by cleanup failure. The path is the failed cleanup's observation, and later recovery may have removed it.

| Code | Phase | Meaning / next action |
| --- | --- | --- |
| `invalid_command` | `validation` / `resolve` | Correct the request; no script fallback is attempted |
| `interpreter_missing`, `program_missing` | `resolve` | Correct the explicitly selected interpreter/program |
| `workspace_io_failed`, `script_write_failed` | `workspace` | Check paths, permissions or available storage |
| `process_start_failed` | `start` | Process creation/preparation failed |
| `command_exit_nonzero` | `execute` | Command ran and returned a nonzero exit code |
| `command_timeout`, `command_canceled` | `execute` / `start` | Inspect completed side effects before requesting another execution |
| `process_wait_failed` | `execute` | Final process/output collection failed; inspect the result |
| `start_deadline_exceeded` | `queued` / `submit` | Deadline passed; command was not started |
| `execution_unknown`, `replay_rejected` | `recovery` | Execution is not repeated automatically; reconcile the target |
| `cleanup_failed` | Separate cleanup object | Execution result remains authoritative; inspect the retained path |

Persistence and upload are independent observations. While available, `nodes[].status.transfer` and the active task's `transfer` field identify `task_id`, `error_code`, `phase`, explanation and update time. `result_write_failed` / `persist` means the worker retains its completed result and retries the failed disk commit, without accepting more work. `result_upload_failed` / `upload` means the immutable local result is retained and uploading will retry. Neither turns a successful command into a failed command. Complete tasks suppress stale transfer warnings. These observations require a working management path and are not durable incident history; look at their timestamps. A process crash while a result is only in memory can still leave execution `unknown`.

HTTP failures return `{error_code, phase, error}` with stable codes such as `invalid_request`, `unauthenticated`, `permission_denied`, `not_found`, `conflict`, `storage_io_failed` and `capacity_unavailable`. Request-specific codes include `invalid_request_id`, `request_clock_skew`, `request_conflict` and `request_retired`. Neither capacity errors nor response loss are reasons to replace an existing submission identity.

## Queue deadline and node status

`start_before` is an absolute RFC3339 timestamp. `command.timeout_seconds` separately limits runtime after local process preparation. The Controller expires queued work during maintenance and before dispatch; the Node checks again immediately before process launch. Once a task has started, passing `start_before` does not cancel it. An `expired` task has no process exit code. Queue cancellation and queue expiry are reclaimable without waiting for a receipt from an offline device.

`nodes[].online` remains the legacy lease indicator. `status.management` distinguishes an active management request (`connected`), an unexpired lease without a held request (`lease_valid`), `offline` and `never_seen`. Status also includes `accepting_tasks`, `current_task`, `backlog_reason`, and `updated_at`. Backlog reasons include a running task, result/receipt pressure and result persistence failure; `deadline_capability_required` means a queued deadline needs a newer Node. Older nodes may have no detailed observation.

`status.tunnel.state` independently reports `disabled`, `connecting`, `connected`, `disconnected` or `unknown`, with its own `updated_at`. Tunnel transitions wake the management report without increasing the normal idle heartbeat. `connected` means the embedded client completed its authenticated tunnel handshake; it is not a health check of every forwarded destination. If management is unreachable, the last tunnel observation becomes stale. Verify an actual authorized transfer when checking service availability.

## Correlation, filtering and export

`caller_id` is a caller-supplied correlation ID; `owner` is assigned from the authenticated operator. Both use the existing restricted identifier alphabet and a maximum of 96 characters. `tack tasks` supports `--node`, `--state`, `--caller-id`, `--owner`, `--since`, `--until`, `--limit` and `--after`. Time bounds are inclusive RFC3339 creation timestamps. `/v2/tasks` uses the corresponding query names, including `caller_id`. Pagination is by opaque task ID, not time; retain the returned cursor and the same filters. Pages are summaries; fetch individual tasks for output. Filtering historical files uses bounded memory but may scan disk history.

## Restricted operator credentials

Operators access only the private listener, using a separate file containing `{"token":"..."}` with `--credential`. Never give an automation agent the controller config or its administrator token. These permissions authorize task operations, not shell sandboxing: an executing task still has the Node account's privileges.

An administrator creates or replaces an operator with `tack operator-set --config controller.json --file operator.json` plus the normal server/CA flags. The file contains:

```json
{
  "id": "inventory-agent",
  "token": "REPLACE_WITH_A_FRESH_RANDOM_TOKEN_AT_LEAST_32_CHARACTERS",
  "node_ids": ["customer-a-node-01"],
  "read": true,
  "execute": false
}
```

Use an independently generated random secret, e.g. `secrets.token_hex(32)`. For an agent that runs commands and reads results, grant both `read` and `execute`. `read` permits scoped node/task listing and result queries; `execute` permits submission and cancellation on whitelisted nodes. Operators cannot manage credentials or read controller-wide metrics. Both task collections and direct task access enforce the node whitelist. `owner` is for attribution/filtering; it is not an additional isolation boundary between operators authorized for the same node.

Use `tack operators` as administrator to list scopes without secrets. Rotation supplies a fresh token for the **same operator ID** with `operator-set`; old tokens stop authenticating new requests immediately. `tack operator-revoke --id inventory-agent` disables it immediately for new requests. Neither action cancels tasks or requests already accepted. Re-enable by replacing its definition with a fresh token. Disabled IDs are retained to preserve their request namespace; the store is bounded to 256 stable operator IDs and 1024 node entries per scope. Reuse stable IDs through rotation rather than creating one per call. Only token digests are stored in `STATE.operators.json`; keep the supplied credential file private.

## Upgrade and recovery

Controller storage is now v3, adding the request retirement watermark. Back up the state file, complete `.history` and `.uploads` directories, and `.operators.json` together while the Controller is stopped or using a consistent snapshot. Older controller binaries must not write this state. Node journals remain storage v4 but can contain new diagnostics; rolling a live pending journal back to an older release is not supported. A Modern/Legacy switch within this release remains supported. Retain the entire Node state directory.

Upgrade the Controller before Nodes. Do not downgrade or delete state to work around a protocol, credential, disk or queue error. v1–v3 fallback omits v4 diagnostic fields from the wire; full diagnostics and deadline guarantees require a v4 session. A CLI submit with the new fields requires the updated Controller; use matching CLI/controller versions.
