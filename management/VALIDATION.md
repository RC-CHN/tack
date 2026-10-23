# Tack validation record

Recorded on 2026-10-23 for the initial management implementation. This records local checks; it is not a claim that hosted CI or every target device has passed.

The module tests, race checks, vet, HTTPS integration and all 13 cross-build configurations below were rerun successfully after the Tack rename. Native builds of `tack-control`, `tack-node` and `tack` passed; CLI smoke checks verified both the new `-node-config`/`-node-ca` flags and their legacy aliases. The renamed `tack-deploy` skill metadata and local documentation links also passed validation.

The test host was Linux amd64, kernel `7.0.0-38-generic`. Native race checks and vet used Go 1.26.3. Legacy module tests and Node cross-compilation used Go 1.23.12.

| Check | Result |
| --- | --- |
| `go test -race ./...` and `go vet ./...` in protocol, control, node and integration modules | Passed |
| HTTPS Node communication with a private CA and a separate administrator listener; targeted execution and cancellation | Passed on the Linux test host |
| Node credential isolation, inaccessible administrator routes on Node ingress, durable queue reload, duplicate result acknowledgement | Passed |
| Repeated task delivery without repeated execution; interrupted intent reported as unknown | Passed |
| Output limits, stderr/exit status, timeout and cancellation, literal direct arguments, BusyBox invocation fallback | Passed; fallback test uses a controlled test executable, not a packaged BusyBox release |
| Provisioning distinct node credentials while retaining existing nodes; rejecting duplicate identities and existing config outputs | Passed |
| Protocol and Node `go test ./...` using Go 1.23.12 | Passed on the modern Linux test host |
| Deployment skill metadata validation | Passed |

All Node cross-builds used `CGO_ENABLED=0` and `-trimpath`:

| OS | Architectures/configurations | Result |
| --- | --- | --- |
| Linux | amd64, 386, arm64 | Compiled |
| Linux | ARM `GOARM=5`, `6`, `7` | Compiled |
| Linux | mips, mipsle with `GOMIPS=softfloat` | Compiled |
| Linux | mips64, mips64le with `GOMIPS64=softfloat` | Compiled |
| Windows | amd64, 386, arm64 | Compiled |

Native Windows Job Object/process handling and PowerShell encoding tests are configured in CI but have not run in this workspace. CentOS 6's real 2.6.32 kernel, ARM/MIPS devices, NAS/overlay storage and reboot persistence have not been validated. Go 1.23.12 compilation alone does not establish support for these targets.

Certificate enrollment and revocation, hardware-backed keys, customer/RBAC isolation, Chisel supervision, actual BusyBox bundles, obfuscation, packing and signed commercial releases remain later-stage work. Current tests do not establish credential secrecy on a compromised node or production network isolation.


## Link and output extension — 2026-10-23

The final local worktree passed `go test -race ./...` and `go vet ./...` in all four management modules with Go 1.26.3. Protocol and Node tests also passed with Go 1.23.12. All 13 Node cross-build configurations in the table above were rebuilt successfully with that legacy toolchain, `CGO_ENABLED=0` and `-trimpath`. Native `tack-control` and `tack` builds passed. These are local results; this section does not claim a hosted CI run or native Windows/embedded execution.

The added tests exercise these behaviors:

| Evidence | Verified behavior |
| --- | --- |
| `control/TestLongHeartbeatDoesNotDelayDispatchOrCancellation` | A 2700-second waiting request wakes promptly for task creation and cancellation; it does not wait for the heartbeat deadline |
| `integration/TestIdleControlOutlivesShortServerWriteTimeout` | Long polling survives the fixture's 500 ms default HTTP write timeout; a command submitted after 750 ms idle succeeds without a control failure |
| `integration/TestBlockedUploadStillDispatchesAndCancelsWithLongHeartbeat` | While a result upload is blocked, another task starts and is canceled; at the configured two-result backlog limit a third task stays queued until uploads drain |
| Same HTTPS fault test | A durable 64 KiB chunk loses its HTTP acknowledgement, a subsequent probe receives HTTP 503, then transfer resumes at the confirmed offset; exact 160 KiB stdout and separate stderr are recovered, with one execution marker |
| `integration/TestNodeRestartResumesOutputWithoutReexecution` | Node restart with persisted identity/journal resumes after the first durable chunk, without waiting for the long identity lease or executing the command again |
| `control/TestUploadResumeAfterControllerRestartAndLostAck` | Controller restarts preserve partial offsets and final acknowledgement; byte-identical duplicate chunks are idempotent |
| `control/TestUploadAuthorizationLimitsAndDigest` | Other-node/admin credentials cannot upload as this node; invalid offsets, conflicting bytes and oversized chunks are rejected; digest mismatch resets the transfer without acknowledging completion |
| `control/TestV1LostFinalAckCanFinishThroughV2Upload` and legacy wire tests | Lost v1 final acknowledgement reconciles through v2; v1 responses/requests omit new wire fields; existing node tests exercise v1 fallback |
| `node/TestReplayedPendingTaskResendsMetadata` | A replay after controller state restoration re-reports retained metadata instead of executing the task or suppressing recovery behind a stale metadata acknowledgement |
| Node backpressure, rate-wait and telemetry tests | Result count/byte reservation, cancellable pacing, heartbeat defaults/jitter bounds, metadata bounds, request failure/cancellation accounting and state JSON write accounting |
| Built-binary loopback smoke check | `tack metrics` returns the running node and controller counters; SIGTERM shuts down the controller with a long poll pending (observed 0.003 seconds locally) |

