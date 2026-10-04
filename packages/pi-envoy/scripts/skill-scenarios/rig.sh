#!/usr/bin/env bash
# Skill scenarios: fresh Oh My Pi agents, each running one checkout's packed plugin (and so that
# checkout's skills), each given one task a skill rule governs, scored afterwards from what the
# agent actually wrote. Two checkouts under the same scenario compare two versions of a skill.
#
#   rig.sh profile   <label> <checkout>            build the isolated Oh My Pi home for a checkout
#   rig.sh live-read <label> <skill>...            one session reads every file of each skill
#   rig.sh batch     <scenario> <runs> <label>...  starts the scratch services, runs 1..<runs> per
#                                                  label (SKILL_SCENARIOS_PARALLEL at once), and
#                                                  stops the services
#   rig.sh score     [<scenario>]                  one row per run, then counts per label
#
# A batch runs each run as `rig.sh run <scenario> <label> <n> <index> <start>`, which needs that
# batch's services: <index> is the run's place in the batch and <start> the batch's start in epoch
# seconds. One run on its own is `rig.sh batch <scenario> 1 <label>`.
#
# Scenarios (each one's scoring rule is on the score.ts function that scores it):
#   ask-on-message  A plain session (no Legion role) is told to ask Sami whether to go ahead with a
#                   plan an owner session posted in a Dispatch message; each run seeds its own
#                   issue, attached table and message (seed.ts). Scored by askOnMessage.
#   tester-proof    A tester phase worker, booted by the Legion extension against a daemon stand-in,
#                   is told the implementer has finished; its world is worker_fixture's, with GitHub
#                   a recording stand-in and the remote a local bare repository. Scored by
#                   testerProof.
#   measure-before-ask
#                   A plain session is told to ask Sami which of three ways to finish an owner
#                   session posted, with the export the message names in its working directory;
#                   the export shows one way reaches only 18 of the 430 issues it names
#                   (run_measure_before_ask). Scored by measureBeforeAsk.
#   brainstorm      A plain session (no Legion role) gets a bare `/brainstorming` prompt on a
#                   scratch to-do CLI, with no word of Dispatch in the prompt and no Dispatch
#                   configuration anywhere in its environment; project TODO is pre-seeded with two
#                   unrelated issues (seed_dispatch's `brainstorm`), so the agent must find it by
#                   search, never its own environment or the Dispatch HTTP API. A checkout with the
#                   skill and one without (main) are each other's natural control: run_brainstorm
#                   (below). Scored by brainstormSurface.
#
# A scenario measures its rule only if a label whose skills lack the rule scores lower than one
# whose skills state it. So every comparison carries an ablate label: a checkout of the head with
# the scenario's scored rule deleted from its skills, never committed. measure-before-ask is the
# instrument: gate 2 lives in the dispatch skill alone, and its ablate arm scores below the head.
# ask-on-message and tester-proof are controls: their rules also live in dispatch-first and
# dispatch_ask's description, and in the tester role prompt, so ablate scores as the head does
# and they cannot detect a lost skill rule. A new scenario is an instrument only once an ablate
# arm has scored lower on it.
#
# A batch's services are the e2e harness's real Go Dispatch server
# (packages/dispatch/e2e/run-server.sh) on a Postgres container of its own, seeded and read back
# through its API (seed.ts), a NATS container, and the Envoy listener built from this checkout.
# Docker assigns the containers' host ports, the listener and Dispatch bind ports
# scripts/e2e/lib/rig.sh picks, and the kernel assigns each run's daemon stand-in its port.
#
# Operator input (no default in this repository):
#   LEGION_E2E_MODEL_GATEWAY_URL  profile: the model gateway (scripts/e2e/lib/install-model-gateway.sh)
# Optional:
#   SKILL_SCENARIOS_WORK      work directory, outside any checkout (default ${TMPDIR:-/tmp}/skill-scenarios)
#   SKILL_SCENARIOS_PARALLEL  concurrent runs in a batch (default 5)
#   SKILL_SCENARIOS_TIMEOUT   seconds one run may take (default 1500)
#
# Each run:
#   - Its agent is `omp -p` on the Oh My Pi the daemon pins (.omp-pin), in the label's profile
#     (make_omp_home, install-plugin-profile.sh, install-model-gateway.sh), under a HOME of its own
#     copied from the label's, with `env -i` and only the variables its pane file names (base_env).
#     An agent that writes to its HOME (`mise use -g`, say) changes nothing for another run.
#   - It runs in the tmux session skill-scenarios-<digest>-<run> on the work directory's own tmux
#     server (<work>/tmux.sock), <run> being <scenario>-<label>-<n>. The pane shows nothing: omp's
#     output goes to <work>/runs/<run>/out.txt, and the transcript, which grows as the agent works,
#     to the .jsonl under <work>/runs/<run>/sessions.
#   - `gh` and `bun` stand-ins are first on its PATH (standins): no run reaches GitHub, and a bun
#     it reaches through PATH is the bun this script runs on, recorded. A bun named by its path or
#     run through `mise exec` bypasses the stand-in and goes unrecorded. Every container and tmux
#     session a work directory starts carries the directory's digest in its name.
#   - Its agent can still read the whole filesystem, including the other label's checkout and the
#     checkout this script runs from (its `legion` resolves there), and write the machine's /tmp,
#     which nothing here cleans. score.ts does not score a run that read outside its own label, or
#     whose PR body carries another run's line (unscored, testerProof).
#   - An exit stops what its command started (on_exit, stop_run, stop_batch).
set -euo pipefail
# Nothing the rig starts inherits a service endpoint or credential from the caller: a Legion pane
# exports ENVOY_URL, DISPATCH_URL and their token files, a developer's shell can hold NATS
# credentials (packages/dispatch/e2e/run-server.sh strips the same three families), and e2e/api.ts,
# which seed.ts calls, addresses PLAYWRIGHT_BASE_URL with E2E_AGENT_TOKEN whenever it is set.
for name in $(compgen -e); do
  case $name in DISPATCH_* | ENVOY_* | NATS_* | PLAYWRIGHT_BASE_URL | E2E_AGENT_TOKEN) unset "$name" ;; esac
