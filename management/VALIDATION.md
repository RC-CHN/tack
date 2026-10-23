# Tack first-stage validation

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
