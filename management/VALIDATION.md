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
