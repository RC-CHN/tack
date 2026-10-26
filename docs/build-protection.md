# Protected builds

The default release profile is `packed`: compile with Garble, attempt UPX packing,
validate the transformed executable, and archive its exact bytes. `obfuscated`
skips UPX; `plain` is the comparison baseline. All profiles keep CGO disabled and
use the same release version, target and Go compiler. Existing rc.3 downloads are
unchanged; these changes do not publish a new version.

| Component | Go | Garble |
| --- | --- | --- |
| Modern unified Node, unified Controller, operator CLI | 1.26.3 | v0.17.0 |
| Linux Legacy unified Node | 1.23.12 | v0.14.2 |

The release Node is built from root `./cmd/tack-node` and contains both the
management worker and Chisel client. Its `BUILD.json` lists `management` and
`embedded-chisel` capabilities. Garble and UPX transform the complete linked
binary. The Linux Legacy release uses the same entrypoint with the pinned
`go.legacy.mod` adapter. The management-only module remains a development entrypoint.
Earlier footprint/protection measurements below used
that management-only binary and must not be presented as unified-client results.

From v0.1.2 the release Controller builds from root `./cmd/tack-control` and
contains both management and the Chisel server. Its build record lists
`management` and `embedded-chisel-server`; transformations apply to the complete
linked binary. The standalone Chisel command remains source-buildable for
compatibility but is not part of this release matrix.

Legacy deliberately skips UPX and records an obfuscated platform exception:
UPX 5.0.2 amd64 loaders exited 139 on the real CentOS 6 2.6.32 kernel, including
a packed Go 1.23 input. The final obfuscated unified Legacy artifact passed TLS,
shell, tunnel and shutdown checks on that kernel. Other Legacy targets also
retain obfuscation until their packed loaders have old-kernel qualification.
See [compatibility, dependency provenance and selection](legacy-compatibility.md).

UPX is pinned to **5.0.2**, with SHA-256-pinned official installer archives for
Linux amd64/arm64 and Windows amd64 build hosts. A local qualification found that
UPX 5.2.1 produced a SIGTRAP before Go startup for the static Legacy Node built
with Garble v0.13.0, for both NRV and LZMA; 5.0.2 ran the same input successfully
20/20 times on the modern host; that result did not establish old-kernel support.
Do not automatically upgrade the packer without rerunning the final
artifact tests. Garble v0.14.2 continues Go 1.23 support and includes toolchain and
reflection compatibility fixes.

Garble uses `-literals` and
`GOGARBLE=tack.local/*,github.com/jpillora/chisel`: application packages and their
subpackages are obfuscated, while standard-library and third-party packages are
not selected. This limits resource costs. Exported/interface names, protocol
strings and constant expressions may remain visible. There is no `-tiny` or
control-flow transformation; panic/fatal diagnostics remain available. UPX uses
`--no-env -9` (NRV), without forced packing or LZMA. Windows additionally requires `--exact`; PE files that UPX cannot restore byte-for-byte retain the obfuscated executable as an explicit exception. UPX 5.0.2 does not support Windows ARM64 or Linux MIPS64/MIPS64el packing either. These transformations raise analysis
costs; they do not provide credential protection or change network behavior.

```sh
python3 -B scripts/install-build-tools.py --output bin/build-tools
python3 -B scripts/build-release.py --version 0.1.2 --output dist/protected
python3 -B scripts/verify-release.py dist/protected --extract-node /tmp/protected-node
```

`--native-only`, `--components tack-node` and `--node-runtime modern|legacy|both`
are useful for local comparisons. Runtime selection defaults to `both`; the
complete v0.1.2 matrix has 33 archives; standalone Chisel is no longer packaged. `verify-release.py --node-runtime legacy`
extracts the Legacy Node explicitly, avoiding an overwrite by the Modern variant.
`--require-clean` rejects a dirty checkout or arbitrary filtered build. An exact
`--shard` build is permitted for CI, but cannot be published by itself: the
assembler and `verify-release.py --require-complete` require the whole matrix.
macOS can install Garble with `--without-upx` and run the `obfuscated` profile;
the complete release matrix is packaged on Linux. Tools are installed into the
specified directory, never over system binaries. If an existing tool has the
wrong version, use a fresh directory; mismatches are rejected.

