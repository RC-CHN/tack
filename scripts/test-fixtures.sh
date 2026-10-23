#!/usr/bin/env bash
# Repeat fault recovery and measure a separate, non-race Node. All endpoints
# and state are disposable and local. Run from any directory in this checkout.
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
output=${1:?Usage: test-fixtures.sh NEW_REPORT_DIRECTORY [SECONDS_PER_PHASE] [REPEATS]}
seconds=${2:-30}
repeats=${3:-3}
[[ "$seconds" =~ ^[0-9]+$ && "$repeats" =~ ^[0-9]+$ ]] || { echo 'durations and repeats must be integers' >&2; exit 2; }
(( seconds >= 3 && seconds <= 86400 && repeats >= 1 && repeats <= 100 )) || exit 2
[[ ! -e "$output" ]] || { echo 'report directory must not already exist' >&2; exit 2; }
mkdir -p -- "$output"
output=$(cd -- "$output" && pwd)

finish() {
    local result=$?
    printf '%s\n' "$result" >"$output/exit-status.txt"
    python3 - "$output" <<'PY'
import hashlib, pathlib, sys
root = pathlib.Path(sys.argv[1])
files = sorted(p for p in root.iterdir() if p.is_file() and p.name != 'SHA256SUMS')
(root / 'SHA256SUMS').write_text(''.join(f'{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n' for p in files))
PY
    return "$result"
}
trap finish EXIT

cd -- "$root"
{
    printf 'format=tack-fixture-run-v1\n'
    printf 'commit=%s\n' "$(git rev-parse HEAD)"
    printf 'seconds_per_phase=%s\nrepeats=%s\n' "$seconds" "$repeats"
    go version
    go env GOOS GOARCH
} >"$output/manifest.txt"
git status --short >"$output/source-status.txt"
# Include untracked additions as well as modified tracked files in the source
# identity without copying unrelated files or credentials into the report.
python3 - "$root" "$output/source-sha256.json" <<'PY'
import hashlib, json, pathlib, sys
root = pathlib.Path(sys.argv[1])
paths = []
for directory in ('management', 'test', 'share', 'client', 'server', 'scripts'):
    paths.extend(p for p in (root / directory).rglob('*') if p.is_file() and p.suffix in ('.go', '.mod', '.sum', '.py', '.sh'))
paths.extend([root / 'go.mod', root / 'go.sum'])
pathlib.Path(sys.argv[2]).write_text(json.dumps({str(p.relative_to(root)): hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(paths)}, indent=2) + '\n')
PY

(cd test/fixtures && go test -race -short -count=1 -timeout=3m ./... && go vet ./...) >"$output/fixtures.log" 2>&1
python3 scripts/check-fixture-coverage.py --require-complete >"$output/coverage.log" 2>&1
python3 -B -m unittest discover -s scripts -p test_fixture_coverage.py >>"$output/coverage.log" 2>&1
(cd management/node && go test -race -count=1 -timeout=3m ./... && go vet ./...) >"$output/node.log" 2>&1
(cd management/protocol && go test -race -count=1 -timeout=3m ./... && go vet ./...) >"$output/protocol.log" 2>&1
(cd management/control && go test -race -count=1 -timeout=3m ./... && go vet ./...) >"$output/control.log" 2>&1
go test -race -count=1 -timeout=3m ./client ./share/tunnel ./test/campaign ./test/cmd/... >"$output/contracts.log" 2>&1
(cd management/integration && go test -race -run '^TestManagementThroughImpairedTransport$' -count="$repeats" -timeout=30m -v .) >"$output/management-recovery.log" 2>&1
go test -race -run '^Test(TunnelRecoversAcrossImpairedTransport|SharedCarrierRecoveryWithConcurrentFlows|SOCKSMethodAndCommandRejections|SOCKSAssociationSurvivesUnreachableDestination)$' -count="$repeats" -timeout=30m -v ./test/e2e >"$output/tunnel-recovery.log" 2>&1

if [[ $(go env GOOS) != linux ]]; then
    echo 'Linux /proc is required for the independent Node footprint gates.' >&2
    exit 1
fi
node_go=${TACK_NODE_GO:-go}
(cd management/node && "$node_go" build -trimpath -o "$output/tack-node" ./cmd/tack-node)
for http2 in 0 1; do
    (cd management/integration && \
        TACK_NODE_BINARY="$output/tack-node" TACK_RESOURCE_SECONDS="$seconds" TACK_RESOURCE_HTTP2="$http2" \
        TACK_RESOURCE_REPORT="$output/footprint-http2-$http2.json" \
        go test -run '^TestNodeLowFootprint$' -count=1 -timeout="$((seconds * 4 + 300))s" -v .) >"$output/footprint-http2-$http2.log" 2>&1
done
printf 'Reports: %s\n' "$output"