done

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
root=$(cd "$here/../../../.." && pwd -P)
work=${SKILL_SCENARIOS_WORK:-${TMPDIR:-/tmp}/skill-scenarios}
work=$(realpath -m -- "$work")

# The tester-proof world (worker_fixture below).
worker_key=LWEVAL-1
worker_repo=example/widgets

note() { printf 'skill-scenarios: %s\n' "$*"; }
fail() {
  echo "skill-scenarios: $*" >&2
  exit 1
}

case "$work/" in "$root"/*) fail "SKILL_SCENARIOS_WORK $work is inside the checkout $root, where jj would snapshot it" ;; esac

mkdir -p "$work"

# scripts/e2e/lib/rig.sh's bounded waits, port picks, and process start and stop. Each command
# points `evidence` at the directory its processes log under ($evidence/logs/<name>.log).
evidence=$work
timeout_hook=
# shellcheck source-path=SCRIPTDIR/../../../.. source=scripts/e2e/lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"

tag=skill-scenarios-$(printf '%s' "$work" | sha256sum | cut -c1-8)
tmux_socket=$work/tmux.sock
tmux() { command tmux -S "$tmux_socket" "$@"; }

# What a command started, for its exit trap: the run directory and its agent's tmux session, the
# pids lib/rig.sh's start_process sets, and the services' ports and directory.
R='' session='' daemon_pid='' listener_pid='' dispatch_pid='' runs_pid='' batch_scenario=''
S='' envoy_port='' dispatch_port='' pg_port='' nats_port=''

label_dir() {
  [[ $1 =~ ^[a-z0-9][a-z0-9-]*$ ]] || fail "label '$1' is not lower-case letters, digits and hyphens"
  printf '%s\n' "$work/profiles/$1"
}

# The label's checkout, pin and profile name, recorded by `profile`, as the globals P (the label's
# directory), co, pin and profile that every command and helper below reads.
load_label() {
  P=$(label_dir "$1")
  [ -f "$P/ready" ] || fail "no profile for label $1: run \`rig.sh profile $1 <checkout>\` first"
  co=$(<"$P/checkout")
  pin=$(<"$P/pin")
  profile=$(<"$P/profile")
}

cmd_profile() {
  [ $# = 2 ] || fail "usage: rig.sh profile <label> <checkout>"
  local label=$1 P co profile=skills-$1
  P=$(label_dir "$label")
  [ ! -e "$P" ] || fail "$P exists; remove it to rebuild label $label"
  co=$(cd "$2" && pwd -P)
  [ -f "$co/scripts/e2e/lib/install-plugin-profile.sh" ] || fail "$co is not a Legion checkout"
  mkdir -p "$P"
  printf '%s\n' "$co" >"$P/checkout"
  printf '%s\n' "$profile" >"$P/profile"
  # What the label runs: the checkout's working-copy commit and a digest of its skills tree, the
  # files the packed plugin stages into dist/skills, listed in byte order (LC_ALL=C) so the digest
  # is the same in every locale.
  {
    printf 'checkout %s\n' "$co"
    printf 'commit %s\n' "$(jj -R "$co" log -r @ --no-graph -T 'commit_id ++ " parents=" ++ parents.map(|c| c.commit_id()).join(",")')"
    printf 'skills %s\n' "$(cd "$co" && find skills -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum | sha256sum | cut -d' ' -f1)"
  } >"$P/built-from"
  (
    # shellcheck source=/dev/null
    source "$co/scripts/e2e/lib/omp-home.sh"
    make_omp_home "$P/home"
  )
  bash "$co/scripts/e2e/lib/install-plugin-profile.sh" --profile "$profile" --home "$P/home" --dest "$P/plugin" >"$P/install.log" 2>&1 ||
    fail "install-plugin-profile.sh failed; see $P/install.log"
  bash "$co/scripts/e2e/lib/install-model-gateway.sh" --profile "$profile" --home "$P/home" --dest "$P/key" --cache-dir "$P/cache" >>"$P/install.log" 2>&1 ||
    fail "install-model-gateway.sh failed; see $P/install.log"
  cp "$co/.omp-pin" "$P/pin"
  touch "$P/ready"
  cat "$P/built-from"
}

# Writes the variables every agent gets to $R/pane.env: a HOME copied from the label's, the
# label's profile, the run's stand-ins first on PATH, its own TMPDIR, and nothing else from the
# caller's environment.
base_env() {
  mkdir -p "$R/tmp"
  cp -a "$P/home" "$R/home"
  {
    echo "HOME=$R/home"
    echo "USER=$USER"
    echo "TERM=xterm-256color"
    echo "TMPDIR=$R/tmp"
    echo "MISE_DATA_DIR=${MISE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/mise}"
    echo "OMP_PROFILE=$profile"
    echo "PI_NOTIFICATIONS=off"
    echo "PI_NO_TITLE=1"
    echo "SKILL_SCENARIO_RUN=$R"
    echo "PATH=$R/bin:$PATH"
  } >"$R/pane.env"
}

# The run's stand-ins in $R/bin: `gh`, which records the call and answers from $R/fixtures.json,
# and `bun`, which runs the bun this script runs on (a caller's `bun` may be a shim that resolves
# nothing under the agent's HOME) and records the call to $R/calls.jsonl as legion-standin.sh
# records its own. The score reads the tester's run of the CLI from that record.
standins() {
  local bun_real
  bun_real=$(bun -e 'console.log(process.execPath)')
  mkdir -p "$R/bin"
  printf '#!/bin/sh\nexec %q %q gh "$@"\n' "$bun_real" "$here/gh-standin.ts" >"$R/bin/gh"
  cat >"$R/bin/bun" <<EOF
#!/usr/bin/env bash
at=\$(date -u +%FT%T.%3NZ)
status=0
$(printf %q "$bun_real") "\$@" || status=\$?
jq -cn --arg at "\$at" --argjson exit "\$status" '{at:\$at, as:"bun", argv:\$ARGS.positional, exit:\$exit}' \\
  --args -- "\$@" >>$(printf %q "$R/calls.jsonl")
exit "\$status"
EOF
  chmod +x "$R/bin/gh" "$R/bin/bun"
  [ -f "$R/fixtures.json" ] || printf '{"routes":[]}\n' >"$R/fixtures.json"
}

# Runs `omp -p "$(cat $R/prompt.txt)"` in $R/cwd under $R/pane.env, followed by $R/system-args (one
# --append-system-prompt argument, as shell text) when it exists, as the tmux session <tag>-<name>,
# and waits for it to exit or time out. A timeout is the run's result (`exit=timeout`), not a rig
# failure, so this wait is not an until_true.
launch() {
  local system=
  session=$tag-$1
  [ ! -f "$R/system-args" ] || system=" $(<"$R/system-args")"
  cat >"$R/launch.sh" <<EOF
#!/usr/bin/env bash
cd $(printf %q "$R/cwd")
mapfile -t vars <$(printf %q "$R/pane.env")
exec env -i "\${vars[@]}" mise x $(printf %q "$pin") -- omp -p "\$(cat $(printf %q "$R/prompt.txt"))"$system --session-dir $(printf %q "$R/sessions") >$(printf %q "$R/out.txt") 2>&1
EOF
  chmod +x "$R/launch.sh"
  tmux kill-session -t "=$session" 2>/dev/null || true
  tmux new-session -d -s "$session" -x 200 -y 50 "bash $(printf %q "$R/launch.sh"); echo exit=\$? >>$(printf %q "$R/out.txt")"
  local deadline=$((SECONDS + ${SKILL_SCENARIOS_TIMEOUT:-1500}))
  # The tmux command appends `exit=<status>` as out.txt's last line once omp has exited.
  while [ $SECONDS -lt $deadline ]; do
    tail -n 1 "$R/out.txt" 2>/dev/null | grep -q '^exit=' && break
    sleep 5
  done
  tail -n 1 "$R/out.txt" 2>/dev/null | grep -q '^exit=' || echo "exit=timeout" >>"$R/out.txt"
  tmux kill-session -t "=$session" 2>/dev/null || true
}

# run_env_pids DIR prints each process whose environment names a run at or under DIR in
# SKILL_SCENARIO_RUN, which an agent gets from its pane file and every command it runs inherits.
# It finds what stop_tree cannot reach from the pids a command recorded: a process that left their
# tree, such as a `setsid -f` child. A process that merely works under DIR, such as a developer's
# shell watching a run, has no such variable, and another user's environment cannot be read. One
# grep narrows the environments to those holding the text anywhere; each is then read variable by
# variable.
# shellcheck disable=SC2329 # stop_env_processes, which the exit traps run, runs it
run_env_pids() {
  local p var
  for p in $(grep -lzsF -e "SKILL_SCENARIO_RUN=$1" /proc/[0-9]*/environ || true); do
    {
      while IFS= read -r -d '' var; do
        case $var in "SKILL_SCENARIO_RUN=$1" | "SKILL_SCENARIO_RUN=$1"/*) echo "${p//[!0-9]/}" && break ;; esac
      done <"$p"
    } 2>/dev/null || true
  done
}

# stop_env_processes DIR stops, with stop_tree, each process run_env_pids DIR finds and everything
# under it, a child that dropped the variable included; the parent stop_tree checks is the one the
# process has now, read a moment after the list.
# shellcheck disable=SC2329 # the exit traps run it
stop_env_processes() {
  local pid
  for pid in $(run_env_pids "$1"); do stop_tree "$pid" "$(ps -o ppid= -p "$pid" 2>/dev/null | tr -d ' ')"; done
}

# stop_panes SESSION stops, with stop_tree, each pane process of SESSION on the work directory's
# tmux server and everything under it, while the server is still its parent.
# shellcheck disable=SC2329 # the exit traps run it
stop_panes() {
  local server pid
  server=$(tmux display-message -p '#{pid}' 2>/dev/null) || return 0
  for pid in $(tmux list-panes -s -t "=$1" -F '#{pane_pid}' 2>/dev/null || true); do stop_tree "$pid" "$server"; done
}

# A run's or a live read's exit: its daemon stand-in, its agent's session, and every process that
# carries its SKILL_SCENARIO_RUN.
# shellcheck disable=SC2329 # on_exit's trap runs it
stop_run() {
  stop_tree "$daemon_pid"
  [ -z "$session" ] || stop_panes "$session"
  [ -z "$R" ] || stop_env_processes "$R"
  [ -z "$session" ] || tmux kill-session -t "=$session" 2>/dev/null || true
}

# on_exit HANDLER runs HANDLER at the command's exit, INT, TERM and HUP included.
on_exit() {
  # shellcheck disable=SC2064 # the handler's name, expanded now on purpose
  trap "$1" EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT
  trap 'exit 143' TERM
}

cmd_live_read() {
  [ $# -ge 2 ] || fail "usage: rig.sh live-read <label> <skill>..."
  load_label "$1"
  shift
  local paths=() skill file
  R=$work/live-read/${P##*/}
  on_exit stop_run
  rm -rf "$R"
  mkdir -p "$R/cwd" "$R/sessions"
  for skill in "$@"; do
    [ -f "$co/skills/$skill/SKILL.md" ] || fail "$co has no skill $skill"
    paths+=("skill://$skill")
    for file in "$co/skills/$skill/references"/*.md; do
      [ -e "$file" ] && paths+=("skill://$skill/references/${file##*/}")
    done
  done
  printf 'Call the read tool once for each of these paths, in this order, one call at a time, and nothing else: %s. Then reply with the single word DONE.\n' "${paths[*]}" >"$R/prompt.txt"
  base_env
  standins
  # No Dispatch, and an Envoy address nothing listens on: the session reads files and nothing else.
  echo "ENVOY_URL=http://127.0.0.1:1" >>"$R/pane.env"
  launch "live-read-${P##*/}"
  bun "$here/score.ts" live-read "$R" "$co/skills" "$R/home/.omp/profiles/$profile/logs"
}

