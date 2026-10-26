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
#
# The daemon supervises its agents under tmux, so it refuses to start without what a launch
# needs: tmux on PATH, an operator token file for the spawn surface, and an OMP to run, which its
# plugin gate holds to this daemon's contract before anything else. This proof launches no agent,
# so the OMP it names is a stub that answers the gate's load probe and nothing else, under a home
# of the run's own that holds the checkout's plugin manifest where the gate reads it.
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
mkdir -p "$work/state" "$work/xdg" "$work/home"
# The daemon runs from here, so mise resolves its tmux from here too, whatever directory the run
# was started from.
cd "$root/packages/daemon"
# lib/rig.sh's real_bin_path calls it inside $(…), which would swallow a message on stdout.
fail() {
  echo "$*" >&2
  exit 1
}
# wait_healthz [SUFFIX] polls the daemon ($pid) on $port for 10 s. A daemon that exits first stops
# the run at once, naming the daemon's exit status, rather than after the full wait.
wait_healthz() {
  local i status=0
  for i in $(seq 1 50); do
    curl -fs "http://127.0.0.1:$port/healthz" >/dev/null && return 0
    if ! kill -0 "$pid" 2>/dev/null; then
      wait "$pid" || status=$?
      pid= # reaped: cleanup must not signal a number the kernel may reuse
      echo "daemon exited ($status) before it answered /healthz${1:-}"
      exit 1
    fi
    sleep 0.2
  done
  echo "daemon never answered /healthz${1:-}"
  exit 1
}
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# A devbox's first tmux can be a wrapper script or a mise shim. mise runs it from the daemon's
# working directory, under the daemon's tmux environment, which moves the XDG directories under the
# state directory; this run's own HOME (pane_home, below) has the same effect. From a working
# directory under the operator's real HOME, mise then finds that HOME's config as a parent
# directory's config with no trust record, so it exits 1. The daemon's boot scrubs its private tmux
# server's environment (`tmux show-environment`) and reads that exit as a failure, not as no
# server, so boot refuses.
# So when the first tmux is a script or mise itself, the run's PATH, and so the daemon's, starts
# with the real binary (lib/rig.sh, real_bin_path), which fails loudly when mise cannot name it.
# CI's tmux is the runner's apt binary, which this leaves alone. mise reads which configs are
# trusted under XDG_STATE_HOME, so this runs before the run moves that.
first_tmux=$(command -v tmux || true)
mise_bin=$(command -v mise || true)
if [ -n "$first_tmux" ] && {
  [ "$(head -c 2 "$first_tmux")" = "#!" ] || {
    [ -n "$mise_bin" ] && [ "$(readlink -f "$first_tmux")" = "$(readlink -f "$mise_bin")" ]
  }
}; then
  PATH="$(real_bin_path "$work/realbin" tmux):$PATH"
fi
command -v tmux >/dev/null || {
  echo "tmux is required: the Go daemon supervises under tmux and refuses to start without it"
  exit 1
}
export XDG_STATE_HOME="$work/xdg" # the registry lands here, never in the devbox's real one
project="E2E$$$(date +%s)"        # daemon_boot counts per project; a fresh key makes boots==1 true on any store
# A port this run holds alone, so a daemon of another run is never mistaken for this one's, and
# below the kernel's ephemeral range, so no socket the run opens before the daemon binds can take
# it (scripts/e2e/lib/free-port.sh). The restart reuses it.
port=$(bash "$root/scripts/e2e/lib/free-port.sh")
go build -o "$work/legion" ./cmd/legion
bash "$root/scripts/e2e/lib/built-from.sh" "$root" "$work/legion" | sed 's/^/   /'

# CI passes its service's DSN; the devbox brings its own container on an ephemeral port.
if [ -z "${LEGION_E2E_PG_DSN:-}" ]; then
  docker ps >/dev/null # a broken docker is a failure of this run, not of the daemon
  docker run -d --name "$container" --cpus 2 --memory 2g -e POSTGRES_USER=legion -e POSTGRES_PASSWORD=legion \
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

