#!/usr/bin/env bash
# Skill scenarios: fresh Oh My Pi agents, each running one checkout's packed plugin (and so that
# checkout's skills), each given one task a skill rule governs, scored afterwards from what the
# agent actually wrote. Two checkouts under the same scenario compare two versions of a skill.
#
#   rig.sh profile   <label> <checkout>        build the isolated Oh My Pi home for a checkout
#   rig.sh live-read <label> <skill>...        one session reads every file of each skill
#   rig.sh services  up | down                 the scratch Dispatch, NATS and Envoy listener every run uses
#   rig.sh run       <scenario> <label> <n>    one fresh agent
#   rig.sh batch     <scenario> <runs> <label>...  runs 1..<runs> per label, SKILL_SCENARIOS_PARALLEL at once
#   rig.sh score     [<scenario>]               one row per run, then pass counts per label
#
# Scenarios:
#   ask-on-message  A plain session (no Legion role) is told to ask Sami whether to go ahead with a
#                   plan an owner session posted in a Dispatch message. The dispatch skill's rule:
#                   the ask carries the plan and its options itself and never points at the message
#                   in prose. Each run seeds its own issue, attached table and message.
#   tester-proof    A tester phase worker, booted by the Legion extension against a daemon stand-in,
#                   is told the implementer has finished. The legion-worker skill's rule: the PR body
#                   gets an `E2E (tester)` line, then `handoff write --phase test` with a proof, then
#                   a push carrying .legion/test.json, then `handoff complete`. GitHub is a recording
#                   stand-in and the remote a local bare repository.
#
# Dispatch is the e2e harness's real Go server (packages/dispatch/e2e/run-server.sh) on a Postgres
# container of its own, seeded through its API (seed.ts); NATS is a container and the Envoy
# listener is built from this checkout. Every port the rig binds is in 27000-27999.
#
# Operator input (no default in this repository):
#   LEGION_E2E_MODEL_GATEWAY_URL  profile: the model gateway (scripts/e2e/lib/install-model-gateway.sh)
# Optional:
#   SKILL_SCENARIOS_WORK      work directory, outside any checkout (default ${TMPDIR:-/tmp}/skill-scenarios)
#   SKILL_SCENARIOS_PARALLEL  concurrent runs in a batch (default 5)
#   SKILL_SCENARIOS_TIMEOUT   seconds one run may take (default 1500)
#
# Each run's agent is `omp -p` on the Oh My Pi both daemons pin (omp-pin.ts), under the label's own
# HOME and profile (make_omp_home, install-plugin-profile.sh, install-model-gateway.sh), in a
# private tmux server (`tmux -L skill-scenarios`), with `env -i` and only the variables its pane
# file names. A `gh` stand-in is first on every run's PATH, so no run reaches GitHub.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
root=$(cd "$here/../../../.." && pwd -P)
work=${SKILL_SCENARIOS_WORK:-${TMPDIR:-/tmp}/skill-scenarios}
work=$(realpath -m -- "$work")
socket=skill-scenarios

# The tester-proof world (worker_fixture below).
worker_key=LWEVAL-1
worker_repo=example/widgets
worker_pr_url=https://github.com/$worker_repo/pull/7

fail() {
  echo "skill-scenarios: $*" >&2
  exit 1
}

case "$work/" in "$root"/*) fail "SKILL_SCENARIOS_WORK $work is inside the checkout $root, where jj would snapshot it" ;; esac

# pick_port <variable>: sets <variable> to a port in 27000-27999 that nothing listens on and this
# process has not picked before.
picked=" "
pick_port() {
  local port
  for _ in $(seq 1 200); do
    port=$((27000 + RANDOM % 1000))
    case $picked in *" $port "*) continue ;; esac
    [ -z "$(ss -ltnH "sport = :$port")" ] || continue
    picked+="$port "
    printf -v "$1" '%s' "$port"
    return 0
  done
  fail "no free port in 27000-27999"
}
mkdir -p "$work"

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

# Runs `omp -p "$(cat $R/prompt.txt)"` in $R/cwd under $R/pane.env, with $R/system-prompt.md as the
# one --append-system-prompt when it exists, and waits for it to exit or time out.
launch() {
  local R=$1 name=$2 system=
  [ -f "$R/system-prompt.md" ] && system=" --append-system-prompt \"\$(cat $(printf %q "$R/system-prompt.md"))\""
  cat >"$R/launch.sh" <<EOF
#!/usr/bin/env bash
cd $(printf %q "$R/cwd")
mapfile -t vars <$(printf %q "$R/pane.env")
exec env -i "\${vars[@]}" mise x $(printf %q "$pin") -- omp -p "\$(cat $(printf %q "$R/prompt.txt"))"$system --session-dir $(printf %q "$R/sessions") >$(printf %q "$R/out.txt") 2>&1
EOF
  chmod +x "$R/launch.sh"
  tmux -L "$socket" kill-session -t "=$name" 2>/dev/null || true
  tmux -L "$socket" new-session -d -s "$name" -x 200 -y 50 "bash $(printf %q "$R/launch.sh"); echo exit=\$? >>$(printf %q "$R/out.txt")"
  local deadline=$((SECONDS + ${SKILL_SCENARIOS_TIMEOUT:-1500}))
  # The tmux command appends `exit=<status>` as out.txt's last line once omp has exited.
  while [ $SECONDS -lt $deadline ]; do
    tail -n 1 "$R/out.txt" 2>/dev/null | grep -q '^exit=' && break
    sleep 5
  done
  tail -n 1 "$R/out.txt" 2>/dev/null | grep -q '^exit=' || echo "exit=timeout" >>"$R/out.txt"
  tmux -L "$socket" kill-session -t "=$name" 2>/dev/null || true
}

cmd_live_read() {
  [ $# -ge 2 ] || fail "usage: rig.sh live-read <label> <skill>..."
  load_label "$1"
  shift
  local R=$work/live-read/${P##*/} paths=() skill file
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