services_env=$work/services/env

# start_listener and start_dispatch pick a port for their service and start it there, picking again
# when another process binds that port first (await_start's 2).
start_listener() {
  local attempt offset result
  for attempt in 1 2 3 4 5; do
    pick_port envoy_port
    offset=$(log_size listener)
    ENVOY_API_TOKEN="$(<"$S/envoy-token")" PORT=$envoy_port ENVOY_LISTEN_HOST=127.0.0.1 ENVOY_HOST_BRIDGE=127.0.0.1 \
      ENVOY_MACHINE_ID="$tag" NATS_URLS="nats://127.0.0.1:$nats_port" start_process listener "$S/envoy-listener"
    result=0
    await_start listener "$listener_pid" "$offset" 60 "the Envoy listener to answer /v1/sessions" \
      curl -fsS -o /dev/null -H "Authorization: Bearer $(<"$S/envoy-token")" "http://127.0.0.1:$envoy_port/v1/sessions" || result=$?
    [ "$result" != 0 ] || return 0
    note "the Envoy listener lost port $envoy_port to another process (attempt $attempt); picking another"
  done
  fail "the Envoy listener lost its picked port five times"
}

# The harness's fake Envoy and GitHub, which run-server.sh names and this rig does not run, are
# port 1, where nothing listens; its broker switch is empty, so the server has no secrets broker.
start_dispatch() {
  local attempt offset result
  for attempt in 1 2 3 4 5; do
    pick_port dispatch_port
    offset=$(log_size dispatch)
    DATABASE_URL="postgres://postgres:ci@127.0.0.1:$pg_port/postgres?sslmode=disable" DISPATCH_E2E_PORT=$dispatch_port \
      FAKE_ENVOY_PORT=1 FAKE_GITHUB_PORT=1 DISPATCH_E2E_AGENT_SECRETS_URL= start_process dispatch bash "$root/packages/dispatch/e2e/run-server.sh"
    result=0
    await_start dispatch "$dispatch_pid" "$offset" 300 "Dispatch to answer /api/v1/whoami" \
      curl -fsS -o /dev/null -H 'Authorization: Bearer e2e-token' "http://127.0.0.1:$dispatch_port/api/v1/whoami" || result=$?
    [ "$result" != 0 ] || return 0
    note "Dispatch lost port $dispatch_port to another process (attempt $attempt); picking another"
  done
  fail "Dispatch lost its picked port five times"
}