The final race-enabled HTTPS fault run observed about **50 ms** from cancellation submission to observing the canceled task, with **27 ms** recorded between controller cancellation and terminal-report timestamps. Output transfer took approximately **3.4 seconds** including deliberate faults and retry backoff. These are observations from a loopback fault-injection test, not a cross-border latency SLA, NIC QoS guarantee or benchmark of CentOS 6/NAS/router hardware.

Faults are injected at HTTP request/response boundaries: blocking, bounded delay, connection abortion after durable processing and HTTP 503. Actual packet loss/reordering, NAT expiry, proxy maximum-duration policies, bandwidth contention, power loss and device flash behavior still require deployment-specific tests. Metrics label long-poll duration as request duration, not RTT, and JSON payload writes as application state writes, not physical disk writes. Live streaming output, persistent metrics history, automatic retention and orphan upload cleanup remain out of scope for this batch.

## Resource lifecycle and fault recovery — 2026-10-23

This pass found and fixed actual retention/lifecycle problems: acknowledged Node journal entries retained scripts, arguments and environment; output upload allocated another full stdout/stderr buffer; HTTP/2 shutdown could leave a connection open because its canceled stream was not idle yet; and slow control/admin clients could hold the controller's global queue mutex during network I/O. Node shutdown now cancels pending dials and closes its owned sockets before waiting for background workers. Task workspace cleanup errors are surfaced in the result. Compact journal entries retain task identity for replay suppression.

Inspection of the original tunnel also reproduced a disconnected-wait leak: 100 unsuccessful `Ready` calls retained about 100 helper goroutines until a connection eventually arrived. Cancelable waits now select directly on a shared completion channel, and connection-wait/ping timers are explicitly stopped. The regression covers both caller cancellation and timeout, and completion-generation tests cover reconnect reuse.

| Test/evidence | Observed result |
| --- | --- |
| Node `TestRepeatedShutdownReleasesConnectionsAndBuffers` | HTTP/1.1 and HTTP/2 each passed 24 shutdown cycles with control/upload requests blocked and a retained 2 MiB result. Every cycle closed all server-observed connections and request handlers within 3 seconds. After warmup, goroutines stayed 4 → 4 and Linux FDs 8 → 8 for both protocols; post-GC heap grew about 69 KiB and 367 KiB respectively. |
| Node lifecycle/retention tests | Shutdown cancels and joins an in-progress dial; upload chunks preserve stdout/stderr boundaries; v1/v2 acknowledgements and startup migration discard acknowledged command payloads. |
| Controller slow-client tests | Blocked response writes and administrator request bodies no longer prevent unrelated queue/metrics requests. Both cases failed before the lock refactor. |
| Persistence fault tests | Failed intent persistence prevents execution; a controller commit failure rolls back in-memory state; failed final-result commit retains staging and does not acknowledge completion, then recovers on retry. Failed JSON writes leave no temporary file. Fault injection replaces the destination with a directory; this is not a real ENOSPC or power-loss test. |
| Go checks | Node, controller and integration `go test -race ./...` and `go vet ./...` passed after the fixes. Node tests also passed with Go 1.23.12; all 13 Linux/Windows cross-build configurations above compiled again. |
| Original tunnel regressions | The disconnected-wait regression failed before the fix (about 100 extra goroutines per 100 waits) and passed afterward. Root-module `go test -race ./...` including TCP/UDP/SOCKS end-to-end tests and `go vet ./...` passed after the wait/timer changes. |

