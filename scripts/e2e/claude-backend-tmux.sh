#!/usr/bin/env bash
# The Claude Code backend's live proof on tmux. A real Go daemon whose legion.yaml puts the
# implementer on Claude Code (`worker_backends: {implementer: claude}`) drives one scratch Dispatch
# issue: a real Oh My Pi architect and planner take it to implementing, and the implementer then runs
# as `legion claude-bridge -- claude -p …` in its pane. The proof holds that the Claude Code
# implementer registers its transcript as its session and takes its phase, completes it through the
# `legion` MCP tool's handoff_complete with a pushed `.legion/<issue>/` handoff commit, and, once its
# pane is killed, comes back with `--resume` into the same Claude Code session, its transcript's
# earlier turns intact and in its context.
#
# Run it as `LEGION_E2E_CLAUDE_KEY_FILE=<file> LEGION_E2E_MODEL_GATEWAY_URL=<gateway>/anthropic
# bash scripts/e2e/claude-backend-tmux.sh`. LEGION_E2E_CLAUDE_KEY_FILE is a file holding the
# Anthropic API key the Claude Code implementer reaches its model with: the run copies it once into
# its work directory (0600), and the implementer's claim directory names a key command that prints
# that copy (Claude Code's apiKeyHelper), so the key reaches no pane's environment, no argv and no
# transcript. The Oh My Pi agents reach their model through the Hawk model gateway, as Stage 3's do
# (lib/install-model-gateway.sh). The rig is Stage 3's without its GitHub ingress bridge: scratch
# Postgres, NATS, Envoy listener and Dispatch, Legion's role Apps through `secrets` (resolved by the
# daemon's private_key_command and once by lib/app-logins.ts, never in this shell), and the smoke
# repository sjawhar/legion-smoke, where the implementer pushes a branch and opens no pull request.
set -Eeuo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d "/tmp/legion-e2ecc.$$.XXXXXXXX")
evidence=${CLAUDE_E2E_EVIDENCE_DIR:-$(mktemp -d /tmp/legion-e2ecc-evidence.XXXXXXXX)}
if ! reason=$(bash "$root/scripts/e2e/lib/model-gateway-unserved.sh" --fresh "$evidence"); then
  echo "FAIL setup: $reason" >&2
  rmdir "$work"
  exit 1
fi
mkdir -p "$evidence/logs" "$evidence/transcripts"
# shellcheck source-path=SCRIPTDIR source=lib/transcript.sh
. "$root/scripts/e2e/lib/transcript.sh"
transcript_to "$evidence/transcript.log"
ok=
check=setup
TZ=UTC printf -v check_started '%(%FT%TZ)T' -1 # when the current check began (lib/model-gateway-unserved.sh)
project="CC$(( ($$ + $(date +%s)) % 100000000 ))"
project=${project:0:10}
ptoken=${project,,}
profile="legion-e2ecc-$$-$(date +%s)"
state="$work/state"
omp_home="$work/omp-home"
profile_agent="$omp_home/.omp/profiles/$profile/agent"
repo="sjawhar/legion-smoke"
# The model the Claude Code implementer runs, set in its claim directory's settings.json.
claude_model=claude-sonnet-5
pg_container="legion-e2ecc-pg-$$"
nats_container="legion-e2ecc-nats-$$"
daemon_pid=
dispatch_pid=
listener_pid=
watcher_pid=
port_daemon=
port_listener=
port_dispatch=
port_worker_stream=
port_pg=
port_nats=
dispatch_base=
dispatch_actor=
proof_human_user=smoke
issue=
pr_number=
smoke_file=
timeout_hook=

begin() { check=$1; TZ=UTC printf -v check_started '%(%FT%TZ)T' -1; printf '== %s\n' "$check"; }
note() { printf '   %s\n' "$*"; }
pass() { printf 'ok %s\n' "$check"; }
fail() { printf 'FAIL %s: %s\n' "$check" "$*" >&2; exit 1; }
# shellcheck source-path=SCRIPTDIR source=lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"
# shellcheck source-path=SCRIPTDIR source=lib/omp-home.sh
. "$root/scripts/e2e/lib/omp-home.sh"
# shellcheck source-path=SCRIPTDIR source=lib/workflow.sh
. "$root/scripts/e2e/lib/workflow.sh"
# shellcheck source-path=SCRIPTDIR source=lib/leftovers.sh
. "$root/scripts/e2e/lib/leftovers.sh"
# Nothing the run starts inherits the operator's production Dispatch bearer.
unset DISPATCH_TOKEN