# The operator bearer, as the 0600 file operator_token_file names, and the OMP the daemon resolves
# at boot (LEGION_OMP_PATH): a stub that answers the plugin gate's load probe (`omp models
# --extension <probe> --json`) as a pane that loads pi-legion beside pi-envoy does — pi-legion
# loaded, from the package whose manifest the gate read, speaking Envoy plugin interface 1, and
# pi-envoy publishing interface 1 from a file an @sjawhar/pi-envoy manifest owns (the gate holds
# the Envoy entry's file to a manifest of that name) — says why anything else should never have
# run, and fails it. The gate's contract probe reads the pi-legion manifest where Oh My Pi finds it
# under the pane's HOME and default profile: the daemon runs with HOME=$work/home and neither
# profile variable, and the manifest there is the checkout's own, so it declares the contract this
# checkout's daemon requires. The gate also asks the load probe for the task agents and skills the
# role prompts name (LEGION_PROMPT_AGENTS, LEGION_PROMPT_SKILLS) and whether each of those agents'
# models resolves (LEGION_AGENT_MODELS), and the stub answers that they all resolve.
(umask 077 && printf 'stage1-operator-%s\n' "$project" >"$work/operator-token")
cat >"$work/omp" <<'EOF'
#!/bin/sh
if [ "$1" = models ]; then
  echo LEGION_PLUGIN_LOADED=yes >&2
  echo "LEGION_PLUGIN_LOADED_FROM=file://$HOME/.omp/plugins/node_modules/@sjawhar/pi-legion/dist/legion.js" >&2
  echo LEGION_PLUGIN_ENVOY_INTERFACE=1 >&2
  echo LEGION_ENVOY_INTERFACE=1 >&2
  echo "LEGION_ENVOY_LOADED_FROM=file://$HOME/.omp/plugins/node_modules/@sjawhar/pi-envoy/dist/envoy.js" >&2
  [ -n "${LEGION_PROMPT_AGENTS:-}" ] && echo LEGION_PROMPT_AGENTS=resolved >&2
  [ -n "${LEGION_PROMPT_AGENTS:-}" ] && [ -z "${LEGION_SKIP_AGENT_MODELS:-}" ] && echo LEGION_AGENT_MODELS=resolved >&2
  [ -n "${LEGION_PROMPT_SKILLS:-}" ] && echo LEGION_PROMPT_SKILLS=resolved >&2
  exit 0
fi
echo "stage 1 launches no agent" >&2
exit 1
EOF
chmod 0755 "$work/omp"
export LEGION_OMP_PATH="$work/omp"
plugins="$work/home/.omp/plugins/node_modules/@sjawhar"
mkdir -p "$plugins/pi-legion" "$plugins/pi-envoy"
jq '{name, version, legion}' "$root/packages/pi-legion/package.json" >"$plugins/pi-legion/package.json"
jq '{name, version}' "$root/packages/pi-envoy/package.json" >"$plugins/pi-envoy/package.json"
pane_home=(env -u OMP_PROFILE -u PI_PROFILE HOME="$work/home")

# Beyond what boot needs, the file plants what the state's `settings` must show and must not
# (LEGION-791): a max_fix_attempts the default would not answer, a github_apps block whose
# inline PEM fixture is the App key the state must never print, and an envoy_url carrying userinfo
# at the default host and port, the credential the settings must mask. The loader takes github_apps
# as the workflow configured and then wants projects too (resolveStage3: `projects must declare at
# least one project`, `github_apps is required`), so the two come together; a project's
# repositories are Dispatch's mapping, so its entry is empty. The one project is not this daemon's
# own, so boot resolves no host git, jj or clone credential for it (readTmux reads tools only for
# cfg.Projects[cfg.Project]), and with no dispatch_url there is no workflow and no App token mint
# (openWorkflow): the proof still launches nothing and needs nothing new.
cat >"$work/legion.yaml" <<EOF
project: $project
port: $port
postgres_dsn: $LEGION_E2E_PG_DSN
state_dir: $work/state
operator_token_file: $work/operator-token
max_fix_attempts: 5
envoy_url: http://legion:fixture-envoy-password@127.0.0.1:9020
projects:
  FIXTURE: {}
