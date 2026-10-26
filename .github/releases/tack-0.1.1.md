Tack 0.1.1 is the first stable release. The customer runs one `tack-node`
executable and foreground process for targeted non-interactive commands and
configured Chisel tunnels. The hosted Controller and tunnel server remain
separate services. Deployment paths, accounts and startup registration remain
external deployment choices.

## Changes since rc.4

- Persistent submission `request_id`: the CLI saves the identity in the task
  file before sending it. Matching retries return the same task; changed
  parameters return a conflict. Reclaimed identities are rejected rather than
  executed again. Deduplication storage remains bounded.
- Stable execution `error_code` and `phase`, with cleanup errors and paths
  reported independently. A cleanup or upload problem does not turn a
  successful command into a failed command.
- Readable `get` output, decoded JSON, exact stdout/stderr file export, and
  explicit exit status, pending-upload and truncation indicators.
- Bounded `wait --check`: waits for completion and complete output. Timeout ends
  only the local wait; explicit `cancel` is required to stop remote work.
- `start_before` queue deadlines, enforced by both Controller and Node,
  independently of command runtime timeout.
- Separate management and tunnel status, update times, current task and backlog
  reasons, including result persistence and upload failures.
- Caller correlation, operator attribution and node/state/time task filtering.
- Node-scoped read/execute operator credentials with live rotation and
  revocation. Automation can use a restricted credential file instead of the
  Controller's administrator configuration.

Ordinary diagnostic logs remain disabled by default. Durable execution intent,
result retry, receipt-confirmed retention and the existing long heartbeat with
immediate command delivery remain in place.

## Upgrade and compatibility

Upgrade **Controller and CLI first, then Nodes**. Stop or drain work according
to the deployment policy and take a consistent backup of Controller state,
complete `.history` (including `released`) and `.uploads` directories, and the
new `.operators.json` credential store. Preserve each complete Node state
directory, including identity, journal, replay checkpoint, result and recovery
files.

Management protocol is now **v4**, Controller storage **v3**, and Node journal
storage remains **v4** with additional diagnostics. Older releases must not
write the new Controller state or pending Node journals. Switching between
Modern and Legacy from this same release is supported; it is not a downgrade
to a previous release. Ordinary tasks retain v1–v3 wire compatibility, while
queue deadlines and full diagnostics require a v4 Node session.

`get` retains query-success semantics even if the remote task failed. Its
default output is now readable text; existing protocol-JSON scripts must add
`--format json-raw`. Use `wait --check` to test remote success. Retry the same
persisted task file after a lost submission response; a new identity means a
new execution. Submission conflicts and retired identities must be reconciled,
not bypassed by automatically generating another identity. An interrupted
execution can still be `unknown`; arbitrary external side effects are not
covered by an exactly-once guarantee.

## Platforms and artifacts

There are **46 platform archives**, plus `manifest.json` and `SHA256SUMS`.
Each archive contains its executable, license, operation guide and `BUILD.json`.
Select one unified Node variant:

- **Modern:** Go 1.26.3 for the published Linux and Windows targets. Windows
  requires Windows 10 / Server 2016 or newer.
- **Linux Legacy:** Go 1.23.12, filenames ending in `_legacy.tar.gz`, including
  Linux amd64, 386, ARM v5/v6/v7, arm64 and MIPS variants. Legacy amd64 is
  qualified on the CentOS 6.10 kernel `2.6.32-754.el6.x86_64` in a QEMU VM with
  minimal BusyBox userspace. ARM requires at least Linux 3.1. Cross-compilation
  is distinct from native ARM/MIPS, NAS, router and vendor-storage validation.

Modern artifacts retain the configured Garble/UPX protection where supported.
Legacy retains Garble without UPX because the tested packed loader does not
work on the qualified old kernel. Exceptions are recorded in `BUILD.json`.
Binaries are unsigned. BusyBox remains separately supplied; TLS certificate
and SSH fingerprint verification remain enabled by the deployment workflow.
Go 1.23's compatibility path does not provide Modern's upstream maintenance
coverage. Stable release status does not expand the documented platform or
feature support boundaries.

The release workflow gates publication on native Linux/Windows/macOS tests,
race and Legacy checks, resource/fault tests, and verification of the exact
archived bytes. Final Node artifacts undergo shell/tunnel/reconnect smoke
checks, both HTTP resource and intermittent-link gates, and Legacy kernel and
bidirectional Modern/Legacy pending-result recovery checks. Short resource
checks are not 24–72 hour device endurance guarantees.

See [task operations and retry boundaries](https://github.com/RC-CHN/tack/blob/tack-v0.1.1/docs/task-operations.md),
[unified Node configuration](https://github.com/RC-CHN/tack/blob/tack-v0.1.1/docs/unified-node.md),
[artifact selection and platform limits](https://github.com/RC-CHN/tack/blob/tack-v0.1.1/docs/legacy-compatibility.md),
and the [deployment skill](https://github.com/RC-CHN/tack/blob/tack-v0.1.1/skills/tack-deploy/SKILL.md).
