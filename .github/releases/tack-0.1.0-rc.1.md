Tack 0.1.0-rc.1 is the first candidate release of the independent control plane,
operator CLI and customer Node runtime, alongside the Chisel tunnel binary.

It supports explicitly targeted non-interactive commands, separate node/admin
credentials and ingress, Linux process-group and Windows Job Object cleanup,
bounded stdout/stderr, durable execution intent and cancellation. HTTPS long
polling defaults to a 30-minute heartbeat with ±50% jitter while task creation
and cancellation wake the waiting request immediately. Result uploads use a
separate connection pool and resume at durable offsets after interruptions.

This candidate fixes retained command/output payloads, HTTP/2 shutdown socket
cleanup, pending dial cancellation and disconnected tunnel wait goroutines.
Slow network clients and state writes no longer hold the Controller's read
mutex. Completed history is stored on disk, active work and accepted result
capacity are bounded, and task lists use paginated summaries. Upload/control
concurrency and retained node metadata are bounded before large allocations. Staging cleanup,
migration and failed persistence have regression coverage.

Before upgrading, stop the Controller and back up its state file together with
`.history` and `.uploads`; preserve every Node's identity and journal. Legacy
Controller snapshots migrate at startup. The new storage format and v2 wire
fields prevent a binary-only downgrade. `tack tasks` now returns a paginated
object; use its `next_cursor` with `-after`, and `tack get` for complete output.
At configured capacity the Controller refuses new submissions; the Node stops
accepting new work at 10,000 retained task identities. Records are never silently
deleted to bypass these limits.

The release includes 36 platform archives, `manifest.json` with the source
commit/toolchains, and `SHA256SUMS`. Verify checksums before installation.
Deployment and startup persistence remain external to the Node.

This is a prerelease. Native CI and cross-compilation do not establish native
CentOS 6, NAS/overlay, ARM/MIPS or 24–72 hour reliability. Node uses Go 1.23.12;
the controller, CLI and Chisel tunnel use Go 1.26.3, so the tunnel's compatibility
must be checked separately. BusyBox is not bundled. These are ordinary builds,
without obfuscation, packing or signing. Hardware-backed credentials, online
enrollment/rotation, tenancy/RBAC and replay-safe journal rollover remain later
work. A compromised node account can read that node's current token; it does not
grant administrator access.

See the repository's `management/README.md`, `management/VALIDATION.md` and
`docs/releases.md` for operation, actual test evidence and release procedure.