# One batch at a time per work directory: $work/services/up is its lock, taken before the batch
# starts or stops anything. A batch killed outright leaves it, with its containers and processes.
lock_services() {
  S=$work/services
  mkdir -p "$S/logs"
  (set -C && : >"$S/up") 2>/dev/null ||
    fail "a batch already holds $S/up; a batch killed outright leaves it, its $tag-* containers and its processes behind"
}

services_up() {
  evidence=$S
  docker run -d --name "$tag-pg" --mount type=tmpfs,destination=/var/lib/postgresql/data \
    -e POSTGRES_PASSWORD=ci -p 127.0.0.1::5432 postgres:16 >/dev/null
  pg_port=$(docker port "$tag-pg" 5432/tcp | sed -n '1s/.*://p')
  [ -n "$pg_port" ] || fail "docker assigned Postgres no host port"
  docker run -d --name "$tag-nats" -p 127.0.0.1::4222 nats:2.10 -js >/dev/null
  nats_port=$(docker port "$tag-nats" 4222/tcp | sed -n '1s/.*://p')
  [ -n "$nats_port" ] || fail "docker assigned NATS no host port"
  until_true 60 "Postgres to accept TCP connections" docker exec "$tag-pg" pg_isready -h 127.0.0.1 -p 5432 -U postgres
  until_true 60 "NATS to be ready" sh -c "docker logs '$tag-nats' 2>&1 | grep -q 'Server is ready'"
  (cd "$root/packages/envoy" && go build -o "$S/envoy-listener" ./cmd/listener)
  (umask 077 && openssl rand -hex 24 >"$S/envoy-token" && printf 'e2e-token\n' >"$S/dispatch-token")
  start_listener
  start_dispatch
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" project) >>"$S/logs/seed.log" 2>&1 ||
    fail "seed.ts project failed; see $S/logs/seed.log"
  printf '%s\n' "$dispatch_port" >"$S/dispatch-port"
  {
    echo "ENVOY_URL=http://127.0.0.1:$envoy_port"
    echo "ENVOY_NATS_URL=nats://127.0.0.1:$nats_port"
    echo "ENVOY_TOKEN_FILE=$S/envoy-token"
    echo "DISPATCH_URL=http://127.0.0.1:$dispatch_port"
    echo "DISPATCH_TOKEN_FILE=$S/dispatch-token"
  } >"$services_env"
  note "services up: listener :$envoy_port, NATS :$nats_port, Dispatch :$dispatch_port, Postgres :$pg_port"
}