github_apps:
  implement:
    app_id: "1001"
    private_key: |
      -----BEGIN PRIVATE KEY-----
      fixture
      -----END PRIVATE KEY-----
  review:
    app_id: "1002"
    private_key: |
      -----BEGIN PRIVATE KEY-----
      fixture
      -----END PRIVATE KEY-----
EOF

# 1. refuses without Postgres, naming the host and never the password. The bad DSN carries its
#    own credentials so both halves of that assertion mean something wherever this runs.
sed 's#^postgres_dsn: .*#postgres_dsn: postgres://legion:hunter2@127.0.0.1:5499/legion#' \
  "$work/legion.yaml" >"$work/bad.yaml"
if "${pane_home[@]}" "$work/legion" start --config "$work/bad.yaml" 2>"$work/refusal.log"; then
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
"${pane_home[@]}" "$work/legion" start --config "$work/legion.yaml" &
pid=$!
wait_healthz
"$work/legion" state --json --port "$port" >"$work/state1.json"
jq -e --arg p "$project" \
  '.daemon.project == $p and .daemon.boots == 1 and .admission.cap == 4 and (.issues | length) == 0' \
  "$work/state1.json" || {
  echo "the first boot's state is not the project, boot 1, cap 4 and no issues stage 1 promises"
  exit 1
}
# The deployment's capability report rides the same document (LEGION-578): every row of the table,
# and under tmux with nothing decided the resource-limits row open with the legion.yaml line that
# records a decision — a report, never a refusal, so the daemon served it — and the pool-capacity
# row present, since the tmux runtime runs no probe pod. Both carry `source` "configuration"
# (contract 25): under tmux, resource-limits is what the runtime alone decides and pool-capacity
# reads that tmux runs no probe pod, so neither is boot's measurement; and no live, image or
# withheld row carries `source` at all.
jq -e '(.capabilities | length) == 20
  and (.capabilities | map(select(.name == "resource-limits" and .status == "open"
    and .configLine == "capabilities.decided.resource-limits: \"<reason>\""
    and .source == "configuration")) | length) == 1
  and (.capabilities | map(select(.name == "pool-capacity" and .status == "present"
    and .source == "configuration")) | length) == 1
  and (.capabilities | map(select((.name != "resource-limits" and .name != "pool-capacity"
    and .name != "secrets" and .name != "model-fallback") and has("source"))) | length) == 0' \
  "$work/state1.json" || {
  echo "the state does not carry the 20-row capability report with resource-limits and pool-capacity's source under tmux"
  exit 1
}
# The daemon's resolved settings ride the same document (LEGION-791, contract 35): the value the
# fixture set over its default, a default the fixture left alone, the runtime, the envoy_url with
# its userinfo masked, and none of what the fixture planted as a credential, a credential file or an
# App key — the App rides as its id and installations alone, and nothing anywhere in the document
# reads as a PEM, a DSN, the planted password or a `user:password@` of any URL.
jq -e '.settings.max_fix_attempts == 5
  and .settings.worker_boot_timeout_seconds == 120
  and .settings.runtime.name == "tmux"
  and .settings.envoy_url == "http://xxxxx@127.0.0.1:9020"
  and ((.settings | has("postgres_dsn") or has("operator_token_file")) | not)
  and ([.. | strings | select(test("BEGIN|PRIVATE KEY|postgres://|fixture-envoy-password|://[^/@ ]+:[^/@ ]+@"))] | length) == 0
  and (.settings.github_apps.implement | keys) == ["app_id", "installations"]' \
  "$work/state1.json" || {
  echo "the state does not carry the resolved settings with the fixture's max_fix_attempts, the masked envoy_url and no credential, credential file or App key"
  exit 1
}
"$work/legion" legions --json |
  jq -e --arg p "$project" --argjson port "$port" 'map(select(.team == $p and .port == $port)) | length == 1' || {
  echo "the legions registry does not carry exactly one entry for $project on $port"
  exit 1
}
kill -TERM "$pid"
stop_daemon SIGTERM

"${pane_home[@]}" "$work/legion" start --config "$work/legion.yaml" &
pid=$!
wait_healthz " after restart"
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
