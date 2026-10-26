#!/usr/bin/env bash
# Scores a Dispatch build against LEGION-386/LEGION-549's duplicate-issue and paraphrase query
# sets on a real corpus copy (corpus-copy.sh): for each `query<TAB>target_key` line of a queries
# file, searches query and records the 1-indexed rank at which an issue hit named target_key
# appears (0 if it is not in the first --limit results), then prints each query's rank and the
# set's MRR (mean reciprocal rank; a miss scores 0). Two query files ship beside this script:
# searchbench-pairs.txt (known related-issue pairs, searched by one's own title) and
# searchbench-paraphrases.txt (paraphrases sharing no meaningful word with their target's title -
# LEGION-549's acceptance bar, meaning search's share of the result).
#
# Run it once per build (main, a keyword-fusion head, a meaning-search head) against the same
# corpus copy and compare the printed MRR; a build whose server calls Bedrock needs the usual AWS
# credentials (AWS_REGION and the default credential chain) in its own environment, not this
# script'''s.
set -euo pipefail

usage() {
  printf 'usage: DATABASE_URL=<copy> %s --queries <file> [--binary <envoy-dispatch>] [--limit N] [--label NAME]\n' "$0" >&2
  exit 2
}

queries=""
binary=""
limit=5
label=""
while (($# > 0)); do
  case "$1" in
    --queries) queries="${2:?}"; shift 2 ;;
    --binary) binary="${2:?}"; shift 2 ;;
    --limit) limit="${2:?}"; shift 2 ;;
    --label) label="${2:?}"; shift 2 ;;
    *) usage ;;
  esac
done
[[ -n "$queries" ]] || usage
[[ -f "$queries" ]] || { printf 'queries file not found: %s\n' "$queries" >&2; exit 2; }
: "${DATABASE_URL:?DATABASE_URL must name a copy of the Dispatch database (corpus-copy.sh)}"
for command in curl jq python3; do
  command -v "$command" >/dev/null || { printf '%s is required\n' "$command" >&2; exit 1; }
done

workdir="$(mktemp -d)"
server_pid=""
cleanup() {
  if [[ -n "$server_pid" ]]; then
    kill "$server_pid" 2>/dev/null && wait "$server_pid" 2>/dev/null
  fi
  rm -rf "$workdir"
}
trap cleanup EXIT

if [[ -z "$binary" ]]; then
  binary="${workdir}/envoy-dispatch"
  (cd "$(dirname "$0")/.." && go build -o "$binary" ./cmd/dispatch)
fi
root="$(cd "$(dirname "$0")/../../.." && pwd)"
readonly root
# shellcheck source-path=SCRIPTDIR source=../../../scripts/e2e/lib/port-file.sh
. "${root}/scripts/e2e/lib/port-file.sh"

# held-port.py opens 127.0.0.1:0 and hands the socket to the server, so the port the kernel chose
# stays held until the server serves on it; it then runs as the server, so $! is the server's PID.
mkdir "${workdir}/home"
HOME="${workdir}/home" \
  DATABASE_URL="$DATABASE_URL" \
  DISPATCH_AGENT_TOKEN=search-smoke-not-a-credential \
  DISPATCH_IDENTITY=header:X-Dispatch-User \
  DISPATCH_IDENTITY_HEADER_TRUSTED=1 \
  DISPATCH_NATS_DISABLED=1 \
  DISPATCH_LISTEN_HOST=127.0.0.1 \
  python3 "${root}/scripts/e2e/lib/held-port.py" exec --port-file "${workdir}/port" \
    --fd-env DISPATCH_LISTEN_FD \
    --env 'DISPATCH_PORT={port}' --env 'DISPATCH_SERVER_URL=http://127.0.0.1:{port}' \
    -- "$binary" >"${workdir}/server.log" 2>&1 &
server_pid=$!

port=""
waited=0
read_port_file "${workdir}/port" "$server_pid" port || waited=$?
case $waited in
  0) ;;
  1)
    printf 'the server exited before its port was held; its log:\n' >&2
    cat "${workdir}/server.log" >&2
    exit 1
    ;;
  *) printf 'no port was held for the server within %d seconds\n' "$port_file_seconds" >&2; exit 1 ;;
esac
readonly base="http://127.0.0.1:${port}"

# The held socket queues a request until the server serves it, so each poll is bounded.
for _ in $(seq 1 300); do
  if curl -fsS --max-time 2 "${base}/healthz" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$server_pid" 2>/dev/null; then
    printf 'the server exited; its log:\n' >&2
    cat "${workdir}/server.log" >&2
    exit 1
  fi
  sleep 1
done
curl -fsS "${base}/healthz" >/dev/null

printf '%s\n' "${label:-$binary}"
reciprocal_sum="0"
count=0
while IFS=$'\t' read -r query target || [[ -n "$query" ]]; do
  [[ -z "$query" || "$query" == \#* ]] && continue
  count=$((count + 1))
  answer="$(curl -fsS -G -H 'X-Dispatch-User: smoke@example.test' "${base}/api/v1/search" \
    --data-urlencode "q=${query}" --data-urlencode "limit=${limit}")"
  rank="$(jq -r --arg target "$target" '
    [.results | to_entries[] | select(.value.owner.key == $target or .value.id == $target) | .key + 1] | (.[0] // 0)
  ' <<<"$answer")"
  degraded="$(jq -r '.degraded // ""' <<<"$answer")"
  printf '  rank=%d\tdegraded=%s\t%s -> %s\n' "$rank" "$degraded" "$query" "$target"
  reciprocal_sum="$(python3 -c "print(${reciprocal_sum} + (1.0/${rank} if ${rank} > 0 else 0.0))")"
done <"$queries"
mrr="$(python3 -c "print(round(${reciprocal_sum}/${count}, 4) if ${count} else 0)")"
printf '  MRR (top %d, %d queries) = %s\n' "$limit" "$count" "$mrr"
