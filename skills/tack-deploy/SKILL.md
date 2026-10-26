---
name: tack-deploy
description: Deploy, register, update or remove this repository's hosted Tack control plane and customer Nodes, adapting paths and startup persistence to Linux, Windows, NAS and embedded devices. Use for deployment work, not for changing Node runtime behavior or generic remote administration.
---

# Tack deployment

Use `tack-control` for the hosted control plane and tunnel server, `tack-node` for the customer runtime and `tack` for operator commands. From v0.1.2 both service binaries embed their Chisel component: deploy one foreground service on each side. The CLI is an operator tool. Older releases used a separate hosted Chisel service; migrate it explicitly using [the unified hosted server guide](../../docs/unified-control.md).

Keep Controller hosting under the operator's control. Customer devices receive only their own Node configuration and authorized binaries. Node is a foreground worker; all installation, path selection and boot persistence are external deployment concerns.

Read the repository's [management guide](../../management/README.md) for commands and current implementation limits, and the [design](../../docs/tack-design.md) when compatibility, identity or release requirements matter. These references belong to this repository; when installing the skill elsewhere, retain the references or resolve the checked-out repository explicitly.

## Establish the target

Use the target and access mechanism the user supplied. If the actual server or device is unspecified, prepare concrete deployment artifacts with replaceable target parameters; do not guess a live host. An existing task authorization covers its routine deployment steps but does not authorize reformatting storage, changing unrelated networking or rebooting without an agreed maintenance window.

Inspect OS, CPU, endianness and float ABI where relevant, kernel, process supervisor, current account, available space, mount flags and boot mount order. For Linux/NAS/router decisions read [platform deployment](references/platform-deployment.md); for credential boundaries and hosted layout read [hosted control plane](references/hosted-control-plane.md).

## Produce a deployment record

Select and record independently:

- Executable path for the unified Node and optional external BusyBox; the customer does not need a standalone Chisel client.
- Configuration/credential path and owning account.
- Task state directory and whether it survives reboot, power loss and firmware upgrades.
- Temporary task work directory, log destination and storage/write limits.
- Startup mechanism, definition location, required mounts/network and stop/uninstall commands.
- Controller and tunnel origins, trust roots, server SSH fingerprint, node ID, artifact hashes and tested platform level.

Paths are decisions for this target, not global defaults. Prefer an existing appropriate service manager or documented vendor startup facility. For read-only roots, delayed NAS volumes, overlay storage or limited flash, select a suitable writable volume and separate durable state from temporary work. Do not remount root writable, expand overlay or modify vendor boot partitions merely to fit a default path. If no persistence is available, explicitly describe a volatile or manually started deployment; do not claim persistence.

## Provision and verify

Generate node credentials on the operator side with `tack provision`. For both capabilities, supply `-tunnel-config FILE` with that node's tunnel origin, scoped tunnel credential, verified SSH fingerprint and explicitly selected remotes; see the repository's [unified guide](../../docs/unified-node.md). Transfer only the selected node configuration over the authorized access channel. Never copy the Controller configuration, administrator token, shared registration secret, signing key or general-purpose Chisel server account to a customer node. Do not reuse the management token as a tunnel password. Apply filesystem permissions or Windows ACLs for the selected account; credentials belong in files, not command arguments or logs.

Use matching, verified artifacts. Read [runtime compatibility and selection](../../docs/legacy-compatibility.md), collect OS/kernel/CPU facts on the target, then run `scripts/select-node-artifact.py` on the operator host against a verified release manifest. Select one Modern or Linux Legacy binary; both contain `management` and `embedded-chisel` capabilities. The Go 1.23 management-only worker is not a substitute. Legacy amd64 has passed a CentOS 6 kernel VM check; vendor storage, other architectures and reboot behavior still need target validation. Windows requires Windows 10 / Server 2016 or newer; there is no Win7 fallback.

