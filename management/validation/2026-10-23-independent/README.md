# Independent fixture validation, 2026-10-23

`fixtures/` is the final routine runner: three recovery repetitions and ten
seconds per phase for each HTTP version, with a standalone non-race Go 1.23.12
Node. `benchmarks/` is a 16-iteration, one-repetition functional benchmark smoke
run. It exercises all worker counts, but is not a stable performance baseline.
`checks/` contains full root/integration race and vet logs, repeated recovery,
fuzz, legacy compiler, isolated netem and UDP overload evidence.

The final fixture and benchmark runner source hashes were checked against the
checkout without differences. `RUNNER-SHA256SUMS` preserves the original runner
manifest. The generated Node executable is omitted; its original digest is in
`fixtures/excluded-artifacts.json`. Use the root `SHA256SUMS` to validate this
retained bundle. Source status describes each run's start, before report archival.

Windows binaries were cross-compiled but not run natively. Real packet loss was
injected only inside a disposable user/network namespace, not a physical WAN.
Full documentation and interpretation are in `management/VALIDATION.md` and
`test/fixtures/README.md`. The earlier direct-import reports in the sibling
`2026-10-23-fixtures` directory describe a different implementation.
