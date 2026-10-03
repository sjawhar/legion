#!/usr/bin/env bash
# Records the Legion section's two terminal casts, `state.cast` and `controller.cast` beside this
# file, which the `legion-state` and `legion-controller` walkthroughs render. Both come from one
# local Go daemon in the shape scripts/e2e/controller-start-tmux.sh builds: this checkout's `legion`
# on a scratch postgres:16, a NATS server and an Envoy listener of its own, and this checkout's
# plugin in an Oh My Pi profile under the run's own HOME. Every name on screen is example data: the
# project is SHOP, its one running claim is the architect of SHOP-2, and the model the agents are
# configured with is an example provider served by a local listener that accepts each request and
# never answers, so no agent's turn ends, and none fails, on camera.
#
#   bash docs/site/media/walkthroughs/legion-operator/record-casts.sh [state] [controller]
#
# With no argument it records both, state first, then the controller registering with that same
# daemon. Each cast is typed into a clean shell in tmux at 100x28, the size docs/site/media/README.md
# names, and recorded with asciinema. Everything the run starts is its own and goes on any exit:
# its scratch directory, its tmux servers and both containers. The daemon, listener and model logs
# go to an evidence directory outside it, a fresh /tmp directory the run keeps and names on exit.
set -euo pipefail
unset NATS_NKEY_SEED NATS_NKEY_SEED_FILE NATS_DAEMON_NKEY_SEED NATS_DAEMON_NKEY_SEED_FILE

check=setup
note() { echo "   $*"; }
fail() { echo "FAIL $check: $*" >&2; exit 1; }
for tool in go docker jq curl tmux bun mise jj asciinema; do command -v "$tool" >/dev/null || fail "$tool is required"; done

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../../../.." && pwd)
casts=("$@")
[ ${#casts[@]} -gt 0 ] || casts=(state controller)
for cast in "${casts[@]}"; do
  case $cast in state | controller) ;; *) echo "unknown cast $cast: state or controller" >&2 && exit 2 ;; esac
done

work=$(mktemp -d /tmp/legion-docs-casts.XXXXXX)
evidence=$(mktemp -d /tmp/legion-docs-casts-evidence.XXXXXX)
mkdir -p "$evidence/logs" "$work/bin" "$work/xdg" "$work/tmux"
timeout_hook=
pg_container=legion-docs-casts-pg-$$
nats_container=legion-docs-casts-nats-$$
daemon_pid=
listener_pid=
model_pid=
daemon_port=
envoy_port=
model_port=
# shellcheck source-path=SCRIPTDIR source=../../../../../scripts/e2e/lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=../../../../../scripts/e2e/lib/omp-home.sh
. "$root/scripts/e2e/lib/omp-home.sh"
# shellcheck source-path=SCRIPTDIR source=../../../../../scripts/e2e/lib/stage-role-prompts.sh
. "$root/scripts/e2e/lib/stage-role-prompts.sh"

# Unconditional: every run removes what it made, whatever it ended on, and keeps its evidence.
cleanup() {
  set +e
  TMUX_TMPDIR=$work/tmux tmux -L cast kill-server >/dev/null 2>&1
  stop_pid "$daemon_pid"
  TMUX_TMPDIR=$work/tmux tmux -L legion-shop kill-server >/dev/null 2>&1
  stop_pid "$listener_pid"
  stop_pid "$model_pid"
  for p in $(run_processes); do kill -KILL "$p" 2>/dev/null; done
  docker rm -f "$pg_container" "$nats_container" >/dev/null 2>&1
  rm -rf "$work"
  echo "evidence: $evidence (logs/daemon.log, logs/listener.log, logs/model.log)"
  return 0
}
trap cleanup EXIT
trap 'echo "FAIL $check: line $LINENO exited $?: $BASH_COMMAND" >&2' ERR
trap 'exit 130' INT
trap 'exit 143' TERM

# ---- the rig ---------------------------------------------------------------------------------------
check=build
omp_home=$work/home
make_omp_home "$omp_home"
operator=$omp_home/shop
mkdir -p "$operator"
export XDG_STATE_HOME=$work/xdg TMUX_TMPDIR=$work/tmux
pin=$(bun "$root/packages/daemon/src/daemon/omp-pin.ts")
mise where "$pin" >/dev/null 2>&1 || mise install "$pin" >&2
# The pinned Oh My Pi as the run's own `omp` (LEGION_OMP_PATH), so what `legion controller start`
# prints of its invocation names the run's directory and nothing of the machine's tool layout.
printf '#!/bin/sh\nexec %q "$@"\n' "$(mise where "$pin")/bin/omp" >"$work/bin/omp"
chmod +x "$work/bin/omp"
(cd "$root/packages/daemon-go" && nice -n 19 go build -o "$work/bin/legion" ./cmd/legion)
stage_role_prompts "$root" "$work/bin"
(cd "$root/packages/envoy" && nice -n 19 go build -o "$work/envoy-listener" ./cmd/listener)
pick_port daemon_port
pick_port envoy_port
pick_port model_port

