# Tack retention and recovery

Protocol v3 adds rolling retention while retaining v1/v2 endpoints for staged
upgrades. New control requests use `/v3/node/sync`; bounded result transfer
continues to use the v2 chunk endpoint. An old session cannot opt into safe
compaction simply by deleting its journal.

## Execution and acknowledgement order

1. The Controller allocates a monotonically increasing per-node `sequence` in
   the same durable snapshot that accepts a task. Dispatch follows sequence order.
2. Before running a new task, the Node flushes `replay.json` with that sequence,
   then persists execution intent in `journal.json`. A sequence at or below the
   watermark without a live journal record produces `unknown`, never execution.
3. The Node saves the complete result before uploading it. A transient journal
   write failure retains the in-memory result and retries without starting another
   command. The Controller flushes chunks, verifies the digest, commits the full
   history record and active snapshot, and removes staging before the final ack.
4. After receiving that ack, the Node persists a small acknowledged receipt.
   It may now discard its local command/output copy. It sends up to 128 receipt
   IDs per control request; the Controller moves their completed history files
   into `.history/released` and flushes the directory changes before replying.
5. Only after the receipt reply does the Node remove each per-task record. The
   durable watermark continues to reject replay after records are removed. Deleting
   acknowledged receipts is batched until receipt draining finishes or another
   durable transition writes the journal; a crash can only resend old receipts.

A lost final ack leaves the full Controller result pinned and the Node result
available for retry. A lost receipt reply leaves only a retryable Node receipt;
the Controller already has proof that its final ack was received. Repeated
receipts are harmless, including after the corresponding history has expired.
Partial uploads, active commands and unconfirmed results never enter the
reclaimable directory. Cancellation before dispatch needs no Node receipt.

## Disk and memory bounds

The default Controller budgets remain 100,000 historical/reserved tasks and
1 GiB of history plus conservative accepted-work reservations. Before admission,
oldest receipt-confirmed files are reclaimed until the next task fits. Confirmed
results older than 30 days expire at startup and on the hosted process's
one-minute maintenance tick. `history_retention_seconds`, `max_history_tasks`
and `max_history_bytes` are configurable. A cache retains at most 128 oldest candidates and is refreshed by maintenance;
admissions reuse it until exhausted. Receipt ownership reads only a bounded
archive header, not the output payload. No history payload cache is retained. Queries are paginated and may omit concurrently evicted
records; a removed result returns 404.

Finite capacity cannot preserve unlimited unconfirmed results during an unlimited
outage. If active/unconfirmed work occupies the remaining budget, new execution
is paused and submissions can receive 503 while accepted work drains. Ordinary
confirmed-history accumulation is reclaimed automatically. Protected orphan
uploads left by an inconsistent restore are counted against capacity and kept
for reconciliation. These logical budgets are not filesystem block quotas;
atomic replacement, filesystem metadata and recovery need additional headroom.

Each stdout/stderr buffer keeps the first ceil(limit/2) bytes and a circular
tail of floor(limit/2) bytes. Storage grows on demand up to the cap; empty and
short output do not preallocate the full configured limit. `*_truncated` means the middle was omitted; bytes
are concatenated without adding a textual marker. The default limit remains
64 KiB per stream, with a maximum of 1 MiB. Diagnostic logs can use a separately
chosen rotating path: `-log-file PATH -log-max-bytes 4194304 -log-files 4`.
Results, checkpoints and pending uploads never use this lossy rotation mechanism.

## Crash recovery and migration

Node-owned temporary workspaces live under the chosen work directory in a
persistent-instance-specific subdirectory. Only its `task-*` entries are swept;
unrecognized entries cause an error. The runtime never chooses a deployment
volume, installs a service or deletes arbitrary files in the supplied parent.
Unowned pre-upgrade workspaces require operator inspection. A command can write
other files using its account permissions; hard bounds on those writes require
deployment-level storage quotas.

Atomic writes use a fixed pending filename per destination, avoiding one new
temporary filename on every crash. Complete pending Node checkpoints/results
are recovered before deletion; a recovery write failure preserves its source.
Recognized incomplete Controller archive writes can be discarded because no
final ack was issued. Completed archives win over stale active snapshots, so a
restart does not resurrect work that already finished. Unknown upload files
are not assumed disposable.

Upgrade the Controller and CLI before Nodes. Drain queued work first. The first
Controller storage upgrade scans existing archives one at a time to release
old cancellations that were never dispatched and therefore need no Node receipt. A legacy
Node's first v3 handshake fences all already allocated sequences before retiring
legacy IDs. As large legacy maps drain, the Node shrinks their bucket storage
geometrically; small normal-workload maps are reused. Old tasks without sufficient evidence become `unknown`; inspect
their side effects before deciding to submit another task. Protocol v1/v2 keeps
its old tombstone limits until that Node is upgraded.

Back up Node identity, journal and replay checkpoint as one unit. The v3 journal
wrapper intentionally prevents older binaries from ignoring a new watermark.
Back up Controller state together with all of `.history` and `.uploads`. On
Controller rollback, a live Node raises its sequence allocation floor; commands
behind that floor may be refused. Restoring or deleting the Node's own execution
evidence, or losing volatile storage, cannot preserve replay protection. A
process killed before output reaches durable storage may only report `unknown`;
at-most-once execution does not promise completion or recover RAM from a failed
disk. Physical power-loss behavior still needs validation on the target device.
