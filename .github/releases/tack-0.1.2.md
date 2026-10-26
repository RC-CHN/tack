# Tack 0.1.2

Tack 0.1.2 combines the hosted Controller and Chisel tunnel server into one
`tack-control` executable and foreground process. Customer machines continue to
run one `tack-node` for management and tunneling. The `tack` operator CLI remains
a separate, on-demand tool.

## Changes since 0.1.1

- Add an optional Controller `tunnel` configuration with its own listener, server
  SSH key, account/ACL file, TLS certificate, reverse/SOCKS policy and keepalive.
  Management credentials, tunnel accounts and the private administrator listener
  retain separate authorization boundaries.
- Add `tack-control -tunnel-keygen PATH` for exclusive creation of a server key and
  `-config FILE -tunnel-fingerprint` for reading its public fingerprint without
  starting listeners. New deployment no longer needs a standalone Chisel binary.
- Close embedded tunnel connections and authorization-file monitoring with the
  Controller lifecycle. Invalid startup configuration or a listener failure stops
  the combined service. A configured relay requires authentication, including
  when its account file is empty.
- Remove the 13 standalone Chisel release targets. The release now contains
  **33 archives**, plus `manifest.json` and `SHA256SUMS`, with every existing Node
  platform retained.
- Build one component/platform/runtime per Actions matrix shard, with up to
  eight build workers. Build tools are prepared once. Assembly rejects missing,
  duplicate or inconsistent shards and retains the exact compiled bytes.
- Run final Modern HTTP/1.1 and HTTP/2 qualifications and the Legacy qualification
  independently. Draft creation still waits for every required check; no local
  or unverified partial build is published.

## Deployment

Use the [unified hosted server guide](https://github.com/RC-CHN/tack/blob/master/docs/unified-control.md)
for a new installation. Install one `tack-control` service on the hosted machine
and one `tack-node` service on each customer machine; run `tack` as the operator CLI.
Generate a persistent tunnel server SSH key, create node-specific tunnel accounts
and destination ACLs, configure the Controller `tunnel` object and verified TLS,
then provision each Node with its own credentials and the public fingerprint.

A Controller restart interrupts both its management and tunnel connections.
Nodes reconnect independently; locally executing commands and durable pending
results retain their execution and recovery semantics. Verify a targeted harmless
command and a real authorized tunnel transfer before installing startup entries.
Updating an authorization file does not immediately disconnect an established
authenticated tunnel.

For subsequent updates, back up the full Controller state together with `.history`
(including `released`), `.uploads` and `.operators.json`, plus its tunnel key,
account file, TLS material and service configuration. Keep each Node's complete
identity, journal, replay state, pending results and recovery files. Use matching
release versions and preserve task request IDs across retries.

Build the unified Controller from root `cmd/tack-control` and the unified Node
from root `cmd/tack-node`. Installation paths and startup persistence remain
external deployment choices.

## Artifacts and qualification boundaries

| Component | Targets |
| --- | --- |
| Modern unified Node | 10 Linux and three Windows architectures |
| Legacy unified Node | The same 10 Linux architectures |
| Unified Controller | Linux amd64/arm64; Windows amd64 |
| Operator CLI | Linux/macOS amd64/arm64; Windows amd64/386/arm64 |

Modern builds use Go 1.26.3; Legacy Nodes use Go 1.23.12 with the pinned dependency
adapter. Both Node variants embed Chisel. Controller build records additionally
declare `management` and `embedded-chisel-server` capabilities. Protection applies
to the whole linked executable; unsupported packing targets retain their
recorded obfuscated exception. Legacy deliberately omits UPX because its loader
failed qualification on the tested old kernel.

Verify the full new matrix with
`python3 -B scripts/verify-release.py DIRECTORY --commit EXPECTED_COMMIT --require-complete`.
The verifier continues to read earlier manifests without `--require-complete`.
Release archives contain the binary, license, operation guide and hash-bound
`BUILD.json`. Intermediate shards and qualification reports remain Actions
artifacts, separate from release downloads.

Release checks exercise the exact protected binaries, combined shell/tunnel
operation, reconnect and shutdown, resource and intermittent-link recovery,
Modern/Legacy replacement in both directions, and a real CentOS 6
`2.6.32-754.el6.x86_64` kernel with minimal BusyBox userspace in QEMU. These are
bounded qualification tests, not a 24–72 hour device endurance claim.

CentOS 6 qualification applies to the Legacy amd64 Node, not the modern hosted
Controller. ARM/MIPS and vendor NAS storage still need target-device validation;
ARM Legacy requires at least Linux 3.1. Windows requires Windows 10 / Server 2016
or newer. BusyBox is external, binaries remain unsigned, and the Legacy compiler
is an explicit maintenance exception. Same-process management and tunneling do
not provide process isolation or make local credentials inaccessible to root.

See the [release procedure](https://github.com/RC-CHN/tack/blob/tack-v0.1.2/docs/releases.md),
[compatibility guide](https://github.com/RC-CHN/tack/blob/master/docs/legacy-compatibility.md)
and [deployment skill](https://github.com/RC-CHN/tack/blob/master/skills/tack-deploy/SKILL.md)
for the exact matrix and operating requirements.
