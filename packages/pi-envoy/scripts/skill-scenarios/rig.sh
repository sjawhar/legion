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
#   rig.sh score     [<scenario>]                  one row per run, then pass counts per label
#
# A batch runs each run as `rig.sh run <scenario> <label> <n>`, which needs that batch's services;
# one run on its own is `rig.sh batch <scenario> 1 <label>`.
#
# Scenarios:
#   ask-on-message  A plain session (no Legion role) is told to ask Sami whether to go ahead with a
#                   plan an owner session posted in a Dispatch message. The dispatch skill's rule:
#                   the ask carries the plan and its options itself and never points at the message
#                   in prose. Each run seeds its own issue, attached table and message.
#   tester-proof    A tester phase worker, booted by the Legion extension against a daemon stand-in,
#                   is told the implementer has finished. The legion-worker skill's rule, as scored:
#                   the PR body gets an `E2E (tester)` line (at any point), and a `handoff write
#                   --phase test` the CLI accepts comes before the first push carrying
#                   .legion/test.json, which comes before an accepted `handoff complete`; the
#                   test.json the branch holds at that completion is a valid test handoff with a
#                   proof. GitHub is a recording stand-in and the remote a local bare repository.
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
# Each run's agent is `omp -p` on the Oh My Pi both daemons pin (omp-pin.ts), under the label's own
# HOME and profile (make_omp_home, install-plugin-profile.sh, install-model-gateway.sh), in a tmux
# session on the work directory's own tmux server (`tmux -S <work>/tmux.sock attach -t <session>`
# watches one), with `env -i` and only the variables its pane file names. A `gh` stand-in is first
# on every run's PATH, so no run reaches GitHub. Every container and tmux session a work directory
# starts carries its digest in its name. An exit, INT or TERM stops what the command started: a
# batch its runs, their agents and its services; a run or a live read its agent and daemon stand-in.
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
worker_pr=7
worker_pr_url=https://github.com/$worker_repo/pull/$worker_pr

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

# The label's checkout, pin and profile name, recorded by `profile`.
load_label() {
  P=$(label_dir "$1")
  [ -f "$P/ready" ] || fail "no profile for label $1: run \`rig.sh profile $1 <checkout>\` first"
  co=$(<"$P/checkout")
  pin=$(<"$P/pin")
  profile=skills-$1
}