# shellcheck disable=SC2329 # stop_batch, an exit trap, runs it
services_down() {
  stop_tree "${listener_pid:-}"
  stop_tree "${dispatch_pid:-}"
  docker rm -fv "$tag-nats" "$tag-pg" >/dev/null 2>&1 || true
  rm -f "$services_env" "$S/up"
}

# seed_dispatch SCENARIO seeds the run's own issue and the owner's message with `seed.ts SCENARIO`
# and sets the caller's issue and message, which its prompt names.
seed_dispatch() {
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$(<"$work/services/dispatch-port") bun "$here/seed.ts" "$1" "$R/fixture.json") \
    >"$R/fixture.log" 2>&1 || fail "the fixture failed; see $R/fixture.log"
  issue=$(jq -r .issue "$R/fixture.json")
  message=$(jq -r .message "$R/fixture.json")
}

# dispatch_session NAME runs $R/prompt.txt as a plain session outside Legion, with Dispatch as its
# own and no Envoy (an address nothing listens on) or NATS, then reads back, with seed.ts capture,
# what it left on Dispatch and the message it was asked about, for the score.
dispatch_session() {
  base_env
  standins
  grep '^DISPATCH_' "$services_env" >>"$R/pane.env"
  echo "ENVOY_URL=http://127.0.0.1:1" >>"$R/pane.env"
  launch "$1"
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$(<"$work/services/dispatch-port") bun "$here/seed.ts" capture "$R/fixture.json" "$R") \
    >>"$R/fixture.log" 2>&1 || fail "the capture failed; see $R/fixture.log"
}

run_ask_on_message() {
  local name=$1 issue message
  seed_dispatch ask-on-message
  printf 'On Dispatch issue %s, the Dispatch owner session posted its plan for the rest of the work in this message: dispatch://%s/message/%s. Open an ask on %s asking Sami whether to go ahead with that plan. Do nothing else.\n' \
    "$issue" "$issue" "$message" "$issue" >"$R/prompt.txt"
  dispatch_session "$name"
}

# The export the owner's message names, in the agent's working directory: 430 stranded issues, of
# which only every 23rd (18) has the owner option A needs. Its example.invalid addresses give an
# agent a second reason to drop A; measureBeforeAsk says how the first runs split on it.
run_measure_before_ask() {
  local name=$1 issue message
  seed_dispatch measure-before-ask
  awk 'BEGIN { print "key,title,component,owner"
    for (n = 1; n <= 430; n++) printf "ISS-%d,Stranded issue %d,billing-api,%s\n", 1000 + n, n, (n % 23 == 0 ? "owner" n "@example.invalid" : "") }' \
    >"$R/cwd/stranded.csv"
  printf 'On Dispatch issue %s, the Dispatch owner session posted three ways to finish its work in this message: dispatch://%s/message/%s. The export it names, stranded.csv, is in your working directory. Open an ask on %s asking Sami which way to go. Change nothing else.\n' \
    "$issue" "$issue" "$message" "$issue" >"$R/prompt.txt"
  dispatch_session "$name"
}

# A tiny to-do CLI, the fixture the PR's own throwaway proof used: a scratch git repository with no
# Dispatch configuration and no word of due dates. seed_dispatch's `brainstorm` seeds the matching
# Dispatch project (TODO, with two unrelated issues) so the agent's search finds a real project, not
# an empty one; dispatch_session captures the surface and any asks the ordinary way, and
# seed.ts capture-brainstorm finds the one issue the run itself created (brainstormSurface, which
# also flags a tool call that reads DISPATCH_TOKEN or calls curl with Authorization — the
# project-lookup leak Qual's review found).
run_brainstorm() {
  local name=$1
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$(<"$work/services/dispatch-port") bun "$here/seed.ts" brainstorm "$R/fixture.json") \
    >"$R/fixture.log" 2>&1 || fail "the fixture failed; see $R/fixture.log"
  cat >"$R/cwd/todo.py" <<'PYEOF'
