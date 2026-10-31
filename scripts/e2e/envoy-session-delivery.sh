#!/usr/bin/env bash
# envoy-session-delivery.sh — live proof that every Envoy event reaches a real Oh My Pi session and
# a real Claude Code session exactly once, also when it was published while the session, its NATS
# connection, either listener or Dispatch was down (INFRA-594).
#
# The stack runs in the pod, every part of it built from a checkout: natsd
# (packages/envoy/internal/testnats/cmd/natsd, JetStream on a store directory of the run's own),
# two Envoy listeners (machine id `shared`, where both sessions register, and `dispatch`, the one
# Dispatch sends through), Dispatch (`envoy-dispatch`) on a database of the run's own in the
# loopback Postgres LEGION_TEST_PG_DSN names, and two sessions:
#
#   pi   Oh My Pi in `--mode rpc --no-extensions -e <the checkout's packed pi-envoy>`
#   cc   Claude Code under `legion claude-bridge -- claude -p --input-format stream-json
#        --output-format stream-json --verbose --replay-user-messages …`, Claude Code from
#        `bunx --package @anthropic-ai/claude-code@<version> claude`, with claude-envoy built from
#        the checkout (`packages/claude-envoy/scripts/build.ts --out` into the run's directory: a
#        pull request's tree carries no fresh dist/) as
#        packages/daemon/internal/runtime/tmux/claude.go configures it (an MCP config naming
#        dist/envoy-channel.js, both plugin directories, LEGION_CLAUDE_INBOX).
#
# Each session reaches NATS only through a socat proxy of its own, which the script can cut. Every
# server takes its port as lib/held-port.py gives it, never a port picked free a moment earlier:
# natsd binds ports the kernel chooses and reports them (-p -1 -m -1); each listener, Dispatch and
# each proxy (socat's ACCEPT-FD) serves on a copy of a socket a keeper holds for the whole run, so a
# restart serves the same port. Only a --build-from listener that takes no ENVOY_LISTEN_FD binds a
# number, a port picked free below the kernel's ephemeral range, which its restart binds again. At
# setup each session is told (one model turn) to follow the issue topic
# notifications.dispatch.issue.<KEY>.> and the run's own team topic
# notifications.team.live-delivery-<run>, and to hold a role of its own; the listener's registry is
# then read to show them. Then six windows run, one disruption each, with every event of the window
# published while the disruption holds:
#
#   a  both sessions SIGKILLed and resumed under the same id (omp --resume <session file>; the
#      bridge relaunched with claude --resume <id>)
#   b  both sessions' NATS connections cut at their proxies, and restored
#   c  listener `shared` SIGKILLed and restarted
#   d  listener `dispatch` SIGKILLed and restarted
#   e  Dispatch's own NATS connection (through a proxy of its own) cut, Dispatch's writes made,
#      Dispatch SIGKILLed right after they return, the proxy restored and Dispatch restarted, so
#      events it committed before the kill are published after the restart (the stored times of
#      the window's comments are printed beside the restart time)
#   f  none (the control)
#
# Events of every window, each carrying a unique marker in its text:
#   agent  POST /v1/messages/send to each session; a Dispatch message to each session (an aside
#          on a second issue the sessions do not follow, which Dispatch sends through its listener)
#   issue  two Dispatch comments on the followed issue
#   role   POST /v1/messages/publish to each session's role; a core NATS publish of an Envoy
#          envelope on notifications.role.<role>, which the listeners' role lane forwards
#   team   POST /v1/messages/publish to the team topic both sessions follow, one event for both:
#          the stream stores notifications.team.>, so each session's own consumer holds it until
#          the session is back (window a kills both sessions, and both must still count it once)
# The listener sends go to listener `dispatch`, and to `shared` in window d.
#
# After each window, up to SESSION_DELIVERY_SETTLE_S for every marker to reach its session, then
# SESSION_DELIVERY_HOLD_S more for a repeat. At the end every marker is counted in each
# transcript it was meant for (the Oh My Pi session file's envoy-message cards and user messages;
# the Claude Code transcript <CLAUDE_CONFIG_DIR>/projects/*/<id>.jsonl's user messages and queued
# prompts), the issue's comments are checked to appear in their publish order (the order the
# stream stored them in, read back from it; a note says when Dispatch's outbox published them in
# another order than they were written), and the table is printed. A Dispatch message Dispatch
# reports it could not send (its listener down, window d) is expected 0 times: nothing was
# published.
#
#   bash scripts/e2e/envoy-session-delivery.sh [--disruptions a,b,c,d,e,f]
#       [--build-from <rev> [--expect-lost <agent,issue,role,team>]] [--plugins-from <rev>]
#       [--resume-plugins-from <rev>]
#       [--second-followed-issue]
#
# --build-from <rev> builds the listener, pi-envoy, claude-envoy and the `legion` bridge from <rev>
# instead, extracted with `git archive <rev> | tar -x` into the run's directory; natsd and Dispatch
# stay this checkout's. claude-envoy is that tree's `scripts/build.ts --out` build when its
# build.ts takes `--out`, else (a revision from before pull requests stopped committing dist/) its
# committed dist/ and legion/. The bridge's argv carries --replay-user-messages only when <rev>'s
# argv source (claudelaunch.go, or tmux/claude.go before it) does. --expect-lost names the kinds that rev
# is expected to lose under disruption a: the run then exits 0 only when every event of those kinds
# in window a counted 0 in both sessions (the negative control), and the rest of the table is
# printed and not judged.
#
# --plugins-from <rev> is the mixed rollout: pi-envoy and claude-envoy are built from <rev>, as
# --build-from builds them, and everything else — the listeners, natsd, Dispatch, the bridge —
# stays this checkout's. It is the pair a release meets first, a hub already updated and a session
# still running the plugins of the release before it. The two flags name one tree each and are not
# given together.
#
# --resume-plugins-from <rev> is the upgrade, and it runs only with disruption a: both sessions
# start on the plugins built from <rev>, and the resume after the SIGKILL starts them on this
# checkout's, so one session's own life crosses the release. It is the move every Legion worker
# makes when its image is re-pinned: the session registered on the old plugin's path, went away,
# the stream stored its events, and the new plugin opens its first consumer. The run judges the
# table as it always does, so every event of every window must still be counted once. The flag
# names one tree, as --plugins-from and --build-from do, and is not given with either.
#
# --second-followed-issue makes both sessions follow a second Dispatch issue's topic beside the
# first, and publishes one more comment there in every window. Two followed issue topics and a
# role put more than one cursor in the plugin's ledger, so a resume must take each frame on the
# topic it was stored for, and no cursor of another topic may rescue or drop it.
#
# Inputs (required ones refuse to start without them):
#   LEGION_TEST_PG_DSN            required: a loopback Postgres with pgvector, as
#                                 postgres://<user>@127.0.0.1:<port>/<db>?sslmode=disable; the run
#                                 creates a database of its own there and drops it on exit
#   SESSION_DELIVERY_MODEL_KEY_FILE  the model key's file (default /var/run/model-gateway-key/key,
#                                 where every role pod mounts the model gateway's key); the run
#                                 reads it into no variable it exports, hands it to Oh My Pi and
#                                 Claude Code alone as their ANTHROPIC_API_KEY, and never prints it
#   ANTHROPIC_BASE_URL            when set, passed to Oh My Pi and Claude Code only, beside the key
#                                 (a model gateway, such as an issue pod's, whose key it is)
#   SESSION_DELIVERY_OMP          Oh My Pi (default: omp on PATH, else /opt/omp/bin/omp)
#   SESSION_DELIVERY_MODEL        default claude-haiku-4-5 (Oh My Pi gets anthropic/<model>)
#   SESSION_DELIVERY_CLAUDE_CODE  the Claude Code version bunx runs (default 2.1.311)
#   SESSION_DELIVERY_HEARTBEAT_MS the sessions' ENVOY_HEARTBEAT_MS (default 10000)
#   SESSION_DELIVERY_SETTLE_S     per-window wait for every marker (default 60)
#   SESSION_DELIVERY_HOLD_S       wait after that for a repeat (default 15)
#   SESSION_DELIVERY_EVIDENCE_DIR default a fresh directory under TMPDIR; kept and printed
#
# No DISPATCH_*, ENVOY_*, NATS_*, OMP*, PI_*, LEGION_* or CLAUDE_* variable of the caller reaches
# anything the run starts. Every service and session gets PATH, what the script sets, and, for the
# sessions alone, ANTHROPIC_API_KEY (read from SESSION_DELIVERY_MODEL_KEY_FILE), ANTHROPIC_BASE_URL
# when the caller sets it, and BUN_INSTALL_CACHE_DIR for bunx when the caller sets it.
# Everything started is stopped on exit by the pid it was started as; the database is dropped; the
# evidence directory is kept. Manual only, never CI: it needs a model provider.
#
# Exit status: 0 every count as expected and the order held (or, with --expect-lost, every named
# kind lost under a); 1 a count or the order is wrong, or a step failed (`FAIL <step>: …`); 2
# refused (an input or tool is missing); 130, 143 on SIGINT, SIGTERM.
# shellcheck disable=SC2317 # cleanup runs from the EXIT trap, the probes from await and wait_until
set -euo pipefail
for variable in $(compgen -e); do
  case $variable in DISPATCH_* | ENVOY_* | NATS_* | OMP* | PI_* | LEGION_* | CLAUDE_*)
    [ "$variable" = LEGION_TEST_PG_DSN ] || unset "$variable" ;;
  esac
