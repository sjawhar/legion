#!/usr/bin/env bash
# Stage 1's gate for the Go coordinator: it boots against a real Postgres, answers /healthz and
# GET /legion/v1/state, registers itself, survives a restart against the same store with its
# first boot time intact, and refuses an unreachable Postgres by the host it could not reach and
# never by the password. Every step is fatal — no `|| true` outside the cleanup a trap must never
# fail on — and the run leaves no container, no daemon, and nothing in the real state home.
set -euo pipefail

pid=
# cleanup never returns non-zero: a trap that fails under set -e would mask the real status.
cleanup() {
  if [ -n "${pid:-}" ]; then kill -TERM "$pid" 2>/dev/null || true; fi
  docker rm -f legion-e2e-pg >/dev/null 2>&1 || true
  return 0
}

# stop_daemon: $1 is how it was asked to stop. Bounded, and the exit status read and judged
# (daemon.Run returns 0 on a cancelled context, so a non-zero status is a defect, not a stop).
stop_daemon() {
  for i in $(seq 1 50); do
    kill -0 "$pid" 2>/dev/null || break
    # SIGKILL on the way out: a leaked daemon holds the port and, under $(...) or a pipe, the caller.
    [ "$i" = 50 ] && {
      echo "daemon ignored $1"
      kill -KILL "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      exit 1
    }
    sleep 0.2
  done
  st=0
  wait "$pid" || st=$?
  [ "$st" = 0 ] || {
    echo "daemon exited $st on $1"
    exit 1
  }
  pid=
}

root=$(cd "$(dirname "$0")/../.." && pwd)
work=/tmp/legion-e2e
rm -rf "$work"
mkdir -p "$work/state" "$work/xdg"
export XDG_STATE_HOME="$work/xdg" # the registry lands here, never in the devbox's real one
project="E2E$(date +%s)"          # daemon_boot counts per project; a fresh key makes boots==1 true on any store
cd "$root/packages/daemon-go" && go build -o "$work/legion" ./cmd/legion

# CI passes its service's DSN; the devbox brings its own container on an ephemeral port.
if [ -z "${LEGION_E2E_PG_DSN:-}" ]; then
  docker rm -f legion-e2e-pg >/dev/null 2>&1 || docker ps >/dev/null # a missing container is fine; a broken docker is not
  docker run -d --name legion-e2e-pg -e POSTGRES_USER=legion -e POSTGRES_PASSWORD=legion \
    -e POSTGRES_DB=legion -p 127.0.0.1::5432 postgres:16 >/dev/null
  trap cleanup EXIT
  # Over TCP, not the container's unix socket: the entrypoint's bootstrap phase answers on the
  # socket while nothing listens on 5432 yet, and a daemon that connects then is reset.
  for i in $(seq 1 40); do
    docker exec legion-e2e-pg pg_isready -h 127.0.0.1 -p 5432 -U legion -d legion >/dev/null && break
    [ "$i" = 40 ] && {
      echo "postgres never became ready"
      exit 1
    }
    sleep 0.5
  done
  pgport=$(docker port legion-e2e-pg 5432/tcp | head -1 | sed 's/.*://')
  LEGION_E2E_PG_DSN="postgres://legion:legion@127.0.0.1:$pgport/legion"
fi

cat >"$work/legion.yaml" <<EOF
project: $project
port: 13399
postgres_dsn: $LEGION_E2E_PG_DSN
state_dir: $work/state
EOF

# 1. refuses without Postgres, naming the host and never the password. The bad DSN carries its
#    own credentials so both halves of that assertion mean something wherever this runs.
sed 's#^postgres_dsn: .*#postgres_dsn: postgres://legion:hunter2@127.0.0.1:5499/legion#' \
  "$work/legion.yaml" >"$work/bad.yaml"
if "$work/legion" start --config "$work/bad.yaml" 2>"$work/refusal.log"; then
  echo "expected refusal"
  exit 1
fi
grep -q '127.0.0.1:5499' "$work/refusal.log" || {
  echo "the refusal does not name the host it could not reach"
  exit 1
}
# `! grep …` would be read as a negation set -e ignores (shellcheck SC2251): the assertion could
# never fail. The password must fail the run, so the run says so itself.
grep -q 'hunter2' "$work/refusal.log" && {
  echo "the refusal printed the password"
  exit 1
}

# 2. starts, answers, restarts against the same store
! curl -fs http://127.0.0.1:13399/healthz >/dev/null || {
  echo "port 13399 is already answering: a leftover daemon"
  exit 1
}
"$work/legion" start --config "$work/legion.yaml" &
pid=$!
trap cleanup EXIT
for i in $(seq 1 50); do
  curl -fs http://127.0.0.1:13399/healthz >/dev/null && break
  [ "$i" = 50 ] && {
    echo "daemon never answered /healthz"
    exit 1
  }
  sleep 0.2
done
"$work/legion" state --json --port 13399 >"$work/state1.json"
jq -e --arg p "$project" \
  '.daemon.project == $p and .daemon.boots == 1 and .admission.cap == 4 and (.issues | length) == 0' \
  "$work/state1.json" || {
  echo "the first boot's state is not the project, boot 1, cap 4 and no issues stage 1 promises"
  exit 1
}
"$work/legion" legions --json |
  jq -e --arg p "$project" 'map(select(.team == $p and .port == 13399)) | length == 1' || {
  echo "the legions registry does not carry exactly one entry for $project on 13399"
  exit 1
}
kill -TERM "$pid"
stop_daemon SIGTERM

"$work/legion" start --config "$work/legion.yaml" &
pid=$!
for i in $(seq 1 50); do
  curl -fs http://127.0.0.1:13399/healthz >/dev/null && break
  [ "$i" = 50 ] && {
    echo "daemon never answered /healthz after restart"
    exit 1
  }
  sleep 0.2
done
"$work/legion" state --json --port 13399 >"$work/state2.json"
jq -e '.daemon.boots == 2' "$work/state2.json" || {
  echo "the restart did not record a second boot"
  exit 1
}
[ "$(jq -r .daemon.firstBootAt "$work/state1.json")" = "$(jq -r .daemon.firstBootAt "$work/state2.json")" ] || {
  echo "firstBootAt changed across the restart: the store is not the same one"
  exit 1
}
"$work/legion" stop --config "$work/legion.yaml"
stop_daemon "legion stop"

echo "stage 1 e2e: PASS"