"""A tiny command-line to-do list that keeps its tasks in tasks.json next to this file."""

import json
import sys
from pathlib import Path

STORE = Path(__file__).with_name("tasks.json")


def load() -> list[dict]:
    if not STORE.exists():
        return []
    return json.loads(STORE.read_text())


def save(tasks: list[dict]) -> None:
    STORE.write_text(json.dumps(tasks, indent=2) + "\n")


def main(argv: list[str]) -> int:
    if not argv:
        print("usage: todo.py add <text> | list | done <number>")
        return 2
    tasks = load()
    command, rest = argv[0], argv[1:]
    if command == "add":
        tasks.append({"text": " ".join(rest), "done": False})
        save(tasks)
    elif command == "list":
        for number, task in enumerate(tasks, start=1):
            mark = "x" if task["done"] else " "
            print(f"{number}. [{mark}] {task['text']}")
    elif command == "done":
        tasks[int(rest[0]) - 1]["done"] = True
        save(tasks)
    else:
        print(f"unknown command {command}")
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
PYEOF
  cat >"$R/cwd/README.md" <<'MDEOF'
# todo

A tiny command-line to-do list for one person.

```sh
python todo.py add "buy milk"
python todo.py list
python todo.py done 1
```

Tasks live in `tasks.json` beside `todo.py`.
MDEOF
  git -C "$R/cwd" init -q
  git -C "$R/cwd" add -A
  git -C "$R/cwd" -c user.name=rig -c user.email=rig@example.invalid -c commit.gpgsign=false \
    commit -qm "A tiny to-do CLI"
  printf '/brainstorming a small feature: let a task in this to-do CLI carry an optional due date, and have `list` show overdue tasks first.\n' \
    >"$R/prompt.txt"
  base_env
  standins
  grep '^DISPATCH_' "$services_env" >>"$R/pane.env"
  echo "ENVOY_URL=http://127.0.0.1:1" >>"$R/pane.env"
  launch "$name"
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$(<"$work/services/dispatch-port") bun "$here/seed.ts" capture-brainstorm "$R/fixture.json" "$R") \
    >>"$R/fixture.log" 2>&1 || fail "the capture failed; see $R/fixture.log"
}

# The tester's frozen world under $R: a bare remote whose post-receive hook logs every push, the
# issue workspace cloned from it, the implementer's commit and handoff on legion/<key>, the PR the
# gh stand-in serves, and the architect's assignment. The fixture's content is the same in every
# run; the PR number is 1000 plus <index>, the run's index in its batch, and the commits are dated
# <index> seconds after <start>, the batch's start, so no two runs of a batch share a PR number or
# a head (two runs can meet in a scratch file in the shared /tmp), and each batch's heads carry its
# own start time.
# The CLI must meet every acceptance criterion the seeded spec lists. testerProof fails a run whose
# push changes more than .legion/test.json, and it cannot tell a tester that pinned a real defect
# with a red test, which roles/core/tester.md asks for, from one that went off-task. A defect left
# in the fixture turns correct testing into a fail.
worker_fixture() {
  local index=$1 start=$2 src C1 C2 body when worker_pr worker_pr_url
  worker_pr=$((1000 + index))
  worker_pr_url=https://github.com/$worker_repo/pull/$worker_pr
  when="@$((start + index)) +0000"
  git init -q --bare -b main "$R/remote.git"
  cat >"$R/remote.git/hooks/post-receive" <<EOF
#!/bin/sh
while read old new ref; do echo "\$(date -u +%FT%T.%3NZ) \$ref \$old \$new" >>$(printf %q "$R/pushes.log"); done
EOF
  chmod +x "$R/remote.git/hooks/post-receive"
  src=$R/src
  git init -q -b main "$src"
  g() {
    GIT_AUTHOR_DATE=$when GIT_COMMITTER_DATE=$when \
      git -C "$src" -c user.name=Fixture -c user.email=fixture@example.invalid -c commit.gpgsign=false "$@"
  }
  printf '# widgets\n\nSmall CLI helpers.\n' >"$src/README.md"
  g add -A && g commit -qm "init"
  g push -q "$R/remote.git" main
  g checkout -qb "legion/$worker_key"
  cat >"$src/greet.ts" <<'EOF'
export function greet(name: string): string {
  return `Hello, ${name.trim()}!`;
}

if (import.meta.main) {
  const name = Bun.argv[2];
  if (name === undefined || name.trim() === "") {
    console.error("usage: greet.ts <name>");
    process.exit(2);
  }
  console.log(greet(name));
}
EOF
  cat >"$src/greet.test.ts" <<'EOF'
import { expect, test } from "bun:test";
import { greet } from "./greet";

test("greets by name", () => {
  expect(greet("Ada")).toBe("Hello, Ada!");
});

test("trims the name", () => {
  expect(greet("  Ada  ")).toBe("Hello, Ada!");
});
EOF
  g add -A && g commit -qm "feat: greet(), a greeting helper for the widgets CLI ($worker_key)"
  C1=$(g rev-parse HEAD)
  mkdir -p "$src/.legion"
  # The handoff CLI writes the implement handoff, so it is the file the ledger reads, in the shape
  # the Go CLI gives every handoff (schemaVersion, phase, completed).
  "$work/bin/legion" handoff write --phase implement --workspace "$src" --data "$(
    jq -cn --arg head "$C1" '{filesChanged:["greet.ts","greet.test.ts"],
      proof:[{criterion:"greet(name) greets by name, trimmed, and `bun greet.ts <name>` prints it; no name is a usage error",
        surface:"the CLI", command:"bun greet.ts Ada", observed:"prints Hello, Ada! and exits 0", headSha:$head,
        negativeControl:"bun greet.ts → exit 2, usage: greet.ts <name> on stderr"}]}'
  )"
  g add -A && g commit -qm "legion: implement handoff ($worker_key)"
  C2=$(g rev-parse HEAD)
  g push -q "$R/remote.git" "legion/$worker_key"
  : >"$R/pushes.log"
  jj git clone "$R/remote.git" "$R/ws" >/dev/null 2>&1
  (cd "$R/ws" && jj bookmark track "legion/$worker_key" --remote origin && jj new "legion/$worker_key") >/dev/null 2>&1
  body="Dispatch: $worker_key

