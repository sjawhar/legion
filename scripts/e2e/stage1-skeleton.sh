#!/usr/bin/env bash
# Stage 1's gate for the Go coordinator: it boots against a real Postgres, answers /healthz and
# GET /legion/v1/state, registers itself, survives a restart against the same store with its
# first boot time intact, and refuses an unreachable Postgres by the host it could not reach and
# never by the password. Every step is fatal — no `|| true` outside the cleanup a trap must never
# fail on — and the run leaves no container, no daemon, and nothing in the real state home.
#
# Everything it takes is this run's own: its work directory, its container name, its project key
# and its port. Two runs on one box (a CI job and a devbox session, or two sessions) do not
# collide, and neither reports the other as a leftover.
set -euo pipefail

pid=
ok=
container=legion-e2e-pg-$$
work=$(mktemp -d /tmp/legion-e2e.XXXXXXXX)
# cleanup never returns non-zero: a trap that fails under set -e would mask the real status. The
# work directory survives a failure — its state documents and refusal log are the evidence — and
# goes when the run passed.
cleanup() {
  if [ -n "${pid:-}" ]; then kill -TERM "$pid" 2>/dev/null || true; fi
  docker rm -f "$container" >/dev/null 2>&1 || true
  if [ -n "${ok:-}" ]; then
    rm -rf "$work"
  else
    echo "the run's workspace is $work"
  fi
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
trap cleanup EXIT
mkdir -p "$work/state" "$work/xdg"
export XDG_STATE_HOME="$work/xdg" # the registry lands here, never in the devbox's real one
project="E2E$$$(date +%s)"        # daemon_boot counts per project; a fresh key makes boots==1 true on any store
# A port this run holds alone, so a daemon of another run is never mistaken for this one's. The
# check for `ss` is not decoration: `ss -ltn … | grep -q LISTEN || break` fails open without it —
# a missing `ss` exits 127, grep sees nothing, and the loop leaves with an unchecked port.
command -v ss >/dev/null || {
  echo "ss (iproute2) is required to pick this run's port"
  exit 1
}
for i in $(seq 1 50); do
  port=$((20000 + RANDOM % 20000))
  ss -ltn "sport = :$port" | grep -q LISTEN || break
  [ "$i" = 50 ] && {
    echo "no free port found for the daemon"
    exit 1
  }
done
cd "$root/packages/daemon-go" && go build -o "$work/legion" ./cmd/legion

# CI passes its service's DSN; the devbox brings its own container on an ephemeral port.
if [ -z "${LEGION_E2E_PG_DSN:-}" ]; then
  docker ps >/dev/null # a broken docker is a failure of this run, not of the daemon
  docker run -d --name "$container" -e POSTGRES_USER=legion -e POSTGRES_PASSWORD=legion \
    -e POSTGRES_DB=legion -p 127.0.0.1::5432 postgres:16 >/dev/null
  # Over TCP, not the container's unix socket: the entrypoint's bootstrap phase answers on the
  # socket while nothing listens on 5432 yet, and a daemon that connects then is reset.
  for i in $(seq 1 40); do
    docker exec "$container" pg_isready -h 127.0.0.1 -p 5432 -U legion -d legion >/dev/null && break
    [ "$i" = 40 ] && {
      echo "postgres never became ready"
      exit 1
    }
    sleep 0.5
  done
  pgport=$(docker port "$container" 5432/tcp | head -1 | sed 's/.*://')
  LEGION_E2E_PG_DSN="postgres://legion:legion@127.0.0.1:$pgport/legion"
fi

cat >"$work/legion.yaml" <<EOF
project: $project
port: $port
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
"$work/legion" start --config "$work/legion.yaml" &
pid=$!
for i in $(seq 1 50); do
  curl -fs "http://127.0.0.1:$port/healthz" >/dev/null && break
  [ "$i" = 50 ] && {
    echo "daemon never answered /healthz"
    exit 1
  }
  sleep 0.2
done
"$work/legion" state --json --port "$port" >"$work/state1.json"
jq -e --arg p "$project" \
  '.daemon.project == $p and .daemon.boots == 1 and .admission.cap == 4 and (.issues | length) == 0' \
  "$work/state1.json" || {
  echo "the first boot's state is not the project, boot 1, cap 4 and no issues stage 1 promises"
  exit 1
}
"$work/legion" legions --json |
  jq -e --arg p "$project" --argjson port "$port" 'map(select(.team == $p and .port == $port)) | length == 1' || {
  echo "the legions registry does not carry exactly one entry for $project on $port"
  exit 1
}
kill -TERM "$pid"
stop_daemon SIGTERM

"$work/legion" start --config "$work/legion.yaml" &
pid=$!
for i in $(seq 1 50); do
  curl -fs "http://127.0.0.1:$port/healthz" >/dev/null && break
  [ "$i" = 50 ] && {
    echo "daemon never answered /healthz after restart"
    exit 1
  }
  sleep 0.2
done
"$work/legion" state --json --port "$port" >"$work/state2.json"
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

ok=1
echo "stage 1 e2e: PASS"