Stage the selected executable and run its version command before replacement. Modern uses packing where supported; Legacy retains obfuscation without UPX because the tested packed loader crashes on the qualified old kernel. Record this explicit exception from `BUILD.json`; a missing protected artifact is not permission to call a plain binary protected. A confirmed startup compatibility problem may justify selecting the same release's Legacy artifact externally. Network, credential, certificate, ACL or disk errors do not trigger runtime downgrade or disabled verification. The Node must not download, unpack or replace itself. Record cross-compilation separately from native/device validation.

Prepare the selected state and work directories externally. Node/Controller routine diagnostics are disabled by default, including the embedded Chisel client and server; fatal errors still reach stderr. For troubleshooting, explicitly select `-log-stderr` with bounded supervisor capture or `-log-file PATH -log-max-bytes N -log-files COUNT` (defaults: 4 MiB per file, 4 files total). Do not install an unbounded `>> logfile` launch command. Run the unified Node in the foreground and verify both a harmless targeted command and a transfer through an authorized tunnel endpoint. A visible Node or open local port alone does not prove tunnel connectivity. The Node must not install itself or alter startup configuration. Tunnel configuration currently changes by updating its config and restarting the Node under the task-drain policy.

For task verification and automation credentials, read [task operations](../../docs/task-operations.md). Keep the CLI-persisted `request_id` and task file across retries. Use `wait --check` for execution success; a successful `get` only proves the query succeeded. Wait for `result_pending` to clear before exporting output. Treat cleanup and upload errors separately from command failure; never replace the request identity merely to retry a lost response. Prefer a scoped `--credential` file for automation, with the authorized node whitelist and read/execute permissions. Check management and tunnel status independently, including their timestamps.

After the foreground check, generate and install only the startup definition appropriate to the target. Keep its name and stop/uninstall path explicit. Capture the actual configuration and test restart through the chosen supervisor. Validate reboot persistence only within an authorized maintenance window; otherwise report it as unverified. Unsupported platform behavior should lead to a documented alternative or a clear limitation, not repeated invasive boot modifications.

## Upgrade and remove

Modern and Legacy artifacts of the same release share the current state format. Keep the entire state/configuration, drain running commands, stop the previous instance and verify foreground shell/tunnel behavior after switching. A runtime-profile switch is not a downgrade to an older storage format. See the compatibility guide for the tested bidirectional pending-result recovery path.

For hosted upgrades to v0.1.2, preserve the relay's SSH key, account/ACL file, TLS identity and advertised address. Stop the separate relay before the unified Controller binds its port; verify both capabilities before removing its old startup definition. A return to v0.1.1 requires removing the unsupported server `tunnel` field from the old Controller's configuration and restoring the separate relay, in addition to the state and task checks below.

Upgrade the Controller before Nodes and use matching CLI binaries. Drain the queue before the first v3 Node handshake: migration fences older tasks and uncertain work may become `unknown`; check side effects before resubmitting. Retain the complete Node state directory, including identity, journal, `replay.json`, `results/` and recovery files, across binary replacement. Node storage v4 migrates v3 inline results automatically; older Nodes cannot read v4 state, so swapping only binaries is not a valid downgrade. Controller storage v3 includes the request retirement watermark. Back up Controller state with the complete `.history` (including `released`), `.uploads` directories and `.operators.json` credential store. Older releases must not write this state; new diagnostic fields also prevent assuming that pending Node journals can be rolled back to an earlier release. Stop or drain current work according to the deployment's task policy, stage and verify the new executable, switch paths using the platform's supported mechanism, and retain a rollback artifact. Windows may require stopping the process before replacement. Revalidate connectivity and a targeted command after upgrade.

For removal, stop and disable the specific startup entry, revoke that node's management credential and its separate tunnel authorization, and remove only files owned by this deployment. Handle established tunnel connections according to the hosted relay's policy; removing a management token alone does not revoke a Chisel account. Restarting the unified Controller disconnects every hosted tunnel, so account for other nodes before using a restart to close an existing session. Keep or export task records according to the user's retention instruction. Report remaining node-side files and any unverified reboot behavior.
