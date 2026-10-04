#!/usr/bin/env bash
# Prints what a searcher sees for each query in a file: Dispatch's own server, started against
# DATABASE_URL (a corpus copy from corpus-copy.sh), answers GET /api/v1/search, and each answer
# prints as one line of totals and its rows as `pos kind owner id snippet`. The output is plain
# text, so two builds' runs diff; `took_ms` sits on its own line for that reason.
#
# The server runs with a throwaway HOME, a made-up agent token and a trusted identity header, so it
# reads no operator configuration and holds no credential; NATS is off. It migrates the database
# it is given, as it does at every start, so point it only at a copy.
set -euo pipefail

usage() {
  printf 'usage: DATABASE_URL=<copy> %s --queries <file> [--binary <envoy-dispatch>] [--limit N] [--offset N]\n' "$0" >&2
  exit 2
}

queries=""
binary=""
limit=20
offset=0
while (($# > 0)); do
  case "$1" in
    --queries) queries="${2:?}"; shift 2 ;;
    --binary) binary="${2:?}"; shift 2 ;;
    --limit) limit="${2:?}"; shift 2 ;;
    --offset) offset="${2:?}"; shift 2 ;;
    *) usage ;;
  esac
done
[[ -n "$queries" ]] || usage
[[ -f "$queries" ]] || { printf 'queries file not found: %s\n' "$queries" >&2; exit 2; }
: "${DATABASE_URL:?DATABASE_URL must name a copy of the Dispatch database}"
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
port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
readonly base="http://127.0.0.1:${port}"

mkdir "${workdir}/home"
env HOME="${workdir}/home" \
  DATABASE_URL="$DATABASE_URL" \
  DISPATCH_AGENT_TOKEN=search-smoke-not-a-credential \
  DISPATCH_IDENTITY=header:X-Dispatch-User \
  DISPATCH_ALLOWED_LOGINS=smoke \
  DISPATCH_NATS_DISABLED=1 \
  DISPATCH_LISTEN_HOST=127.0.0.1 \
  DISPATCH_PORT="$port" \
  DISPATCH_SERVER_URL="$base" \
  "$binary" >"${workdir}/server.log" 2>&1 &
server_pid=$!

for _ in $(seq 1 300); do
  if curl -fsS "${base}/healthz" >/dev/null 2>&1; then
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

while IFS= read -r query || [[ -n "$query" ]]; do
  [[ -z "$query" || "$query" == \#* ]] && continue
  answer="$(curl -fsS -G -H 'X-Dispatch-User: smoke' "${base}/api/v1/search" \
    --data-urlencode "q=${query}" --data-urlencode "limit=${limit}" --data-urlencode "offset=${offset}")"
  # An answer from a server that predates totals prints "-" for them.
  jq -r --arg query "$query" --argjson offset "$offset" '
    "== \($query)  total=\(.total // "-") reachable=\(.reachable // "-")",
    "   took_ms=\(.took_ms)",
    (.results | to_entries[] | .value as $hit |
      "\(.key + 1 + $offset)\t\($hit.kind)\t\(if $hit.owner.kind == "issue" then $hit.owner.key else "\($hit.owner.project)/\($hit.owner.slug)" end)\t\($hit.id)\t\($hit.snippet | gsub("</?mark>"; "") | gsub("\\s+"; " ") | .[0:60])")
  ' <<<"$answer"
done <"$queries"