Two final race-enabled HTTPS runs each executed **64 real bounded-output tasks**, verified exactly 64 execution markers, compacted the journal to about **50 KiB**, and left the task workspace empty. Each then held task history constant for **180 seconds** while injecting connection abortions and HTTP 503 responses. Raw samples are retained in [HTTP/1.1 report](validation/2026-10-23-resource-http1.json) and [HTTP/2 report](validation/2026-10-23-resource-http2.json).

| Measurement | HTTP/1.1 | HTTP/2 |
| --- | --- | --- |
| Total test duration, excluding Go test exit delay | 206.26 s | 208.90 s |
| Control / upload requests | 627 / 138 | 629 / 138 |
| Injected connection abortions / HTTP 503s | 92 / 102 | 93 / 103 |
| Post-GC live heap during reconnect phase | 3.14–3.18 MiB | 3.30–3.35 MiB |
| Live goroutines / Linux FDs | 8 / 12 throughout | 11 / 14 throughout |
| Goroutines / FDs after closing fixture | 2 / 7 | 2 / 7 |
| RSS range during reconnect phase | 94.3–96.8 MiB | 98.6–110.2 MiB |

These measurements include the controller, Node, TLS servers and test harness in one process, with race instrumentation enabled. They are **not** per-agent memory requirements. HTTP/2 RSS rose by about 11.7 MiB while post-GC live heap did not grow; allocator/race overhead may contribute, but the short run does not establish the cause or prove RSS has plateaued. The soak's coarse pass gates are at most 8 MiB additional post-GC heap and 8 additional goroutines/FDs; RSS is recorded, not gated. There was no sampled goroutine/FD accumulation in either final run.

