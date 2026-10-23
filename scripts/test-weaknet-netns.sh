#!/usr/bin/env bash
# Apply packet impairments only inside a disposable user/network namespace.
# Build test executables first: this namespace has no external network route.
set -euo pipefail

if (($# == 0)); then
    echo 'Usage: test-weaknet-netns.sh COMMAND [ARG...]' >&2
    echo 'Environment: TACK_NETEM_DELAY_MS=25 TACK_NETEM_LOSS_PERCENT=2 TACK_NETEM_RATE_KBIT=1024 TACK_NETEM_SEED=7' >&2
    exit 2
fi
delay_ms=${TACK_NETEM_DELAY_MS:-25}
loss_percent=${TACK_NETEM_LOSS_PERCENT:-2}
rate_kbit=${TACK_NETEM_RATE_KBIT:-1024}
seed=${TACK_NETEM_SEED:-7}
for value in "$delay_ms" "$loss_percent" "$rate_kbit" "$seed"; do
    [[ "$value" =~ ^[0-9]+$ ]] || { echo 'netem parameters must be nonnegative integers' >&2; exit 2; }
done
(( loss_percent <= 100 && rate_kbit > 0 )) || { echo 'invalid loss or rate' >&2; exit 2; }
for tool in unshare ip tc; do command -v "$tool" >/dev/null || { echo "missing $tool" >&2; exit 1; }; done

# No sudo, host interface changes, or fallback to a shared network namespace.
exec unshare --user --map-root-user --net -- bash -c '
    set -euo pipefail
    ip link set lo up
    tc qdisc add dev lo root netem delay "${1}ms" loss "${2}%" rate "${3}kbit" seed "$4"
    shift 4
    tc -s qdisc show dev lo >&2
    set +e
    "$@"
    result=$?
    tc -s qdisc show dev lo >&2
    exit "$result"
' bash "$delay_ms" "$loss_percent" "$rate_kbit" "$seed" "$@"