cmd_services() {
  local S=$work/services
  case "${1:-}" in
  up)
    [ ! -f "$services_env" ] || fail "services are already up ($services_env); run \`rig.sh services down\` first"
    mkdir -p "$S"
    local prefix nats_port pg_port envoy_port dispatch_port fake_envoy fake_github
    prefix=skill-scenarios-$(printf '%s' "$work" | sha256sum | cut -c1-8)
    pick_port nats_port
    pick_port pg_port
    pick_port envoy_port
    pick_port dispatch_port
    pick_port fake_envoy
    pick_port fake_github
    printf '%s\n' "$prefix-nats" "$prefix-pg" >"$S/containers"
    docker run -d --name "$prefix-nats" -p "127.0.0.1:$nats_port:4222" nats:2.10 -js >/dev/null
    docker run -d --name "$prefix-pg" -e POSTGRES_PASSWORD=ci -p "127.0.0.1:$pg_port:5432" postgres:16 >/dev/null
    # The image's entrypoint runs a temporary server for initdb and restarts it, so readiness
    # counts only after the init log line.
    for _ in $(seq 1 120); do
      docker logs "$prefix-pg" 2>&1 | grep -q 'PostgreSQL init process complete' &&
        docker exec "$prefix-pg" pg_isready -U postgres -h 127.0.0.1 >/dev/null 2>&1 && break
      sleep 1
    done
    docker exec "$prefix-pg" pg_isready -U postgres -h 127.0.0.1 >/dev/null || fail "Postgres $prefix-pg never became ready"
    (cd "$root/packages/envoy" && go build -o "$S/envoy-listener" ./cmd/listener)
    (umask 077 && openssl rand -hex 24 >"$S/envoy-token")
    # The listener's port was picked above.
    ENVOY_API_TOKEN="$(<"$S/envoy-token")" PORT=$envoy_port ENVOY_LISTEN_HOST=127.0.0.1 ENVOY_MACHINE_ID="$prefix" \
      NATS_URLS="nats://127.0.0.1:$nats_port" nohup env -u NATS_NKEY_SEED -u NATS_NKEY_SEED_FILE "$S/envoy-listener" >"$S/listener.log" 2>&1 &
    echo $! >"$S/listener.pid"
    # Dispatch and the two harness ports its server names were picked above.
    DATABASE_URL="postgres://postgres:ci@127.0.0.1:$pg_port/postgres?sslmode=disable" DISPATCH_E2E_PORT=$dispatch_port \
      FAKE_ENVOY_PORT=$fake_envoy FAKE_GITHUB_PORT=$fake_github nohup bash "$root/packages/dispatch/e2e/run-server.sh" >"$S/dispatch.log" 2>&1 &
    echo $! >"$S/dispatch.pid"
    for _ in $(seq 1 60); do
      curl -fsS -H "Authorization: Bearer $(<"$S/envoy-token")" "http://127.0.0.1:$envoy_port/v1/sessions" >/dev/null 2>&1 && break
      sleep 1
    done
    curl -fsS -o /dev/null -H "Authorization: Bearer $(<"$S/envoy-token")" "http://127.0.0.1:$envoy_port/v1/sessions" || fail "the Envoy listener never answered; see $S/listener.log"
    for _ in $(seq 1 300); do
      curl -fsS -o /dev/null -H 'Authorization: Bearer e2e-token' "http://127.0.0.1:$dispatch_port/api/v1/whoami" 2>/dev/null && break
      sleep 1
    done
    curl -fsS -o /dev/null -H 'Authorization: Bearer e2e-token' "http://127.0.0.1:$dispatch_port/api/v1/whoami" || fail "Dispatch never answered; see $S/dispatch.log"
    printf '%s\n' "$dispatch_port" >"$S/dispatch-port"
    (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" project)
    printf 'e2e-token\n' >"$S/dispatch-token"
    chmod 0600 "$S/dispatch-token"
    {
      echo "ENVOY_URL=http://127.0.0.1:$envoy_port"
      echo "ENVOY_NATS_URL=nats://127.0.0.1:$nats_port"
      echo "ENVOY_TOKEN_FILE=$S/envoy-token"
      echo "DISPATCH_URL=http://127.0.0.1:$dispatch_port"
      echo "DISPATCH_TOKEN_FILE=$S/dispatch-token"
    } >"$services_env"
    echo "services up: listener :$envoy_port, NATS :$nats_port, Dispatch :$dispatch_port"
    ;;
  down)
    local pid container
    for pid in listener dispatch; do
      if [ -f "$S/$pid.pid" ]; then
        # run-server.sh execs `go run`, whose child is the server itself.
        pkill -P "$(<"$S/$pid.pid")" 2>/dev/null || true
        kill "$(<"$S/$pid.pid")" 2>/dev/null || true
      fi
      rm -f "$S/$pid.pid"
    done
    if [ -f "$S/containers" ]; then
      while IFS= read -r container; do docker rm -f "$container" >/dev/null 2>&1 || true; done <"$S/containers"
      rm -f "$S/containers"
    fi
    rm -f "$services_env"
    echo "services down"
    ;;
  *) fail "usage: rig.sh services up|down" ;;
  esac
}

