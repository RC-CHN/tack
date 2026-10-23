#!/usr/bin/env bash
# Compare immutable non-race Node binaries with one shared Linux fixture.
set -euo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
before=$(realpath -- "${1:?Usage: compare-node-footprint.sh BEFORE_NODE AFTER_NODE NEW_REPORT_DIR [SECONDS_PER_PHASE] [REPEATS]}")
after=$(realpath -- "${2:?after binary required}")
output=${3:?new report directory required}
seconds=${4:-10}
repeats=${5:-3}
[[ "$seconds" =~ ^[0-9]+$ && "$repeats" =~ ^[0-9]+$ ]] || exit 2
(( seconds >= 3 && seconds <= 86400 && repeats >= 1 && repeats <= 100 )) || exit 2
[[ -x "$before" && -x "$after" && ! -e "$output" ]] || exit 2
mkdir -p -- "$output"
output=$(cd -- "$output" && pwd)
export GOMAXPROCS=${GOMAXPROCS:-2}
finish() {
    local result=$?
    printf '%s\n' "$result" >"$output/exit-status.txt"
    python3 - "$output" <<'PY'
import hashlib,pathlib,sys
root=pathlib.Path(sys.argv[1])
with (root/'SHA256SUMS').open('w') as out:
    for path in sorted(root.iterdir()):
        if path.is_file() and path.name!='SHA256SUMS':
            with path.open('rb') as f:
                digest=hashlib.file_digest(f,'sha256').hexdigest()
            out.write(f'{digest}  {path.name}\n')
PY
    return "$result"
}
trap finish EXIT
python3 - "$root" "$output" "$before" "$after" "$seconds" "$repeats" <<'PY'
import hashlib,json,os,pathlib,platform,subprocess,sys
root,out,before,after,seconds,repeats=sys.argv[1:]
root=pathlib.Path(root);out=pathlib.Path(out)
binaries={}
for label,path in [('before',before),('after',after)]:
    with open(path,'rb') as f: digest=hashlib.file_digest(f,'sha256').hexdigest()
    binaries[label]={'path':path,'sha256':digest,'build':subprocess.check_output(['go','version','-m',path],text=True)}
manifest={'format':'tack-footprint-comparison-v1','binaries':binaries,'seconds_per_phase':int(seconds),'repeats_per_http_version':int(repeats),'gomaxprocs':os.environ['GOMAXPROCS'],'gogc':os.environ.get('GOGC','default'),'gomemlimit':os.environ.get('GOMEMLIMIT','default'),'platform':platform.platform(),'fixture_go':subprocess.check_output(['go','version'],text=True).strip(),'output_bytes_per_task':2097152,'order':'before/after on odd rounds, after/before on even rounds'}
(out/'manifest.json').write_text(json.dumps(manifest,indent=2)+'\n')
paths=[p for directory in ('management','test','scripts') for p in (root/directory).rglob('*') if p.is_file() and p.suffix in ('.go','.mod','.sum','.sh','.py')]
(out/'fixture-source-sha256.json').write_text(json.dumps({str(p.relative_to(root)):hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(paths)},indent=2)+'\n')
PY
(cd -- "$root/management/integration" && go test -c -o "$output/footprint.test" .)
result=0
for http2 in 0 1; do
    for ((round=1; round<=repeats; round++)); do
        order=(before after)
        if (( round % 2 == 0 )); then order=(after before); fi
        for label in "${order[@]}"; do
            binary=$before
            if [[ "$label" == after ]]; then binary=$after; fi
            name="$label-h2-$http2-round-$round"
            if TACK_NODE_BINARY="$binary" TACK_RESOURCE_SECONDS="$seconds" TACK_RESOURCE_HTTP2="$http2" \
                TACK_RESOURCE_MAX_OUTPUT=1 TACK_RESOURCE_REPORT="$output/$name.json" \
                "$output/footprint.test" -test.run '^TestNodeLowFootprint$' -test.count=1 -test.timeout="$((seconds*4+300))s" -test.v >"$output/$name.log" 2>&1; then
                printf '%s PASS\n' "$name"
            else
                printf '%s FAIL (see %s)\n' "$name" "$output/$name.log" >&2
                result=1
            fi
        done
    done
done
exit "$result"
