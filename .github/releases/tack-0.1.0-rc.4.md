Tack 0.1.0-rc.4 combines the management worker and Chisel client into one
customer executable, `tack-node`. One foreground process provides explicitly
targeted non-interactive commands and configured tunnels. The hosted
`tack-control` and Chisel server remain separate services.

Add a `tunnel` object to the Node configuration, or supply `tack provision
-tunnel-config FILE`. Use a node-specific tunnel credential, verified HTTPS
certificate and complete SSH fingerprint. Management and tunnel credentials
remain separate. The two connections retry independently; a tunnel outage does
not block command delivery. Omitting `tunnel` preserves management-only use of
the same unified executable. Deployment paths, accounts, mount readiness and
startup registration remain external; the Node does not install itself.

The release provides two complete Node variants:

- **Modern:** Go 1.26.3, for supported Linux and Windows targets. Windows requires
  Windows 10 / Server 2016 or newer.
- **Linux Legacy:** Go 1.23.12, filenames ending in `_legacy.tar.gz`. Both command
  execution and tunneling passed on the real CentOS 6.10 kernel
  `2.6.32-754.el6.x86_64` in a QEMU system VM with minimal BusyBox userspace.
  This does not certify a complete vendor OS, reboot persistence, NAS storage or
  ARM/MIPS hardware. ARM requires at least Linux 3.1 for the Legacy path;
  architecture-specific constraints remain in the compatibility guide.

Install one selected Node binary, not both variants. The operator-side
`scripts/select-node-artifact.py` selects from an already verified release
manifest using the customer's OS, CPU and kernel facts. No Python installation
is required on the customer machine. Missing or incompatible artifacts are
rejected rather than substituted with the management-only development worker.

Protected builds now use pinned Garble and UPX tools with per-artifact build
records, integrity checks and exact decompression checks. Modern Node uses
Garble v0.17.0 and UPX 5.0.2 where supported. **Legacy retains Garble v0.14.2
obfuscation without UPX:** tested packed amd64 loaders crash on the qualified
2.6.32 kernel, so this exception is explicit in `BUILD.json`. Other unsupported
packing targets also retain obfuscation with a recorded reason. Binaries are
unsigned; obfuscation and packing do not protect credentials from a compromised
administrator account.

Legacy adapters retain the current pinned SSH dependency and its connection
cleanup fixes, with source/checksum provenance. They omit unsupported SSH
ML-KEM and FIPS functionality instead of downgrading to an old SSH library.
Go 1.23 itself is no longer under routine upstream maintenance; this compatibility
path does not provide the same standard-library maintenance coverage as Modern.

The default transport remains native verified TLS. Optional TLS profiles and
bounded framing added since rc.3 remain explicitly selected experiments; the
embedded tunnel uses native TLS, and Legacy does not include browser TLS
profiles. No network-classification, censorship-resistance or throughput claim
is made. Upload retry handling also avoids immediate retries after stale durable
progress. Routine diagnostic logging remains disabled by default, including the
embedded Chisel client; explicit bounded logs and fatal diagnostics remain.

Validation covers both final Node variants, management during tunnel outages,
reconnect without changing the Node PID, shutdown/listener cleanup, and both
directions of Modern/Legacy replacement with pending results. Tests verify exact
result bytes and execution counts. Both HTTP modes retain the 32 MiB Node RSS
gate and intermittent-link recovery checks. The tag workflow repeats the old
kernel and state-replacement checks on the exact final Legacy archive before
creating a draft. Short test results are not universal resource ceilings or
24–72 hour device endurance guarantees.

Upgrade the Controller and CLI before Nodes. Drain queued/running work under
the deployment's task policy, stop the relevant process, and back up Controller
state together with `.history` (including `released`) and `.uploads`. Preserve
each complete Node state directory, including `identity.json`, `journal.json`,
`replay.json`, `results/` and recovery files. This release retains Node storage
v4, Controller storage v2 and wire protocol v3. Same-release Modern/Legacy
replacement preserves this state; it is not permission to downgrade to an older
storage format or discard execution evidence. Revalidate a harmless targeted
command and an authorized tunnel transfer after switching the binary.

This remains a prerelease. There are **46 platform archives**, plus
`manifest.json` and `SHA256SUMS`. Each archive includes the executable, license,
operation guide and `BUILD.json`; the manifest records runtime profile,
capabilities, toolchain, protection exceptions and validation level. BusyBox is
supplied separately. Native/device verification is distinct from cross-compilation.

See the [unified client guide](https://github.com/RC-CHN/tack/blob/tack-v0.1.0-rc.4/docs/unified-node.md),
[artifact selection and fallback](https://github.com/RC-CHN/tack/blob/tack-v0.1.0-rc.4/docs/legacy-compatibility.md),
[deployment skill](https://github.com/RC-CHN/tack/blob/tack-v0.1.0-rc.4/skills/tack-deploy/SKILL.md)
and [validation summaries](https://github.com/RC-CHN/tack/blob/tack-v0.1.0-rc.4/management/VALIDATION.md).