done
model_key_file=${SESSION_DELIVERY_MODEL_KEY_FILE:-/var/run/model-gateway-key/key}
# A caller's own key reaches nothing the run starts: the builds, bun's lifecycle scripts and the
# tools would otherwise inherit it. The sessions get the key file's through isolated_exec.
unset ANTHROPIC_API_KEY

readonly me=envoy-session-delivery
refuse() {
  printf '%s: %s\n' "$me" "$1" >&2
  exit 2
}
note() { printf '[%s] %s\n' "$(date -u +%H:%M:%SZ)" "$*"; }
step=setup
begin() {
  step=$1
  printf '== %s\n' "$step"
}
fail() {
  printf 'FAIL %s: %s\n' "$step" "$1"
  exit 1
}

usage() {
  sed -n '/^#   bash scripts/,/^#       \[--second-followed-issue/p' "$0" | sed 's/^# \{0,3\}//'
}

# ---- arguments and inputs ---------------------------------------------------------------------
build_from=""
plugins_from=""
resume_plugins_from=""
second_followed_issue=""
expect_lost=""
disruptions="a,b,c,d,e,f"
while (($#)); do
  case $1 in
  --build-from) [ $# -ge 2 ] || refuse "--build-from needs a revision"; build_from=$2; shift 2 ;;
  --plugins-from) [ $# -ge 2 ] || refuse "--plugins-from needs a revision"; plugins_from=$2; shift 2 ;;
  --resume-plugins-from) [ $# -ge 2 ] || refuse "--resume-plugins-from needs a revision"; resume_plugins_from=$2; shift 2 ;;
  --second-followed-issue) second_followed_issue=1; shift ;;
  --expect-lost) [ $# -ge 2 ] || refuse "--expect-lost needs a list of kinds"; expect_lost=$2; shift 2 ;;
  --disruptions) [ $# -ge 2 ] || refuse "--disruptions needs a list"; disruptions=$2; shift 2 ;;
  -h | --help) usage; exit 0 ;;
  *) refuse "unknown argument '$1' (--help lists them)" ;;
  esac
done
[[ "$disruptions" =~ ^[a-f](,[a-f])*$ ]] || refuse "--disruptions takes letters a-f separated by commas, not '$disruptions'"
[[ -z "$expect_lost" || "$expect_lost" =~ ^(agent|issue|role|team)(,(agent|issue|role|team))*$ ]] ||
  refuse "--expect-lost takes agent, issue, role and team separated by commas, not '$expect_lost'"
[[ -z "$expect_lost" || -n "$build_from" ]] || refuse "--expect-lost applies only with --build-from"
[[ -z "$expect_lost" || ",$disruptions," == *",a,"* ]] || refuse "--expect-lost judges disruption a, which --disruptions leaves out"
[[ -z "$plugins_from" || -z "$build_from" ]] ||
  refuse "--plugins-from and --build-from each name the tree the plugins are built from; give one"
[[ -z "$resume_plugins_from" || ( -z "$build_from" && -z "$plugins_from" ) ]] ||
  refuse "--resume-plugins-from names the tree the sessions start on; do not give it with --build-from or --plugins-from"
[[ -z "$resume_plugins_from" || ",$disruptions," == *",a,"* ]] ||
  refuse "--resume-plugins-from upgrades the sessions at disruption a, which --disruptions leaves out"

[ -n "${LEGION_TEST_PG_DSN:-}" ] ||
  refuse "LEGION_TEST_PG_DSN is required: a loopback Postgres with pgvector (postgres://<user>@127.0.0.1:<port>/<db>?sslmode=disable). The run starts no database; docs/solutions/testing/the-real-dispatch-stack-runs-inside-a-legion-pod-with-no-postgres-no-docker-and-no-root-INFRA-42.md starts one without root."
