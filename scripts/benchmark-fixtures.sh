#!/usr/bin/env bash
# Repeat allocation/latency/resource benchmarks, with complete raw Go JSON logs.
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
output=${1:?Usage: benchmark-fixtures.sh NEW_REPORT_DIRECTORY [BENCHTIME] [REPEATS]}
benchtime=${2:-250ms}
repeats=${3:-3}
[[ "$benchtime" =~ ^[1-9][0-9]*(ns|us|ms|s|x)$ && "$repeats" =~ ^[1-9][0-9]*$ ]] || exit 2
(( repeats <= 100 )) || exit 2
[[ ! -e "$output" ]] || { echo 'report directory must be new' >&2; exit 2; }
mkdir -p -- "$output"
output=$(cd -- "$output" && pwd)
finish() {
    local result=$?
    printf '%s\n' "$result" >"$output/exit-status.txt"
    python3 - "$output" <<'PY'
import hashlib,pathlib,sys
root=pathlib.Path(sys.argv[1])
with (root/'SHA256SUMS').open('w') as out:
    for path in sorted(root.iterdir()):
        if path.is_file() and path.name!='SHA256SUMS':
            hasher=hashlib.sha256()
            with path.open('rb') as source:
                for chunk in iter(lambda:source.read(65536),b''):
                    hasher.update(chunk)
            digest=hasher.hexdigest()
            out.write(f'{digest}  {path.name}\n')
PY
    return "$result"
}
trap finish EXIT
cd -- "$root"
{
    printf 'format=tack-benchmark-v1\ncommit=%s\nbenchtime=%s\nrepeats=%s\n' "$(git rev-parse HEAD)" "$benchtime" "$repeats"
    go version
    go env GOOS GOARCH
} >"$output/manifest.txt"
git status --short >"$output/source-status.txt"
python3 - "$root" "$output/source-sha256.json" <<'PY'
import hashlib,json,pathlib,sys
root=pathlib.Path(sys.argv[1]); files=[]
for directory in ('management','test','share','client','server','scripts'):
    files.extend(p for p in (root/directory).rglob('*') if p.is_file() and p.suffix in ('.go','.mod','.sum','.sh','.py'))
files.extend((root/'go.mod',root/'go.sum'))
pathlib.Path(sys.argv[2]).write_text(json.dumps({str(p.relative_to(root)):hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(files)},indent=2)+'\n')
PY
run() {
    local directory=$1 name=$2
    shift 2
    (cd -- "$directory" && go test -run '^$' -bench . -benchmem -benchtime="$benchtime" -count="$repeats" -timeout=15m -json "$@") >"$output/$name.jsonl" 2>&1
}
run "$root/test/fixtures" fixture ./linklab
run "$root/management/node" node . ./runner
run "$root/management/protocol" protocol . ./rotatelog ./telemetry
run "$root/management/control" control .
run "$root" tunnel ./test/e2e ./test/campaign
printf 'Benchmark evidence: %s\n' "$output"
