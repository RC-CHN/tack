Tack 0.1.0-rc.3 improves low-resource operation and result delivery over intermittent
links, and adds independently implemented network/resource test fixtures.

Node results now live in immutable spool files, with one bounded upload buffer
instead of repeatedly loading complete results. Failed availability probes avoid
reading or hashing the spool. Runtime heap snapshots avoid the global pause of
ReadMemStats, and expired rate-limit waits avoid timer allocations. Routine Node
and Controller diagnostic logging is disabled by default; fatal errors still
reach stderr, and bounded diagnostic logging remains available explicitly.

Uploads reduce their chunk size only after a probe confirms that a failed data
request made no durable progress, down to 4 KiB. Probe-confirmed progress resets
retry backoff even when a data acknowledgement was lost. Offline exponential
backoff and jitter remain in place. Chisel's existing SSH timeout now covers
negotiation and the configuration acknowledgement; cancellation also closes a
connection stalled before tunnel binding.

Local measurements use separate, non-race Go 1.23.12 Nodes with GOMAXPROCS=2:

- One failed probe for a retained 2 MiB result allocates 3,560 B instead of
  104,745 B and avoids reading the result file.
- With 600 ms added RTT, 8 KiB/s per connection/direction and a 500 ms outage
  every four seconds, all three 32 KiB-plus-15-byte transfers complete in each
  HTTP mode. Median completion is 23.850 seconds for H1 and 47.697 seconds for
  H2; each baseline run had zero durable progress within 75 seconds.
- Adding actual 5% packet loss gives H1 completion in 51.973 seconds. The H2
  120-second qualification does **not** complete: 32,771 of 32,783 bytes are
  retained. A separate H2 run completes in 51.973 seconds, showing substantial
  timing variation rather than a universal completion guarantee.
- Existing H1/H2 footprint gates pass. The final 2 MiB-output workloads observe
  at most 15.551 MiB Node RSS/high-water, with zero observed idle/recovered-idle
  stream traffic, CPU ticks, storage writes and default diagnostic logs.

Smaller chunks increase active request/acknowledgement traffic. These results
measure result delivery and fault recovery, not general tunnel throughput,
traffic-fingerprint resistance, universal memory ceilings or zero physical
power consumption. The severe H2 packet-loss failure remains in the published
evidence; it is separate from the release CI's passing regression gates.

The fixture suite covers bounded stream/datagram impairment, recovery,
SOCKS TCP/UDP, output/backlog limits, logging, process resources, fuzzing and
field-campaign accounting. Its upstream behavior inventory records adaptations
and explicitly inapplicable features; it does not claim Queqiao wire or feature
parity. Tag builds also run and retain the fixture benchmarks.

Before upgrading from rc.2, drain queued work and stop the Controller. Back up
its state file together with `.history` (including `released`) and `.uploads`.
Upgrade the Controller and CLI before Nodes. Back up each complete Node state
directory, including `identity.json`, `journal.json`, `replay.json`, `results/`
and recovery files. Node journal **v4** migrates v3 records to separate result
files; older Nodes reject v4 state. Controller storage remains v2 and the wire
protocol remains v3. A binary-only Node downgrade is unsafe; restore a consistent
backup and reconcile task side effects before resubmitting work. Legacy replay
fences can mark old tasks `unknown` rather than execute them again.

This remains a prerelease with 36 archives, `manifest.json` and `SHA256SUMS`.
Node uses Go 1.23.12; Controller, CLI and Chisel use Go 1.26.3. Cross-compilation
does not establish native CentOS 6, ARM/MIPS, NAS/overlay or 24–72 hour reliability.
BusyBox is supplied separately. Binaries are ordinary builds without obfuscation,
packing or signing; credential enrollment/rotation and hardware-backed keys
remain later work.

See the [validation record](https://github.com/RC-CHN/tack/blob/tack-v0.1.0-rc.3/management/VALIDATION.md),
[resource comparison](https://github.com/RC-CHN/tack/blob/tack-v0.1.0-rc.3/management/validation/2026-10-23-resource-optimization/README.md)
and [severe-link evidence, including failures](https://github.com/RC-CHN/tack/blob/tack-v0.1.0-rc.3/management/validation/2026-10-23-severe-link/README.md).