cmd_profile() {
  [ $# = 2 ] || fail "usage: rig.sh profile <label> <checkout>"
  local label=$1 P co
  P=$(label_dir "$label")
  [ ! -e "$P" ] || fail "$P exists; remove it to rebuild label $label"
  co=$(cd "$2" && pwd -P)
  [ -f "$co/scripts/e2e/lib/install-plugin-profile.sh" ] || fail "$co is not a Legion checkout"
  mkdir -p "$P"
  printf '%s\n' "$co" >"$P/checkout"
  # What the label runs: the checkout's working-copy commit and a digest of its skills tree, the
  # files the packed plugin stages into dist/skills.
  {
    printf 'checkout %s\n' "$co"
    printf 'commit %s\n' "$(jj -R "$co" log -r @ --no-graph -T 'commit_id ++ " parents=" ++ parents.map(|c| c.commit_id()).join(",")')"
    printf 'skills %s\n' "$(cd "$co" && find skills -type f -print0 | sort -z | xargs -0 sha256sum | sha256sum | cut -d' ' -f1)"
  } >"$P/built-from"
  (
    # shellcheck source=/dev/null
    source "$co/scripts/e2e/lib/omp-home.sh"
    make_omp_home "$P/home"
  )
  bash "$co/scripts/e2e/lib/install-plugin-profile.sh" --profile "skills-$label" --home "$P/home" --dest "$P/plugin" >"$P/install.log" 2>&1 ||
    fail "install-plugin-profile.sh failed; see $P/install.log"
  bash "$co/scripts/e2e/lib/install-model-gateway.sh" --profile "skills-$label" --home "$P/home" --dest "$P/key" --cache-dir "$P/cache" >>"$P/install.log" 2>&1 ||
    fail "install-model-gateway.sh failed; see $P/install.log"
  bun "$co/packages/daemon/src/daemon/omp-pin.ts" >"$P/pin"
  touch "$P/ready"
  cat "$P/built-from"
}

# Writes the variables every agent gets to <file>: the label's home and profile, the run's
# stand-ins first on PATH, and nothing else from the caller's environment.
base_env() {
  local R=$1
  {
    echo "HOME=$P/home"
    echo "USER=$USER"
    echo "TERM=xterm-256color"
    echo "MISE_DATA_DIR=${MISE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/mise}"
    echo "OMP_PROFILE=$profile"
    echo "PI_NOTIFICATIONS=off"
    echo "PI_NO_TITLE=1"
    echo "SKILL_SCENARIO_RUN=$R"
    echo "PATH=$R/bin:$PATH"
  } >"$R/pane.env"
}

# The run's `gh`: the stand-in, which records the call and answers from $R/fixtures.json.
gh_standin() {
  local R=$1
  mkdir -p "$R/bin"
  printf '#!/bin/sh\nexec bun %q gh "$@"\n' "$here/gh-standin.ts" >"$R/bin/gh"
  chmod +x "$R/bin/gh"
  [ -f "$R/fixtures.json" ] || printf '{"routes":[]}\n' >"$R/fixtures.json"
}

# Runs `omp -p "$(cat $R/prompt.txt)"` in $R/cwd under $R/pane.env, followed by $R/system-args (one
# --append-system-prompt argument, as shell text) when it exists, as the tmux session <tag>-<name>,
# and waits for it to exit or time out. A timeout is the run's result (`exit=timeout`), not a rig
# failure, so this wait is not an until_true.
launch() {
  local R=$1 system=
  session=$tag-$2
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

# stop_working_under DIR SIGKILLs every process but this shell whose working directory is under DIR:
# an agent and every command it runs work in its run directory. The tmux server is not among them,
# although its command line names the run that started it (so lib/rig.sh's run_processes would pick
# it), and it serves the work directory's other sessions.
# shellcheck disable=SC2329 # the exit traps run it
stop_working_under() {
  local p cwd
  for p in /proc/[0-9]*; do
    cwd=$(readlink "$p/cwd" 2>/dev/null) || continue
    case "$cwd/" in
    "$1"/*)
      p=${p#/proc/}
      [ "$p" = "$$" ] || [ "$p" = "$BASHPID" ] || kill -KILL "$p" 2>/dev/null || true
      ;;
    esac
  done
}

# A run's or a live read's exit: its agent's session, its daemon stand-in, and whatever the agent
# left running.
# shellcheck disable=SC2329 # on_exit's trap runs it
stop_run() {
  [ -z "$session" ] || tmux kill-session -t "=$session" 2>/dev/null || true
  stop_tree "$daemon_pid"
  [ -z "$R" ] || stop_working_under "$R"
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
  base_env "$R"
  gh_standin "$R"
  # No Dispatch, and an Envoy address nothing listens on: the session reads files and nothing else.
  echo "ENVOY_URL=http://127.0.0.1:1" >>"$R/pane.env"
  launch "$R" "live-read-${P##*/}"
  bun "$here/score.ts" live-read "$R" "$co/skills" "$P/home/.omp/profiles/$profile/logs"
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

# The harness's fake Envoy and GitHub, which run-server.sh names and this rig does not run, are port
# 1, where nothing listens.
start_dispatch() {
  local attempt offset result
  for attempt in 1 2 3 4 5; do
    pick_port dispatch_port
    offset=$(log_size dispatch)
    DATABASE_URL="postgres://postgres:ci@127.0.0.1:$pg_port/postgres?sslmode=disable" DISPATCH_E2E_PORT=$dispatch_port \
      FAKE_ENVOY_PORT=1 FAKE_GITHUB_PORT=1 start_process dispatch bash "$root/packages/dispatch/e2e/run-server.sh"
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

run_ask_on_message() {
  local R=$1 name=$2 issue message dispatch_port
  dispatch_port=$(<"$work/services/dispatch-port")
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" ask-on-message "$R/fixture.json") \
    >"$R/fixture.log" 2>&1 || fail "the fixture failed; see $R/fixture.log"
  issue=$(jq -r .issue "$R/fixture.json")
  message=$(jq -r .message "$R/fixture.json")
  base_env "$R"
  gh_standin "$R"
  # Dispatch as the agent's own, with no Envoy (an address nothing listens on) and no NATS: a plain
  # session outside Legion.
  grep '^DISPATCH_' "$services_env" >>"$R/pane.env"
  echo "ENVOY_URL=http://127.0.0.1:1" >>"$R/pane.env"
  printf 'On Dispatch issue %s, the Dispatch owner session posted its plan for the rest of the work in this message: dispatch://%s/message/%s. Open an ask on %s asking Sami whether to go ahead with that plan. Do nothing else.\n' \
    "$issue" "$issue" "$message" "$issue" >"$R/prompt.txt"
  launch "$R" "$name"
  # What the agent left on Dispatch, and the message it was asked about, for the score.
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" capture "$R/fixture.json" "$R") \
    >>"$R/fixture.log" 2>&1 || fail "the capture failed; see $R/fixture.log"
}

# The tester's frozen world under $R: a bare remote whose post-receive hook logs every push, the
# issue workspace cloned from it, the implementer's commit and handoff on legion/<key>, the PR the
# gh stand-in serves, and the architect's assignment.
worker_fixture() {
  local R=$1 src C1 C2 body
  git init -q --bare -b main "$R/remote.git"
  cat >"$R/remote.git/hooks/post-receive" <<EOF
#!/bin/sh
while read old new ref; do echo "\$(date -u +%FT%T.%3NZ) \$ref \$old \$new" >>$(printf %q "$R/pushes.log"); done
EOF
  chmod +x "$R/remote.git/hooks/post-receive"
  src=$R/src
  git init -q -b main "$src"
  g() { git -C "$src" -c user.name=Fixture -c user.email=fixture@example.invalid -c commit.gpgsign=false "$@"; }
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
  if (name === undefined || name === "") {
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
  jq -n --arg head "$C1" '{schemaVersion:1, phase:"implement", filesChanged:["greet.ts","greet.test.ts"],
    proof:[{surface:"the CLI", command:"bun greet.ts Ada", observed:"prints Hello, Ada! and exits 0", head:$head,
      negativeControl:"bun greet.ts → exit 2, usage: greet.ts <name> on stderr"}]}' >"$src/.legion/implement.json"
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
  # The world the scorer reads.
  jq -n --arg key "$worker_key" --arg repo "$worker_repo" --argjson pr "$worker_pr" --arg head "$C2" \
    '{key:$key, repo:$repo, pr:$pr, branch:("legion/" + $key), head:$head}' >"$R/world.json"
  printf 'Architect → tester, %s. The implementer has finished: PR #%s (%s, repository %s) is at %s, and its implement handoff is on the branch. Test it and finish your phase.\n' \
    "$worker_key" "$worker_pr" "$worker_pr_url" "$worker_repo" "$C2" >"$R/prompt.txt"
}

run_tester_proof() {
  local R=$1 name=$2 n=$3 state project port
  worker_fixture "$R" >"$R/fixture.log" 2>&1 || fail "the fixture failed; see $R/fixture.log"
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
  base_env "$R"
  # The pane the daemon would give the tester, over the base every agent gets: worker.env and
  # system-args.
  bun "$here/worker-pane.ts" "$R" "$R/pane.env" "$port" "$profile" "$project" "$worker_key" tester "$co/packages/pi-envoy/roles"
  {
    cat "$R/worker.env" "$services_env"
    echo "GIT_CONFIG_COUNT=0"
    echo "GIT_TERMINAL_PROMPT=0"
    echo "JJ_USER=Rig Worker"
    echo "JJ_EMAIL=rig@example.invalid"
    echo "JJ_CONFIG=$R/jj.toml"
    echo "GH_REPO=$worker_repo"
  } >"$R/pane.env"
  gh_standin "$R"
  launch "$R" "$name"
}

cmd_run() {
  [ $# = 3 ] || fail "usage: rig.sh run <ask-on-message|tester-proof> <label> <n>"
  local scenario=$1 n=$3 name
  load_label "$2"
  [[ $n =~ ^[0-9]+$ ]] || fail "run number '$n' is not a number"
  [ -f "$services_env" ] || fail "a run needs its batch's services: run \`rig.sh batch $scenario 1 $2\`"
  name=$scenario-${P##*/}-$n
  R=$work/runs/$name
  evidence=$R
  on_exit stop_run
  rm -rf "$R"
  mkdir -p "$R/bin" "$R/sessions" "$R/cwd" "$R/logs"
  cp "$P/built-from" "$R/built-from"
  case $scenario in
  ask-on-message) run_ask_on_message "$R" "$name" ;;
  tester-proof) run_tester_proof "$R" "$name" "$n" ;;
  *) fail "unknown scenario $scenario" ;;
  esac
  echo "$name: $(tail -n 1 "$R/out.txt")"
}

# A batch's exit: its runs (each run's shell, daemon stand-in and fixture commands), the agents they
# started, and the services.
# shellcheck disable=SC2329 # on_exit's trap runs it
stop_batch() {
  local s
  stop_tree "${runs_pid:-}"
  for s in $(tmux list-sessions -F '#{session_name}' 2>/dev/null || true); do
    case $s in "$tag-$batch_scenario"-*) tmux kill-session -t "=$s" 2>/dev/null || true ;; esac
  done
  stop_working_under "$work/runs"
  services_down
}

cmd_batch() {
  [ $# -ge 3 ] || fail "usage: rig.sh batch <scenario> <runs> <label>..."
  local runs=$2 label n
  batch_scenario=$1
  case $batch_scenario in ask-on-message | tester-proof) ;; *) fail "unknown scenario $batch_scenario" ;; esac
  [[ $runs =~ ^[0-9]+$ ]] || fail "run count '$runs' is not a number"
  shift 2
  for label in "$@"; do load_label "$label"; done
  lock_services
  on_exit stop_batch
  services_up
  for label in "$@"; do for n in $(seq 1 "$runs"); do printf '%s %s %s\n' "$batch_scenario" "$label" "$n"; done; done |
    xargs -P "${SKILL_SCENARIOS_PARALLEL:-5}" -L 1 bash "$here/rig.sh" run &
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