# collect_transcripts copies every Oh My Pi session of the run's profile and every Claude Code
# transcript under the claims' own Claude Code directories into the evidence directory.
collect_transcripts() {
  local f
  [ ! -d "$profile_agent/sessions" ] || cp -a "$profile_agent/sessions/." "$evidence/transcripts/" 2>/dev/null || true
  for f in "$state"/claude/*/projects/*/*.jsonl; do
    [ -e "$f" ] || continue
    mkdir -p "$evidence/transcripts/claude"
    cp -a "$f" "$evidence/transcripts/claude/" 2>/dev/null || true
  done
}

# remove_run_branch closes any pull request of the run's project on the smoke repository and deletes
# the issue's branch, which the implementer pushed: a passing run opens no pull request, so neither
# outlives it. It makes no gh call before the proof human is established.
remove_run_branch() {
  [ -n "$proof_human" ] && [ -n "$issue" ] || return 0
  close_run_pull_requests >&2 || printf "some of this run's pull requests may still be open on %s (above)\n" "$repo" >&2
  [ "$(smoke_branch_state "legion/$issue" 2>/dev/null)" = present ] || return 0
  if timeout 60 gh api -X DELETE "repos/$repo/git/refs/heads/legion/$issue" >/dev/null 2>&1; then
    printf 'deleted %s branch legion/%s\n' "$repo" "$issue" >&2
  else
    printf 'could not delete %s branch legion/%s\n' "$repo" "$issue" >&2
  fi
}

cleanup() {
  local status=$? p
  set +e
  # Teardown is best effort: a failing command here is a warning about the teardown, never a check's
  # FAIL line, and the run's exit status stays the one the checks set.
  trap 'printf "cleanup warning: line %s exited %s\n" "$LINENO" "$?" >&2' ERR
  rm -f "$work/claude-model-key" "$work/claude-key-header"
  stop_tree "$watcher_pid"
  stop_pid "$daemon_pid"
  stop_pid "$dispatch_pid"
  stop_pid "$listener_pid"
  TMUX_TMPDIR="$work/tmux" tmux -L "legion-$ptoken" kill-server >/dev/null 2>&1 || true
  for p in $(run_processes); do kill -KILL "$p" 2>/dev/null || true; done
  docker rm -f "$pg_container" "$nats_container" >/dev/null 2>&1 || true
  collect_transcripts
  remove_run_branch
  rm -rf "$work/model-gateway-cache" || true
  if [ -n "${ok:-}" ]; then
    rm -rf "$work" || true
  else
    printf "the run's scratch workspace is %s\n" "$work" >&2
  fi
  printf "the run's evidence is %s\n" "$evidence" >&2
  [ -n "${ok:-}" ] || [[ $status =~ ^(129|130|143)$ ]] || [ ! -d "$evidence/model-gateway" ] ||
    bash "$root/scripts/e2e/lib/model-gateway-unserved.sh" --notes "$evidence/model-gateway" "$check_started" "$check" >&2 || true
  return 0
}
trap cleanup EXIT
trap 'printf "FAIL %s: line %s exited %s: %s\n" "$check" "$LINENO" "$?" "$BASH_COMMAND" >&2' ERR
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# ---- the scratch services ------------------------------------------------------------------------
start_listener() {
  local token attempt offset result
  token=$(cat "$work/envoy-token")
  for attempt in 1 2 3 4 5; do
    pick_port port_listener
    offset=$(log_size listener)
    ENVOY_API_TOKEN="$token" PORT="$port_listener" ENVOY_LISTEN_HOST=127.0.0.1 \
      ENVOY_MACHINE_ID="legion-e2ecc-$$" NATS_URLS="nats://127.0.0.1:$port_nats" \
      start_process listener env -u NATS_NKEY_SEED -u NATS_NKEY_SEED_FILE -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 \
        -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 "$work/envoy-listener"
    result=0
    await_start listener "$listener_pid" "$offset" 60 "the Envoy listener to answer /v1/sessions" \
      curl -fsS -H "@$work/envoy-auth-header" "http://127.0.0.1:$port_listener/v1/sessions" || result=$?
    [ "$result" != 0 ] || return 0
    note "the Envoy listener lost port $port_listener to another process (attempt $attempt); picking another"
  done
  fail "the Envoy listener lost its picked port five times"
}