[[ "$LEGION_TEST_PG_DSN" =~ ^(postgres(ql)?://[^/]+)/([^?]*)(\?.*)?$ ]] ||
  refuse "LEGION_TEST_PG_DSN is not a postgres:// URL with a database path"
pg_server=${BASH_REMATCH[1]}
pg_params=${BASH_REMATCH[4]}
[ -r "$model_key_file" ] ||
  refuse "the model key file $model_key_file is not readable: both sessions run their turns on it (SESSION_DELIVERY_MODEL_KEY_FILE names another)"
# Read into a shell variable that is never exported: isolated_exec hands it to the two sessions'
# own environment alone. Its contents are never printed; only its being empty is checked.
anthropic_key=$(<"$model_key_file")
[ -n "$anthropic_key" ] || refuse "the model key file $model_key_file is empty"
for tool in go bun jq curl socat setsid psql git tar pgrep mkfifo python3; do
  command -v "$tool" >/dev/null 2>&1 || refuse "$tool is required and is not on PATH"
done
socat_help=$(socat -h 2>/dev/null || true)
[[ "$socat_help" == *"ACCEPT-FD:"* ]] ||
  refuse "socat $(socat -V 2>/dev/null | sed -n 's/^socat version \([^ ]*\).*/\1/p') has no ACCEPT-FD address (socat 1.8.0 or later): the proxies serve on a held socket"
omp=${SESSION_DELIVERY_OMP:-$(command -v omp || true)}
if [ -z "$omp" ] && [ -x /opt/omp/bin/omp ]; then omp=/opt/omp/bin/omp; fi
[[ -n "$omp" && -x "$omp" ]] || refuse "no Oh My Pi: set SESSION_DELIVERY_OMP (omp is not on PATH and /opt/omp/bin/omp is absent)"
model=${SESSION_DELIVERY_MODEL:-claude-haiku-4-5}
claude_version=${SESSION_DELIVERY_CLAUDE_CODE:-2.1.311}
heartbeat_ms=${SESSION_DELIVERY_HEARTBEAT_MS:-10000}
settle_s=${SESSION_DELIVERY_SETTLE_S:-60}
hold_s=${SESSION_DELIVERY_HOLD_S:-15}
for value in "$heartbeat_ms" "$settle_s" "$hold_s"; do
  [[ "$value" =~ ^[1-9][0-9]*$ ]] || refuse "SESSION_DELIVERY_HEARTBEAT_MS, _SETTLE_S and _HOLD_S take positive integers, not '$value'"
done

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
readonly repo_root
readonly held_port="$repo_root/scripts/e2e/lib/held-port.py"
# shellcheck source-path=SCRIPTDIR source=lib/port-file.sh
. "$repo_root/scripts/e2e/lib/port-file.sh"
[ -f "$repo_root/node_modules/nats/package.json" ] || refuse "$repo_root/node_modules has no nats package: run bun install in the checkout"
work=${SESSION_DELIVERY_EVIDENCE_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/$me.XXXXXXXX")}
mkdir -p "$work/logs" "$work/bin" "$work/sessions" "$work/ports"
work=$(cd -- "$work" && pwd -P)
readonly work
events="$work/events.jsonl"
: >"$events"

# ---- processes and teardown -------------------------------------------------------------------
declare -A pid=()        # service or session label -> pid
declare -A fd=()         # session label -> the write end of its stdin fifo
database=""
running() {
  local state
  [[ -r "/proc/$1/stat" ]] || return 1
  read -r _ _ state _ 2>/dev/null <"/proc/$1/stat" || return 1
  [[ "$state" != Z ]]
}
# kill_label LABEL SIGNAL: the process started under LABEL, and when it leads its own process group
# (the proxies and the sessions run under setsid), everything in that group.
kill_label() {
  local label=$1 signal=$2 p=${pid[$1]:-} group
  [[ "$p" =~ ^[1-9][0-9]*$ ]] || return 0
  group=""
  if [[ -r "/proc/$p/stat" ]]; then read -r _ _ _ _ group _ <"/proc/$p/stat" 2>/dev/null || true; fi
  if [[ "$group" == "$p" ]]; then
    kill "-$signal" -- "-$p" 2>/dev/null || true
  else
    kill "-$signal" "$p" 2>/dev/null || true
  fi
  if [[ "$signal" == KILL ]]; then
    wait "$p" 2>/dev/null || true
    unset "pid[$label]"
  fi
}
stop_label() {
  local p=${pid[$1]:-} i
  [[ -n "$p" ]] || return 0
  kill_label "$1" TERM
  for ((i = 0; i < 25; i++)); do running "$p" || break; sleep 0.2; done
  kill_label "$1" KILL
}
close_fd() {
  local f=${fd[$1]:-}
  [[ -z "$f" ]] || exec {f}>&-
  unset "fd[$1]"
}
cleanup() {
  local status=$? label
  trap - EXIT
  set +e
  for label in pi cc; do close_fd "$label"; stop_label "$label"; done
  for label in proxy-pi proxy-cc dispatch proxy-dispatch listener-shared listener-dispatch natsd \
    keep-proxy-pi keep-proxy-cc keep-proxy-dispatch keep-dispatch keep-listener-shared keep-listener-dispatch; do
    stop_label "$label"
  done
  if [[ -n "$database" ]]; then
    psql "$LEGION_TEST_PG_DSN" -qtA -c "drop database if exists \"$database\" with (force)" >/dev/null 2>&1 ||
      printf '%s: could not drop database %s\n' "$me" "$database" >&2
  fi
  printf '%s: evidence kept in %s\n' "$me" "$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# isolated_exec KEEP NAME=VALUE... -- COMMAND...: replaces this subshell with COMMAND, run with no
# variable of this shell but PATH, the names KEEP lists and the assignments, which are exported
# (never put on a command line, which /proc shows every local user). It closes every session's
# stdin fifo first, so no child holds another session's input. Run it as `(isolated_exec ...) &`.
isolated_exec() {
  local keep=" PATH $1 " name f
  shift
  for f in "${fd[@]}"; do exec {f}>&-; done
  for name in $(compgen -e); do
    [[ "$keep" == *" ${name} "* ]] || export -n "${name?}"
  done
  while [[ "$1" != "--" ]]; do export "${1?}"; shift; done
  shift
  exec "$@"
}

wait_until() {
  local deadline=$((SECONDS + $1))
  shift
  while ((SECONDS < deadline)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.5
  done
  "$@" >/dev/null 2>&1
}
# await LABEL LOG SECONDS COMMAND...: wait_until, failing at once with LOG's tail when LABEL's
# process exits first.
await() {
  local label=$1 log=$2 limit=$3 deadline=$((SECONDS + $3))
  shift 3
  while ((SECONDS < deadline)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    running "${pid[$label]:-0}" || fail "$label exited: $(tail -n 5 "$log" | tr '\n' ' ')"
    sleep 0.5
  done
  "$@" >/dev/null 2>&1 || fail "timed out after ${limit}s waiting for $label: $(tail -n 5 "$log" | tr '\n' ' ')"
}
# ---- ports ------------------------------------------------------------------------------------
# Every server's port is held from the kernel's choice until it serves (lib/held-port.py), so no
# other process can take it in between. A server that restarts in a window serves on a copy of a
# keeper's socket, so the restart serves the same port. Between two servers a kept port still
# listens and nothing answers (the kernel queues the connection), so every poll of one carries a
# timeout (curl --max-time, read -t) and takes the timeout for down.

# await_port_file LABEL FILE VAR: waits (read_port_file, lib/port-file.sh) for FILE, the port file
# held-port.py writes for LABEL's process, and assigns VAR its port. The locals' names are ones no
# caller passes.
await_port_file() {
  local _apf_label=$1 _apf_file=$2 _apf_var=$3 _apf_status=0
  read_port_file "$_apf_file" "${pid[$_apf_label]}" "$_apf_var" || _apf_status=$?
  case $_apf_status in
  0) ;;
  1) fail "$_apf_label exited before it held a port: $(tail -n 5 "$work/logs/$_apf_label.log" | tr '\n' ' ')" ;;
  *) fail "timed out after ${port_file_seconds}s waiting for $_apf_label to hold a port" ;;
  esac
}
# keep_port LABEL VAR: the keeper LABEL (held-port.py keep), which holds one socket listening on
# 127.0.0.1 for the whole run and hands a copy to each server started with
# `held-port.py exec --from $work/ports/LABEL.sock`; assigns VAR its port.
keep_port() {
  rm -f "$work/ports/$1.port" "$work/ports/$1.sock"
  (isolated_exec "" -- python3 "$held_port" keep --port-file "$work/ports/$1.port" --socket "$work/ports/$1.sock") \
    >>"$work/logs/$1.log" 2>&1 &
  pid[$1]=$!
  await_port_file "$1" "$work/ports/$1.port" "$2"
}
# reported_port LOG PATTERN VAR: assigns VAR the port a process that bound port 0 itself reported,
# the digits right after the first match of the extended regular expression PATTERN in LOG. It
# fails while LOG holds no such line, so it is also the poll that waits for one.
reported_port() {
  local match
  match=$(grep -m1 -oE -- "$2[0-9]+" "$1" 2>/dev/null) || return 1
  printf -v "$3" '%s' "${match##*[!0-9]}"
}
# pick_port VAR: a loopback port nothing answers on, below the kernel's ephemeral range (which
# every connection's own local port comes from) and new to this run. Only for a server that can
# only bind a number it is given: a --build-from listener that takes no ENVOY_LISTEN_FD.
picked_ports=" "
pick_port() {
  local candidate i low
  read -r low _ </proc/sys/net/ipv4/ip_local_port_range
  ((low > 11000)) || fail "the kernel's ephemeral range starts at ${low}, leaving no ports below it above 10000"
  for ((i = 0; i < 100; i++)); do
    candidate=$((10000 + RANDOM % (low - 10000)))
    [[ "$picked_ports" != *" ${candidate} "* ]] || continue
    (exec 3<>"/dev/tcp/127.0.0.1/$candidate") 2>/dev/null && continue
    picked_ports+="${candidate} "
    printf -v "$1" '%s' "$candidate"
    return 0
  done
  fail "no free loopback port below ${low}"
}
new_uuid() { cat /proc/sys/kernel/random/uuid; }

# ---- builds -----------------------------------------------------------------------------------
begin build
tree=$repo_root
tree_label="this checkout ($(cd "$repo_root" && git rev-parse --short HEAD 2>/dev/null || echo unknown) plus the working copy)"
if [[ -n "$build_from" ]]; then
  rev=$(git -C "$repo_root" rev-parse --verify --quiet "${build_from}^{commit}") ||
    refuse "--build-from $build_from names no commit git can read"
  tree="$work/tree-${rev:0:12}"
  mkdir -p "$tree"
  git -C "$repo_root" archive "$rev" | tar -x -C "$tree" || fail "git archive $rev | tar -x failed"
  tree_label="$rev (git archive into $tree)"
fi
readonly tree
# The plugins come from the same tree, unless --plugins-from names another: the mixed rollout,
# this checkout's listeners with the plugins of an earlier release.
plugins_tree=$tree
plugins_label=$tree_label
if [[ -n "$plugins_from" ]]; then
  rev=$(git -C "$repo_root" rev-parse --verify --quiet "${plugins_from}^{commit}") ||
    refuse "--plugins-from $plugins_from names no commit git can read"
  plugins_tree="$work/plugins-tree-${rev:0:12}"
  mkdir -p "$plugins_tree"
  git -C "$repo_root" archive "$rev" | tar -x -C "$plugins_tree" ||
    fail "git archive $rev | tar -x failed"
  plugins_label="$rev (git archive into $plugins_tree)"
fi
readonly plugins_tree
note "listener and the bridge from ${tree_label}; pi-envoy and claude-envoy from ${plugins_label}; natsd and Dispatch from this checkout"
(cd "$repo_root/packages/envoy" &&
  go build -o "$work/bin/natsd" ./internal/testnats/cmd/natsd &&
  go build -o "$work/bin/envoy-dispatch" ./cmd/dispatch) >"$work/logs/build-head.log" 2>&1 ||
  fail "natsd or Dispatch did not build; see $work/logs/build-head.log"
(cd "$tree/packages/envoy" && go build -o "$work/bin/envoy-listener" ./cmd/listener &&
  cd "$tree/packages/daemon" && go build -o "$work/bin/legion" ./cmd/legion) >"$work/logs/build-tree.log" 2>&1 ||
  fail "the listener or legion did not build from $tree; see $work/logs/build-tree.log"
(cd "$tree" && bun install --frozen-lockfile) >"$work/logs/bun-install.log" 2>&1 ||
  fail "bun install in $tree failed; see $work/logs/bun-install.log"
if [[ "$plugins_tree" != "$tree" ]]; then
  (cd "$plugins_tree" && bun install --frozen-lockfile) >"$work/logs/bun-install-plugins.log" 2>&1 ||
    fail "bun install in $plugins_tree failed; see $work/logs/bun-install-plugins.log"
fi
# pi-envoy packed as the release packs it (that tree's lib/pack-plugin.sh) and unpacked as the
# worker image unpacks it, so `-e` loads what a Legion pane loads.
tarball=$(bash "$plugins_tree/scripts/e2e/lib/pack-plugin.sh" pi-envoy "$work/pack" 2>"$work/logs/pack-pi-envoy.log") ||
  fail "pi-envoy did not pack; see $work/logs/pack-pi-envoy.log"
mkdir -p "$work/pi-envoy"
tar xzf "$tarball" -C "$work/pi-envoy" --strip-components=1
pi_plugin="$work/pi-envoy/dist/envoy.js"
[ -r "$pi_plugin" ] || fail "the packed pi-envoy holds no dist/envoy.js"
# claude-envoy built from the plugins tree (`scripts/build.ts --out`), since a pull request's tree
# carries no fresh dist/ or legion/; a revision whose build.ts takes no --out predates that and
# committed both, so its own package directory is the plugin.
if grep -q -- '--out <dir>' "$plugins_tree/packages/claude-envoy/scripts/build.ts"; then
  claude_plugin="$work/claude-envoy"
  (cd "$plugins_tree" && bun packages/claude-envoy/scripts/build.ts --out "$claude_plugin") >"$work/logs/build-claude-envoy.log" 2>&1 ||
    fail "claude-envoy did not build from $plugins_tree; see $work/logs/build-claude-envoy.log"
else
  claude_plugin="$plugins_tree/packages/claude-envoy"
fi
[ -r "$claude_plugin/dist/envoy-channel.js" ] || fail "$claude_plugin has no dist/envoy-channel.js"
# use_plugins PI_JS CLAUDE_DIR: what the next start_pi and start_cc load. The Claude Code plugin
# directories and the MCP config name the directory, so both follow it.
use_plugins() {
  pi_plugin=$1
  claude_plugin=$2
  plugin_dirs=(--plugin-dir "$claude_plugin")
  [ -d "$claude_plugin/legion" ] && plugin_dirs+=(--plugin-dir "$claude_plugin/legion")
  return 0
}
# --resume-plugins-from: the sessions start on <rev>'s plugins and are resumed on this run's, so
# one session's own life crosses the release. Both sets are built before anything starts, since a
# build between the SIGKILL and the resume would hold the sessions down for minutes.
pi_plugin_after=$pi_plugin
claude_plugin_after=$claude_plugin
upgrade_label=""
if [[ -n "$resume_plugins_from" ]]; then
  before_rev=$(git -C "$repo_root" rev-parse --verify --quiet "${resume_plugins_from}^{commit}") ||
    refuse "--resume-plugins-from $resume_plugins_from names no commit git can read"
  before_tree="$work/plugins-before-${before_rev:0:12}"
  mkdir -p "$before_tree"
  git -C "$repo_root" archive "$before_rev" | tar -x -C "$before_tree" ||
    fail "git archive $before_rev | tar -x failed"
  (cd "$before_tree" && bun install --frozen-lockfile) >"$work/logs/bun-install-before.log" 2>&1 ||
    fail "bun install in $before_tree failed; see $work/logs/bun-install-before.log"
  before_tarball=$(bash "$before_tree/scripts/e2e/lib/pack-plugin.sh" pi-envoy "$work/pack-before" 2>"$work/logs/pack-pi-envoy-before.log") ||
    fail "pi-envoy did not pack from $before_tree; see $work/logs/pack-pi-envoy-before.log"
  mkdir -p "$work/pi-envoy-before"
  tar xzf "$before_tarball" -C "$work/pi-envoy-before" --strip-components=1
  [ -r "$work/pi-envoy-before/dist/envoy.js" ] || fail "the packed pi-envoy of $before_rev holds no dist/envoy.js"
  if grep -q -- '--out <dir>' "$before_tree/packages/claude-envoy/scripts/build.ts"; then
    claude_plugin_before="$work/claude-envoy-before"
    (cd "$before_tree" && bun packages/claude-envoy/scripts/build.ts --out "$claude_plugin_before") >"$work/logs/build-claude-envoy-before.log" 2>&1 ||
      fail "claude-envoy did not build from $before_tree; see $work/logs/build-claude-envoy-before.log"
  else
    claude_plugin_before="$before_tree/packages/claude-envoy"
  fi
  [ -r "$claude_plugin_before/dist/envoy-channel.js" ] || fail "$claude_plugin_before has no dist/envoy-channel.js"
  use_plugins "$work/pi-envoy-before/dist/envoy.js" "$claude_plugin_before"
  upgrade_label="pi-envoy $(jq -r .version "$work/pi-envoy-before/package.json") and claude-envoy $(jq -r .version "$claude_plugin_before/package.json") from ${before_rev:0:12} until the resume of window a, then this run's"
fi
# The bridge argv Legion builds: claudelaunch.Argv since main moved it there, tmux/claude.go before.
replay_flag=()
for argv_source in packages/daemon/internal/claudelaunch/claudelaunch.go packages/daemon/internal/runtime/tmux/claude.go; do
  if [ -r "$tree/$argv_source" ] && grep -q -- '--replay-user-messages' "$tree/$argv_source"; then
    replay_flag=(--replay-user-messages)
  fi
done
use_plugins "$pi_plugin" "$claude_plugin"
printf '#!/bin/sh\nexec bunx --package @anthropic-ai/claude-code@%s claude "$@"\n' "$claude_version" >"$work/bin/claude"
chmod +x "$work/bin/claude"
bun_bin=$(command -v bun)
claude_reported=$("$work/bin/claude" --version 2>"$work/logs/claude-version.log") ||
  fail "bunx could not run Claude Code $claude_version; see $work/logs/claude-version.log"
# A listener that takes ENVOY_LISTEN_FD serves on a keeper's socket; an earlier revision's binds the
# number it is given (PORT), so it gets a picked one.
listener_held=0
if grep -rqs --include='*.go' --exclude='*_test.go' ENVOY_LISTEN_FD "$tree/packages/envoy/cmd/listener"; then listener_held=1; fi
listener_ports="serve on kept ports (ENVOY_LISTEN_FD)"
((listener_held == 1)) || listener_ports="take no ENVOY_LISTEN_FD: each binds a picked port, again on its restart"
note "pi-envoy $(jq -r .version "$work/pi-envoy/package.json"); claude-envoy $(jq -r .version "$work/claude-envoy/package.json" 2>/dev/null || jq -r .version "$claude_plugin_after/package.json"); Claude Code ${claude_reported}; omp $("$omp" --version 2>/dev/null | head -n 1); bridge argv ${replay_flag[*]:-without --replay-user-messages}; the listeners ${listener_ports}"
[[ -z "$upgrade_label" ]] || note "upgrade: the sessions start on ${upgrade_label}"

# ---- services ---------------------------------------------------------------------------------
begin services
# natsd binds the ports the kernel chooses (-p -1, -m -1) and reports both in its log.
(isolated_exec "" HOME="$work/nats-home" -- "$work/bin/natsd" -p -1 -m -1 -js -store_dir "$work/nats-store") \
  >"$work/logs/natsd.log" 2>&1 &
pid[natsd]=$!
nats_port="" monitor_port=""
await natsd "$work/logs/natsd.log" 30 reported_port "$work/logs/natsd.log" 'Listening for client connections on [^ ]*:' nats_port
await natsd "$work/logs/natsd.log" 30 reported_port "$work/logs/natsd.log" 'Starting http monitor on [^ ]*:' monitor_port
nats_url="nats://127.0.0.1:${nats_port}"
nats_monitor="http://127.0.0.1:${monitor_port}"
nats_ready() { curl -fsS --max-time 2 "${nats_monitor}/healthz" | jq -e '.status == "ok"'; }
await natsd "$work/logs/natsd.log" 30 nats_ready

# Each listener, Dispatch and each proxy restarts in a window, and the sessions' envoy.json and
# Dispatch's environment name their ports before they first start: each serves on a keeper's port.
declare -A listener_port=() proxy_port=()
for name in shared dispatch; do
  if ((listener_held == 1)); then
    keep_port "keep-listener-$name" "listener_port[$name]"
  else
    pick_port "listener_port[$name]"
  fi
done
for label in proxy-pi proxy-cc proxy-dispatch; do keep_port "keep-$label" "proxy_port[$label]"; done
dispatch_port=""
keep_port keep-dispatch dispatch_port
listener_url() { printf 'http://127.0.0.1:%s' "${listener_port[$1]}"; }
dispatch_url="http://127.0.0.1:${dispatch_port}"

listener_ready() { curl -fsS --max-time 2 "$(listener_url "$1")/healthz" | jq -e '.status == "healthy"'; }
start_listener() {
  local name=$1
  local -a serve=("$work/bin/envoy-listener")
  if ((listener_held == 1)); then
    serve=(python3 "$held_port" exec --from "$work/ports/keep-listener-$name.sock" --fd-env ENVOY_LISTEN_FD -- "${serve[@]}")
  fi
  (isolated_exec "" HOME="$work/listener-$name-home" PORT="${listener_port[$name]}" ENVOY_LISTEN_HOST=127.0.0.1 \
    ENVOY_MACHINE_ID="$name" NATS_URLS="$nats_url" -- "${serve[@]}") \
    >>"$work/logs/listener-$name.log" 2>&1 &
  pid[listener-$name]=$!
  await "listener-$name" "$work/logs/listener-$name.log" 90 listener_ready "$name"
  note "listener $name (pid ${pid[listener-$name]}) healthy at $(listener_url "$name")"
}
start_listener shared
start_listener dispatch

# proxy_ready PORT: a connection to PORT reads natsd's INFO line within 2 s, so the proxy there
# accepts and forwards (a kept port with no proxy accepts the connection and sends nothing).
proxy_ready() {
  local word=""
  (exec 3<>"/dev/tcp/127.0.0.1/$1" && read -r -t 2 -N 4 word <&3 && [[ "$word" == INFO ]]) 2>/dev/null
}
# start_proxy LABEL: socat forwarding to natsd from the socket the keeper keep-LABEL holds
# (ACCEPT-FD; held-port.py names the descriptor PROXY_LISTEN_FD, which socat does not read), one
# fork per connection, in a process group of its own (setsid and held-port.py exec in place), so
# one SIGKILL to the group cuts every connection through it and the keeper keeps the port.
start_proxy() {
  local label=$1 group
  (isolated_exec "" -- setsid python3 "$held_port" exec --from "$work/ports/keep-$label.sock" --fd-env PROXY_LISTEN_FD -- \
    socat "ACCEPT-FD:3,fork" "TCP:127.0.0.1:${nats_port}") >>"$work/logs/$label.log" 2>&1 &
  pid[$label]=$!
  await "$label" "$work/logs/$label.log" 10 proxy_ready "${proxy_port[$label]}"
  read -r _ _ _ _ group _ <"/proc/${pid[$label]}/stat"
  [[ "$group" == "${pid[$label]}" ]] || fail "$label (pid ${pid[$label]}) is not in a process group of its own"
}
start_proxy proxy-pi
start_proxy proxy-cc

database="session_delivery_$$_$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')"
psql "$LEGION_TEST_PG_DSN" -qtA -c "create database \"$database\"" >/dev/null || fail "could not create database $database in LEGION_TEST_PG_DSN"
database_url="${pg_server}/${database}${pg_params}"
agent_token=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
signing_key=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
mkdir -p "$work/web-dist"
dispatch_ready() { curl -fsS --max-time 2 "${dispatch_url}/healthz" | jq -e '.ok and .nats'; }
start_dispatch() {
  (isolated_exec "" HOME=/nonexistent XDG_CONFIG_HOME=/nonexistent XDG_DATA_HOME=/nonexistent \
    DATABASE_URL="$database_url" DISPATCH_AGENT_TOKEN="$agent_token" DISPATCH_IDENTITY=header:X-Dispatch-User \
    DISPATCH_IDENTITY_HEADER_TRUSTED=1 DISPATCH_LISTEN_HOST=127.0.0.1 DISPATCH_PORT="$dispatch_port" \
    DISPATCH_SERVER_URL="$dispatch_url" DISPATCH_SIGNING_KEY="$signing_key" DISPATCH_WEB_DIST="$work/web-dist" \
    ENVOY_URL="$(listener_url dispatch)" NATS_URLS="nats://127.0.0.1:${proxy_port[proxy-dispatch]}" -- \
    python3 "$held_port" exec --from "$work/ports/keep-dispatch.sock" --fd-env DISPATCH_LISTEN_FD -- "$work/bin/envoy-dispatch") \
    >>"$work/logs/dispatch.log" 2>&1 &
  pid[dispatch]=$!
  await dispatch "$work/logs/dispatch.log" 120 dispatch_ready
  note "Dispatch (pid ${pid[dispatch]}) serving ${dispatch_url}, with NATS"
}
start_proxy proxy-dispatch
start_dispatch

# dispatch_user METHOD PATH [BODY]: a request as the person live-delivery (the trusted header).
dispatch_user() {
  curl -sS --max-time 20 -X "$1" -H 'Content-Type: application/json' -H 'X-Dispatch-User: live-delivery' \
    ${3:+-d "$3"} -w '\n%{http_code}' "${dispatch_url}$2"
}
# dispatch_agent METHOD PATH BODY: a request as the session live-delivery-driver (the agent token).
dispatch_agent() {
  curl -sS --max-time 20 -X "$1" -H 'Content-Type: application/json' -H "Authorization: Bearer ${agent_token}" \
    -d "$(jq -c '. + {actor: {kind: "session", id: "live-delivery-driver"}}' <<<"$3")" -w '\n%{http_code}' "${dispatch_url}$2"
}
answer_ok() { [[ "${1##*$'\n'}" == 2?? ]]; }
answer_body() { printf '%s' "${1%$'\n'*}"; }

answer=$(dispatch_user POST /api/v1/projects '{"key":"LIVE","name":"Live delivery"}')
answer_ok "$answer" || fail "Dispatch refused the project: $answer"
answer=$(dispatch_user POST /api/v1/issues '{"project":"LIVE","title":"The followed issue"}')
answer_ok "$answer" || fail "Dispatch refused the followed issue: $answer"
followed_issue=$(answer_body "$answer" | jq -r .key)
answer=$(dispatch_user POST /api/v1/issues '{"project":"LIVE","title":"Direct messages"}')
answer_ok "$answer" || fail "Dispatch refused the messages issue: $answer"
message_issue=$(answer_body "$answer" | jq -r .key)
issue_topic="notifications.dispatch.issue.${followed_issue}.>"
note "Dispatch issues: ${followed_issue} (followed, ${issue_topic}) and ${message_issue} (direct messages)"
second_topic=""
second_issue=""
if [[ -n "$second_followed_issue" ]]; then
  # force: its title overlaps the first followed issue's, which Dispatch's duplicate check refuses
  # with POSSIBLE_DUPLICATE (internal/dispatch/api/issue_create.go); the overlap is deliberate here.
  answer=$(dispatch_user POST /api/v1/issues '{"project":"LIVE","title":"The second followed issue","force":true}')
  answer_ok "$answer" || fail "Dispatch refused the second followed issue: $answer"
  second_issue=$(answer_body "$answer" | jq -r .key)
  second_topic="notifications.dispatch.issue.${second_issue}.>"
  note "second followed issue ${second_issue} (${second_topic}): both sessions follow two issue topics and a role"
fi

# ---- the sessions -----------------------------------------------------------------------------
declare -A sid=() transcript=() launches=() role=() home=()
run_id="$(od -An -N2 -tx1 /dev/urandom | tr -d ' \n')"
role[pi]="live-delivery-pi-${run_id}"
role[cc]="live-delivery-cc-${run_id}"
# The one team topic both sessions follow. notifications.team.> is a stream subject, so a message
# published to it while a session is down waits in that session's own consumer until it returns.
readonly team_topic="notifications.team.live-delivery-${run_id}"
note "the run's team topic is ${team_topic}; the setup prompt makes both sessions follow it"
readonly pi_note="This session is an automated delivery test. Do exactly what the user's first message says, using the Envoy tools it names. After that, answer every message and every Envoy card with the single word ok, and call no tools."
readonly cc_note="This session is an automated delivery test. Do exactly what the user's first message says, using the Envoy tools it names. After that, answer every message with exactly one line, WAITING for test events, and call no tools."
natives=$HOME/.omp/natives
if [ -n "${XDG_DATA_HOME:-}" ] && [ -d "$XDG_DATA_HOME/omp" ]; then natives=$XDG_DATA_HOME/omp/natives; fi

prepare_session() {
  local label=$1
  home[$label]="$work/sessions/$label"
  mkdir -p "${home[$label]}/home/.config/legion" "${home[$label]}/home/.omp" "${home[$label]}/cwd" \
    "${home[$label]}/sessions" "${home[$label]}/tmp" "${home[$label]}/config"
  local config=${home[$label]}/home/.config/legion/envoy.json
  [[ "$label" == pi ]] || config=${home[$label]}/config/envoy.json
  jq -n --arg url "$(listener_url shared)" --arg nats "nats://127.0.0.1:${proxy_port[proxy-$label]}" \
    '{envoy: {url: $url, natsUrls: [$nats]}, dispatch: {enabled: false}}' >"$config"
  launches[$label]=0
}
prepare_session pi
prepare_session cc
[ -d "$natives" ] && ln -sfT "$natives" "${home[pi]}/home/.omp/natives"
# write_mcp_config: the one MCP server Claude Code loads, naming the claude-envoy the run uses now
# (use_plugins). The upgrade rewrites it before the resume, so the resumed Claude Code loads the
# new plugin's channel server rather than the one it started on.
write_mcp_config() {
  jq -n --arg bun "$bun_bin" --arg entry "$claude_plugin/dist/envoy-channel.js" --arg data "${home[cc]}/config/envoy-plugin-data" \
    '{mcpServers: {legion: {command: $bun, args: [$entry], env: {CLAUDE_PLUGIN_DATA: $data}}}}' >"${home[cc]}/config/mcp.json"
  chmod 600 "${home[cc]}/config/mcp.json"
}
write_mcp_config

rpc_send() { printf '%s\n' "$2" >&"${fd[$1]}"; }
rpc_log() { printf '%s/rpc-%s.log' "${home[$1]}" "${launches[$1]}"; }
err_log() { printf '%s/stderr-%s.log' "${home[$1]}" "${launches[$1]}"; }
rpc_frame() {
  jq -ecnR --arg type "$2" --arg id "$3" 'first(inputs | fromjson? | select(.type == $type and .id == $id))' "$(rpc_log "$1")"
}
open_stdin() {
  local label=$1 f
  rm -f "${home[$label]}/stdin"
  mkfifo "${home[$label]}/stdin"
  exec {f}<>"${home[$label]}/stdin"
  fd[$label]=$f
}

# start_pi [RESUME_FILE]: omp in rpc mode with this run's pi-envoy as its only extension, in a
# process group of its own; sets sid[pi] and transcript[pi] from its get_state answer.
start_pi() {
  local resume=${1:-}
  launches[pi]=$((launches[pi] + 1))
  open_stdin pi
  local -a args=(--mode rpc --no-ui --model "anthropic/$model" --thinking off --cwd "${home[pi]}/cwd"
    --session-dir "${home[pi]}/sessions" --no-extensions --no-skills --no-rules --no-tools --no-lsp
    --no-title --append-system-prompt "$pi_note" -e "$pi_plugin")
  [[ -z "$resume" ]] || args+=(--resume "$resume")
  (isolated_exec ANTHROPIC_BASE_URL HOME="${home[pi]}/home" TMPDIR="${home[pi]}/tmp" OMP_SESSION_STORAGE=file \
    ENVOY_HEARTBEAT_MS="$heartbeat_ms" ANTHROPIC_API_KEY="$anthropic_key" -- setsid "$omp" "${args[@]}") \
    <"${home[pi]}/stdin" >"$(rpc_log pi)" 2>"$(err_log pi)" &
  pid[pi]=$!
  rpc_send pi "{\"type\":\"get_state\",\"id\":\"state-${launches[pi]}\"}"
  await pi "$(err_log pi)" 90 rpc_frame pi response "state-${launches[pi]}"
  local state
  state=$(rpc_frame pi response "state-${launches[pi]}")
  sid[pi]=$(jq -r '.data.sessionId' <<<"$state")
  transcript[pi]=$(jq -r '.data.sessionFile' <<<"$state")
  note "omp (pid ${pid[pi]}) session ${sid[pi]}${resume:+, resumed from ${resume}}"
}

# start_cc fresh|resume: the bridge around Claude Code, in a process group of its own, with the
# argv claude.go builds; Claude Code is told its session id, so sid[cc] is ours.
start_cc() {
  local how=$1 flag=--session-id
  [[ "$how" == fresh ]] || flag=--resume
  [[ "$how" != fresh ]] || sid[cc]=$(new_uuid)
  launches[cc]=$((launches[cc] + 1))
  open_stdin cc
  local keep="ANTHROPIC_BASE_URL BUN_INSTALL_CACHE_DIR"
  (cd "${home[cc]}/cwd" && isolated_exec "$keep" HOME="${home[cc]}/home" TMPDIR="${home[cc]}/tmp" CLAUDE_CONFIG_DIR="${home[cc]}/config" \
    CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1 CLAUDE_CODE_DISABLE_CRON=1 LEGION_ROLE=implementer \
    LEGION_ISSUE="$followed_issue" ENVOY_HEARTBEAT_MS="$heartbeat_ms" ANTHROPIC_API_KEY="$anthropic_key" -- \
    setsid "$work/bin/legion" claude-bridge -- "$work/bin/claude" -p --input-format stream-json \
    --output-format stream-json --verbose "${replay_flag[@]}" --permission-mode bypassPermissions \
    --strict-mcp-config --mcp-config "${home[cc]}/config/mcp.json" "${plugin_dirs[@]}" \
    "$flag" "${sid[cc]}" --model "$model" --append-system-prompt "$cc_note") \
    <"${home[cc]}/stdin" >"$(rpc_log cc)" 2>"$(err_log cc)" &
  pid[cc]=$!
  note "legion claude-bridge (pid ${pid[cc]}) for Claude Code session ${sid[cc]} (${flag})"
}
cc_transcript() {
  local f
  for f in "${home[cc]}/config/projects"/*/"${sid[cc]}.jsonl"; do
    [[ -f "$f" ]] && { printf '%s\n' "$f"; return 0; }
  done
  return 1
}

registered() { curl -fsS --max-time 5 "$(listener_url shared)/v1/registry/$1" >/dev/null; }
follows_and_holds() {
  local label=$1
  curl -fsS --max-time 5 "$(listener_url shared)/v1/interests/${sid[$label]}" |
    jq -e --args '(.topics // []) as $t | all($ARGS.positional[]; . as $want | $t | index($want) != null)' "$issue_topic" "$team_topic" ${second_topic:+"$second_topic"} >/dev/null &&
    curl -fsS --max-time 5 "$(listener_url shared)/v1/roles/${role[$label]}" |
    jq -e --arg s "${sid[$label]}" '.holder == $s' >/dev/null
}
session_view() {
  printf 'entry %s; interest %s; role %s' \
    "$(curl -fsS --max-time 5 "$(listener_url shared)/v1/sessions" | jq -c --arg s "${sid[$1]}" 'first(.[] | select(.session_id == $s))' 2>/dev/null)" \
    "$(curl -fsS --max-time 5 "$(listener_url shared)/v1/interests/${sid[$1]}" | jq -c '.topics' 2>/dev/null)" \
    "$(curl -fsS --max-time 5 "$(listener_url shared)/v1/roles/${role[$1]}" | jq -c . 2>/dev/null)"
}

# session_delivery_shown WHEN: both sessions take their messages from a session consumer of their
# own, printed under WHEN and a failure when it is untrue. The listener reports `delivery` on every
# row of GET /v1/sessions, written always and never omitempty: "consumer" for a session its own
# consumer holds the stored messages of, empty for one no consumer was given. A registration that
# can be given no consumer is refused before anything is written, so a registered session that does
# not read "consumer" is one this run would otherwise count deliveries for without proving which
# path carried them.
session_delivery_shown() {
  local when=$1 label entries
  entries=$(curl -fsS --max-time 5 "$(listener_url shared)/v1/sessions" |
    jq -c --arg pi "${sid[pi]}" --arg cc "${sid[cc]}" \
      '[.[] | select(.session_id == $pi or .session_id == $cc) | {session_id, delivery}]') ||
    fail "could not read the session entries ($when)"
  note "session delivery ($when): ${entries}"
  for label in pi cc; do
    jq -e --arg s "${sid[$label]}" 'any(.[]; .session_id == $s and .delivery == "consumer")' >/dev/null <<<"$entries" ||
      fail "session $label (${sid[$label]}) does not report consumer delivery ($when): ${entries}"
  done
}

begin sessions
start_pi
start_cc fresh
setup_prompt() {
  local label=$1 subscribe=envoy_subscribe role_set=envoy_role_set
  [[ "$label" == pi ]] || { subscribe=mcp__legion__envoy_subscribe; role_set=mcp__legion__envoy_role_set; }
  local topics
  topics=$(jq -nc --args '$ARGS.positional' "$issue_topic" "$team_topic" ${second_topic:+"$second_topic"})
  jq -nc --arg m "Call ${subscribe} with topics ${topics}. Then call ${role_set} with role \"${role[$label]}\". Then reply with the single word ok." \
    '{type: "prompt", id: "setup", message: $m}'
}
rpc_send pi "$(setup_prompt pi)"
rpc_send cc "$(setup_prompt cc)"
await pi "$(err_log pi)" 240 follows_and_holds pi
note "pi: $(session_view pi)"
await cc "$(err_log cc)" 300 follows_and_holds cc
transcript[cc]=$(cc_transcript) || fail "Claude Code wrote no transcript for ${sid[cc]} under ${home[cc]}/config/projects"
note "cc: $(session_view cc); transcript ${transcript[cc]}"
session_delivery_shown "both sessions registered"

# ---- publishing -------------------------------------------------------------------------------
seq=0
marker=""
next_marker() {
  seq=$((seq + 1))
  printf -v marker 'LDT%s-%03dZ' "$run_id" "$seq"
}
# record DISRUPTION KIND SOURCE SESSIONS EXPECTED ANSWER: one line of events.jsonl for the marker.
record() {
  jq -nc --arg marker "$marker" --arg d "$1" --arg kind "$2" --arg source "$3" --arg sessions "$4" \
    --argjson expected "$5" --arg answer "$6" --arg at "$(date -u +%H:%M:%S.%3NZ)" \
    '{marker: $marker, disruption: $d, kind: $kind, source: $source, sessions: ($sessions | split(" ")),
      expected: $expected, answer: ($answer | .[0:2000]), at: $at}' >>"$events"
}
listener_post() {
  curl -sS --max-time 20 -X POST -H 'Content-Type: application/json' -d "$3" -w '\n%{http_code}' "$(listener_url "$1")$2" 2>&1 || true
}
readonly core_publish_js='
import { connect, StringCodec } from "nats";
const { LIVE_NATS_URL, LIVE_SUBJECT, LIVE_ENVELOPE } = process.env;
const nc = await connect({ servers: LIVE_NATS_URL, name: "envoy-session-delivery-publisher", timeout: 5000 });
nc.publish(LIVE_SUBJECT, StringCodec().encode(LIVE_ENVELOPE));
await nc.flush();
await nc.close();
console.log("published");
'
text_for() { printf 'live delivery event %s (disruption %s, %s via %s)' "$marker" "$1" "$2" "$3"; }
# stored_comments: one JSON line per message the stream holds on the followed issue's subjects,
# {seq, stored, marker}, read with an ordered consumer from the stream's start.
readonly stored_js='
import { connect, StringCodec } from "nats";
const { LIVE_NATS_URL, LIVE_SUBJECT } = process.env;
const nc = await connect({ servers: LIVE_NATS_URL, name: "envoy-session-delivery-reader", timeout: 5000 });
const consumer = await nc.jetstream().consumers.get("ENVOY_NOTIFICATIONS", { filterSubjects: [LIVE_SUBJECT] });
const messages = await consumer.fetch({ max_messages: 1000, expires: 3000 });
for await (const m of messages) {
  const marker = (StringCodec().decode(m.data).match(/LDT[0-9a-f]+-[0-9]{3}Z/) ?? [""])[0];
  const stored = new Date(Math.floor(m.info.timestampNanos / 1e6)).toISOString();
  console.log(JSON.stringify({ seq: m.seq, stored, marker }));
  if (m.info.pending === 0) break;
}
await nc.close();
'
stored_comments() {
  (cd -- "$repo_root" && isolated_exec "" HOME="$work/bun-home" TMPDIR="$work" LIVE_NATS_URL="$nats_url" \
    LIVE_SUBJECT="$issue_topic" -- bun -e "$stored_js") 2>>"$work/logs/stored-comments.log"
}

# The window's events, in this order: Dispatch's writes (when Dispatch is up), the listener sends,
# the role publishes, then the team publish. publish_dispatch and publish_rest split them for
# window e.
publish_dispatch() {
  local d=$1 label answer text body expected n
  for n in 1 2; do
    next_marker
    text=$(text_for "$d" issue "a comment on ${followed_issue}")
    answer=$(dispatch_user POST "/api/v1/issues/${followed_issue}/comments" "$(jq -nc --arg b "$text" '{body: $b}')" 2>&1 || true)
    expected=1
    answer_ok "$answer" || expected=0
    record "$d" issue "comment-$n" "pi cc" "$expected" "$answer"
  done
  if [[ -n "$second_issue" ]]; then
    next_marker
    text=$(text_for "$d" issue "a comment on ${second_issue}")
    answer=$(dispatch_user POST "/api/v1/issues/${second_issue}/comments" "$(jq -nc --arg b "$text" '{body: $b}')" 2>&1 || true)
    expected=1
    answer_ok "$answer" || expected=0
    record "$d" issue "second-comment" "pi cc" "$expected" "$answer"
  fi
  for label in pi cc; do
    next_marker
    text=$(text_for "$d" agent "a Dispatch message")
    body=$(jq -nc --arg b "$text" --arg t "session:${sid[$label]}" '{body: $b, target: $t, delivery: "aside"}')
    answer=$(dispatch_agent POST "/api/v1/issues/${message_issue}/messages" "$body" 2>&1 || true)
    # A message Dispatch reports it could not send was never published: expected 0.
    expected=1
    if ! answer_ok "$answer" || answer_body "$answer" |
      jq -e '(.deliveries // []) | last | . != null and .state == "failed" and .envelope_id == null' >/dev/null 2>&1; then
      expected=0
    fi
    record "$d" agent dispatch-message "$label" "$expected" "$answer"
  done
}
publish_rest() {
  local d=$1 via=$2 label answer text envelope
  for label in pi cc; do
    next_marker
    text=$(text_for "$d" agent "POST /v1/messages/send to listener $via")
    answer=$(listener_post "$via" /v1/messages/send "$(jq -nc --arg s "${sid[$label]}" --arg m "$text" \
      '{target_session: $s, source: "agent", source_session: "envoy-session-delivery", message: $m}')")
    record "$d" agent listener-send "$label" 1 "$answer"
  done
  for label in pi cc; do
    next_marker
    text=$(text_for "$d" role "POST /v1/messages/publish to listener $via")
    answer=$(listener_post "$via" /v1/messages/publish "$(jq -nc --arg t "notifications.role.${role[$label]}" --arg m "$text" \
      '{topic: $t, source: "agent", source_session: "envoy-session-delivery", message: $m}')")
    record "$d" role listener-publish "$label" 1 "$answer"
  done
  for label in pi cc; do
    next_marker
    text=$(text_for "$d" role "a core publish on notifications.role.${role[$label]}")
    local id
    id=$(new_uuid)
    envelope=$(jq -nc --arg id "$id" --arg src "$marker" --arg topic "notifications.role.${role[$label]}" \
      --argjson at "$(date +%s%3N)" --arg summary "$text" \
      '{event_id: $id, source: "agent", source_event_id: $src, topic: $topic,
        dedupe_key: ("envoy-session-delivery-" + $id), issued_at: $at, payload_summary: $summary, trace_id: $id}')
    answer=$( (cd -- "$repo_root" && isolated_exec "" HOME="$work/bun-home" TMPDIR="$work" LIVE_NATS_URL="$nats_url" \
      LIVE_SUBJECT="notifications.role.${role[$label]}" LIVE_ENVELOPE="$envelope" -- bun -e "$core_publish_js") 2>&1) || true
    record "$d" role core-publish "$label" 1 "$answer"
  done
  next_marker
  text=$(text_for "$d" team "POST /v1/messages/publish to listener $via")
  answer=$(listener_post "$via" /v1/messages/publish "$(jq -nc --arg t "$team_topic" --arg m "$text" \
    '{topic: $t, source: "agent", source_session: "envoy-session-delivery", message: $m}')")
  record "$d" team team-publish "pi cc" 1 "$answer"
}

# ---- counting ---------------------------------------------------------------------------------
# The markers each transcript holds, one `<marker> <line>` per entry that shows the session the
# event: Oh My Pi's envoy-message cards and user messages; Claude Code's user messages (not tool
# results or meta lines) and the prompts it queued mid-turn. A marker twice in one entry counts once.
# shellcheck disable=SC2016 # jq's variables, not the shell's
readonly marker_scan='[scan("LDT[0-9a-f]+-[0-9]{3}Z")] | unique | .[] as $m | "\($m) \(input_line_number)"'
# shellcheck disable=SC2016 # jq's variables, not the shell's
readonly pi_entries='def text: if type == "string" then . else (map(.text? // empty) | join("\n")) end;
  inputs | fromjson? | select((.type == "custom_message" and .customType == "envoy-message")
    or (.type == "message" and .message.role == "user")) | (.content // .message.content | text)'
readonly cc_entries='def text: if type == "string" then . else (map(select(.type? == "text") | .text) | join("\n")) end;
  inputs | fromjson? | select((.type == "user" and (.isMeta | not) and (.message.role == "user"))
    or (.type == "attachment" and .attachment.type == "queued_command"))
  | (if .type == "user" then .message.content else .attachment.prompt end | text)'
markers_in() {
  local label=$1 file=${transcript[$1]} filter=$pi_entries
  [[ "$label" == pi ]] || filter=$cc_entries
  [[ -f "$file" ]] || return 0
  jq -rnR "$filter | $marker_scan" "$file" 2>/dev/null || true
}
declare -A count=() first_line=()
tally() {
  local label m line
  count=()
  first_line=()
  for label in pi cc; do
    while read -r m line; do
      [[ -n "$m" ]] || continue
      count[$label:$m]=$((${count[$label:$m]:-0} + 1))
      [[ -n "${first_line[$label:$m]:-}" ]] || first_line[$label:$m]=$line
    done < <(markers_in "$label")
  done
}
# window_shown D: every event of window D expected once has reached each of its sessions.
window_shown() {
  local m s
  tally
  while read -r m s; do
    ((${count[$s:$m]:-0} >= 1)) || return 1
  done < <(jq -r --arg d "$1" 'select(.disruption == $d and .expected == 1) | .marker as $m | .sessions[] | "\($m) \(.)"' "$events")
}
# settle D: up to settle_s for window D's events, plus, for window e, the outbox's one-minute claim
# window, which an event the killed Dispatch had claimed waits out before the new one publishes it.
settle() {
  local d=$1 limit=$settle_s
  [[ "$d" != e ]] || limit=$((settle_s + 60))
  local deadline=$((SECONDS + limit))
  while ((SECONDS < deadline)); do
    window_shown "$d" && break
    sleep 3
  done
  if window_shown "$d"; then
    note "window $d: every event shown; holding ${hold_s}s for a repeat"
  else
    note "window $d: not every event shown after ${limit}s; holding ${hold_s}s more"
  fi
  sleep "$hold_s"
}

# ---- the disruptions --------------------------------------------------------------------------
connz_has() {
  curl -fsS --max-time 5 "${nats_monitor}/connz?subs=1&limit=1024" |
    jq -e --arg a "notifications.agent.$1" --arg b "notifications.envoy.deliver.$1" \
      'any(.connections[]; (.subscriptions_list // []) | (index($a) != null or index($b) != null))' >/dev/null
}
sessions_cut() { ! connz_has "${sid[pi]}" && ! connz_has "${sid[cc]}"; }

declare -A disruption_name=([a]=session-restart [b]=nats-cut [c]=listener-shared [d]=listener-dispatch
  [e]=dispatch-kill [f]=none)
run_window() {
  local d=$1 via=dispatch
  begin "window $d (${disruption_name[$d]})"
  case $d in
  a)
    kill_label pi KILL
    close_fd pi
    kill_label cc KILL
    close_fd cc
    if registered "${sid[pi]}" && registered "${sid[cc]}"; then
      note "both sessions SIGKILLed; their registrations stay until they lapse"
    else
      note "both sessions SIGKILLed; a registration went with its process"
    fi
    publish_dispatch a
    publish_rest a "$via"
    sleep 3
    if [[ -n "$resume_plugins_from" ]]; then
      use_plugins "$pi_plugin_after" "$claude_plugin_after"
      write_mcp_config
      note "upgrade: both sessions resume on this run's pi-envoy and claude-envoy, after the window's events were stored"
    fi
    start_pi "${transcript[pi]}"
    start_cc resume
    ;;
  b)
    kill_label proxy-pi KILL
    kill_label proxy-cc KILL
    wait_until 20 sessions_cut || fail "natsd still shows a session subscribed 20s after its proxy was cut"
    note "both proxies cut; natsd shows neither session's agent or relay subscription"
    publish_dispatch b
    publish_rest b "$via"
    sleep 3
    start_proxy proxy-pi
    start_proxy proxy-cc
    note "both proxies back"
    ;;
  c)
    kill_label listener-shared KILL
    note "listener shared SIGKILLed"
    publish_dispatch c
    publish_rest c "$via"
    sleep 3
    start_listener shared
    ;;
  d)
    kill_label listener-dispatch KILL
    via=shared
    note "listener dispatch SIGKILLed; the listener sends go to shared"
    publish_dispatch d
    publish_rest d "$via"
    sleep 3
    start_listener dispatch
    ;;
  e)
    # Dispatch's own NATS connection is cut first, so its outbox cannot publish what the writes
    # commit: those events are committed before the kill and can only be published after the
    # restart, whatever the race between the outbox and the kill would otherwise decide.
    kill_label proxy-dispatch KILL
    publish_dispatch e
    kill_label dispatch KILL
    note "Dispatch's NATS connection cut, its writes made, and Dispatch SIGKILLed right after they returned"
    publish_rest e "$via"
    sleep 3
    start_proxy proxy-dispatch
    local restarted
    restarted=$(date -u +%Y-%m-%dT%H:%M:%S.%3NZ)
    start_dispatch
    settle e
    note "Dispatch restarted at ${restarted}; the stream stored window e's comments at:"
    stored_comments | jq -r --slurpfile ev "$events" '. as $s | select(any($ev[]; .disruption == "e" and .kind == "issue" and .marker == $s.marker))
      | "   \(.marker) stream seq \(.seq), stored \(.stored)"' ||
      note "could not read the stream (see $work/logs/stored-comments.log)"
    return 0
    ;;
  f)
    publish_dispatch f
    publish_rest f "$via"
    ;;
  esac
  settle "$d"
}

IFS=, read -r -a windows <<<"$disruptions"
for d in "${windows[@]}"; do run_window "$d"; done
transcript[cc]=$(cc_transcript) || true

# ---- the table --------------------------------------------------------------------------------
begin table
session_delivery_shown "the windows are over"
tally
bad=0
lost_ok=1
table="$work/table.txt"
{
  printf 'build: listener and bridge %s; plugins %s\n' "$tree_label" "$plugins_label"
  printf '%-22s %-6s %-17s %-14s %4s %4s\n' disruption kind event marker pi cc
  while IFS=$'\t' read -r m d kind source sessions expected; do
    row=""
    for label in pi cc; do
      if [[ " $sessions " == *" $label "* ]]; then
        n=${count[$label:$m]:-0}
        cell=$n
        if ((n != expected)); then
          cell="${n}!"
          bad=1
        fi
        ((expected == 1)) || cell="${cell}/0"
        if [[ "$d" == a && ",$expect_lost," == *",$kind,"* ]] && ((n != 0)); then lost_ok=0; fi
      else
        cell=-
      fi
      row+=$(printf ' %4s' "$cell")
    done
    printf '%-22s %-6s %-17s %-14s%s\n' "$d ${disruption_name[$d]}" "$kind" "$source" "$m" "$row"
  done < <(jq -r '[.marker, .disruption, .kind, .source, (.sessions | join(" ")), (.expected | tostring)] | @tsv' "$events")
  printf '(a cell is the count of entries carrying the marker in that session'"'"'s transcript; "!" marks a count other than expected; "/0" marks an event its publisher refused, expected 0 times; "-" an event not meant for that session)\n'
} >"$table"
cat "$table"

# The followed issue's comments, in each transcript, in their publish order: the order the stream
# stored them in, read back from the stream. Dispatch's outbox decides that order; where it
# differs from the order the comments were written in, the run says so, and does not judge it.
declare -A stream_seq=()
while read -r seq_no m; do
  [[ -n "$m" ]] && stream_seq[$m]=$seq_no
done < <(stored_comments | jq -r '"\(.seq) \(.marker)"')
published_order=()
while read -r m; do published_order+=("$m"); done < <(
  jq -r 'select(.kind == "issue" and .expected == 1 and (.event | startswith("comment-"))) | .marker' "$events" |
    while read -r m; do printf '%s %s\n' "${stream_seq[$m]:-0}" "$m"; done | sort -n | cut -d' ' -f2)
# Only the first followed issue's comments: the order is read back from that issue's own subject,
# and --second-followed-issue's comment is stored on another (its delivery is counted like any
# other event, above).
written_order=$(jq -r 'select(.kind == "issue" and .expected == 1 and (.event | startswith("comment-"))) | .marker' "$events" | tr '\n' ' ')
if [[ "${published_order[*]} " != "$written_order" ]]; then
  printf 'note: Dispatch published %s'"'"'s comments in another order than they were written: written %s; stored %s\n' \
    "$followed_issue" "$written_order" "${published_order[*]}" | tee -a "$table"
fi
order_ok=1
for m in "${published_order[@]}"; do
  [[ -n "${stream_seq[$m]:-}" ]] || { printf 'the stream holds no comment carrying %s\n' "$m" | tee -a "$table"; order_ok=0; }
done
for label in pi cc; do
  last=0
  for m in "${published_order[@]}"; do
    line=${first_line[$label:$m]:-}
    [[ -n "$line" ]] || continue
    if ((line <= last)); then
      printf 'order break in %s: %s (stream seq %s, transcript line %s) after line %s\n' "$label" "$m" "${stream_seq[$m]:-?}" "$line" "$last" | tee -a "$table"
      order_ok=0
    fi
    last=$line
  done
done
if ((order_ok == 1)); then
  printf 'issue order: every comment of %s appears in its stream order in both transcripts\n' "$followed_issue" | tee -a "$table"
fi
printf 'transcripts: pi %s; cc %s\n' "${transcript[pi]}" "${transcript[cc]}" | tee -a "$table"

# Every entry carrying a marker whose count is wrong, for the reader.
if ((bad == 1)); then
  for label in pi cc; do
    while IFS=$'\t' read -r m sessions expected; do
      [[ " $sessions " == *" $label "* ]] || continue
      ((${count[$label:$m]:-0} == expected)) && continue
      printf '%s %s: count %s, expected %s; entries:\n' "$label" "$m" "${count[$label:$m]:-0}" "$expected"
      grep -n -- "$m" "${transcript[$label]}" | cut -c1-400 | sed 's/^/   /' || true
    done < <(jq -r '[.marker, (.sessions | join(" ")), (.expected | tostring)] | @tsv' "$events")
  done >"$work/wrong-counts.txt"
  note "entries behind each wrong count: $work/wrong-counts.txt"
fi

if [[ -n "$expect_lost" ]]; then
  if ((lost_ok == 1)); then
    printf 'NEGATIVE CONTROL HOLDS: every %s event of window a counted 0 in both sessions (%s)\n' "$expect_lost" "$tree_label"
    exit 0
  fi
  printf 'NEGATIVE CONTROL FAILED: an event of a kind in %s reached a session under window a (%s)\n' "$expect_lost" "$tree_label"
  exit 1
fi
if ((bad == 0 && order_ok == 1)); then
  printf 'ENVOY SESSION DELIVERY: PASS (every event once, the issue in order)\n'
  exit 0
fi
printf 'ENVOY SESSION DELIVERY: FAIL (see the table above)\n'
exit 1
