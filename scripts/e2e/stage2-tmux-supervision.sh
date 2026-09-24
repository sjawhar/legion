#!/usr/bin/env bash
# Stage 2's gate for the Go coordinator: supervision on tmux, proven against the real things. The
# Go daemon launches a real Oh My Pi — the pinned build (packages/daemon/src/daemon/omp-pin.ts)
# with this checkout's plugin in an isolated OMP profile — in panes of its private tmux server,
# against a real Envoy listener and NATS (scripts/kind-smoke's host-side recipe) and a real
# Postgres. Every gate behaviour is one named check that prints what it observed; the first check
# that does not hold ends the run non-zero, naming it.
#
# Stage 1's discipline holds: the cleanup cannot fail, every wait is bounded, a process that
# ignores its stop is SIGKILLed, and everything the run takes is its own — its mktemp work
# directory, its containers, its OMP profile, its ports, its project keys, its tmux servers
# (TMUX_TMPDIR under the work directory), and its legions registry (XDG_STATE_HOME). The work
# directory survives a failure, because its logs are the evidence, and goes when the run passed.
#
# The one input: LEGION_E2E_PG_DSN, the Postgres to run against; unset, the run starts its own
# postgres:16 on tmpfs. The agents' model is Anthropic through the Hawk model gateway on the
# operator's own hawk login (lib/install-model-gateway.sh), so the operator's keyring must be
# unlocked; no Anthropic key reaches a pane. The provider-key path is proven with
# GEMINI_API_KEY_TESTS from the secret store (agent tier: no YubiKey touch), which the daemon
# itself resolves at boot and hands every pane's shim as a daemon-held file (`provider_keys`); the
# run never reads it.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d /tmp/legion-e2e2.XXXXXXXX)
ok=
daemon_pid=
deadline_pid=
listener_pid=
profile=legion-e2e2-$$-$(date +%s)
project="S2E$$$(date +%s)"            # a retired claim is never spawned again: a fresh key per run
deadline_project="S2D$$$(date +%s)"
ptoken=${project,,} # the daemon's project token: its tmux socket, its claim tokens
deadline_ptoken=${deadline_project,,}
nats_container=legion-e2e2-nats-$$
pg_container=legion-e2e2-pg-$$
state=$work/state
daemon_log=$work/daemon.log
check=setup

# ---- reporting and waiting ------------------------------------------------------------------------