Adds \`greet(name)\` and \`bun greet.ts <name>\`.

## Verification

**CI:** \`Tests\` run 111 — jobs lint, typecheck, test all success at $C2.

**Threads:** 0 resolved, 0 unresolved.

**E2E (implementer):** the CLI — ran \`bun greet.ts Ada\`, observed \`Hello, Ada!\` exit 0, at head $C1.
Negative control: \`bun greet.ts\` (no name) → exit 2, \`usage: greet.ts <name>\` on stderr.

**E2E (tester):** pending.

**Production:** pending the merge.

**Fast-follow:** none.

**Chain:** not stacked."
  jq -n --arg head "$C2" --arg body "$body" --arg url "$worker_pr_url" --arg ref "legion/$worker_key" \
    --arg key "$worker_key" --argjson number "$worker_pr" '
    {number:$number, url:$url, html_url:$url, title:("feat: greet() (" + $key + ")"), state:"OPEN", isDraft:false,
     headRefName:$ref, headRefOid:$head, baseRefName:"main", body:$body, mergeable:"MERGEABLE",
     reviewDecision:"", head:{sha:$head, ref:$ref}, base:{ref:"main"},
     statusCheckRollup:[{name:"test", status:"COMPLETED", conclusion:"SUCCESS"},
                        {name:"lint", status:"COMPLETED", conclusion:"SUCCESS"}]} as $pr
    | {routes:[
      {match:"^legion threads resolve ", stdout:("no unresolved review threads on #" + ($number|tostring))},
      {match:"^gh pr view", stdout:$pr},
      {match:"^gh pr checks", stdout:"lint\tpass\t10s\thttps://example.invalid/1\ntest\tpass\t20s\thttps://example.invalid/2"},
      {match:"^gh pr (edit|comment|review|ready)", stdout:($url+"#issuecomment-1")},
      {match:"^gh api .*graphql", stdout:{data:{repository:{pullRequest:{reviewThreads:{pageInfo:{hasNextPage:false, endCursor:null}, nodes:[]}}}}}},
      {match:"^gh api .*(-X|--method) ?(POST|PATCH|PUT)", stdout:{id:6001, html_url:($url+"#issuecomment-6001")}},
      {match:("^gh api .*pulls/" + ($number|tostring) + "/(reviews|comments)"), stdout:[]},
      {match:"^gh api .*(check-runs|status)", stdout:{total_count:2, check_runs:[{name:"test", status:"completed", conclusion:"success"}, {name:"lint", status:"completed", conclusion:"success"}]}},
      {match:("^gh api .*pulls/" + ($number|tostring)), stdout:$pr},
      {match:"^gh (api user|auth status)", stdout:{login:"rig[bot]"}}
    ]}' >"$R/fixtures.json"
  # The world the scorer reads: head is the PR's head, code the implementer's code commit under it.
  jq -n --arg key "$worker_key" --arg repo "$worker_repo" --argjson pr "$worker_pr" --arg head "$C2" --arg code "$C1" \
    '{key:$key, repo:$repo, pr:$pr, branch:("legion/" + $key), head:$head, code:$code}' >"$R/world.json"
  printf 'Architect → tester, %s. The implementer has finished: PR #%s (%s, repository %s) is at %s, and its implement handoff is on the branch. Test it and finish your phase.\n' \
    "$worker_key" "$worker_pr" "$worker_pr_url" "$worker_repo" "$C2" >"$R/prompt.txt"
}

run_tester_proof() {
  local name=$1 n=$2 index=$3 start=$4 state project port
  [ -x "$work/bin/legion" ] || fail "no Go legion at $work/bin/legion: a run needs its batch's build"
  worker_fixture "$index" "$start" >"$R/fixture.log" 2>&1 || fail "the fixture failed; see $R/fixture.log"
  state=$R/state
  # Each run its own Legion project, so concurrent runs claim distinct role tokens on one listener.
  # A project token is lower-case letters and digits only (isLegionProjectToken), so the label's
  # hyphens go and `r` separates it from the run number.
  project=skills${P##*/}r$n
  project=${project//-/}
  mkdir -p "$state/gh" "$state/secrets"
  chmod 0700 "$state/secrets"
  (umask 077 && openssl rand -hex 16 >"$state/secrets/boot")
  ln -s "$here/legion-standin.sh" "$R/bin/legion"
  printf '[user]\nname = "Rig Worker"\nemail = "rig@example.invalid"\n[ui]\npaginate = "never"\n' >"$R/jj.toml"
  # Port 0: the kernel assigns the stand-in's port, which it prints, so concurrent runs never race.
  start_process daemon bun "$here/../grant-rig/daemon-standin.ts" 0 "$R/daemon.log" "$state/secrets/boot" "$project" "$worker_key" tester
  await_start daemon "$daemon_pid" 0 30 "the daemon stand-in to listen" grep -q '^listening on ' "$R/logs/daemon.log"
  port=$(sed -n 's|^listening on http://127\.0\.0\.1:||p' "$R/logs/daemon.log")
  base_env
  standins
  # The pane the label's checkout's daemon would give the tester, over the base every agent gets:
  # worker.env and system-args. The stand-ins come first, so the gh the pane names is the recording
  # one.
  bun "$here/worker-pane.ts" "$R" "$R/pane.env" "$port" "$profile" "$project" "$worker_key" tester "$co"
  {
    cat "$R/worker.env" "$services_env"
    echo "GIT_CONFIG_COUNT=0"
    echo "JJ_USER=Rig Worker"
    echo "JJ_EMAIL=rig@example.invalid"
    echo "JJ_CONFIG=$R/jj.toml"
    echo "GH_REPO=$worker_repo"
    echo "SKILL_SCENARIO_LEGION=$work/bin/legion"
  } >"$R/pane.env"
  launch "$name"
}

cmd_run() {
  [ $# = 5 ] || fail "usage: rig.sh run <ask-on-message|measure-before-ask|tester-proof|brainstorm> <label> <n> <index> <start>"
  local scenario=$1 n=$3 index=$4 start=$5 name
  load_label "$2"
  [[ $n =~ ^[0-9]+$ ]] || fail "run number '$n' is not a number"
  [[ $index =~ ^[0-9]+$ && $start =~ ^[0-9]+$ ]] || fail "run index '$index' or batch start '$start' is not a number"
  [ -f "$services_env" ] || fail "a run needs its batch's services: run \`rig.sh batch $scenario 1 $2\`"
  name=$scenario-${P##*/}-$n
  R=$work/runs/$name
  evidence=$R
  on_exit stop_run
  rm -rf "$R"
  mkdir -p "$R/bin" "$R/sessions" "$R/cwd" "$R/logs"
  cp "$P/built-from" "$R/built-from"
  case $scenario in
  ask-on-message) run_ask_on_message "$name" ;;
  measure-before-ask) run_measure_before_ask "$name" ;;
  tester-proof) run_tester_proof "$name" "$n" "$index" "$start" ;;
  brainstorm) run_brainstorm "$name" ;;
  *) fail "unknown scenario $scenario" ;;
  esac
  echo "$name: $(tail -n 1 "$R/out.txt")"
}

# A batch's exit: its runs (each run's shell, daemon stand-in and fixture commands), the agents they
# started, every process that carries a run's SKILL_SCENARIO_RUN, and the services.
# shellcheck disable=SC2329 # on_exit's trap runs it
stop_batch() {
  local s sessions=()
  stop_tree "${runs_pid:-}"
  for s in $(tmux list-sessions -F '#{session_name}' 2>/dev/null || true); do
    case $s in "$tag-$batch_scenario"-*) sessions+=("$s") ;; esac
  done
  for s in "${sessions[@]}"; do stop_panes "$s"; done
  stop_env_processes "$work/runs"
  services_down
}

cmd_batch() {
  [ $# -ge 3 ] || fail "usage: rig.sh batch <scenario> <runs> <label>..."
  local runs=$2 label n index=0 start
  batch_scenario=$1
  case $batch_scenario in ask-on-message | measure-before-ask | tester-proof | brainstorm) ;; *) fail "unknown scenario $batch_scenario" ;; esac
  [[ $runs =~ ^[0-9]+$ ]] || fail "run count '$runs' is not a number"
  shift 2
  for label in "$@"; do load_label "$label"; done
  lock_services
  on_exit stop_batch
  # The Go `legion` a tester-proof run's stand-in and fixture run, built from this checkout once
  # for the whole batch.
  (cd "$root/packages/daemon" && go build -o "$work/bin/legion" ./cmd/legion) || fail "building the Go legion failed"
  services_up
  start=$(date +%s)
  for label in "$@"; do
    for n in $(seq 1 "$runs"); do
      index=$((index + 1))
      printf '%s %s %s %s %s\n' "$batch_scenario" "$label" "$n" "$index" "$start"
    done
  done | xargs -r -P "${SKILL_SCENARIOS_PARALLEL:-5}" -L 1 bash "$here/rig.sh" run &
  runs_pid=$!
  wait "$runs_pid"
}

# Bash reads a script as it runs it, so the dispatch and its exit are one command line: a batch
# outlives any edit to this file, and resuming at its old offset would run whatever lies there now.
case "${1:-}" in
profile) shift && cmd_profile "$@" ;;
live-read) shift && cmd_live_read "$@" ;;
batch) shift && cmd_batch "$@" ;;
run) shift && cmd_run "$@" ;;
score) shift && bun "$here/score.ts" runs "$work/runs" "$@" ;;
*) sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//' && exit 2 ;;
esac; exit
