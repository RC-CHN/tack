# Obfuscation and packing qualification — 2026-10-23

> 历史验证摘要，记录原测试时的状态。本次只提交摘要；原始 JSON/log、源码快照、校验清单及分析脚本保留在本地 `management/validation/2026-10-23-protection/`，不随仓库分发。下文所述本地文件和校验命令依赖该归档，未在本次整理中重跑基准或改写原始证据。

Local Linux amd64 measurements, based on `4dde249` plus the accompanying working
tree changes. No new release was published. Local archives carry the development
stamp `0.1.0-rc.4`, `source_dirty=true`, and build-input hashes. The final matrix
contains **36 archives: 20 packed, 16 obfuscated platform exceptions**. All archives
passed SHA-256, executable-header, archive-content and build-record verification.
Four native final executables exactly match the functional-test executables; the
final Node also exactly matches the Node used in the comparison.

The Node uses Go 1.23.12/Garble v0.14.2, the other components Go 1.26.3/Garble
v0.17.0, and the packer UPX 5.0.2. Only Tack/Chisel application packages are selected
for identifier/literal obfuscation. CGO and race instrumentation are disabled;
panic diagnostics remain. See [build protection](../build-protection.md)
for the build/validation commands and limits.

## Node comparison

| Measurement | Plain | Obfuscated | Obfuscated + packed |
| --- | ---: | ---: | ---: |
| Executable bytes | 6,070,424 | 6,385,799 | 2,410,508 |
| Executable MiB | 5.79 | 6.09 | 2.30 |
| tar.gz archive bytes | 2,593,025 | 2,728,021 | 2,384,640 |
| Fresh-process version query median, ms | 2.254 | 2.246 | 48.651 |
| Fresh-process version query p95, ms | 3.864 | 3.076 | 54.318 |
| Startup CPU median, ms | 2.347 | 2.474 | 48.968 |
| Highest Node RSS/high-water across six resource runs, MiB | 13.38 | 13.92 | 14.79 |
| H1 severe-link completion, seconds | 27.624 | 38.921 | 30.126 |
| H2 severe-link completion, seconds | **Incomplete at 75.052** | 35.896 | 43.937 |

Packing reduces executable size by **60.3%** relative to the stripped plain
baseline. Because the ordinary release archive is already compressed, archive
size falls by only **8.0%**. The measured costs are about **46.4 ms** extra startup
and **1.41 MiB** extra peak Node memory. These are local observations, not universal
ceilings or device guarantees.

Startup has 30 samples/profile with rotating order. It creates a new process but
does not drop filesystem caches, so it is not a cold-storage measurement. Its CPU
includes process-creation overhead. The initial driver recorded `wait4` peak RSS,
which can include Python's forked image before exec; **ignore the
`startup_peak_rss_bytes` / startup `peak_rss_bytes` fields in the raw initial
report**. They are deliberately preserved as raw evidence, excluded from the
derived `benchmark-summary.json` and all numbers above, and removed from the
current driver. Memory figures above come from the separate Node's `/proc` RSS and
VmHWM after exec.

Resource tests use 2 MiB outputs, three runs per HTTP version/profile, five seconds
per phase, GOMAXPROCS=2, GOGC=100 and the unchanged 32 MiB gate. **18/18 passed**.
Across all sampled idle/recovered-idle windows, each profile had zero observed
CPU-tick, stream-traffic, storage-write and diagnostic-log increments. The windows
are short and CPU ticks have finite resolution; this is not a zero-power claim.

Severe-link tests use 600 ms added RTT, 8 KiB/s per connection/direction, a 500 ms
outage every four seconds, and a 32,783-byte result. There is one run per HTTP
version/profile, with no netem packet loss. **5/6 completed**. The plain H2 run made
zero durable progress within 75 seconds; the comparison exit status is correctly
**1**. Its failure and TLS interruption logs remain in `comparison/`. Both packed
runs completed with exact output. This small sample demonstrates working recovery,
not a speed benefit from obfuscation or a statistically established latency
regression. Compilations also ran on the shared 104-CPU host; these are not isolated
microbenchmark results.

## Additional verification

- `go test -race ./...` passed in the integration module, including negative tests
  for missing, mismatched, race-enabled and wrong-host build records.
- Four Python protection tests, workflow actionlint, fixture coverage and spelling
  checks passed. Protected CI jobs are configured; no hosted CI run is claimed for
  these uncommitted changes.
- Actual packed CLI/Controller/Node provisioning, execution and result collection
  passed; actual packed Chisel forwarded and verified 262,144 bytes.
- Actual packed Controller/Node TLS backlog test passed: ten results, no duplicate
  executions, enforced eight-result backpressure, restart and output recovery.
  That stress test uses its own untuned runtime settings and is separate from the
  comparison table.
- Nine non-amd64 Linux Node artifacts passed QEMU version queries: 386, ARM v5/v6/v7,
  arm64, MIPS/MIPSel/MIPS64/MIPS64el. This is emulation smoke coverage, not native
  weak-link, old-kernel, reboot or device endurance validation.
- Windows archives retain obfuscated binaries because UPX's exact-round-trip mode
  rejects their PE files, or because Windows ARM64 is unsupported. The two macOS
  CLI archives and four Linux MIPS64/MIPS64el archives also retain obfuscated
  binaries after explicit unsupported-format errors. Native Windows/macOS testing
  remains for CI/target machines; no forced packing was used.

## Evidence and provenance

`artifact-manifest.json` and `artifact-SHA256SUMS.txt` identify all actual archives;
the archives are in `dist/protected-rc4-local/`, not committed here. Source/script
snapshots and `implementation.patch` describe the local implementation. Validation
scripts continued to evolve during qualification; each build record retains its
own source-input digest rather than pretending to be a clean tagged release.
The identical executable hashes bind the final matrix to the measured/tested
bytes despite changes to validation-only scripts.

`benchmark-protection-used.py` is the initial comparison driver; `sources/` holds
the corrected driver and final implementation snapshots. `comparison/` preserves
raw reports/logs and its failing exit status. `benchmark-summary.json` derives the
table while excluding the invalid startup RSS measurement. Binary fixture/test
executables and ephemeral credentials are not included.

`initial-trial-manifest.json`, `upx-compatibility.json`, debugger/syscall logs and
the three earlier matrix logs preserve rejected tool/format combinations. In
particular, UPX 5.2.1 failed before Go startup on the initial static Garble v0.13
Node; UPX 5.0.2 ran the same input 20/20 times. The final combination uses the later
Go-1.23-compatible Garble v0.14.2. PE inspection found 18 changed bytes in headers
and import data after default decompression; requiring `--exact` converts that
unsupported transform into a documented exception instead of waiving integrity.
