Tack 0.1.0-rc.2 adds bounded rolling retention and crash recovery for continuous
operation, with lower allocation and storage overhead.

Confirmed task history now expires by age or is reclaimed oldest first to admit
new tasks. Node journals remove acknowledged per-task records after a durable
receipt exchange; a persisted sequence watermark still rejects old commands.
Lost final acknowledgements keep results pinned. When finite capacity is occupied
by unconfirmed work, backpressure preserves that work until it can drain.

Command output keeps a bounded beginning and ring-buffer tail, allocating only
as needed. Optional diagnostic logs rotate by size and file count. Node-owned
workspaces and recognized crash leftovers are reclaimed; complete pending
results are recovered before cleanup. Transient result-write failures retry
without starting another command. Windows state replacement tolerates brief
reader/scanner sharing conflicts with bounded retry, preserving the old file
when replacement cannot commit. Unknown staging data is preserved and counted.

The Controller caches at most 128 eviction candidates and reads only archive
identity headers for receipts. Local benchmarks reduced a 12-byte output's
allocation from 65,632 to 160 bytes, and a receipt lookup over 2 MiB of output
from about 10 MiB to 1.7 KiB. Legacy receipt cleanup batches optional journal
rewrites while keeping execution/result/checkpoint persistence synchronous.

Before upgrading, drain queued work, stop the Controller and back up its state
with all of `.history` (including `released`) and `.uploads`. Upgrade Controller
and CLI before Nodes. Back up each Node's identity, journal and `replay.json`
together. Controller storage version 2 and Node journal version 3 prevent a
binary-only downgrade. Legacy tasks behind the initial replay fence may become
`unknown`; inspect their side effects before resubmitting. Deployment paths,
startup persistence and filesystem quotas remain operator responsibilities.

Fault regressions cover lost acknowledgements, retention beyond count/byte
budgets, a full 10,000-record legacy journal, restart, Controller rollback and
injected persistence failures. HTTP/1.1 and HTTP/2 resource tests each execute
128 real tasks and then 180 seconds of reconnect faults. See
`management/VALIDATION.md` for exact observations and limitations.

This is a prerelease: 36 platform archives plus `manifest.json` and `SHA256SUMS`.
Node uses Go 1.23.12; Controller, CLI and Chisel use Go 1.26.3. Native CI and
cross-compilation do not establish CentOS 6, ARM/MIPS, NAS/overlay or 24–72 hour
reliability. BusyBox is not bundled. Binaries remain ordinary builds, without
obfuscation, packing or signing. A compromised node can read its current token;
it does not grant administrator access. Hardware-backed keys and online
credential enrollment/rotation remain later work.