The controller still keeps all task history/results in memory and rewrites its entire JSON state under the queue lock. This is deliberate data retention with unbounded capacity growth, and remains unsuitable for unlimited continuous history without a retention/storage design. These runs do not establish 24–72 hour leak freedom, native Windows handle stability, CentOS 6/ARM/MIPS device reliability, disk-full or power-loss recovery, nor endurance of the Chisel data tunnel under sustained traffic. Longer-run commands and limits are documented in [management README](README.md#resource-lifecycle-and-endurance-tests).

## Candidate storage and release hardening — 2026-10-23

This section supersedes the earlier all-history-in-memory limitation. Completed tasks now live in individual disk history files, with a bounded active snapshot, bounded summary pages and count/byte admission limits that reserve output capacity before accepting work. Existing full snapshots migrate one task at a time. Committed reads/status do not hold the mutation lock while state is written. Archive and snapshot failures never acknowledge completion; restart reconciles a completed archive with an older pending active entry. History is not silently deleted.

Local race-enabled tests cover 220 archived results while holding no completed entries in the active map, at most 2 MiB additional post-GC heap after warmup, pages of 13 entries with no missing/duplicate IDs, and an active snapshot below 256 bytes after completion. Count/byte capacity, legacy migration, staged-output cleanup, lost acknowledgements, injected ENOSPC, and 40 concurrent completion/cancellation pairs passed. ENOSPC is injected at the storage call boundary; no shared filesystem was filled.

The final preflight built and integrity-checked all 36 planned platform archives with Go 1.23.12 for Node and Go 1.26.3 for the other components. Native versions and target build metadata matched. These local preflight archives were made from the development worktree and are not release assets; tagged CI rebuilds and verifies the actual assets. Workflow syntax passed actionlint. All management module race/vet tests and legacy Node tests passed locally, and the root race suite also passed with the optional UDP overload test enabled (20,000 packets on each of direct, forward and reverse paths).

An additional tunnel regression covers 100 SSH lifetimes with a parent context kept alive across reconnects: the connection now owns and joins its cancellation watcher, and a previous disconnect cannot prematurely release the next connection's readiness wait. The root race/vet suite passed after this fix.

Request admission tests also passed after adding separate upload/control/history limits: saturating upload bodies returns 503 without blocking a control request; one node cannot consume another node's control allowance; large status/metric strings are rejected before retention. Local controller/integration race tests and vet passed for these final bounds.

The first expanded Windows CI run passed all management modules, including native Job Object execution and the connection lifecycle tests, but exposed a tunnel test-fixture defect: TCP-selected ephemeral ports could be reserved against UDP binding on Windows. The fixture now obtains a UDP port and verifies TCP availability before using the shared port, retaining the original UDP assertions. The final native matrix is rerun before publication.

Two non-race Go 1.26.3 HTTPS tests subsequently passed: each executed 128 real tasks, then injected faults for **900 seconds** (15 minutes) with a fixed history. Full run times were 929.03 s and 928.37 s. These exercise the new disk-history design; subsequent admission and SSH watcher fixes are covered by their own regressions and the final tag workflow. Raw reports: [HTTP/1.1](validation/2026-10-23-candidate-http1.json), [HTTP/2](validation/2026-10-23-candidate-http2.json).

| Measurement | HTTP/1.1 | HTTP/2 |
| --- | --- | --- |
| Control requests / upload requests | 2287 / 277 | 2315 / 277 |
| Injected connection abortions / HTTP 503s | 443 / 464 | 449 / 471 |
| Post-GC heap | 2.45–2.58 MiB | 2.59–2.71 MiB |
| RSS | 20.67–23.82 MiB | 20.48–25.57 MiB |
| Live goroutines | 8–12 | 11–12 |
| Linux FDs | 12–14 | 14–14 |
| After closing fixture: goroutines / FDs | 2 / 7 | 2 / 7 |
| Active Controller snapshot | 42 bytes | 42 bytes |

Transient reconnect samples returned to baseline counts; there was no sustained goroutine/FD accumulation. HTTP/2 RSS did not reproduce the earlier race-enabled growth. This supports treating the earlier RSS observation separately from retained Go heap, but does not prove the instrumentation was its sole cause. Figures still include both Node and Controller in one process and are not individual production memory requirements or a 24–72 hour guarantee.

A local end-to-end run after the port-fixture change returned a failure with truncated diagnostics; a captured rerun and a subsequent three-repetition race run passed (47.979 s for the three repetitions). The unexplained intermittent local failure remains recorded; native CI and tag gates must also pass before publication.


## Rolling retention and low-footprint refinements — rc.2, 2026-10-23

This section supersedes the fixed 10,000-entry Node journal and stop-at-full
confirmed-history limits. Protocol v3 receipts separate results still awaiting
final acknowledgement from reclaimable history. Sequence checkpoints suppress
replay after per-task records are removed. The Controller's storage version is
now 2 and the Node's retained journal wrapper is version 3; binary-only downgrade
is rejected. Upgrade and backup requirements are in [retention design](../docs/tack-retention.md).

Local Go 1.26.3 race/vet checks passed in protocol, control, Node and integration.
Protocol and Node tests also passed with Go 1.23.12 on this Linux host. Subsequent
recovery ordering, receipt-batching and legacy-cancellation migration changes
passed the affected module race/vet checks and Node legacy tests. Native Windows,
macOS and all 13 Node cross-build targets remain the required exact-commit CI
gates, not inferred from local compilation.

| Regression | What it verifies |
| --- | --- |
| `TestReceiptRequiredBeforeEvictionAndRestart` | 30 tasks with a two-entry history budget; one unconfirmed result survives repeated reclamation, TTL expiry and restart, then returns its lost final ack |
| `TestBytePressureReclaimsOnlyConfirmedOutput` | Byte pressure, independently of the count limit, repeatedly reclaims confirmed output while retaining an unconfirmed result |
| `TestRetentionChurnLostAcksAndControllerRollback` | HTTP/1.1 and HTTP/2 each execute 24 real tasks through a four-entry history budget, lose both final and receipt replies, restart the Node and restore an old Controller snapshot; execution-marker count stays exactly 24 |
| `TestCompactedReplayWatermarkAcrossRestart` | Old sequences remain refused after journal compaction/restart; a checkpoint-before-intent crash cannot cause execution; downgrade is rejected |
| `TestFullLegacyJournalMigratesAndKeepsAccepting` | A full 10,000-entry old journal drains, the next task runs once, and an old identity cannot run again; retained-journal writes are capped at 15 during migration |
| Recovery tests | Complete pending output survives an injected ENOSPC; all named recovery sources are read before any recovery destination is reused, including directories with 260 old temporary files; result persistence retries without reexecution |
| Controller startup tests | Durable archives win over a restored active snapshot; never-dispatched legacy cancellations become reclaimable without duplicate archives; unknown staging is preserved and charged to capacity |
| Ring/log/workspace tests | Exact head/tail output across variable write sizes, lazy allocation, bounded log files across rotation/restart/limit reduction, and cleanup restricted to the Node's owned workspace |

The initial full-journal race run hit its 30-second fixture deadline after
repeatedly rewriting the entire journal for each 128-receipt batch. Optional
receipt deletion now shares the next necessary journal write or flushes when
receipts finish draining. Intent, result, checkpoint and final-ack persistence
remain synchronous. The full Node race suite then passed in 43.645 seconds;
the legacy non-race suite passed in 9.646 seconds. No timeout increase was used.

Same-host Go microbenchmarks (`go test -run '^$' -bench ... -benchmem -benchtime=1s`),
Linux amd64 / Intel Xeon CPU Max 9470C, observed:

| Operation | Before | After |
| --- | --- | --- |
| 12-byte output with a 64 KiB cap, including snapshot | 65,632 B/op; 15,869 ns/op | 160 B/op; 157 ns/op |
| 1 MiB input with a 64 KiB cap, including snapshot | 131,152 B/op; 36,607 ns/op | 131,152 B/op; 37,524 ns/op |
| Receipt owner lookup, 2 MiB total raw output | 10,501,492 B/op; 21,936,714 ns/op | 1,733 B/op; 7,021 ns/op |

These are one-run allocation/microbenchmark observations, not a device throughput
or network-latency guarantee. Large-output bounded storage is unchanged; the main
wins are avoiding full preallocation for short output and decoding no confirmed
payload merely to validate a receipt. The eviction regression completes 270
admissions with three directory scans, preserves oldest-first order with late
receipts, and checks idle TTL cleanup. The cache holds at most 128 metadata entries.

The non-race HTTPS resource runs each completed 128 tasks, then 180 seconds of
connection abortions/HTTP 503s. Total durations were 209.44 s (HTTP/1.1) and
207.01 s (HTTP/2). Both ended with a 44-byte empty Node journal and an 82-byte
Controller active snapshot. Raw reports: [HTTP/1.1](validation/2026-10-23-rc2-http1.json),
[HTTP/2](validation/2026-10-23-rc2-http2.json). These runs preceded the final
recovery batching and legacy-cancellation migration refinements; those changes
have targeted regressions, and tagged CI repeats both resource tests.

| Measurement | HTTP/1.1 | HTTP/2 |
| --- | --- | --- |
| Post-GC heap, MiB | 2.45–2.47 | 2.58–2.65 |
| RSS, MiB | 20.21–22.95 | 20.39–23.82 |
| Live goroutines | 8–8 | 11–11 |
| Linux FDs | 12–12 | 14–14 |
| Goroutines / FDs after closing fixture | 2 / 7 | 2 / 7 |

All resource figures include Node, Controller, TLS and the test harness in one
process, not per-agent production memory. There was no sampled goroutine/FD
accumulation. This is not 24–72 hour validation, real disk exhaustion or physical
power-loss testing. Actual CentOS 6, ARM/MIPS, NAS/overlay and flash endurance
remain device-specific validation work. Finite disk cannot keep an unlimited
unconfirmed backlog: backpressure protects accepted results; only confirmed
history and diagnostics are lossy/expirable.


The first rc.2 branch CI passed the management modules on Linux, macOS and
Windows, plus the legacy tests and 13 cross-builds. Its Windows root-module
run exposed an existing SOCKS shutdown test race: `Client.Close()` only requests
cancellation, and the test attempted to rebind before `Client.Wait()` completed
while its own accepted TCP connection remained open. The fixture now closes
that peer, waits for complete client shutdown with a three-second failure bound,
then checks immediate TCP and UDP reuse. It does not retry binding or skip the
assertion on Windows. The local root race/vet suite and 20 repeated shutdown regressions passed.
Native CI now repeats that shutdown regression 20 times on each platform; the
final branch/tag matrices must pass after this fix.

An additional **standalone Node** measurement used the actual Go 1.23.12,
CGO-disabled native binary from the rc.2 management code, a local Python TLS
HTTP/1.1 endpoint holding one v3 long poll, the default 1800-second heartbeat,
and no tasks. After five seconds of warmup, a 30-second `/proc` observation
reported RSS 9,016 → 8,176 KiB, seven FDs, eight OS threads, zero measured CPU
ticks and zero additional disk write bytes. The process was terminated and
joined afterward. Raw [idle report](validation/2026-10-23-rc2-node-idle.json).
This short isolated observation excludes the Controller/test server and command
children; it is not a general RSS ceiling, HTTP/2 measurement or overnight test.
CPU time below the host clock-tick resolution is not observable in this sample.
