# Tack

[Tack](https://github.com/RC-CHN/tack) is a self-hosted control plane for managing customer nodes and running explicitly targeted, non-interactive commands. The control plane and node runtime are separate programs. Chisel is the underlying tunnel component.

| Component | Command | Source |
| --- | --- | --- |
| Hosted control plane | `tack-control` | [management/control](management/control) |
| Customer node runtime | `tack-node` | [management/node](management/node) |
| Operator CLI | `tack` | [management/control/cmd/tack](management/control/cmd/tack) |
| Deployment skill | `tack-deploy` | [SKILL.md](skills/tack-deploy/SKILL.md) |
| Tunnel client/server | `chisel` | Root Go module; [tunnel guide](docs/chisel.md) |

## Start here

- [Design and implementation stages](docs/tack-design.md): architecture, credential boundaries, legacy platforms, BusyBox and release protection.
- [Build and operation guide](management/README.md): build the three Tack commands, provision a node, host the control plane and submit a task.
- [Deployment skill](skills/tack-deploy/SKILL.md): choose paths and startup persistence for Linux, Windows, NAS and embedded systems.
- [Validation record](management/VALIDATION.md): checks actually run and platforms still requiring native tests.

## Current scope

The first-stage development baseline supports independent node credentials, HTTPS communication, a private administrator listener, per-node task queues, output limits, execution journaling, timeout and cancellation. The link extension adds configurable long heartbeats with immediate command delivery over HTTPS long polling, independent resumable output uploads and private metrics. Commands can use direct execution or an explicitly selected shell, with a configured BusyBox fallback for POSIX tasks.

The node runs in the foreground. Deployment chooses executable, configuration, state and work paths, and configures startup externally. This allows read-only NAS roots, router overlays and other device-specific layouts without embedding installation policy in the runtime.

Windows and Linux are targets; CentOS 6 and ARM/MIPS devices require their own compatibility validation. Cross-compilation is recorded separately from native execution. Certificate enrollment/rotation, customer RBAC, Chisel supervision, bundled BusyBox and protected commercial releases remain later-stage work.

## Repository layout

Tack's independent Go modules live under `management/control`, `management/node`, `management/protocol` and `management/integration`. They use the local module namespace `tack.local/management/...` and relative replacements, so they build from this checkout without a new remote repository or module registry.

The root Go module, tunnel packages and existing tunnel release workflow belong to Chisel. Building at the repository root builds `chisel`; build the Tack commands using the management guide. Chisel installation links and published images in the [tunnel guide](docs/chisel.md) supply the tunnel component.

## Upstream and license

The tunnel implementation derives from [jpillora/chisel](https://github.com/jpillora/chisel). Its original attribution and [MIT license](LICENSE) are retained. Tack's design and management implementation are documented separately above.
