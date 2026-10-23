# Queqiao behavior migration

Reference commit: `ee85fae695d4c8a04e4bf496fae916974f312b65`.
The [ledger](upstream-inventory.json) contains 1,068 named entries from 151
upstream files: Go test/fuzz/benchmark entry points, Python/JavaScript cases,
and the measurement scripts. It records identifiers, local evidence and scope
notes; no upstream test bodies are stored in the ledger.

| Ledger result | Entries | Meaning |
| --- | ---: | --- |
| Adapted | 155 | Independent local assertions/drivers for applicable behavior, with differences stated per entry |
| Existing equivalent coverage | 1 | Existing rotation/restart test already covers the contract |
| Not applicable | 912 | Requires absent product features, upstream wire formats or upstream-specific tools |
| Pending | 0 | No inventoried entry is unclassified |

These are not counts of newly written tests. Several upstream cases map to one
local test, and an adaptation does not claim identical protocol semantics.
The ledger's applicability status is separate from execution evidence.
`scripts/check-fixture-coverage.py --require-complete` rejects missing reasons,
missing local references, duplicate entries, changed entry counts and pending
rows. Both the routine fixture runner and CI invoke it.

## Independent implementation boundary

The first attempt directly imported 20 upstream implementation/test files.
Those files and their three packages have been removed. `ORIGIN.json` and
`LICENSE.queqiao` preserve that historical attribution; old validation reports
continue to identify their old source snapshots. Current code is independently
written against [BEHAVIOR.md](BEHAVIOR.md), with new test inputs and assertions.
The author had previously read upstream code, so this is not a claim of
personnel-isolated clean-room development.

## What now runs here

- Explicit-clock and live UDP fault models, live TCP pacing/pause/reset,
  per-peer policing, finite packet/byte/peer limits and joined shutdown.
- HTTP/1.1 and HTTP/2 result recovery after lost acknowledgements, stalled
  upload, disconnection and cancellation; exact output and single execution.
- Sixteen concurrent application flows recovering through one Chisel carrier,
  forward/reverse tunnels, unsupported SOCKS requests, unreachable UDP
  destinations, and cancellation during a silent SSH handshake.
- Concurrent bounded output, ownership transfer without copying full output,
  backlog admission boundaries, late-dial cleanup, bounded retry windows,
  retained log limits, failure counters and recovery accounting.
- Standalone Node RSS/CPU/FD/storage-write/traffic gates for idle, blocked
  output, outage and recovered idle. Harness memory is excluded.
- Packet/SOCKS/JSON/manifest fuzzing and bounded campaign aggregation.
- Allocation/retention, backlog size, logging/counters, HTTP weak-path and
  SOCKS UDP benchmarks; explicit field HTTP/HTTPS/UDP, metrics snapshots,
  matched endpoint ordering, optional comparison gates and an upload sink.

The upstream's three Go `Benchmark` functions measure BBR state pruning and
insertion into a multipath reorder buffer. Those algorithms do not exist in
this product and are explicitly excluded. Their throughput-competition aim
does not define our acceptance gates. Our benchmarks measure actual Tack
backlog/output operations and actual Chisel transports instead. The upstream
campaign drivers are mapped individually to runnable local tools.

## Bugs and limits made visible

The silent-SSH-handshake test failed against the existing client: cancellation
could leave the WebSocket open before `BindSSH` owned it. The client now closes
that connection when its context ends and on handshake failure. Its regression
uses a real WebSocket peer which deliberately never sends an SSH banner.

Two test-harness defects were also corrected: the HTTP echo benchmark now
consumes its bounded request before responding, avoiding partial HTTP/1 output;
the reverse-tunnel fixture waits for application readiness after SSH readiness.

Tack has no duplicate-suppression authorization logger. Rotation tests bound
retained log files, not cumulative enabled-log writes. It has no QUIC/FEC,
multi-lane scheduler, port hopping, mobile VPN, source-bound DNS resolver or
upstream release/visualizer formats. Those exclusions are stated in the ledger,
not masked by unrelated passing tests.

Native Windows/macOS timing, physical WAN/NAT behavior, overnight soaks, old or
embedded hardware and power-loss/flash endurance require separate environments.
Local short campaigns and cross-compilation do not establish those properties.
Reproduction commands and report interpretation are in [README.md](README.md).