start_dispatch() {
  local dispatch_token envoy_token pg_password attempt offset result
  dispatch_token=$(cat "$work/dispatch-token")
  envoy_token=$(cat "$work/envoy-token")
  pg_password=$(cat "$work/postgres-password")
  mkdir -p "$work/dispatch-home"
  chmod 0700 "$work/dispatch-home"
  for attempt in 1 2 3 4 5; do
    pick_port port_dispatch
    offset=$(log_size dispatch)
    # HOME and XDG_CONFIG_HOME are the scratch home, so the server reads no envoy.json of the operator's.
    DATABASE_URL="postgres://legion:$pg_password@127.0.0.1:$port_pg/dispatch?sslmode=disable" \
      DISPATCH_AGENT_TOKEN="$dispatch_token" ENVOY_TOKEN="$envoy_token" HOME="$work/dispatch-home" \
      XDG_CONFIG_HOME="$work/dispatch-home/.config" \
      DISPATCH_IDENTITY=header:X-Dispatch-User DISPATCH_IDENTITY_HEADER_TRUSTED=1 \
      DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT="$port_dispatch" \
      DISPATCH_SERVER_URL="http://127.0.0.1:$port_dispatch" NATS_URLS="nats://127.0.0.1:$port_nats" \
      ENVOY_URL="http://127.0.0.1:$port_listener" \
      start_process dispatch env -u NATS_NKEY_SEED -u NATS_NKEY_SEED_FILE -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 \
        -u GH_AGENT_APP_PRIVATE_KEY_B64 -u GH_REVIEW_APP_PRIVATE_KEY_B64 "$work/envoy-dispatch"
    result=0
    await_start dispatch "$dispatch_pid" "$offset" 60 "the scratch Dispatch server" \
      curl -fsS "http://127.0.0.1:$port_dispatch/api/v1" || result=$?
    dispatch_base="http://127.0.0.1:$port_dispatch"
    [ "$result" != 0 ] || return 0
    note "the scratch Dispatch lost port $port_dispatch to another process (attempt $attempt); picking another"
  done
  fail "the scratch Dispatch lost its picked port five times"
}

# The one setting under test is worker_backends; the design gate is off, so the architect's gate
# registration starts planning with no human approval.
write_legion_config() {
  cat >"$work/legion.yaml" <<EOF
project: $project
port: $port_daemon
worker_stream_port: $port_worker_stream
postgres_dsn: postgres://legion:$(cat "$work/postgres-password")@127.0.0.1:$port_pg/legion?sslmode=disable
state_dir: $state
operator_token_file: $work/operator-token
omp_invocation: mise x $pin -- omp
envoy_url: http://127.0.0.1:$port_listener
envoy_token_file: $work/envoy-token
nats_urls:
  - nats://127.0.0.1:$port_nats
dispatch_url: http://127.0.0.1:$port_dispatch
dispatch_token_file: $work/dispatch-token
# The project's repository is Dispatch's mapping (the PUT below makes $repo its default), not a key of
# legion.yaml.
projects:
  $project: {}
gates:
  design: off
admission_cap: 1
instructions: $work/instructions.md
worker_backends:
  implementer: claude
github_apps:
  implement:
    app_id: "3202636"
    private_key_command: "secrets LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 -- sh -c 'value=\$(printf %s \"\${LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64}\" | tr \"_-\" \"/+\"); case \$((\${#value} % 4)) in 1) value=\${value%?};; 2) value=\"\${value}==\";; 3) value=\"\${value}=\";; esac; printf %s \"\$value\" | base64 -d'"
  review:
    app_id: "3202653"
    private_key_command: "secrets GH_REVIEW_APP_PRIVATE_KEY_B64 -- sh -c 'value=\$(printf %s \"\${GH_REVIEW_APP_PRIVATE_KEY_B64}\" | tr \"_-\" \"/+\"); case \$((\${#value} % 4)) in 1) value=\${value%?};; 2) value=\"\${value}==\";; 3) value=\"\${value}=\";; esac; printf %s \"\$value\" | base64 -d'"
EOF
}

# start_daemon runs the Go daemon with the run's Claude Code launch pinned: the claude and bun it
# resolved here, and this checkout's claude-envoy package, whose dist/envoy-channel.js is every
# Claude Code pane's MCP server and whose legion/ is the Legion plugin the pane loads.
start_daemon() {
  local attempt offset result
  for attempt in 1 2 3 4 5; do
    pick_port port_daemon
    pick_port port_worker_stream
    write_legion_config
    offset=$(log_size daemon)
    HOME="$omp_home" OMP_PROFILE="$profile" PATH="$(dirname "$real_gh"):$PATH" \
      LEGION_CLAUDE_PATH="$claude_bin" LEGION_BUN_PATH="$bun_bin" LEGION_CLAUDE_PLUGIN_DIR="$root/packages/claude-envoy" \
      env -u NATS_NKEY_SEED -u NATS_NKEY_SEED_FILE -u NATS_DAEMON_NKEY_SEED -u NATS_DAEMON_NKEY_SEED_FILE \
      -u GH_PUBLIC_REPO_PAT -u LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 -u GH_AGENT_APP_PRIVATE_KEY_B64 \
      -u GH_REVIEW_APP_PRIVATE_KEY_B64 -u ANTHROPIC_API_KEY \
      "$work/legion" start --config "$work/legion.yaml" >>"$evidence/logs/daemon.log" 2>&1 &
    daemon_pid=$!
    result=0
    await_start daemon "$daemon_pid" "$offset" 180 "the Go daemon to answer /healthz" \
      curl -fsS "http://127.0.0.1:$port_daemon/healthz" || result=$?
    [ "$result" != 0 ] || return 0
    note "the Go daemon lost port $port_daemon or $port_worker_stream to another process (attempt $attempt); picking again"
  done
  fail "the Go daemon lost a picked port five times"
}