run_ask_on_message() {
  local R=$1 name=$2 issue message dispatch_port
  [ -f "$services_env" ] || fail "the scenarios need the scratch Dispatch: run \`rig.sh services up\` first"
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
  local api=http://127.0.0.1:$dispatch_port/api/v1 auth='Authorization: Bearer e2e-token'
  curl -fsS -H "$auth" "$api/issues/$issue/messages/$message" >"$R/message.json"
  curl -fsS -H "$auth" "$api/issues/$issue/asks" >"$R/asks.json"
  curl -fsS -H "$auth" "$api/issues/$issue/events?limit=200" >"$R/events.json"
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
  jq -n --arg head "$C2" --arg body "$body" --arg url "$worker_pr_url" --arg ref "legion/$worker_key" '
    {number:7, url:$url, html_url:$url, title:"feat: greet() (LWEVAL-1)", state:"OPEN", isDraft:false,
     headRefName:$ref, headRefOid:$head, baseRefName:"main", body:$body, mergeable:"MERGEABLE",
     reviewDecision:"", head:{sha:$head, ref:$ref}, base:{ref:"main"},
     statusCheckRollup:[{name:"test", status:"COMPLETED", conclusion:"SUCCESS"},
                        {name:"lint", status:"COMPLETED", conclusion:"SUCCESS"}]} as $pr
    | {routes:[
      {match:"^legion threads resolve ", stdout:"no unresolved review threads on #7"},
      {match:"^gh pr view", stdout:$pr},
      {match:"^gh pr checks", stdout:"lint\tpass\t10s\thttps://example.invalid/1\ntest\tpass\t20s\thttps://example.invalid/2"},
      {match:"^gh pr (edit|comment|review|ready)", stdout:($url+"#issuecomment-1")},
      {match:"^gh api .*graphql", stdout:{data:{repository:{pullRequest:{reviewThreads:{pageInfo:{hasNextPage:false, endCursor:null}, nodes:[]}}}}}},
      {match:"^gh api .*(-X|--method) ?(POST|PATCH|PUT)", stdout:{id:6001, html_url:($url+"#issuecomment-6001")}},
      {match:"^gh api .*pulls/7/(reviews|comments)", stdout:[]},
      {match:"^gh api .*(check-runs|status)", stdout:{total_count:2, check_runs:[{name:"test", status:"completed", conclusion:"success"}, {name:"lint", status:"completed", conclusion:"success"}]}},
      {match:"^gh api .*pulls/7", stdout:$pr},
      {match:"^gh (api user|auth status)", stdout:{login:"rig[bot]"}}
    ]}' >"$R/fixtures.json"
  jq -n --arg C1 "$C1" --arg C2 "$C2" '{C1:$C1, C2:$C2, head:$C2}' >"$R/shas.json"
  printf 'Architect → tester, %s. The implementer has finished: PR #7 (%s, repository %s) is at %s, and its implement handoff is on the branch. Test it and finish your phase.\n' \
    "$worker_key" "$worker_pr_url" "$worker_repo" "$C2" >"$R/prompt.txt"
}

