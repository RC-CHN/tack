# Network and resource test fixtures

These independent test packages run in a separate Go 1.23 module with no
external dependencies. Production Node and Chisel binaries do not import them.

## Independent migration and provenance

The reference is [Queqiao](https://github.com/bojieli/queqiao) commit
`ee85fae695d4c8a04e4bf496fae916974f312b65`. The earlier direct import has been
removed: no `pathsim`, `lossmodel`, or `udperr` package remains. `ORIGIN.json`
and `LICENSE.queqiao` are historical records of that import, not a declaration
that current implementations came from those files. Reports under
`management/validation/2026-10-23-fixtures` describe that earlier implementation.

[BEHAVIOR.md](BEHAVIOR.md) is the independent behavioral specification.
[MIGRATION.md](MIGRATION.md) summarizes the mapping, implementation boundary and limits.
[upstream-inventory.json](upstream-inventory.json) records 1,068 upstream entry
points: 942 Go tests, 11 fuzz targets, 3 Go benchmarks, 89 Python tests, 13
JavaScript tests and 10 measurement drivers. The ledger maps 155 entries to
independent adaptations and one to existing equivalent coverage; 912 are
explicitly inapplicable to this product (QUIC/FEC/lane mechanics, mobile,
upstream release formats and similar absent features). There are no pending
rows. These counts are upstream entries, not numbers of new local test files.
Every adaptation states its limits; this is not wire/feature parity with Queqiao.
The author previously saw upstream source, so this is independent behavioral
reimplementation, not personnel-isolated clean-room development.

| Package | Behavior and limits |
| --- | --- |
| `linklab` | Explicit-clock datagram model plus a bounded live UDP adapter; seeded independent/burst/asymmetric/live-changing loss, delay/jitter/wander, aggregate/per-peer serialization, optional per-peer drop policing, burst allowance, loss placement and cleanup. Default modeled queue: 256 KiB, 1,024 packets, 32 peers. UDP ingress: 16 datagrams; one socket reader per admitted peer and one scheduler. |
| `faultnet` | Live loopback TCP proxy with directional delay/rate limits, bounded pipeline, half-close, per-direction pause, reset, offline rejection, active connection ceiling and joined shutdown. Default at most 16 connection pairs; each direction has six 16 KiB chunks, including producer and consumer buffers. |
| `resource` | Linux external-process RSS/high-water, CPU ticks, storage writes, descriptors and thread samples. Does not force Node GC or include the test process/children. |

Delay and rate in the live proxy apply per TCP connection and direction. The
explicit-clock model also tests a bottleneck shared by peers. Their scheduling
semantics differ and benchmark reports must identify which model was used.
Neither byte-stream pausing nor datagram simulation causes real TCP packet
retransmissions.

For actual TCP packet loss on Linux, use `scripts/test-weaknet-netns.sh` with a
prebuilt test executable. It creates an isolated user/network namespace and
applies netem only to its loopback. It requires `unshare`, `ip`, `tc`, user
namespaces and kernel netem; there is no fallback to changing host interfaces.
Its impairments affect all traffic inside the namespace, including fixture
administration traffic.

```sh
(cd management/integration && go test -c -o /tmp/tack-integration.test)
TACK_NETEM_DELAY_MS=10 TACK_NETEM_LOSS_PERCENT=1 \
  ./scripts/test-weaknet-netns.sh /tmp/tack-integration.test \
  -test.run '^TestManagementThroughImpairedTransport$' -test.timeout 3m -test.v
```

## Routine and repeated checks

```sh
(cd test/fixtures && go test -race -short ./... && go vet ./...)
python3 scripts/check-fixture-coverage.py --require-complete
(cd management/integration && go test -race -run '^TestManagementThroughImpairedTransport$' -v .)
go test -race -run '^TestTunnelRecoversAcrossImpairedTransport$' -v ./test/e2e
(cd management/protocol && go test -run '^$' -fuzz '^FuzzManagementDecode$' -fuzztime 10s)
```

Use `scripts/test-fixtures.sh /tmp/tack-fixture-report 30 3` for checksummed
reports, three recovery repetitions, and 30 seconds in each of four resource
phases for both HTTP versions. The output directory must be new. Set
`TACK_NODE_GO` to a Go 1.23.12 executable to measure the legacy Node toolchain;
the default is the `go` on PATH. The script builds a non-race Node independently
of the race-enabled test driver and records source identity and exit status.

The Tack CI matrix runs portable short fixtures on Linux, Windows and macOS;
legacy CI compiles/runs the fixture module on Go 1.23. Tag/manual resource jobs
run the separate-process gates. These configuration statements are not evidence
that remote CI or native device checks have already run.

## Benchmark and field campaign drivers

Run `scripts/benchmark-fixtures.sh /tmp/tack-benchmarks 250ms 3` for three
repetitions of allocation/retention, backlog scaling, logging, packet scheduling,
HTTP clean/weak-path and SOCKS UDP direct/forward/reverse benchmarks. Reports
include complete raw Go JSON logs, source hashes, toolchain and exit status.
`8x 1` is a useful smoke run, not a stable performance baseline. Allocation
figures for end-to-end tests include the in-process harness; standalone Node RSS
gates below isolate the actual product process. Packet-model benchmarks measure
the harness itself. No peak-throughput ranking is a default release gate.

The Node upload benchmark includes a deterministic unavailable probe, an
already-committed result, and a complete 2 MiB transfer. The transfer's allocation
figures include the in-process HTTP responder; they are not standalone Node
allocations. `read-B/op` is Linux `rchar` (cached reads included), with a small
measurement overhead, not physical disk reads. Runtime snapshots and ready
rate-limit waits have separate allocation benchmarks.

`go run ./test/cmd/fieldbench -config campaign.json -output /tmp/new-evidence`
uses already-running endpoints. It never chooses public sites automatically.
Example availability campaign (replace the explicit target and SOCKS address):

```json
{
  "targets": [{"name":"health", "url":"https://your-test-host/health", "bytes":-1}],
  "endpoints": [{"name":"tack", "socks":"127.0.0.1:1080"}],
  "duration_seconds":3600, "interval_ms":5000, "timeout_ms":5000,
  "flows":1, "min_success":0.99, "final_successes":5
}
```

For exact transfers, set nonnegative `bytes` and the expected lowercase `sha256`.
Availability probes (`bytes:-1`) are not counted as verified goodput. Set
`udp_echo` to a controlled echo endpoint to keep one association per SOCKS
endpoint alive for the whole run, including through failures. It validates
sequence and payload instead of relying on a public DNS resolver. Add another
endpoint for alternating matched trials; optional `compare_to`,
`min_completion_ratio` and `min_goodput_ratio` gate against that endpoint using
separately measured wall time. Every endpoint must complete some work even if
`min_success` is zero. Failed/partial attempts are retained in JSONL; latency
quantiles cover the latest 4,096 attempts per endpoint/target, including failures.
All-attempt totals and all-target-failed round counts cover the full campaign.

Optional `pid` and `max_rss_bytes` sample/gate an external Linux process.
`metrics_url` captures bounded Tack JSON metrics; `metrics_token_env` names an
environment variable containing its token. Metric failures are explicit gaps.
The manifest records runtime build metadata and configuration digest, not URLs
or tokens. Keep the original configuration separately to reproduce the run.
Use the benchmark runner's source manifest for uncommitted-source identity;
standalone Go build metadata may omit a revision.

`go run ./test/cmd/bench-origin` starts a one-request, one-minute loopback HTTP
endpoint and prints its address. `/bytes?n=N` emits bounded zero-filled data;
`POST /upload` requires bounded Content-Length and returns observed byte count
and SHA-256 while hashing the body as a stream. `-requests`, `-duration`,
`-max-bytes`, and `-listen` explicitly change these limits. The field driver
currently measures GET/UDP; the upload sink can be driven by an explicit HTTP
upload client. No multipart application generator or third-party binary
installer is included.

Enabled logging tests bound retained files and preserve the recovery record.
They do not imply duplicate suppression or a limit on cumulative storage
writes. Tack's default-disabled logging is independently checked by the
standalone footprint test.

## Tack footprint gates

`TestNodeLowFootprint` requires Linux `/proc`, `getconf`, `TACK_NODE_BINARY`, and
`TACK_RESOURCE_SECONDS` (3..86400 seconds **per phase**). It rejects race-built
Node executables. `TACK_RESOURCE_HTTP2=1` requires observed HTTP/2 requests;
otherwise the test server permits only HTTP/1.1. `TACK_RESOURCE_REPORT` retains
all samples and a pass/fail field, including failed gates.

Set `TACK_RESOURCE_MAX_OUTPUT=1` to use the protocol maximum of 1 MiB on each
output stream (2 MiB per task, two pending results). The default remains the
smaller 160 KiB stdout workload. Reports record the workload size, cached read
bytes, recovery to three verified results, and shutdown time. To compare two
normal Node binaries with one shared fixture, run:

```sh
GOMAXPROCS=2 scripts/compare-node-footprint.sh /tmp/before-node /tmp/after-node /tmp/new-comparison 10 3
```

This runs each binary three times for both H1 and H2, alternating their order,
with 10 seconds in each phase. It retains both binary hashes/build settings,
fixture source hashes, raw samples and logs, and an explicit overall exit code.
Use the same Node compiler/build flags and leave GC/memory settings unchanged
between binaries. Long recovery times include the unchanged retry jitter and
backoff. A short comparison does not establish a recovery percentile.

The test warms a Node, measures idle, blocks output while filling a two-result
backlog, disconnects the transport, restores it, verifies exact output and one
execution per task, and measures recovered idle. It does not force Node GC.
The queued third task must stay queued while backpressure applies.

| Gate | Current default |
| --- | --- |
| Node RSS and kernel lifetime high-water mark | At most 32 MiB; override explicitly with `TACK_NODE_MAX_RSS_MIB`, recorded in the report |
| CPU per phase | At most 0.20 CPU seconds + 2% of phase wall time; ticks converted with the host's `CLK_TCK` |
| State without new work | Unchanged logical file bytes, at most 64 KiB additional kernel-accounted storage writes per phase |
| Routine diagnostic output | Zero bytes with default logging disabled |
| Idle/fully offline stream traffic | At most 4096 + 16 × phase seconds bytes, both directions combined |
| Retry attempts while fully offline | At most 8 + 2 × floor(phase seconds / 5), counting both control/upload transports |
| FD count | At most baseline +4 within a phase; recovered idle at most original baseline +2 |
| Recovered RSS | At most original idle baseline +8 MiB, also subject to the absolute cap |
| Shutdown | Exit within 3 seconds with a long poll pending |

These are regression budgets for this workload, not universal device promises.
RSS includes runtime/native memory; it is sampled separately from Node children.
The report also samples the kernel-reported `VmHWM`; neither these observations
nor a finite sampling run establish a universal peak-memory ceiling. TLS stream counters
exclude IP/TCP overhead and retransmissions; `/proc` storage writes do not count
physical flash wear. Main-thread context switches are recorded, not labelled as
whole-process wakeups.
The short campaign does not establish 24-hour stability, NAT behavior, actual
CentOS 6/ARM/MIPS support, disk-full behavior or physical power-loss durability.

## Severe intermittent link qualification

`TestNodeSevereLink` runs an external, non-race Node against the real Controller.
It adds 300 ms delay per direction, limits each connection/direction to 8 KiB/s,
and drops all proxy connections for 500 ms every four seconds. The bounded proxy
has two queued 2 KiB chunks per direction, plus producer/consumer buffers.
Each run verifies a 32 KiB stdout plus 15-byte stderr result and exactly one
command execution. Measurements start at the first upload probe, after command
execution; completion includes upload, retry and reconnect time, but not initial
registration/task delivery. The controller, command child and proxy are outside
the measured Node. Task observation runs in-process outside the impaired link.

```sh
(cd management/node && go build -trimpath -o /tmp/tack-node ./cmd/tack-node)
(cd management/integration && go test -c -o /tmp/tack-integration.test)
GOMAXPROCS=2 TACK_NODE_BINARY=/tmp/tack-node TACK_RESOURCE_HTTP2=1 \
  TACK_SEVERE_SECONDS=75 TACK_SEVERE_REQUIRE_COMPLETE=1 \
  TACK_SEVERE_REPORT=/tmp/severe-h2.json \
  /tmp/tack-integration.test -test.run '^TestNodeSevereLink$' -test.v -test.timeout 3m

# Add real TCP packet loss in a disposable namespace (repeat with HTTP2=0):
GOMAXPROCS=2 TACK_NODE_BINARY=/tmp/tack-node TACK_RESOURCE_HTTP2=1 \
  TACK_SEVERE_SECONDS=120 TACK_SEVERE_REQUIRE_COMPLETE=1 \
  TACK_SEVERE_REPORT=/tmp/severe-loss-h2.json \
  TACK_NETEM_DELAY_MS=2 TACK_NETEM_LOSS_PERCENT=5 \
  TACK_NETEM_RATE_KBIT=4096 TACK_NETEM_SEED=7 \
  scripts/test-weaknet-netns.sh /tmp/tack-integration.test \
  -test.run '^TestNodeSevereLink$' -test.v -test.timeout 3m
```

`TACK_SEVERE_SECONDS` accepts 20..600 seconds. Without
`TACK_SEVERE_REQUIRE_COMPLETE=1`, an incomplete baseline may pass the integrity
and resource gates: always inspect the separate `completed` and `progress_bytes`
fields. A completed result is byte-verified; retained partial bytes are only
durable-progress observations. Raw reports include attempt times and lengths,
TLS stream bytes, process samples, FD/RSS maxima, diagnostic bytes and shutdown.
Resource gates are 32 MiB RSS/high-water, baseline+4 FDs, zero default diagnostic
bytes and exit within three seconds. Stream bytes exclude TCP/IP retransmission;
the netem runner separately retains actual packet/drop counts in its log.

`BenchmarkStalledHandshake` independently measures a silent SSH peer with a
100 ms configured handshake timeout and a 500 ms external watchdog. Watchdog
expirations are failures, not successful timeout completions. Companion race
tests cover the silent config ACK, reconnect, cancellation, and a successfully
established tunnel outliving the handshake timer.
