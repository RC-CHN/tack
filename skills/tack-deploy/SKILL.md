---
name: tack-deploy
description: Deploy, register, update or remove this repository's hosted Tack control plane and customer Nodes, adapting paths and startup persistence to Linux, Windows, NAS and embedded devices. Use for deployment work, not for changing Node runtime behavior or generic remote administration.
---

# Tack deployment

Use `tack-control` for the hosted control plane, `tack-node` for the customer runtime and `tack` for operator commands. Chisel names refer to the separate tunnel component.

Keep Controller hosting under the operator's control. Customer devices receive only their own Node configuration and authorized binaries. Node is a foreground worker; all installation, path selection and boot persistence are external deployment concerns.

Read the repository's [management guide](../../management/README.md) for commands and current implementation limits, and the [design](../../docs/tack-design.md) when compatibility, identity or release requirements matter. These references belong to this repository; when installing the skill elsewhere, retain the references or resolve the checked-out repository explicitly.

## Establish the target

Use the target and access mechanism the user supplied. If the actual server or device is unspecified, prepare concrete deployment artifacts with replaceable target parameters; do not guess a live host. An existing task authorization covers its routine deployment steps but does not authorize reformatting storage, changing unrelated networking or rebooting without an agreed maintenance window.

Inspect OS, CPU, endianness and float ABI where relevant, kernel, process supervisor, current account, available space, mount flags and boot mount order. For Linux/NAS/router decisions read [platform deployment](references/platform-deployment.md); for credential boundaries and hosted layout read [hosted control plane](references/hosted-control-plane.md).

## Produce a deployment record

Select and record independently:

- Executable paths for Node, Chisel and optional BusyBox.
- Configuration/credential path and owning account.
- Task state directory and whether it survives reboot, power loss and firmware upgrades.
- Temporary task work directory, log destination and storage/write limits.
- Startup mechanism, definition location, required mounts/network and stop/uninstall commands.
- Controller origin, trust root, node ID, artifact hashes and tested platform level.

Paths are decisions for this target, not global defaults. Prefer an existing appropriate service manager or documented vendor startup facility. For read-only roots, delayed NAS volumes, overlay storage or limited flash, select a suitable writable volume and separate durable state from temporary work. Do not remount root writable, expand overlay or modify vendor boot partitions merely to fit a default path. If no persistence is available, explicitly describe a volatile or manually started deployment; do not claim persistence.

## Provision and verify

Generate node credentials on the operator side with `tack provision`. Transfer only the selected node configuration over the authorized access channel. Never copy the Controller configuration, administrator token, shared registration secret, signing key or general-purpose Chisel server account to a customer node. Do not print credentials into logs or place them in command arguments. Apply filesystem permissions or Windows ACLs for the selected account.

Use matching, verified artifacts. Distinguish development binaries from protected commercial releases. The Node release policy is obfuscation plus packing where supported; missing protected artifacts are a release gap, not permission to call a plain binary protected. Record cross-compilation separately from native/device validation.

Prepare the selected state and work directories externally. Configure bounded supervisor log capture, or explicitly supply `-log-file PATH -log-max-bytes N -log-files COUNT` to Node/Controller (defaults: 4 MiB per file, 4 files total). Do not install an unbounded `>> logfile` launch command. Chisel output needs bounded supervisor capture. Then run the Node in the foreground. Confirm that the correct node appears on the private management API and that one harmless, explicitly targeted command returns its result. The Node may create its runtime journal and task files, but must not install itself or alter startup configuration.

After the foreground check, generate and install only the startup definition appropriate to the target. Keep its name and stop/uninstall path explicit. Capture the actual configuration and test restart through the chosen supervisor. Validate reboot persistence only within an authorized maintenance window; otherwise report it as unverified. Unsupported platform behavior should lead to a documented alternative or a clear limitation, not repeated invasive boot modifications.

## Upgrade and remove

Upgrade the Controller before Nodes and use matching CLI binaries. Drain the queue before the first v3 Node handshake: migration fences older tasks and uncertain work may become `unknown`; check side effects before resubmitting. Retain Node identity, journal and `replay.json` together across binary replacement; v3 state cannot be downgraded by swapping only binaries. Back up Controller state with the complete `.history` (including `released`) and `.uploads` directories. Stop or drain current work according to the deployment's task policy, stage and verify the new executable, switch paths using the platform's supported mechanism, and retain a rollback artifact. Windows may require stopping the process before replacement. Revalidate connectivity and a targeted command after upgrade.

For removal, stop and disable the specific startup entry, revoke that node credential on the Controller, and remove only files owned by this deployment. Keep or export task records according to the user's retention instruction. Report remaining node-side files and any unverified reboot behavior.