# write_claude_route gives the implementer's claim its model before the claim exists: the daemon
# makes the claim's Claude Code directory, <state>/claude/<claim token>, only if it is missing, and
# Claude Code reads settings.json there. Its apiKeyHelper prints the run's copy of the key.
write_claude_route() {
  local dir
  dir=$state/claude/$(claim_token "$issue" implementer)
  (umask 077 && mkdir -p "$dir" &&
    printf '#!/bin/sh\nexec cat %s\n' "$work/claude-model-key" >"$work/claude-key-command" &&
    jq -n --arg helper "$work/claude-key-command" --arg model "$claude_model" '{apiKeyHelper: $helper, model: $model}' >"$dir/settings.json")
  chmod 0700 "$work/claude-key-command"
  note "the implementer's Claude Code directory $dir names $claude_model and the run's key command"
}

# ---- the runtime seam: a pane's session file and its issue's workspace live on this host --------
claim_session_file() {
  "$work/legion" claims list --json --config "$work/legion.yaml" --operator-token-file "$work/operator-token" |
    jq -er --arg issue "$1" --arg role "$2" \
      '[.claims[] | select(.issue == $issue and .role == $role and .sessionFile != null and .sessionFile != "")] | last | .sessionFile'
}
claim_session_text() {
  local f
  f=$(claim_session_file "$1" "$2") || return 1
  cat -- "$f"
}
workspace_jj() {
  local issue=$1
  shift
  jj -R "$state/workspaces/$repo/${issue,,}" "$@"
}

