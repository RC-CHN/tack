# Tack release procedure

Tack releases use tags such as `tack-v0.1.0-rc.1`. Chisel's inherited `v*` release and Docker promotion jobs are restricted to `jpillora/chisel`; they cannot publish from this repository. Tack publication writes only to this repository's GitHub Releases.

1. Commit reviewed changes in separate logical batches and push `master`.
2. Wait for the native Linux, Windows and macOS tests, race checks, legacy tests and Node cross-builds. Review local resource/fault evidence in [the validation record](../management/VALIDATION.md).
3. Write `.github/releases/tack-VERSION.md`, including compatibility limits, state migration and backup requirements. Tag the tested commit `tack-vVERSION` and push that tag.
4. The Tack tag workflow repeats native/legacy tests and both HTTP resource tests, then builds archives. It verifies gzip/ZIP integrity and the exact archived bytes, checks native version output and target build metadata, and creates `manifest.json` and `SHA256SUMS`.
5. Only after all checks pass does the workflow create a **draft** release. Download the draft's actual assets, verify their SHA-256 checksums and manifest commit against the tag, and inspect the asset matrix. Publish the verified draft using `gh release edit TAG --draft=false`; keep candidate releases marked prerelease. Publishing does not promote Docker tags or rebuild assets.

Use `python3 scripts/build-release.py --version VERSION --output EMPTY_DIRECTORY` to reproduce packaging locally. `--native-only` is a packaging smoke test and cannot replace the release matrix. The build script never overwrites existing output files. Publication must use the clean, tagged CI checkout; local smoke artifacts may contain uncommitted development changes and must not be uploaded as release artifacts.

| Component | Toolchain | Artifact targets |
| --- | --- | --- |
| `tack-node` | Go 1.23.12, CGO disabled | Linux amd64, 386, ARM v5/v6/v7, arm64, MIPS/MIPSel/MIPS64/MIPS64el soft-float; Windows amd64, 386, arm64 |
| `tack-control` | Go 1.26.3, CGO disabled | Linux amd64/arm64; Windows amd64 |
| `tack` operator CLI | Go 1.26.3, CGO disabled | Linux and macOS amd64/arm64; Windows amd64/386/arm64 |
| `chisel` tunnel | Go 1.26.3, CGO disabled | Same architecture matrix as `tack-node`, using the modern runtime |

There are 36 archives, plus the manifest and checksums. Every archive contains its executable, license and operation guide. Linux/macOS use `.tar.gz`; Windows uses `.zip`. MIPS filenames explicitly state `softfloat`, and ARM filenames state the ARM version. Node legacy-toolchain builds are distinct from the modern Chisel tunnel and do not establish support for the whole stack on CentOS 6. BusyBox is supplied separately. Candidate binaries are ordinary builds, without obfuscation, packing or signing; do not describe them as protected commercial releases.

The resource workflow is also runnable through `workflow_dispatch`. Its 180-second tests and the longer local tests exercise faults and lifecycle cleanup, not 24–72 hour device endurance. Native Windows amd64 CI does not validate every Windows architecture. Real old-kernel, NAS storage, overlay, ARM/MIPS hardware and power-loss tests must be recorded separately before promoting those combinations to supported deployment targets.
