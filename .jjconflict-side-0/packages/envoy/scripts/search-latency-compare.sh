#!/usr/bin/env bash
# Compares search latency between a base checkout (usually main) and this one on the same corpus
# copy: it compiles internal/dispatch/api's tests in each, runs TestSearchLatencyOnCorpus from the
# two alternately, so both see the same machine load, and prints every run's p50 and p95. It exits
# 1 when this checkout's median p95 is more than 10% above the base's.
set -euo pipefail

if (($# < 1 || $# > 2)); then
  printf 'usage: DISPATCH_BENCH_DATABASE_URL=<copy> %s <base-checkout> [rounds]\n' "$0" >&2
  exit 2
fi
: "${DISPATCH_BENCH_DATABASE_URL:?DISPATCH_BENCH_DATABASE_URL must name a corpus copy (corpus-copy.sh)}"
readonly base_envoy="$1/packages/envoy"
readonly rounds="${2:-5}"
head_envoy="$(cd "$(dirname "$0")/.." && pwd)"
readonly head_envoy
[[ -f "${base_envoy}/go.mod" ]] || { printf 'not a legion checkout: %s\n' "$1" >&2; exit 2; }
command -v python3 >/dev/null || { printf 'python3 is required\n' >&2; exit 1; }

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
(cd "$base_envoy" && go test -c -o "${workdir}/base.test" ./internal/dispatch/api)
(cd "$head_envoy" && go test -c -o "${workdir}/head.test" ./internal/dispatch/api)

# Appends one run's "p50=<duration> p95=<duration>" line to runs.txt, read whether or not the test
# passed: an older checkout's test also fails on an absolute bound after logging it.
measure() {
  local round="$1" name="$2" envoy="$3" measured
  (cd "${envoy}/internal/dispatch/api" && "${workdir}/${name}.test" -test.run '^TestSearchLatencyOnCorpus$' -test.count=1 -test.v) \
    >"${workdir}/${name}.log" 2>&1 || true
  if ! measured="$(grep -o 'p50=[^ ]* p95=[^ ]*' "${workdir}/${name}.log")"; then
    printf '%s produced no measurement; its log:\n' "$name" >&2
    cat "${workdir}/${name}.log" >&2
    exit 1
  fi
  printf 'round %s %s %s\n' "$round" "$name" "$measured" | tee -a "${workdir}/runs.txt"
}

for round in $(seq 1 "$rounds"); do
  measure "$round" base "$base_envoy"
  measure "$round" head "$head_envoy"
done

python3 - "${workdir}/runs.txt" <<'PY'
import re, statistics, sys

units = {"ns": 1e-6, "µs": 1e-3, "us": 1e-3, "ms": 1.0, "s": 1000.0}

def millis(duration):
    total = 0.0
    for number, unit in re.findall(r"([0-9.]+)(ns|µs|us|ms|s)", duration):
        total += float(number) * units[unit]
    return total

p95 = {"base": [], "head": []}
for line in open(sys.argv[1], encoding="utf-8"):
    _, _, name, _, p95_field = line.split()
    p95[name].append(millis(p95_field.removeprefix("p95=")))
base, head = statistics.median(p95["base"]), statistics.median(p95["head"])
print(f"median p95: base {base:.1f} ms, head {head:.1f} ms ({(head / base - 1) * 100:+.1f}%)")
if head > base * 1.10:
    print("head is more than 10% slower than base")
    sys.exit(1)
PY