# ---- the production guards -------------------------------------------------------------------
# agent_descendant PID prints the first process under PID (PID included) that is an agent: Oh My Pi
# (argv[0] omp) or Claude Code (argv[0] claude). A Claude Code pane's MCP server is claude's child,
# so the walk meets claude first.
agent_descendant() {
  local queue=("$1") pid children name
  while [ "${#queue[@]}" -gt 0 ]; do
    pid=${queue[0]}
    queue=("${queue[@]:1}")
    name=$(basename "$(tr '\0' '\n' <"/proc/$pid/cmdline" 2>/dev/null | sed -n 1p)")
    if [ "$name" = omp ] || [ "$name" = claude ]; then
      printf '%s\n' "$pid"
      return 0
    fi
    children=$(cat /proc/"$pid"/task/*/children 2>/dev/null || true)
    for child in $children; do queue+=("$child"); done
  done
  return 1
}
claim_pane_pid() {
  daemon_state | jq -er --arg issue "$1" --arg role "$2" \
    '(if $role == "architect" then .issues[$issue].architect else .issues[$issue].workers[$role].claim end).locator.incarnation | split(":")[0]'
}
pane_value() { tr '\0' '\n' <"/proc/$1/environ" | sed -n "s/^$2=//p"; }
# endpoint_mismatch AGENT_PID prints the first rig endpoint the agent does not carry, or the
# ANTHROPIC_API_KEY it must not carry: every agent's key is a key command's, never its environment's.
endpoint_mismatch() {
  local agent=$1 name want got
  for name in DISPATCH_URL DISPATCH_TOKEN_FILE ENVOY_URL ENVOY_NATS_URL; do
    case "$name" in
      DISPATCH_URL) want="http://127.0.0.1:$port_dispatch" ;;
      DISPATCH_TOKEN_FILE) want="$state/secrets/dispatch-token" ;;
      ENVOY_URL) want="http://127.0.0.1:$port_listener" ;;
      ENVOY_NATS_URL) want="nats://127.0.0.1:$port_nats" ;;
    esac
    got=$(pane_value "$agent" "$name")
    if [ "$got" != "$want" ]; then
      printf '%s=%s, want %s\n' "$name" "${got:-<unset>}" "$want"
      return 0
    fi
  done
  if grep -qz '^ANTHROPIC_API_KEY=' "/proc/$agent/environ" 2>/dev/null; then
    printf 'ANTHROPIC_API_KEY set, want unset\n'
    return 0
  fi
  return 1
}
assert_claim_endpoints() {
  local issue=$1 role=$2 pane_pid agent mismatch
  pane_pid=$(claim_pane_pid "$issue" "$role") || fail "$role pane on $issue has no process locator"
  agent=$(agent_descendant "$pane_pid") || fail "$role pane on $issue (pid $pane_pid) has no agent process"
  if mismatch=$(endpoint_mismatch "$agent"); then
    fail "ABORT: $role pane on $issue (agent pid $agent) has $mismatch"
  fi
}
# pane_watcher checks every pane the daemon launches as soon as its agent process exists, before it
# registers and before any turn; a mismatch kills the private tmux server, and the next bounded wait
# aborts naming the pane.
pane_watcher() {
  local claims inc issue role agent mismatch
  trap - EXIT ERR
  set +e
  while kill -0 "$$" 2>/dev/null; do
    if claims=$("$work/legion" claims list --json --config "$work/legion.yaml" --operator-token-file "$work/operator-token" 2>/dev/null); then
      while IFS=$'\t' read -r inc issue role; do
        grep -qF "$inc " "$evidence/pane-endpoints-checked.txt" 2>/dev/null && continue
        agent=$(agent_descendant "${inc%%:*}") || continue
        if mismatch=$(endpoint_mismatch "$agent"); then
          printf '%s pane on %s (incarnation %s, agent pid %s) has %s\n' "$role" "$issue" "$inc" "$agent" "$mismatch" \
            >"$evidence/pane-endpoint-violation.txt"
          TMUX_TMPDIR="$work/tmux" tmux -L "legion-$ptoken" kill-server >/dev/null 2>&1 || true
          return 0
        fi
        printf '%s %s %s %s %s\n' "$inc" "$issue" "$role" "$agent" "$(date -u +%FT%T.%3NZ)" >>"$evidence/pane-endpoints-checked.txt"
      done < <(jq -r '.claims[] | select(.locator != null) | [.locator.incarnation, .issue, .role] | @tsv' <<<"$claims")
    fi
    sleep 0.2
  done
}
unchecked_launches() {
  local inc
  for inc in $(jq -R -r 'fromjson? | select(.msg == "supervise: launched") | .incarnation' "$evidence/logs/daemon.log"); do
    grep -qF "$inc " "$evidence/pane-endpoints-checked.txt" || printf '%s\n' "$inc"
  done
}
no_unchecked_launches() { [ -z "$(unchecked_launches)" ]; }

# ---- the Claude Code implementer --------------------------------------------------------------
implementer_claim() {
  "$work/legion" claims list --json --config "$work/legion.yaml" --operator-token-file "$work/operator-token" |
    jq -ce --arg token "$(claim_token "$issue" implementer)" '.claims[] | select(.token == $token)'
}
# claude_pid PANE_PID prints the pane's Claude Code process, failing when the pane's agent is not one.
claude_pid() {
  local agent
  agent=$(agent_descendant "$1") || return 1
  [ "$(basename "$(tr '\0' '\n' <"/proc/$agent/cmdline" | sed -n 1p)")" = claude ] || return 1
  printf '%s\n' "$agent"
}
# claude_argv CLAUDE_PID prints its argv, one argument per line.
claude_argv() { tr '\0' '\n' <"/proc/$1/cmdline"; }
# bridged CLAUDE_PID: its parent is `legion claude-bridge`, the pane's bridge to the shim.
bridged() {
  local stat parent
  stat=$(cat "/proc/$1/stat")
  read -r -a stat <<<"${stat##*) }"
  parent=${stat[1]}
  tr '\0' ' ' <"/proc/$parent/cmdline" | grep -q ' claude-bridge '
}
# argv_has_pair ARGV FLAG VALUE: ARGV (one argument per line) holds FLAG immediately followed by VALUE.
argv_has_pair() { awk -v flag="$2" -v value="$3" 'prev == flag && $0 == value { found = 1 } { prev = $0 } END { exit !found }' <<<"$1"; }
# legion_handoff_in TRANSCRIPT: the transcript holds a call of the `legion` MCP tool whose input names
# handoff_complete, and a tool result for it that is not an error.
legion_handoff_in() {
  jq -R -s -e '
    [split("\n")[] | fromjson? | select(.message.content | type == "array") | .message.content[]] as $blocks
    | [$blocks[] | select(.type == "tool_use" and (.name | endswith("__legion")) and (.input | tostring | contains("handoff_complete"))) | .id] as $calls
    | any($blocks[]; .type == "tool_result" and (.tool_use_id as $id | $calls | index($id)) and (.is_error != true))' "$1" >/dev/null
}
# resumed_reply_names TRANSCRIPT OFFSET MARKER: an assistant text written past OFFSET bytes quotes MARKER.
resumed_reply_names() {
  local text
  text=$(tail -c "+$(($2 + 1))" "$1" | jq -R -r 'fromjson? | select(.type == "assistant") | .message.content[]? | select(.type == "text") | .text')
  grep -Fq -- "$3" <<<"$text"
}
handoff_recorded() { [ -n "$(role_handoff "$issue" implementer implementing)" ]; }

begin prerequisites
key_file=${LEGION_E2E_CLAUDE_KEY_FILE:-}
[ -n "$key_file" ] ||
  fail "LEGION_E2E_CLAUDE_KEY_FILE is unset: name a file holding the Anthropic API key the Claude Code implementer reaches its model with"
[ -f "$key_file" ] && [ -r "$key_file" ] || fail "LEGION_E2E_CLAUDE_KEY_FILE names $key_file, which is not a readable file"
refuse_leftovers legion-e2ecc
for tool in go docker jq curl ss tmux bun mise secrets gh jj hawk-token pgrep claude; do command -v "$tool" >/dev/null || fail "$tool is required"; done
(umask 077 && tr -d '[:space:]' <"$key_file" >"$work/claude-model-key")
[ -s "$work/claude-model-key" ] || fail "LEGION_E2E_CLAUDE_KEY_FILE ($key_file) holds no key"
# The key's preflight: Anthropic lists its models to a key it accepts. The header goes by file, so the
# key is in no argv.
(umask 077 && printf 'x-api-key: %s\n' "$(cat "$work/claude-model-key")" >"$work/claude-key-header")
code=$(curl -sS --max-time 30 -o /dev/null -w '%{http_code}' -H "@$work/claude-key-header" -H 'anthropic-version: 2023-06-01' \
  https://api.anthropic.com/v1/models) || code="unreachable (curl exited $?)"
rm -f "$work/claude-key-header"
[ "$code" = 200 ] || fail "Anthropic refuses the key in LEGION_E2E_CLAUDE_KEY_FILE ($key_file): GET /v1/models answered $code"
note "the Claude Code implementer's key from $key_file: Anthropic accepts it"
for key in LEGION_IMPLEMENT_APP_PRIVATE_KEY_B64 GH_REVIEW_APP_PRIVATE_KEY_B64; do
  status=$(secrets get "$key" --no-request 2>&1) || fail "secrets get $key does not resolve: $status"
  jq -e --arg key "$key" '.key == $key and .tier == "agent"' <<<"$status" >/dev/null 2>&1 ||
    fail "secrets get $key reports $status, want the agent tier"
  note "secrets get $key: $status"
done
claude_bin=$(command -v claude)
bun_bin=$(command -v bun)
note "Claude Code: $claude_bin ($("$claude_bin" --version)); bun: $bun_bin"
real_gh=$(mise which gh) || fail "mise has no gh"
require_proof_human
gh repo view "$repo" --json name >/dev/null || fail "the devbox's ordinary gh cannot read $repo"
mkdir -p "$state" "$work/xdg" "$work/tmux"
chmod 0700 "$state" "$work/xdg" "$work/tmux"
make_omp_home "$omp_home"
key_command=$(bash "$root/scripts/e2e/lib/install-model-gateway.sh" --profile "$profile" --home "$omp_home" --dest "$evidence/model-gateway" --cache-dir "$work/model-gateway-cache") ||
  fail "the Oh My Pi agents' model route through the Hawk model gateway could not be installed (the reason is above)"
note "the Oh My Pi agents' model route: keyed by $key_command"
export XDG_STATE_HOME="$work/xdg"
export TMUX_TMPDIR="$work/tmux"
pass

begin rig
(umask 077 && head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/envoy-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/envoy-token")" >"$work/envoy-auth-header" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/dispatch-token" &&
  printf 'Authorization: Bearer %s\n' "$(cat "$work/dispatch-token")" >"$work/dispatch-auth-header" &&
  printf 'X-Dispatch-User: %s\n' "$proof_human_user" >"$work/dispatch-human-header" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/operator-token" &&
  head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' >"$work/postgres-password")
chmod 0600 "$work"/*token "$work"/*-header "$work/postgres-password"
(cd "$root/packages/daemon" && go build -o "$work/legion" ./cmd/legion)
(cd "$root/packages/envoy" && go build -o "$work/envoy-listener" ./cmd/listener && go build -o "$work/envoy-dispatch" ./cmd/dispatch)
built=$(bash "$root/scripts/e2e/lib/built-from.sh" "$root" "$work/legion" "$work/envoy-listener" "$work/envoy-dispatch") || fail "lib/built-from.sh could not say what the run built"
while IFS= read -r line; do note "$line"; done <<<"$built"
docker run -d --name "$pg_container" --mount type=tmpfs,destination=/var/lib/postgresql/data \
  -e POSTGRES_USER=legion -e POSTGRES_PASSWORD="$(cat "$work/postgres-password")" -e POSTGRES_DB=dispatch \
  -p "127.0.0.1::5432" pgvector/pgvector@sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a >/dev/null
port_pg=$(docker port "$pg_container" 5432/tcp | sed -n '1s/.*://p')
[ -n "$port_pg" ] || fail "docker assigned Postgres no host port"
until_true 60 "Postgres to accept TCP connections" docker exec "$pg_container" pg_isready -h 127.0.0.1 -p 5432 -U legion -d dispatch
docker exec "$pg_container" createdb -U legion legion
docker run -d --name "$nats_container" -p "127.0.0.1::4222" nats:2.10 -js >/dev/null
port_nats=$(docker port "$nats_container" 4222/tcp | sed -n '1s/.*://p')
[ -n "$port_nats" ] || fail "docker assigned NATS no host port"
until_true 60 "NATS to be ready" sh -c "docker logs '$nats_container' 2>&1 | grep -q 'Server is ready'"
start_listener
start_dispatch
dispatch_human POST projects "$(jq -cn --arg key "$project" --arg name "Claude backend proof $project" '{key:$key,name:$name}')" >/dev/null
dispatch_human PUT "settings/repo-projects/$repo" "$(jq -cn --arg project "$project" '{project:$project,default:true}')" >/dev/null
(cd "$root" && bun install --frozen-lockfile >/dev/null)
envoy_manifest=$(bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --package pi-envoy --profile "$profile" --home "$omp_home" --dest "$work/pi-envoy")
manifest=$(bash "$root/scripts/e2e/lib/install-plugin-profile.sh" --package pi-legion --profile "$profile" --home "$omp_home" --dest "$work/pi-legion")
pin=$(<"$root/.omp-pin")
mise where "$pin" >/dev/null 2>&1 || mise install "$pin" >&2
cat >"$work/instructions.md" <<'EOF'
# Claude Code backend proof instructions

This is a throwaway workflow proof. A tree's root architect starts its tree from the daemon's
`catch-up` notice as its role says: it writes the spec in the issue's own primary document and
registers the gate, then waits. Apart from that, do not act until a human Dispatch message targeted
at your own session gives the next exact proof operation; it arrives in your session as a message to
you. Any other notice is not an instruction. Follow your instruction precisely, use the Go-daemon
Legion tools and handoffs, and do not create work outside the issue's smoke branch.
EOF
start_daemon
read_app_logins
note "project $project, profile $profile, plugins $(jq -r '.name + "@" + .version' "$envoy_manifest") and $(jq -r '.name + "@" + .version' "$manifest"), NATS/Dispatch/daemon ports $port_nats/$port_dispatch/$port_daemon"
pane_watcher &
watcher_pid=$!
pass

begin admission
issue=$(new_issue "Claude Code implementer proof")
write_claude_route
set_status "$issue" todo
until_true 180 "$issue to be admitted" sh -c \
  "'$work/legion' state --json --config '$work/legion.yaml' | jq -e --arg issue '$issue' '.admission.active == [\$issue]' >/dev/null"
pass

begin architect-registers-the-gate
architect_registers_gate "$issue" "the proof's"
wait_for_phase "$issue" planning
wait_for_worker "$issue" planner
note "the design gate is off: the architect's registration started planning"
pass

begin planner-handoff
send_agent "$issue" planner "Claude backend proof planning operation: write the required plan handoff for the one-file smoke change, then call the legion tool's handoff_complete with a concise summary. Do not start another role."
wait_for_phase "$issue" implementing
assert_handoff_committer "$issue" planner planning 0
pass

begin claude-implementer-registers
wait_for_worker "$issue" implementer
token=$(claim_token "$issue" implementer)
claim=$(implementer_claim) || fail "no implementer claim $token in legion claims list"
session=$(jq -r .session <<<"$claim")
transcript=$(jq -r .sessionFile <<<"$claim")
pane=$(jq -r .locator.tmux.pane <<<"$claim")
generation=$(jq -r .generation <<<"$claim")
case "$transcript" in
  "$state/claude/$token/projects/"*"/$session.jsonl") ;;
  *) fail "the implementer registered session file $transcript, want $state/claude/$token/projects/<cwd>/$session.jsonl" ;;
esac
[ -s "$transcript" ] || fail "the implementer's transcript $transcript is missing or empty"
pane_pid=$(claim_pane_pid "$issue" implementer)
cc=$(claude_pid "$pane_pid") || fail "the implementer pane (pid $pane_pid) runs no Claude Code"
argv=$(claude_argv "$cc")
argv_has_pair "$argv" --session-id "$session" || fail "the implementer's claude was not started with --session-id $session: $(tr '\n' ' ' <<<"$argv" | cut -c1-300)"
bridged "$cc" || fail "the implementer's claude (pid $cc) is not a child of legion claude-bridge"
note "implementer claim $token: generation $generation, pane $pane, Claude Code session $session, transcript $transcript"
pass

begin claude-implementer-handoff
marker="claude-proof-marker-$ptoken-$RANDOM$RANDOM"
send_agent "$issue" implementer "Claude backend proof implementation operation ($marker): make the smallest one-file change this issue's spec describes in your $repo workspace, commit it on legion/$issue, write and commit the required implementation handoff, push the branch to origin, then call the legion tool's handoff_complete with a concise summary. Do not open a pull request and do not merge."
until_true 900 "the daemon to record the implementer's handoff_complete" handoff_recorded
commit=$(role_handoff "$issue" implementer implementing)
assert_handoff_committer "$issue" implementer implementing 0
files=$(timeout 60 gh api "repos/$repo/commits/$commit" --jq '.files[].filename') ||
  fail "the handoff commit $commit the implementer reported is not on $repo"
grep -q "^\.legion/$issue/" <<<"$files" || fail "the handoff commit $commit changes nothing under .legion/$issue/: $(tr '\n' ' ' <<<"$files")"
legion_handoff_in "$transcript" || fail "the transcript $transcript holds no successful legion handoff_complete call"
until_true 300 "the implementer's turn to end" issue_worker_state "$issue" implementer idle
note "handoff_complete through mcp__legion__legion recorded commit $commit, pushed to $repo under .legion/$issue/"
pass

begin kill-resumes-the-same-claude-session
size=$(stat -c %s "$transcript")
prefix=$(head -c "$size" "$transcript" | sha256sum)
turns=$(jq -R -s '[split("\n")[] | fromjson? | select(.type == "user" or .type == "assistant")] | length' "$transcript")
tmux -L "legion-$ptoken" kill-pane -t "$pane"
until_true 300 "the implementer to relaunch after its pane was killed" sh -c \
  "'$work/legion' claims list --json --config '$work/legion.yaml' --operator-token-file '$work/operator-token' |
    jq -e --arg token '$token' --argjson gen '$generation' '.claims[] | select(.token == \$token) | .generation > \$gen' >/dev/null"
until_true 300 "the relaunched implementer to register" issue_worker_live "$issue" implementer "$pane"
claim=$(implementer_claim)
[ "$(jq -r .session <<<"$claim")" = "$session" ] || fail "the relaunch registered session $(jq -r .session <<<"$claim"), want $session"
[ "$(jq -r .sessionFile <<<"$claim")" = "$transcript" ] || fail "the relaunch registered session file $(jq -r .sessionFile <<<"$claim"), want $transcript"
cc=$(claude_pid "$(claim_pane_pid "$issue" implementer)") || fail "the relaunched implementer pane runs no Claude Code"
argv=$(claude_argv "$cc")
argv_has_pair "$argv" --resume "$session" || fail "the relaunched claude was not started with --resume $session: $(tr '\n' ' ' <<<"$argv" | cut -c1-300)"
[ "$(head -c "$size" "$transcript" | sha256sum)" = "$prefix" ] || fail "the transcript's first $size bytes changed across the relaunch"
send_agent "$issue" implementer "Claude backend proof after a relaunch: reply with one line starting WAITING that quotes the proof marker your implementation instruction in this session carried. Change nothing and call no tool."
until_true 600 "the resumed implementer to quote its first instruction's marker" resumed_reply_names "$transcript" "$size" "$marker"
note "killed pane $pane (generation $generation): generation $(jq -r .generation <<<"$claim") runs claude --resume $session; the transcript kept its $turns earlier turns ($size bytes unchanged) and the resumed session quoted $marker"
pass

begin every-launch-endpoint-checked
timeout_hook=
until_true 60 "every launched pane to be endpoint-checked" no_unchecked_launches
note "$(wc -l <"$evidence/pane-endpoints-checked.txt") launched panes endpoint-checked before registration"
pass

ok=1
echo "claude backend e2e: PASS"