Every archive contains `BUILD.json`, also copied into `manifest.json`. It records
source commit/dirty state/input hashes, Go and protection-tool versions/hashes,
target, explicit non-race/CGO settings, compiler/obfuscator flags, final binary
hash/size, packing outcome, and validation level. `compile-and-format` means the
target header was checked; `native-version-smoke` adds execution on the build
host. Neither establishes legacy-kernel or device reliability. ARM/MIPS and
Windows ARM64 need their own native qualification.

`scripts/smoke-unified.py` verifies the exact final Linux artifacts using verified
TLS: one Node completes a shell task and tunnel transfer, continues command
execution during a tunnel-server outage, reconnects without changing its PID,
and releases its listener on shutdown. It also checks default diagnostics remain
empty. Run it with `--bin-dir DIR --report REPORT.json` after extracting artifacts.

Successful packing requires executable-header validation, `upx -t`, and an exact
decompression round trip. Native artifacts must also execute their version
command. Known unsupported-format/non-compressible errors, or a native packed
execution failure with a working obfuscated original, create a recorded platform
exception. Unexpected tool failures, integrity failures and wrong architectures
stop the build. A fallback never silently ships a plain executable.

Garble deliberately removes Go build metadata. Resource/weak-link fixtures accept
the separately verified record via `TACK_NODE_BUILD_RECORD` and require its hash
to match `TACK_NODE_BINARY`. Missing/mismatched records, missing race declarations,
wrong hosts and contradictory metadata are rejected. The record is build
provenance, not a cryptographic signature: verify its release checksums first.
Keep the source checkout and exact build inputs for `garble reverse`; the default
seed is derived from build inputs, and diagnostics are not removed. Signing, when
added, must happen after binary transformations and before final checksums.

## Measured comparison

Build three Node-only native directories from the same checkout with
`--protection plain`, `obfuscated`, and `packed`, then run:

```sh
python3 -B scripts/benchmark-protection.py \
  --plain /tmp/plain --obfuscated /tmp/obfuscated --packed /tmp/packed \
  --output /tmp/protection-comparison --seconds 5 --repeats 3 --severe-repeats 1
```

The driver verifies and extracts the actual archives, rejects different source
inputs/toolchains, rotates profile order, and records 30 fresh-process version
launches per profile. This startup measurement does **not** drop the filesystem
page cache, and does not claim a cold-storage startup measurement. It then runs
both HTTP versions of the independent Node footprint and intermittent-link
fixtures, preserving failures, raw logs, exit status and SHA-256 checksums.
The footprint uses 2 MiB outputs; the severe fixture uses 600 ms added RTT,
8 KiB/s per direction and a 500 ms outage every four seconds. Packet loss is not
injected by this driver. All profiles use GOMAXPROCS=2, GOGC=100 and the same
unchanged 32 MiB RSS gate. The independent-process CPU/RSS fixtures exclude Controller, proxy and command child. Startup CPU includes process creation overhead; startup `wait4` peak RSS is not used because it can include the Python parent before exec.

CI validates native protected artifacts on Linux/Windows and native obfuscated
CLI artifacts on macOS. Linux also tests the packed Node's footprint and recovery.
`qualify-legacy.py` verifies both variants, Legacy shell/tunnel lifecycle, both
directions of pending-result state replacement, the real old kernel, and Legacy
resource/intermittent-link gates under both HTTP versions. Tag releases repeat
these checks on the exact artifacts inside the final release archives.
Small measured regressions are acceptable; correctness or recovery failures block
publication. Local measurement evidence is in the [protection qualification](validation/2026-10-23-protection.md).

Sources: [Garble v0.14.2](https://github.com/burrowers/garble/releases/tag/v0.14.2),
[Garble v0.17.0](https://github.com/burrowers/garble/releases/tag/v0.17.0),
[UPX v5.0.2](https://github.com/upx/upx/releases/tag/v5.0.2).