begin() {
  check=$1
  echo "== $check"
}
note() { echo "   $*"; }
pass() { echo "ok $check"; }
fail() {
  echo "FAIL $check: $*" >&2
  exit 1
}
# until_true SECONDS WHAT CMD… polls CMD every half second, failing the current check with WHAT.
until_true() {
  local limit=$1 what=$2 i
  shift 2
  for ((i = 0; i < limit * 2; i++)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  fail "timed out after ${limit}s waiting for $what"
}

# ---- cleanup: never fails, leaves nothing of the run behind ---------------------------------------

# stop_pid PID: SIGTERM, a bounded wait, then SIGKILL. Never fails.
stop_pid() {
  local pid=$1 i
  [ -n "$pid" ] || return 0
  kill -TERM "$pid" 2>/dev/null || return 0
  for i in $(seq 1 50); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.2
  done
  kill -KILL "$pid" 2>/dev/null || true
  return 0
}
# run_processes: every process of this run still alive — any whose command line names the work
# directory (the daemons, the listener, each pane's shell and shim) or whose working directory is
# under it (each OMP, which runs in its claim's workspace).
run_processes() {
  local p cwd
  for p in /proc/[0-9]*; do
    cwd=$(readlink "$p/cwd" 2>/dev/null) || continue
    case "$cwd/" in "$work"/*)
      echo "${p#/proc/}"
      continue
      ;;
    esac
    if grep -qsF "$work" "$p/cmdline" 2>/dev/null; then echo "${p#/proc/}"; fi
  done
}
cleanup() {
  local p
  stop_pid "$daemon_pid"
  stop_pid "$deadline_pid"
  for p in "$ptoken" "$deadline_ptoken"; do
    TMUX_TMPDIR=$work/tmux tmux -L "legion-$p" kill-server >/dev/null 2>&1 || true
  done
  stop_pid "$listener_pid"
  for p in $(run_processes); do kill -KILL "$p" 2>/dev/null || true; done
  docker rm -f "$nats_container" "$pg_container" >/dev/null 2>&1 || true
  rm -rf "$HOME/.omp/profiles/$profile" "$work/model-gateway-cache" || true
  if [ -n "${ok:-}" ]; then
    rm -rf "$work" || true
  else
    echo "the run's workspace is $work (daemon log: $daemon_log; model key command log: $work/model-gateway/hawk-token.log)"
  fi
  return 0
}
trap cleanup EXIT
# Any command that fails outside a check's own assertions still names the check it ended.
trap 'echo "FAIL $check: line $LINENO exited $?: $BASH_COMMAND" >&2' ERR
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# ---- the pieces ------------------------------------------------------------------------------------

legion() { "$work/legion" "$@"; }
claims() { # claims <sub> [flags…] — the operator CLI against the main daemon
  local sub=$1
  shift
  legion claims "$sub" --config "$work/legion.yaml" --operator-token-file "$work/operator-token" "$@"
}
claim_json() { claims list --json | jq -ce --arg t "$1" '.claims[] | select(.token == $t)'; }
claim_is() { claim_json "$1" | jq -e "$2" >/dev/null; } # claim_is TOKEN JQ-PREDICATE
tm() { tmux -L "legion-$ptoken" "$@"; }
panes() { tm list-panes -a -F '#{pane_id}' 2>/dev/null | sort; }
# log_count MSG [DELIVERY]: how many JSON lines of the daemon log carry MSG (and that delivery id).
log_count() {
  jq -R --arg m "$1" --arg d "${2:-}" 'fromjson? | select(.msg == $m and ($d == "" or .delivery == $d))' \
    "$daemon_log" | jq -s length
}
# user_turns FILE MARKER: how many user messages in an OMP session file carry MARKER — the turns
# a task started.
user_turns() {
  jq -R --arg m "$2" 'fromjson? | select(.type == "message" and .message.role == "user")
    | [.message.content[]? | .text? // empty] | join(" ") | select(contains($m))' "$1" | jq -s length
}
argv0() { tr '\0' '\n' <"/proc/$1/cmdline" | head -1; }
# first_child PID: the process's first child. A Go process (the shim) starts its children from
# whichever thread runs the goroutine, so every thread's children list is read, not the main one's.
first_child() { cat /proc/"$1"/task/*/children 2>/dev/null | tr ' ' '\n' | grep -m1 .; }
# omp_of PANE_PID: the OMP process, found by walking first children from the pane's process to the
# one whose argv[0] is omp.
omp_of() {
  local p=$1 i
  for i in 1 2 3 4 5 6; do
    [ "$(basename "$(argv0 "$p")")" = omp ] && {
      echo "$p"
      return 0
    }
    p=$(first_child "$p")
    [ -n "$p" ] || return 1
  done
  return 1
}
env_of() { tr '\0' '\n' <"/proc/$1/environ" | sed -n "s/^$2=//p"; }
# start_daemon: the main daemon, in the background, its log appended to daemon.log. OMP_PROFILE
# selects the isolated profile for the plugin gate and — through the pane allow-list — every pane.
start_daemon() {
  echo "=== boot at $(date -u +%FT%TZ)" >>"$daemon_log"
  env -u LEGION_OMP_PATH OMP_PROFILE="$profile" "$work/legion" start --config "$work/legion.yaml" >>"$daemon_log" 2>&1 &
  daemon_pid=$!
  until_true 180 "the daemon to answer /healthz" curl -fs "http://127.0.0.1:$port/healthz"
}
# stop_daemon PID HOW: bounded, and the exit status judged — daemon.Run returns 0 on a cancelled
# context, so anything else is a defect, not a stop.
stop_daemon() {
  local pid=$1 i st=0
  for i in $(seq 1 100); do
    kill -0 "$pid" 2>/dev/null || break
    [ "$i" = 100 ] && {
      kill -KILL "$pid" 2>/dev/null || true
      fail "the daemon ignored $2 for 20s (SIGKILLed)"
    }
    sleep 0.2
  done
  wait "$pid" || st=$?
  [ "$st" = 0 ] || fail "the daemon exited $st on $2"
}
# expect_refusal NAME PATTERN: `legion start` must refuse, printing PATTERN.
expect_refusal() {
  local out=$work/refusal-$1.log st=0
  env -u LEGION_OMP_PATH OMP_PROFILE="$profile" timeout 300 "$work/legion" start --config "$work/legion.yaml" >"$out" 2>&1 || st=$?
  [ "$st" != 0 ] || fail "legion start was not refused (exit 0)"
  [ "$st" != 124 ] || fail "legion start neither refused nor served within 300s"
  grep -qF "$2" "$out" || fail "the refusal (exit $st) does not say \"$2\": $(grep -v '^{' "$out" | head -3)"
  note "legion start exit $st: $(grep -F "$2" "$out" | head -1)"
}

# ---- setup -----------------------------------------------------------------------------------------

for tool in go docker jq curl ss tmux bun mise socat secrets hawk-token; do
  command -v "$tool" >/dev/null || fail "$tool is required"
done
mkdir -p "$state" "$work/xdg" "$work/tmux" "$work/stub"
# The model route, installed while this shell still holds the operator's XDG directories, which
# the key command runs hawk-token under. Its first mint is the preflight: a locked keyring stops the
# run here, by name.
key_command=$(bash "$root/scripts/e2e/lib/install-model-gateway.sh" --profile "$profile" --dest "$work/model-gateway" --cache-dir "$work/model-gateway-cache") ||
  fail "the agents' model route through the Hawk model gateway could not be installed (the reason is above)"
export XDG_STATE_HOME=$work/xdg # the legions registry this run writes is its own
export TMUX_TMPDIR=$work/tmux   # so are the daemons' private tmux servers

# The daemon receives the ordinary operator PATH, including any OMP wrapper it holds. It must
# resolve the configured tool's executable itself; this proof checks the OMP child is that pinned
# binary, rather than repairing PATH before the daemon sees it.
pin=$(bun "$root/packages/daemon/src/daemon/omp-pin.ts")
mise where "$pin" >/dev/null 2>&1 || mise install "$pin" >&2
omp_bin=$(mise where "$pin")/bin
[ -x "$omp_bin/omp" ] || fail "mise has no omp executable for $pin under $omp_bin"
echo "configured OMP pin: $pin ($("$omp_bin/omp" --version 2>&1 | head -1)) at $omp_bin/omp; ordinary PATH omp: $(command -v omp)"

# Below the kernel's ephemeral range and distinct from one another (scripts/e2e/lib/free-port.sh):
# none is bound until its process starts, so no socket opened in between can take one.
port=$(bash "$root/scripts/e2e/lib/free-port.sh") || fail "no free port for the daemon"
deadline_port=$(bash "$root/scripts/e2e/lib/free-port.sh" "$port") || fail "no free port for the second daemon"
envoy_port=$(bash "$root/scripts/e2e/lib/free-port.sh" "$port" "$deadline_port") || fail "no free port for the Envoy listener"
(cd "$root/packages/daemon-go" && go build -o "$work/legion" ./cmd/legion)
(cd "$root/packages/envoy" && go build -o "$work/envoy-listener" ./cmd/listener)

if [ -z "${LEGION_E2E_PG_DSN:-}" ]; then
  docker ps >/dev/null # a broken docker is a failure of this run, not of the daemon
  # tmpfs, not the image's anonymous volume: creating a volume is one more moving part the proof
  # does not need, and on a box whose volume subsystem stalls it hangs the run.
  docker run -d --name "$pg_container" --mount type=tmpfs,destination=/var/lib/postgresql/data \
    -e POSTGRES_USER=legion -e POSTGRES_PASSWORD=legion -e POSTGRES_DB=legion \
    -p 127.0.0.1::5432 postgres:16 >/dev/null
  until_true 60 "postgres to accept connections over TCP" \
    docker exec "$pg_container" pg_isready -h 127.0.0.1 -p 5432 -U legion -d legion
  LEGION_E2E_PG_DSN="postgres://legion:legion@127.0.0.1:$(docker port "$pg_container" 5432/tcp | head -1 | sed 's/.*://')/legion"
fi

# NATS and the Envoy listener, as scripts/kind-smoke/up.sh runs them on the host (ensure_nats,
# start_listener): nats:2.10 with JetStream, and the listener built from packages/envoy with its
# API bearer, which reaches every pane as a 0600 file (envoy_token_file).
docker run -d --name "$nats_container" -p 127.0.0.1::4222 nats:2.10 -js >/dev/null
until_true 60 "NATS to be ready" sh -c "docker logs '$nats_container' 2>&1 | grep -q 'Server is ready'"
nats_url="nats://127.0.0.1:$(docker port "$nats_container" 4222/tcp | head -1 | sed 's/.*://')"
(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/envoy-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header")
ENVOY_API_TOKEN="$(cat "$work/envoy-token")" PORT=$envoy_port ENVOY_LISTEN_HOST=127.0.0.1 \
  ENVOY_MACHINE_ID="legion-e2e2-$$" NATS_URLS=$nats_url "$work/envoy-listener" >"$work/listener.log" 2>&1 &
listener_pid=$!
until_true 60 "the Envoy listener to answer /v1/sessions" \
  curl -fsS -H "@$work/envoy-auth-header" "http://127.0.0.1:$envoy_port/v1/sessions"
envoy_role() { curl -fsS -H "@$work/envoy-auth-header" "http://127.0.0.1:$envoy_port/v1/roles/$1"; }

# The branch plugin, packed as the release packs it, into this run's own OMP profile.
(cd "$root" && bun install --frozen-lockfile >/dev/null)
manifest=$(bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --profile "$profile" --dest "$work/plugin")
want_contract=$(jq -r .legion.goDaemonApiVersion "$root/packages/pi-envoy/package.json")
echo "plugin: $(jq -r '.name + "@" + .version' "$manifest") in OMP profile $profile (goDaemonApiVersion $want_contract)"

(umask 077 && printf 'stage2-operator-%s\n' "$project" >"$work/operator-token")
cat >"$work/legion.yaml" <<EOF
project: $project
port: $port
postgres_dsn: $LEGION_E2E_PG_DSN
state_dir: $state
operator_token_file: $work/operator-token
omp_invocation: mise x $pin -- omp
provider_keys:
  GEMINI_API_KEY: GEMINI_API_KEY_TESTS
envoy_url: http://127.0.0.1:$envoy_port
nats_urls:
  - $nats_url
envoy_token_file: $work/envoy-token
probe_interval_seconds: 5
EOF
cat >"$work/architect.md" <<'EOF'
You are the root architect of a Legion stage-2 proof. You have no work to do. Reply to every
message with the single word `ready`, run no tools, and wait.
EOF
cat >"$work/worker.md" <<'EOF'
You are a worker in a Legion stage-2 proof. Do exactly what each message asks, in one short reply,
and run no tools.
EOF

# ---- the gate refuses first ------------------------------------------------------------------------

begin gate-refuses-another-contract
cp -p "$work/plugin/package.json" "$work/manifest.orig"
bad_contract=$((want_contract + 1))
jq --argjson v "$bad_contract" '.legion.goDaemonApiVersion = $v' "$work/manifest.orig" >"$work/plugin/package.json"
note "the installed manifest now declares goDaemonApiVersion $bad_contract (the checkout's is $want_contract)"
expect_refusal contract "speaks Go daemon API contract $bad_contract; this daemon requires $want_contract"
cp -p "$work/manifest.orig" "$work/plugin/package.json"
cmp -s "$work/manifest.orig" "$work/plugin/package.json" || fail "the installed manifest was not restored"
pass

begin gate-refuses-a-disabled-plugin
OMP_PROFILE=$profile omp plugin disable @sjawhar/pi-legion-envoy >/dev/null
note "omp plugin disable: $(OMP_PROFILE=$profile omp plugin list --json | jq -c '[.npm[]? | select(.name == "@sjawhar/pi-legion-envoy") | {name, enabled}]')"
expect_refusal disabled "is installed but not loaded by omp (disabled or unregistered)"
OMP_PROFILE=$profile omp plugin enable @sjawhar/pi-legion-envoy >/dev/null
OMP_PROFILE=$profile omp plugin list --json |
  jq -e '[.npm[]? | select(.name == "@sjawhar/pi-legion-envoy" and .enabled == true)] | length == 1' >/dev/null ||
  fail "the plugin did not come back enabled"
pass

# ---- one real agent: register, claim its role, ready ----------------------------------------------

begin architect-registers-and-is-ready
start_daemon
grep -q '"msg":"boot gate: pi-legion-envoy speaks this daemon' "$daemon_log" ||
  fail "the daemon served without its gate's pass line in the log"
expected_omp=$(readlink -f "$omp_bin/omp")
jq -R -e --arg binary "$expected_omp" '
  fromjson? | select(.msg == "legion daemon resolved OMP invocation for boot probes and panes" and (.invocation | contains($binary)))
' "$daemon_log" >/dev/null || fail "the daemon did not log the pinned OMP binary $expected_omp for its boot probes and panes"
note "$(jq -R -c 'fromjson? | select(.msg | startswith("boot gate")) | {msg, version, goDaemonApiVersion}' "$daemon_log" | head -1)"
note "$(jq -R -c 'fromjson? | select(.msg == "legion daemon resolved OMP invocation for boot probes and panes") | {msg, invocation}' "$daemon_log" | head -1)"
c1=$(claims spawn --json --tree S2-1 --issue S2-1 --role architect --prompt-file "$work/architect.md" | jq -r .token)
until_true 240 "claim $c1 to be ready" claim_is "$c1" '.state == "ready"'
c1_json=$(claim_json "$c1")
session1=$(jq -r .session <<<"$c1_json")
[ -n "$session1" ] && [ "$session1" != null ] || fail "the ready claim records no session"
registered=$(jq -R -c --arg c "$c1" 'fromjson? | select(.msg == "api: claim registered" and .claim == $c) | {generation, session, pluginContract}' "$daemon_log" | head -1)
[ -n "$registered" ] || fail "the daemon logged no registration for $c1"
note "claim $c1 ready: generation $(jq -r .generation <<<"$c1_json"), session $session1, pane $(jq -r .locator.tmux.pane <<<"$c1_json")"
note "registered: $registered"
pass

begin envoy-role-held
role=$(envoy_role "$c1") || fail "GET /v1/roles/$c1 failed on the Envoy listener"
[ "$(jq -r .holder <<<"$role")" = "$session1" ] || fail "the role's holder is not the claim's session: $role"
note "GET /v1/roles/$c1 → holder $(jq -r .holder <<<"$role")"
pass

begin ready-in-state
legion state --json --port "$port" >"$work/state-ready.json"
jq -e --arg s "$session1" '.issues["S2-1"].architect | .state == "ready" and .session == $s and .locator.runtime == "tmux"' \
  "$work/state-ready.json" >/dev/null || fail "legion state does not show S2-1's architect ready: $(jq -c '.issues["S2-1"].architect' "$work/state-ready.json")"
note "legion state: .issues[\"S2-1\"].architect = $(jq -c '.issues["S2-1"].architect | {session, state, locator: .locator.tmux}' "$work/state-ready.json")"
pass

# ---- delivery: queued before ready, sent once; a retried frame starts no second turn --------------

begin task-queued-before-ready-runs-once
marker1="S2E-TASK-$RANDOM$RANDOM"
spawned=$(claims spawn --json --tree S2-1 --issue S2-2 --role implementer --prompt-file "$work/worker.md" \
  --task "Reply with the single word pong. ($marker1)")
c2=$(jq -r .token <<<"$spawned")
jq -e '.pending.id != null and .pending.deliveredAt == null and (.state | IN("queued", "launching", "shim_connected"))' \
  <<<"$spawned" >/dev/null || fail "the spawn did not queue its task before the agent was ready: $spawned"
note "spawned $c2 in state $(jq -r .state <<<"$spawned") holding delivery $(jq -r .pending.id <<<"$spawned"), not yet sent"
until_true 300 "claim $c2's task to run and end" claim_is "$c2" '.state == "idle" and .pending == null'
c2_json=$(claim_json "$c2")
session_file2=$(jq -r .sessionFile <<<"$c2_json")
turns=$(user_turns "$session_file2" "$marker1")
[ "$turns" = 1 ] || fail "the task reached the agent $turns times (session file $session_file2)"
note "idle, no pending delivery; the agent's session file holds the task $turns time"
pass

begin model-turn-through-the-gateway
# The task's reply came through the gateway: every assistant turn in the session file ran as the
# profile's pinned model on the anthropic provider, the one provider the profile routes (to
# middleman) and leaves enabled, and none ended in an error; the key command minted for more than
# the preflight.
pinned=$(sed -n 's/^  default: //p' "$HOME/.omp/profiles/$profile/agent/config.yml")
[ -n "$pinned" ] || fail "the profile's config.yml names no default model role: $HOME/.omp/profiles/$profile/agent/config.yml"
replies=$(jq -c 'select(.type == "message" and .message.role == "assistant")
  | {provider: .message.provider, model: .message.model, stopReason: .message.stopReason}' "$session_file2" | jq -sc .)
jq -e --arg pinned "$pinned" 'length > 0 and all(.provider == "anthropic" and "anthropic/" + .model == $pinned and .stopReason != "error")' \
  <<<"$replies" >/dev/null || fail "claim $c2's replies did not all come from $pinned through the gateway: $replies"
calls=$(grep -c ' invoked by pid ' "$work/model-gateway/hawk-token.log")
[ "$calls" -ge 2 ] || fail "the key command $key_command ran $calls time(s), the preflight's alone"
note "claim $c2's replies, from its session file: $replies"
note "the key command $key_command ran $calls times and minted $(grep -c ' minted a key for pid ' "$work/model-gateway/hawk-token.log") ($work/model-gateway/hawk-token.log)"
pass

begin retried-frame-starts-no-second-turn
# The daemon re-sends a delivery under the same id when a send is lost in transit. Stopping the
# shim makes every send go unanswered: each times out (worker_rpc_timeout_seconds), the delivery
# waits, and the next sweep sends the same frame again. Resumed, the shim hands OMP the first
# prompt and answers every repeat from its dedupe record.
pane2_pid=$(jq -r '.locator.incarnation | split(":")[0]' <<<"$c2_json")
shim2=$(first_child "$pane2_pid")
tr '\0' ' ' <"/proc/$shim2/cmdline" | grep -q 'worker-shim' || fail "the first child of pane process $pane2_pid is not the shim"
kill -STOP "$shim2"
marker2="S2E-RETRY-$RANDOM$RANDOM"
delivery2=$(claims deliver --json --claim "$c2" --task "Reply with the single word pong. ($marker2)" | jq -r .pending.id)
lost_msg="supervise: the prompt was lost to the transport; the delivery waits"
until_true 90 "the daemon to send delivery $delivery2 twice into the stopped shim" \
  sh -c "[ \"\$(jq -R --arg m '$lost_msg' --arg d '$delivery2' 'fromjson? | select(.msg == \$m and .delivery == \$d)' '$daemon_log' | jq -s length)\" -ge 2 ]"
sends=$(log_count "$lost_msg" "$delivery2")
kill -CONT "$shim2"
note "shim $shim2 stopped; delivery $delivery2 sent $sends times, every send unanswered; shim resumed"
until_true 180 "claim $c2's retried delivery to run and end" claim_is "$c2" '.state == "idle" and .pending == null'
turns=$(user_turns "$session_file2" "$marker2")
[ "$turns" = 1 ] || fail "delivery $delivery2, sent $sends+ times, reached the agent $turns times"
jq -e '.budgets.promptFailures == 0' <<<"$(claim_json "$c2")" >/dev/null || fail "a transport loss was charged as a prompt failure"
note "the agent's session file holds the task $turns time; promptFailures 0"
pass

# ---- the process: death, suspension, resumption ---------------------------------------------------

begin kill-pane-resumes-the-same-session
(umask 077 && cp "$state/secrets/$c1" "$work/stale-boot-token") # generation 1's boot token, for later
before=$(claim_json "$c1")
old_pane=$(jq -r .locator.tmux.pane <<<"$before")
old_gen=$(jq -r .generation <<<"$before")
tm kill-pane -t "$old_pane"
until_true 240 "claim $c1 to be ready again after its pane was killed" \
  claim_is "$c1" ".generation == $((old_gen + 1)) and .state == \"ready\""
after=$(claim_json "$c1")
[ "$(jq -r .session <<<"$after")" = "$session1" ] || fail "the relaunch registered another session: $(jq -r .session <<<"$after")"
[ "$(jq -r .locator.tmux.pane <<<"$after")" != "$old_pane" ] || fail "the claim still names the killed pane"
omp1=$(omp_of "$(jq -r '.locator.incarnation | split(":")[0]' <<<"$after")") || fail "no omp process under the new pane"
resume_file=$(jq -r .sessionFile <<<"$after")
tr '\0' '\n' <"/proc/$omp1/cmdline" | grep -qxF -- "--resume=$resume_file" ||
  fail "the relaunched omp ($omp1) was not started with --resume=$resume_file: $(tr '\0' ' ' <"/proc/$omp1/cmdline" | cut -c1-200)"
[ "$(envoy_role "$c1" | jq -r .holder)" = "$session1" ] || fail "the resumed session does not hold its Envoy role"
note "killed $old_pane (generation $old_gen) → pane $(jq -r .locator.tmux.pane <<<"$after"), generation $(jq -r .generation <<<"$after"), same session $session1"
note "omp $omp1 started with --resume=$resume_file; the Envoy role still held by $session1"
pass

begin suspend-keeps-the-session
before=$(claim_json "$c1")
pane=$(jq -r .locator.tmux.pane <<<"$before")
claims suspend --claim "$c1" >/dev/null
until_true 60 "claim $c1 to be suspended" claim_is "$c1" '.state == "suspended" and .locator == null'
panes | grep -qxF "$pane" && fail "pane $pane still exists after the suspension"
[ "$(claim_json "$c1" | jq -r .session)" = "$session1" ] || fail "the suspension dropped the session"
session_file1=$(claim_json "$c1" | jq -r .sessionFile)
[ -s "$session_file1" ] || fail "the session file $session_file1 is gone"
note "suspended: pane $pane gone, no locator, session $session1 kept ($session_file1)"
pass

begin resume-on-demand
gen=$(claim_json "$c1" | jq -r .generation)
claims resume --claim "$c1" >/dev/null
until_true 240 "claim $c1 to be ready after its resume" claim_is "$c1" ".state == \"ready\" and .generation == $((gen + 1))"
after=$(claim_json "$c1")
pane=$(jq -r .locator.tmux.pane <<<"$after")
panes | grep -qxF "$pane" || fail "the resumed claim names pane $pane, which tmux does not list"
[ "$(jq -r .session <<<"$after")" = "$session1" ] || fail "the resume registered another session"
note "resumed: pane $pane, generation $(jq -r .generation <<<"$after"), same session $session1"
pass

begin stale-generation-hello-refused
refused_msg="worker-stream: rejected hello (stale worker generation)"
before_count=$(log_count "$refused_msg")
reply=$(printf '{"type":"hello","bootToken":"%s"}\n' "$(cat "$work/stale-boot-token")" |
  socat -t 5 - "UNIX-CONNECT:$state/worker-stream.sock" 2>&1) || true
[ -z "$reply" ] || fail "the daemon answered a stale hello: $reply"
until_true 10 "the daemon to log the stale hello" sh -c "[ \"\$(jq -R --arg m '$refused_msg' 'fromjson? | select(.msg == \$m)' '$daemon_log' | jq -s length)\" -gt $before_count ]"
note "generation 1's hello on $state/worker-stream.sock: closed with nothing written; logged \"$refused_msg\""
pass

begin unregistered-agent-retired-at-the-deadline
# A second daemon whose OMP answers the plugin gate and otherwise never runs the plugin: its pane's
# process lives and its agent never registers. The deadline is worker_boot_timeout_seconds ×
# worker_boot_registration_deadline_intervals = 10 s.
cat >"$work/stub/omp" <<EOF
#!/bin/sh
if [ "\$1" = models ]; then
  echo LEGION_PLUGIN_LOADED=yes >&2
  echo "LEGION_PLUGIN_LOADED_FROM=file://$work/plugin/dist/legion.js" >&2
  exit 0
fi
exec sleep 3600
EOF
chmod 0755 "$work/stub/omp"
mkdir -p "$work/deadline-state"
cat >"$work/deadline.yaml" <<EOF
project: $deadline_project
port: $deadline_port
postgres_dsn: $LEGION_E2E_PG_DSN
state_dir: $work/deadline-state
operator_token_file: $work/operator-token
worker_boot_timeout_seconds: 5
worker_boot_registration_deadline_intervals: 2
EOF
LEGION_OMP_PATH=$work/stub/omp OMP_PROFILE=$profile "$work/legion" start --config "$work/deadline.yaml" >"$work/deadline.log" 2>&1 &
deadline_pid=$!
until_true 120 "the second daemon to answer /healthz" curl -fs "http://127.0.0.1:$deadline_port/healthz"
dclaims() {
  local sub=$1
  shift
  legion claims "$sub" --config "$work/deadline.yaml" --operator-token-file "$work/operator-token" "$@"
}
spawned_at=$(date +%s)
cd3=$(dclaims spawn --json --tree S2-9 --issue S2-9 --role architect --prompt-file "$work/architect.md" | jq -r .token)
until_true 30 "the unregistered claim's shim to connect" sh -c \
  "'$work/legion' claims list --json --config '$work/deadline.yaml' --operator-token-file '$work/operator-token' | jq -e '.claims[0].state == \"shim_connected\"'"
first=$(dclaims list --json | jq -c '.claims[0]')
first_pid=$(jq -r '.locator.incarnation | split(":")[0]' <<<"$first")
kill -0 "$first_pid" || fail "the pane's process $first_pid is not alive before the deadline"
retired_msg="supervise: the agent never registered; retired its process"
until_true 60 "the second daemon to retire the unregistered agent" grep -qF "\"msg\":\"$retired_msg\"" "$work/deadline.log"
retired_after=$(($(date +%s) - spawned_at))
counted=$(dclaims list --json | jq -c '.claims[0] | {state, generation, budgets}')
jq -e '.budgets.launchFailures == 1' <<<"$counted" >/dev/null || fail "the retirement counted no launch failure: $counted"
kill -0 "$first_pid" 2>/dev/null && fail "the retired incarnation's process $first_pid is still alive"
note "claim $cd3: shim_connected with process $first_pid alive, never registered; retired ${retired_after}s after the spawn (deadline 10s); $counted"
legion stop --config "$work/deadline.yaml" >/dev/null
stop_daemon "$deadline_pid" "legion stop"
deadline_pid=
tmux -L "legion-$deadline_ptoken" kill-server >/dev/null 2>&1 || true
pass

# ---- a daemon restart re-adopts; a stray is reaped -------------------------------------------------

begin restart-readopts-the-live-panes
before1=$(claim_json "$c1")
before2=$(claim_json "$c2")
panes_before=$(panes)
boots_before=$(legion state --json --port "$port" | jq -r .daemon.boots)
kill -TERM "$daemon_pid"
stop_daemon "$daemon_pid" SIGTERM
[ "$(panes)" = "$panes_before" ] || fail "the panes changed while the daemon was down"
start_daemon
boots=$(legion state --json --port "$port" | jq -r .daemon.boots)
[ "$boots" = $((boots_before + 1)) ] || fail "the restart recorded boots $boots, want $((boots_before + 1))"
for pair in "$c1:$before1" "$c2:$before2"; do
  token=${pair%%:*}
  was=${pair#*:}
  now=$(claim_json "$token")
  for field in .generation .locator.incarnation .locator.tmux.pane .session; do
    [ "$(jq -r "$field" <<<"$now")" = "$(jq -r "$field" <<<"$was")" ] ||
      fail "$token's $field moved across the restart: $(jq -r "$field" <<<"$was") → $(jq -r "$field" <<<"$now")"
  done
done
[ "$(panes)" = "$panes_before" ] || fail "the restart opened or closed a pane: $(panes | tr '\n' ' ') (before: $(tr '\n' ' ' <<<"$panes_before"))"
# The shim's reconnect hello is what carries a task now: a task delivered after the restart runs.
marker3="S2E-READOPT-$RANDOM$RANDOM"
until_true 60 "claim $c1 to be ready on the restarted daemon" claim_is "$c1" '.state == "ready" or .state == "idle"'
claims deliver --claim "$c1" --task "Reply with the single word ready. ($marker3)" >/dev/null
until_true 180 "the task sent after the restart to run and end" claim_is "$c1" '.state == "idle" and .pending == null'
turns=$(user_turns "$session_file1" "$marker3")
[ "$turns" = 1 ] || fail "the task sent after the restart reached the agent $turns times"
restart_line=$(grep -n '^=== boot' "$daemon_log" | tail -1 | cut -d: -f1)
tail -n +"$restart_line" "$daemon_log" | grep -qF 'rejected hello' && fail "the restarted daemon rejected a hello"
note "boots $boots_before → $boots; $c1 and $c2 kept generation, incarnation $(jq -r .locator.incarnation <<<"$before1") / $(jq -r .locator.incarnation <<<"$before2"), and pane; no pane opened or closed"
note "the shims' reconnect hellos were accepted: a task sent after the restart ran $turns time"
pass

begin omp-child-environment
now1=$(claim_json "$c1")
pane1_pid=$(jq -r '.locator.incarnation | split(":")[0]' <<<"$now1")
mapfile -t shell_argv < <(tr '\0' '\n' <"/proc/$pane1_pid/cmdline")
[ "${shell_argv[0]}" = /bin/sh ] && [ "${shell_argv[1]}" = -c ] ||
  fail "the pane's process runs '${shell_argv[0]} ${shell_argv[1]}', not sh -c"
omp1=$(omp_of "$pane1_pid") || fail "no omp process under pane process $pane1_pid"
chain=$pane1_pid
chain_pids=$pane1_pid
p=$pane1_pid
while [ "$p" != "$omp1" ]; do
  p=$(first_child "$p")
  chain="$chain → $p ($(basename "$(argv0 "$p")"))"
  chain_pids="$chain_pids $p"
done
for p in $chain_pids; do
  tr '\0' ' ' <"/proc/$p/cmdline" | grep -qF "$HOME/.dotfiles/shims/omp" &&
    fail "pane process chain runs the dotfiles OMP wrapper: $(tr '\0' ' ' <"/proc/$p/cmdline")"
done
shim1=$(first_child "$pane1_pid")
# LEGION-206 P1 holds with the gateway route: the pane's shell and its OMP keep the daemon's own XDG
# directories and carry no session bus address (only the key command gets the operator's).
for p in "$pane1_pid" "$omp1"; do
  for name in XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME; do
    dir=$(env_of "$p" "$name")
    case "$dir" in "$state"/home/*) ;; *) fail "process $p ($(basename "$(argv0 "$p")"))'s $name is '$dir', not under $state/home" ;; esac
  done
  ! grep -qz '^DBUS_SESSION_BUS_ADDRESS=' "/proc/$p/environ" ||
    fail "process $p ($(basename "$(argv0 "$p")")) carries DBUS_SESSION_BUS_ADDRESS"
done
xdg=$(env_of "$omp1" XDG_CONFIG_HOME)
! grep -qz '^ANTHROPIC_API_KEY=' "/proc/$omp1/environ" || fail "omp's environment carries ANTHROPIC_API_KEY"
actual_omp=$(readlink -f "/proc/$omp1/exe")
[ "$actual_omp" = "$expected_omp" ] || fail "omp $omp1 executes $actual_omp, not the configured pinned binary $expected_omp"
pane_path=$(env_of "$shim1" PATH)
omp_path=$(env_of "$omp1" PATH)
[ "${omp_path%%:*}" = "${pane_path%%:*}" ] ||
  fail "OMP PATH begins ${omp_path%%:*}, not the pane PATH head ${pane_path%%:*}"
key_length=$(tr '\0' '\n' <"/proc/$omp1/environ" | awk 'index($0, "GEMINI_API_KEY=") == 1 {print length($0) - 15}')
[ -n "$key_length" ] && [ "$key_length" -gt 0 ] || fail "omp's environment carries no GEMINI_API_KEY"
[ -z "$(env_of "$shim1" GEMINI_API_KEY)" ] || fail "the shim's own environment carries the provider key"
for name in GEMINI_API_KEY_TESTS SOPS_AGE_KEY_FILE SECRETSD_CONFIG; do
  [ -z "$(env_of "$omp1" "$name")" ] || fail "omp's environment carries $name"
done
note "pane process $pane1_pid is /bin/sh -c; first children: $chain"
note "omp $omp1 executes the configured pinned binary $actual_omp, no process in its chain runs the dotfiles wrapper, and its PATH head ${omp_path%%:*} matches the pane's; XDG_CONFIG_HOME=$xdg; GEMINI_API_KEY of $key_length bytes (from the daemon-held file), absent from the shim; no ANTHROPIC_API_KEY, GEMINI_API_KEY_TESTS, SOPS_AGE_KEY_FILE or SECRETSD_CONFIG"
note "the pane's shell $pane1_pid and omp $omp1: all four XDG base directories under $state/home, no DBUS_SESSION_BUS_ADDRESS"
pass

begin stray-pane-reaped-after-the-grace
# A window marked as this daemon's that holds no pane any claim records — what a crash between
# opening a window and persisting its locator leaves — and, beside it, a window a human opened.
stray=$(tm new-window -d -t "legion-$ptoken" -P -F '#{window_id}' 'sleep 3600')
tm set-option -w -t "$stray" @legion_owner "legion-$ptoken"
human=$(tm new-window -d -t "legion-$ptoken" -P -F '#{window_id}' 'sleep 3600')
made=$(date +%s)
windows() { tm list-windows -a -F '#{window_id}' 2>/dev/null; }
stray_gone() { ! windows | grep -qxF "$stray"; }
until_true 300 "the periodic orphan sweep to reap window $stray" stray_gone
reaped_after=$(($(date +%s) - made))
[ "$reaped_after" -ge 120 ] || fail "window $stray was reaped ${reaped_after}s after it opened, inside the 120s grace"
windows | grep -qxF "$human" || fail "the unmarked window $human was reaped"
grep -qF '"msg":"tmux runtime: killing an orphaned window"' "$daemon_log" || fail "the daemon logged no orphan reap"
for token in "$c1" "$c2"; do
  pane=$(claim_json "$token" | jq -r .locator.tmux.pane)
  panes | grep -qxF "$pane" || fail "the sweep took $token's pane $pane"
done
tm kill-window -t "$human"
note "stray window $stray (marked, no recorded pane) reaped ${reaped_after}s after it opened; the unmarked window $human and both claims' panes kept"
pass

# ---- done ------------------------------------------------------------------------------------------

begin stop
claims stop --claim "$c1" >/dev/null
claims stop --claim "$c2" >/dev/null
until_true 60 "both claims to be retired" sh -c \
  "'$work/legion' claims list --json --config '$work/legion.yaml' --operator-token-file '$work/operator-token' | jq -e '[.claims[] | select(.state != \"retired\")] | length == 0'"
legion stop --config "$work/legion.yaml" >/dev/null
stop_daemon "$daemon_pid" "legion stop"
daemon_pid=
note "both claims retired; the daemon stopped with exit 0"
pass

begin every-turn-through-the-gateway
# Every agent turn the run recorded, in every session of the isolated profile, each subagent's
# included, was served by the anthropic provider, the gateway's; and the same check refuses a copy
# of one captured session with a turn rewritten as Bedrock's.
route=$(bash "$root/scripts/e2e/lib/check-model-route.sh" --sessions "$HOME/.omp/profiles/$profile/agent/sessions" \
  --control "$work/model-route-control") || fail "an agent turn left the gateway route, or the check proved nothing (the reason is above)"
note "$route"
pass

ok=1
echo "stage 2 e2e: PASS"
