# Independent test migration specification

Reference: Queqiao `ee85fae695d4c8a04e4bf496fae916974f312b65`.
`upstream-inventory.json` lists test identifiers and measurement drivers, not
test bodies. A mapping is evidence of a specific behavior, not a claim that
different protocols implement identical machinery.

The author previously inspected upstream source and copied three fixture
packages. The replacement work is independently authored against the behavior
specification below; it is **not personnel-isolated clean-room development**.
The historical import manifest and license are retained for traceability.
No upstream implementation or test body is an input to the replacement code.

## Network model

Use an explicit simulation clock. Accept bounded datagrams from bounded peers,
schedule deliveries, and expose dropped/delivered/queued byte counters. Equal
timestamps preserve submission order unless jitter changes delivery time.
Identical seeds and submissions produce identical decisions. Independent loss,
consecutive loss bursts, direction-specific loss, changing loss, variable delay,
bandwidth sharing and per-peer limits must be separately observable. Random
loss after serialization still consumes link capacity. A full queue rejects
new work without retaining its payload. Draining or closing clears ownership.
Simulation tests must not sleep or allocate one goroutine per packet.

A live TCP adapter preserves byte order and half-close, applies delay and rate
limits, and bounds both active connections and queued chunks. It supports
independent directional pauses, abrupt disconnect, offline rejection, and
cancelable dialing. TCP stream manipulation is not a packet-loss simulation;
actual TCP loss remains an isolated Linux netem experiment.

## Product resource and recovery contracts

- Output storage remains within its configured budget under concurrent writes.
  Ownership transfer releases buffers without copying full-sized output again.
- Backpressure stops admission before another maximum-sized result is possible.
  Acknowledgement and cancellation restore progress and release retained state.
- A slow reader cannot block unrelated control/cancellation work indefinitely.
  All waits and writes have a cancellation path; shutdown joins workers.
- Many application flows share the carrier reconnect loop. Concurrent recovery
  cannot multiply physical dials by the number of waiting flows. Canceled work
  cannot initiate new connections or revive an old session.
- Retry delays are bounded, jittered, and cancelable. DNS, TLS, timeouts and
  transport failures stay distinguishable without leaking credentials.
- Logs have bounded retained bytes. Measure write amplification separately:
  rotation alone does not prove a bound on lifetime disk writes.
- Measure a standalone non-race Node's RSS, CPU, disk writes, idle traffic,
  descriptors and recovery; do not include the harness in the process sample.

## Benchmarks and campaigns

Report allocations and retained bytes alongside time. Include tiny and bounded
maximum output, backlog scaling, protocol encoding, churn, control latency
during bulk transfer, concurrent flows, and reconnect recovery. Include every
failed or partial transfer in results; success requires exact size and digest.
Matched trials alternate endpoint order. A summary must state completed and
attempted counts, latency quantiles, measurement duration and source identity.
External endpoints are explicit inputs, never automatic public-site defaults.
Long-running drivers need finite per-attempt timeouts and bounded in-memory
aggregation. A short local run is not evidence of overnight field stability.

## Coverage accounting

Inventory statuses are `pending`, `covered`, `adapted`, and `not_applicable`.
Covered/adapted entries require local test or driver references and a precise
scope note. Exclusions require a reason: do not introduce QUIC/FEC/mobile VPN,
GeoIP, upstream deployment tools, or upstream's visualization UI merely to make
their tests runnable. Their transferable resource/fault behavior is accounted
for separately. No pending entries may be silently counted as completed.
