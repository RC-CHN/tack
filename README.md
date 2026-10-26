# Tack

[Tack](https://github.com/RC-CHN/tack) is a self-hosted control plane for managing customer nodes and running explicitly targeted, non-interactive commands. The control plane and node runtime are separate programs. Chisel is the underlying tunnel component.

| Component | Command | Source |
| --- | --- | --- |
| Hosted control plane and embedded tunnel server | `tack-control` | [cmd/tack-control](cmd/tack-control) |
| Customer node runtime: commands and embedded tunnel | `tack-node` | [cmd/tack-node](cmd/tack-node) |
| Operator CLI | `tack` | [management/control/cmd/tack](management/control/cmd/tack) |
| Deployment skill | `tack-deploy` | [SKILL.md](skills/tack-deploy/SKILL.md) |

## Start here

- [Stable release v0.1.2](https://github.com/RC-CHN/tack/releases/tag/tack-v0.1.2): platform downloads and upgrade notes.
- [Task operations](docs/task-operations.md): safe retries, decoded results, bounded waiting and scoped operator credentials.
- [Unified hosted server](docs/unified-control.md): one service for management and tunnels, including migration from a separate Chisel server.
- [Design and implementation stages](docs/tack-design.md): architecture, credential boundaries, legacy platforms, BusyBox and release protection.
- [Build and operation guide](management/README.md): build the three Tack commands, provision a node, host the control plane and submit a task.
- [Deployment skill](skills/tack-deploy/SKILL.md): choose paths and startup persistence for Linux, Windows, NAS and embedded systems.
- [Validation record](management/VALIDATION.md): checks actually run and platforms still requiring native tests.
- [Network and footprint fixtures](test/fixtures/README.md): reusable weak-link simulation, recovery tests and independent Node resource gates.

## Current scope

Tack supports independent node credentials, HTTPS communication, a private administrator listener, per-node task queues, output limits, execution journaling, timeout and cancellation. The link extension adds configurable long heartbeats with immediate command delivery over HTTPS long polling, independent resumable output uploads and private metrics. Commands can use direct execution or an explicitly selected shell, with a configured BusyBox fallback for POSIX tasks.

The customer runtime is one executable and one foreground process: `tack-node` embeds the Chisel client and management worker. Configure its `tunnel` section to run both; no separate Chisel client executable or service is needed. Deployment chooses configuration, state and work paths and configures startup externally. See [the unified client guide](docs/unified-node.md).

From v0.1.2, the hosted runtime is also one executable and one foreground process: `tack-control` embeds the Chisel server. Management, administration and tunnels retain separate listeners and credentials. Configure the Controller's optional `tunnel` section to enable it; existing management-only configurations still work. The `tack` CLI remains an operator tool, not another background service.

Windows and Linux are targets. The same unified client has Modern and Linux Legacy artifacts; the Legacy artifact has passed shell/tunnel checks on a real CentOS 6 kernel in QEMU. ARM/MIPS devices and vendor storage still need their own validation. See [compatibility and artifact selection](docs/legacy-compatibility.md), including the Legacy packing exception and Windows minimum version. Certificate enrollment/rotation, tenant isolation, dynamic tunnel configuration, bundled BusyBox and signed commercial releases remain later-stage work.

## Repository layout

Tack's independent Go modules live under `management/control`, `management/node`, `management/protocol` and `management/integration`. They use the local module namespace `tack.local/management/...` and relative replacements, so they build from this checkout without a new remote repository or module registry.

The root module builds the unified customer client with `go build ./cmd/tack-node` and the unified hosted server with `go build ./cmd/tack-control`. The inherited standalone `chisel` command remains available from source with `go build .` for compatibility; v0.1.2 releases package only `tack-node`, `tack-control` and `tack`. Upstream Chisel downloads do not include Tack command execution.

## Upstream and license

The tunnel implementation derives from [jpillora/chisel](https://github.com/jpillora/chisel). Its original attribution and [MIT license](LICENSE) are retained. Tack's design and management implementation are documented separately above.