run_tester_proof() {
  local R=$1 name=$2 n=$3 state project token dport dpid
  [ -f "$services_env" ] || fail "the tester scenario needs the local services: run \`rig.sh services up\` first"
  worker_fixture "$R" >"$R/fixture.log" 2>&1 || fail "the fixture failed; see $R/fixture.log"
  mkdir -p "$R/cwd"
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
  token=$(cd "$root" && bun -e "import { roleToken } from './packages/contracts/src/index.ts'; console.log(roleToken('$project', '$worker_key', 'tester'))")
  local roles=$co/packages/pi-envoy/roles
  {
    cat "$roles/core/common.md" "$roles/core/tester.md" "$roles/mechanics/headless.md" "$roles/tester.md"
    printf '\n\n'
    (cd "$root" && bun -e "import { addressingFragment } from './packages/daemon/src/daemon/processes.ts'; console.log(addressingFragment('$project', '$worker_key', '$worker_key', 'tester'))")
  } >"$R/system-prompt.md"
  pick_port dport
  bun "$here/../grant-rig/daemon-standin.ts" "$dport" "$R/daemon.log" "$state/secrets/boot" "$project" "$worker_key" tester >"$R/daemon.out" 2>&1 &
  dpid=$!
  for _ in $(seq 1 50); do grep -q '^listening on ' "$R/daemon.out" 2>/dev/null && break; sleep 0.2; done
  grep -q '^listening on ' "$R/daemon.out" || fail "the daemon stand-in did not start; see $R/daemon.out"
  base_env "$R"
  cat "$services_env" >>"$R/pane.env"
  {
    echo "LEGION_TREE=$worker_key"
    echo "LEGION_ISSUE=$worker_key"
    echo "LEGION_ROLE=tester"
    echo "LEGION_GENERATION=1"
    echo "LEGION_PROJECT=$project"
    echo "LEGION_DAEMON_URL=http://127.0.0.1:$dport"
    echo "LEGION_STATE_DIR=$state"
    echo "LEGION_WORKSPACE=$R/ws"
    echo "LEGION_BOOT_TOKEN_FILE=$state/secrets/boot"
    echo "LEGION_GRANT_FILE=$state/secrets/$token-grant"
    echo "GIT_CONFIG_COUNT=0"
    echo "GIT_TERMINAL_PROMPT=0"
    echo "JJ_USER=Rig Worker"
    echo "JJ_EMAIL=rig@example.invalid"
    echo "JJ_CONFIG=$R/jj.toml"
    echo "GH_CONFIG_DIR=$state/gh"
    echo "GH_REPO=$worker_repo"
  } >>"$R/pane.env"
  gh_standin "$R"
  launch "$R" "$name"
  kill "$dpid" 2>/dev/null || true
}

cmd_run() {
  [ $# = 3 ] || fail "usage: rig.sh run <ask-on-message|tester-proof> <label> <n>"
  local scenario=$1 n=$3 R name
  load_label "$2"
  [[ $n =~ ^[0-9]+$ ]] || fail "run number '$n' is not a number"
  R=$work/runs/$scenario-${P##*/}-$n
  name=$scenario-${P##*/}-$n
  rm -rf "$R"
  mkdir -p "$R/bin" "$R/sessions" "$R/cwd"
  cp "$P/built-from" "$R/built-from"
  case $scenario in
  ask-on-message) run_ask_on_message "$R" "$name" ;;
  tester-proof) run_tester_proof "$R" "$name" "$n" ;;
  *) fail "unknown scenario $scenario" ;;
  esac
  echo "$name: $(tail -n 1 "$R/out.txt")"
}

cmd_batch() {
  [ $# -ge 3 ] || fail "usage: rig.sh batch <scenario> <runs> <label>..."
  local scenario=$1 runs=$2 label n
  shift 2
  for label in "$@"; do load_label "$label"; done
  for label in "$@"; do for n in $(seq 1 "$runs"); do printf '%s %s %s\n' "$scenario" "$label" "$n"; done; done |
    xargs -P "${SKILL_SCENARIOS_PARALLEL:-5}" -L 1 bash "$here/rig.sh" run
}

case "${1:-}" in
profile) shift && cmd_profile "$@" ;;
live-read) shift && cmd_live_read "$@" ;;
services) shift && cmd_services "$@" ;;
run) shift && cmd_run "$@" ;;
batch) shift && cmd_batch "$@" ;;
score) shift && bun "$here/score.ts" runs "$work/runs" "$@" ;;
*) sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//' && exit 2 ;;
esac