check=services
docker run -d --name "$pg_container" --cpus 0.5 --mount type=tmpfs,destination=/var/lib/postgresql/data \
  -e POSTGRES_USER=legion -e POSTGRES_PASSWORD=legion -e POSTGRES_DB=legion -p 127.0.0.1::5432 postgres:16 >/dev/null
pg_port=$(docker port "$pg_container" 5432/tcp | head -1 | sed 's/.*://')
until_true 60 "Postgres" docker exec "$pg_container" pg_isready -h 127.0.0.1 -U legion -d legion
docker run -d --name "$nats_container" --cpus 0.5 -p 127.0.0.1::4222 nats:2.10 -js >/dev/null
until_true 90 "NATS" sh -c "docker logs '$nats_container' 2>&1 | grep -q 'Server is ready'"
nats_url="nats://127.0.0.1:$(docker port "$nats_container" 4222/tcp | head -1 | sed 's/.*://')"
(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$operator/envoy-token")
(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$operator/operator-token")
ENVOY_API_TOKEN="$(cat "$operator/envoy-token")" PORT=$envoy_port ENVOY_LISTEN_HOST=127.0.0.1 \
  ENVOY_MACHINE_ID=example-host-legion NATS_URLS=$nats_url "$work/envoy-listener" >"$evidence/logs/listener.log" 2>&1 &
listener_pid=$!
until_true 60 "the Envoy listener" curl -fsS -H "Authorization: Bearer $(cat "$operator/envoy-token")" "http://127.0.0.1:$envoy_port/v1/sessions"
# The agents' model: accepts every request and never answers.
bun -e "Bun.serve({ hostname: '127.0.0.1', port: $model_port, idleTimeout: 0, fetch: () => new Promise(() => {}) })" \
  >"$evidence/logs/model.log" 2>&1 &
model_pid=$!
until_true 30 "the example model listener" bash -c "exec 3<>/dev/tcp/127.0.0.1/$model_port"

check=plugin
(cd "$root" && nice -n 19 bun install --frozen-lockfile >/dev/null)
profile=legion-docs-casts-$$
bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --profile "$profile" --home "$omp_home" --dest "$work/plugin" >/dev/null
agent_dir=$omp_home/.omp/profiles/$profile/agent
mkdir -p "$agent_dir"
cat >"$agent_dir/models.yml" <<EOF
providers:
  example:
    baseUrl: http://127.0.0.1:$model_port
    auth: apiKey
    api: anthropic-messages
    apiKey: example-key
    models:
      - id: example-model
        name: example-model
EOF
# The run's profile is new, so Oh My Pi would open on its first-run setup and welcome screen; the
# operator's own profile has both behind it.
cat >"$agent_dir/config.yml" <<'EOF'
modelRoles:
  default: example/example-model
  review: example/example-model
  oracle: example/example-model
  deep: example/example-model
setupVersion: 2
startup:
  quiet: true
  setupWizard: false
  checkUpdate: false
  changelogMode: hidden
marketplace:
  autoUpdate: "off"
EOF

cat >"$operator/legion.yaml" <<EOF
project: SHOP
port: $daemon_port
postgres_dsn: postgres://legion:legion@127.0.0.1:$pg_port/legion?sslmode=disable
state_dir: $work/state
operator_token_file: ./operator-token
envoy_url: http://127.0.0.1:$envoy_port
nats_urls:
  - $nats_url
envoy_token_file: ./envoy-token
probe_interval_seconds: 5
EOF
cat >"$operator/controller.yaml" <<EOF
project: SHOP
daemon_url: http://127.0.0.1:$daemon_port
operator_token_file: ./operator-token
envoy_url: http://127.0.0.1:$envoy_port
envoy_token_file: ./envoy-token
nats_urls: [$nats_url]
state_dir: ./controller-state
EOF

# The operator's shell: this checkout's `legion` first on PATH, the run's HOME and profile, and
# none of the running session's own Oh My Pi or Envoy variables.
shell_env=(env -i "PATH=$work/bin:$PATH" "HOME=$omp_home" "OMP_PROFILE=$profile" "TERM=xterm-256color"
  "LANG=C.UTF-8" "XDG_STATE_HOME=$work/xdg" "TMUX_TMPDIR=$work/tmux" "XDG_CONFIG_HOME=$XDG_CONFIG_HOME"
  "MISE_DATA_DIR=$MISE_DATA_DIR" "JJ_CONFIG=$JJ_CONFIG" "LEGION_OMP_PATH=$work/bin/omp")
legion() { (cd "$operator" && "${shell_env[@]}" "$work/bin/legion" "$@"); }

check=daemon
(cd "$operator" && exec "${shell_env[@]}" "$work/bin/legion" start --config legion.yaml >>"$evidence/logs/daemon.log" 2>&1) &
daemon_pid=$!
# until_true silences its command, so a daemon that exited says why on the script's own stderr.
exec 3>&2
daemon_up() {
  kill -0 "$daemon_pid" 2>/dev/null || fail "the daemon exited: $(tail -3 "$evidence/logs/daemon.log")" 2>&3
  curl -fs "http://127.0.0.1:$daemon_port/healthz"
}
until_true 180 "the daemon to answer /healthz" daemon_up

check=architect
legion claims spawn --config legion.yaml --operator-token-file operator-token \
  --tree SHOP-2 --issue SHOP-2 --role architect >/dev/null
until_true 240 "the SHOP-2 architect to be ready" sh -c \
  "cd '$operator' && '$work/bin/legion' claims list --json --config legion.yaml --operator-token-file operator-token | jq -e '.claims[] | select(.issue == \"SHOP-2\" and .state == \"ready\")'"

# ---- recording -------------------------------------------------------------------------------------
cast_tmux() { tmux -L cast -f /dev/null "$@"; }
# type_keys PANE TEXT types TEXT into PANE a key at a time, at a steady typing pace, then Enter.
type_keys() {
  local pane=$1 text=$2 i
  for ((i = 0; i < ${#text}; i++)); do
    cast_tmux send-keys -t "$pane" -l -- "${text:i:1}"
    sleep 0.035
  done
  sleep 0.3
  cast_tmux send-keys -t "$pane" Enter
}
# settle PANE waits, bounded, until PANE shows its prompt again with nothing new for half a second.
settle() {
  local pane=$1 before after
  for _ in $(seq 1 120); do
    before=$(cast_tmux capture-pane -p -t "$pane")
    sleep 0.5
    after=$(cast_tmux capture-pane -p -t "$pane")
    if [ "$before" = "$after" ] && [ "$(printf '%s\n' "$after" | sed '/^$/d' | tail -1)" = '$' ]; then return 0; fi
  done
  fail "pane $pane never settled at its prompt"
}
# record NAME: records the tmux session NAME, attached at 100x28, into NAME.cast beside this file,
# until the driver detaches it. asciinema runs in a pty of its own, inside tmux's outer server.
record() {
  local name=$1
  rm -f "$work/$name.cast"
  cast_tmux new-session -d -s "rec-$name" -x 100 -y 28 \
    "asciinema rec -q --cols 100 --rows 28 -c 'tmux -L cast -f /dev/null attach -t $name' '$work/$name.cast'"
  until_true 15 "asciinema to attach $name" sh -c "tmux -L cast list-clients -t '$name' | grep -q ."
  sleep 1
}
stop_recording() {
  local name=$1
  sleep "${2:-2}"
  cast_tmux detach-client -s "$name"
  until_true 15 "asciinema to write $name.cast" sh -c "! tmux -L cast has-session -t 'rec-$name' 2>/dev/null"
  [ -s "$work/$name.cast" ] || fail "asciinema wrote no $name.cast"
  cp "$work/$name.cast" "$here/$name.cast"
  note "wrote $here/$name.cast ($(wc -l <"$here/$name.cast") lines)"
}
new_shell() { # new_shell SESSION: a clean interactive shell in the operator's directory
  cast_tmux new-session -d -s "$1" -x 100 -y 28 -c "$operator" \
    "${shell_env[*]} PS1='\$ ' bash --noprofile --norc"
  cast_tmux set -g status off
  cast_tmux set -g pane-border-style fg=colour240
  cast_tmux set -g pane-active-border-style fg=colour240
}

for cast in "${casts[@]}"; do
  case $cast in
  state)
    check=record-state
    new_shell state
    settle state
    record state
    type_keys state "legion status SHOP"
    settle state
    sleep 0.8
    type_keys state "legion state --config legion.yaml"
    settle state
    sleep 1.2
    type_keys state "legion state --config legion.yaml --json | jq '.issues[\"SHOP-2\"].architect'"
    settle state
    sleep 1.2
    type_keys state "legion claims list --config legion.yaml --operator-token-file operator-token"
    settle state
    stop_recording state 2.5
    ;;
  controller)
    check=record-controller
    new_shell controller
    cast_tmux split-window -v -l 8 -t controller -c "$operator" "${shell_env[*]} PS1='\$ ' bash --noprofile --norc"
    cast_tmux select-pane -t controller.0
    settle controller.0
    settle controller.1
    record controller
    type_keys controller.0 "legion controller start --config controller.yaml"
    until_true 180 "the controller to register" sh -c \
      "cd '$operator' && '$work/bin/legion' state --json --config legion.yaml | jq -e '.controllerLocator.sessionId != null'"
    sleep 2
    cast_tmux select-pane -t controller.1
    type_keys controller.1 "legion state --config legion.yaml --json | jq .controllerLocator"
    settle controller.1
    stop_recording controller 3
    ;;
  esac
done
echo "recorded: ${casts[*]}"
