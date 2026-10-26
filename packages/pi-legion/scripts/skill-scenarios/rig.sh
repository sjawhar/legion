#!/usr/bin/env bash
# Skill scenarios: fresh Oh My Pi agents, each running one checkout's packed plugins (and so that
# checkout's skills), each given one task a skill rule governs, scored afterwards from what the
# agent actually wrote. Two checkouts under the same scenario compare two versions of a skill.
#
#   rig.sh profile   <label> <checkout>            build the isolated Oh My Pi home for a checkout
#   rig.sh live-read <label> <skill>...            one session reads every file of each skill
#   rig.sh batch     <scenario> <runs> <label>...  starts the scratch services, runs 1..<runs> per
#                                                  label (SKILL_SCENARIOS_PARALLEL at once), and
#                                                  stops the services
#   rig.sh score     [<scenario>]                  one row per run, then counts per label
#   rig.sh judge     <run dir>...                  the judge's prompts on each spec-writing or
#                                                  spec-rewrite run (judge.ts run), each in a fresh
#                                                  Oh My Pi session with no Dispatch
#   rig.sh judge-omp <ste|bare> <dir> <output>     one fresh judge session: `omp -p` of
#                                                  <dir>/prompt.txt in <dir>, under the judge's
#                                                  profile (ste: asd-ste100 its one skill; bare: no
#                                                  skill), in a Docker container that sees only
#                                                  <dir> (read-only but for <dir>/<output> and
#                                                  <dir>/sessions), output to <dir>/out.txt, the
#                                                  model and thinking level to <dir>/judge-model.txt
#
# A batch runs each run as `rig.sh run <scenario> <label> <n> <index> <start>`, which needs that
# batch's services: <index> is the run's place in the batch and <start> the batch's start in epoch
# seconds. One run on its own is `rig.sh batch <scenario> 1 <label>`.
#
# The tools of this directory (each one's header has its own usage):
#   seed.ts               seeds a batch's scratch Dispatch and each run's issue, and reads back what
#                         the run left there (rig.sh runs it)
#   score.ts              scores each run from its transcript and what seed.ts read back
#   judge.ts              the person stand-in that answers a spec run's asks (`answer <run dir>
#                         <turn>`), and the judge's prompts on a finished run (`run <run dir>...`)
#   judge-prompts.ts      the ten prompts judge.ts gives the judge, the stand-in's among them
#   label-comments.ts --since <t> --until <t> --out <dir> (--gateway <url> --key-command <file> |
#                     --key-file <file>)
#                         labels people's production Dispatch comments in the window with the
#                         INFRA-140 questions; writes <dir>/labels.jsonl, prints the counts
#   fetch-spec-sources.ts --out <dir> <issue key | dispatch://<PROJECT>/artifact/<slug>>...
#                         reads each source from production Dispatch into <dir>/<name>/
#                         (title.txt, messages.md, asks.json, and old-spec.md for an issue);
#                         --agent-texts --since <t> --until <t> --out <dir> writes
#                         <dir>/agent-texts.json
# label-comments.ts and fetch-spec-sources.ts read production Dispatch with the operator's own
# credentials, which this script unsets, so the operator runs them before a batch. Each refuses an
# --out inside a git or jj checkout (exit 2). Nothing they write is committed.
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
#                   configuration anywhere in its environment; the batch seeds project TODO once,
#                   with two unrelated issues (seed.ts brainstorm), so the agent must find it by
#                   search, never its own environment or the Dispatch HTTP API. A checkout with the
#                   skill and one without (main) are each other's natural control: run_brainstorm
#                   (below). Scored by brainstormSurface.
#   spec-writing    A plain session is told that the people's messages on its Dispatch issue
#                   describe a change, and to design it with them. The prompt names no skill. The
#                   run's issue holds the input of one source, which fetch-spec-sources.ts read
#                   from production Dispatch into $SKILL_SCENARIOS_WORK/sources/<name>/: each
#                   message and comment as a message by the person alice, and each answered ask as
#                   an ask another agent opened and alice answered, all in time order (seed.ts
#                   spec-writing). The run is a conversation with a person stand-in, run_spec (below).
#                   Judged by judge.ts, scored by specRow.
#   spec-rewrite    The same, on an issue whose spec is the source's old spec, with a prompt that
#                   asks for the change the newest message names. Scored by specRow.
#
# A scenario measures its rule only if a label whose skills lack the rule scores lower than one
# whose skills state it. So every comparison carries an ablate label: a checkout of the head with
# the scenario's scored rule deleted from its skills, never committed. measure-before-ask is the
# instrument: gate 2 lives in the dispatch skill alone, and its ablate arm scores below the head.
# ask-on-message and tester-proof are controls: their rules also live in dispatch-first and
# `dispatch ask --help`, and in the tester role prompt, so ablate scores as the head does
# and they cannot detect a lost skill rule. A new scenario is an instrument only once an ablate
# arm has scored lower on it.
#
# A batch's services are the e2e harness's real Go Dispatch server
# (packages/dispatch/e2e/run-server.sh) on a Postgres container of its own, seeded and read back
# through its API (seed.ts), a NATS container, and the Envoy listener built from this checkout.
# Docker assigns the containers' host ports, the listener and Dispatch bind ports
# scripts/e2e/lib/rig.sh picks, and the kernel assigns each run's daemon stand-in its port.
#
# Operator input (no default in this repository), for `profile`, `judge` and `judge-omp`:
#   LEGION_E2E_MODEL_ROUTE        gateway (the default) or key-file: how the agents reach Anthropic;
#                                 on the gateway route a judge's key command runs on the host, once
#                                 per judge, and its container gets only the token (cmd_judge_omp)
#   LEGION_E2E_MODEL_GATEWAY_URL  gateway: the model gateway (scripts/e2e/lib/install-model-gateway.sh)
#   LEGION_E2E_ANTHROPIC_KEY_FILE key-file: a 0600 file holding a plaintext Anthropic key
#                                 (scripts/e2e/lib/install-key-file-model-route.sh); the key command
#                                 that reads it lives in the label's profile directory, and removing
#                                 that directory removes it
# `judge` and `judge-omp` also need Docker: each judge runs in a container (cmd_judge_omp).
# Optional:
#   SKILL_SCENARIOS_WORK      work directory, outside any checkout, and with no `.omp` in it or in
#                             any directory above it (default ${TMPDIR:-/tmp}/skill-scenarios)
#   SKILL_SCENARIOS_PARALLEL  concurrent runs in a batch (default 5)
#   SKILL_SCENARIOS_TIMEOUT   seconds one run, or one turn of a spec run, may take (default 1500)
# For spec-writing and spec-rewrite:
#   SKILL_SCENARIOS_SOURCE      `batch`: a directory name under $SKILL_SCENARIOS_WORK/sources
#   SKILL_SCENARIOS_SUPERPOWERS `profile`: the superpowers plugin's directory; its writing-plans
#                               and brainstorming skills go into the label's profile, and a label
#                               built without it runs neither scenario
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
#     checkout this script runs from, and write the machine's /tmp, which nothing here cleans.
#     score.ts does not score a run that read outside its own label, or whose PR body carries
#     another run's line (unscored, testerProof).
#   - An exit stops what its command started (on_exit, stop_run, stop_batch).
#
# A spec run (run_spec) is a conversation of at most 8 turns. Turn 1 is the prompt. After each turn,
# `seed.ts state` writes the issue's open asks (none a seed opened) and the spec's open approval
# request to <run>/state-<turn>.json. Then:
#   1. An open approval request: `seed.ts approve` approves the spec as alice, and the session runs
#      one last turn, "The person approved the spec of <issue>."
#   2. Open asks: `judge.ts answer` has the person stand-in choose each answer
#      (<run>/answers-<turn>.jsonl, appended to answers.jsonl), `seed.ts answer` posts them as
#      alice, and the session continues, "The person answered your asks on <issue>."
#   3. Neither: the run ends.
# A later turn is `omp -p --continue` on the run's own session directory. On the pinned Oh My Pi,
# --continue opens the newest transcript in that directory that holds a turn
# (SessionManager.continueRecent), which in a run's directory is the run's one transcript, so each
# turn appends to it. The rig fails a run whose directory holds more than one. Each turn's output
# is <run>/out-<turn>.txt, and out.txt is the last turn's. turns.txt has one line per turn:
# `<turn> <first|answered|approved> <exit line>`. `seed.ts capture-spec` then reads back what the
# run wrote (seed.ts's header names each file), and `rig.sh judge` judges the run afterwards.
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

# omp_ancestor DIR prints DIR/.omp, or the .omp of the nearest directory above DIR that holds one,
# and fails when none holds one. Oh My Pi reads a `.omp` in any directory from a session's cwd up
# to / as that project's configuration, past the session's HOME. A run's cwd is under the work
# directory, so a `.omp` above it (the operator's own ~/.omp, say) gives every run the operator's
# plugins in place of its label's packed ones.
omp_ancestor() {
  local dir=$1
  while :; do
    if [ -e "$dir/.omp" ] || [ -L "$dir/.omp" ]; then
      printf '%s\n' "${dir%/}/.omp"
      return 0
    fi
    [ "$dir" != / ] || return 1
    dir=$(dirname -- "$dir")
  done
}

case "$work/" in "$root"/*) fail "SKILL_SCENARIOS_WORK $work is inside the checkout $root, where jj would snapshot it" ;; esac
if found=$(omp_ancestor "$work"); then
  fail "SKILL_SCENARIOS_WORK $work is under $found: Oh My Pi reads that .omp as the configuration of every run's cwd, so a run would load its plugins and not its label's; give a work directory with no .omp in it or above it"
fi

# The judge's model and thinking level, the same for every judge session (cmd_judge_omp), recorded
# in each session's judge-model.txt. The model is the one both model routes enable for a profile
# (install-model-gateway.sh, install-key-file-model-route.sh); the pinned Oh My Pi's catalog gives
# it the thinking levels low, medium, high, xhigh and max.
judge_model=anthropic/claude-opus-4-8
judge_thinking=high
# The container every judge runs in (cmd_judge_omp), by digest: Ubuntu 24.04, whose glibc runs the
# pinned omp binary. The digest is mirror.gcr.io's (AGENTS.md: Docker Hub images through Google's
# mirror).
judge_image=mirror.gcr.io/library/ubuntu@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55
# The superpowers skills a spec run's profile holds as its own user skills (cmd_profile).
superpowers_skills=(writing-plans brainstorming)

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

# install_model_route CHECKOUT PROFILE DIR installs the model route of the profile whose HOME is
# DIR/home, as LEGION_E2E_MODEL_ROUTE picks it, with the checkout's own installer:
#   gateway (the default)  the Hawk model gateway LEGION_E2E_MODEL_GATEWAY_URL names
#                          (scripts/e2e/lib/install-model-gateway.sh)
#   key-file               the 0600 file LEGION_E2E_ANTHROPIC_KEY_FILE names, read fresh on every
#                          call (scripts/e2e/lib/install-key-file-model-route.sh)
# Each route writes its call record to DIR/key. The key-file route's key command names the key
# file's path, so it goes to DIR/model-key, apart from the record: removing the profile (DIR), the
# one way to rebuild a label, removes it, and a failed install removes it at once.
install_model_route() {
  local co=$1 profile=$2 dir=$3
  case ${LEGION_E2E_MODEL_ROUTE:-gateway} in
  gateway)
    bash "$co/scripts/e2e/lib/install-model-gateway.sh" --profile "$profile" --home "$dir/home" --dest "$dir/key" --cache-dir "$dir/cache"
    ;;
  key-file)
    if [ -z "${LEGION_E2E_ANTHROPIC_KEY_FILE:-}" ]; then
      echo "LEGION_E2E_ANTHROPIC_KEY_FILE is unset: name a 0600 file holding a plaintext Anthropic key" >&2
      return 1
    fi
    bash "$co/scripts/e2e/lib/install-key-file-model-route.sh" --profile "$profile" --home "$dir/home" --dest "$dir/key" \
      --key-dir "$dir/model-key" --key-file "$LEGION_E2E_ANTHROPIC_KEY_FILE" || {
      rm -rf "$dir/model-key"
      return 1
    }
    ;;
  *)
    echo "LEGION_E2E_MODEL_ROUTE is '$LEGION_E2E_MODEL_ROUTE': it is gateway or key-file" >&2
    return 1
    ;;
  esac
}

# drop_unready_keys READY KEY_DIR... removes each KEY_DIR, a key-file route's key command
# (install_model_route's DIR/model-key), unless READY exists. A profile setup sets it as its exit
# handler (on_exit) before it installs a route: a setup that stops before its ready file keeps no
# profile that uses the key command, and install-key-file-model-route.sh asks its caller to remove
# the key command on every such exit. A ready profile keeps its key command, and uses it.
# shellcheck disable=SC2329 # on_exit's trap runs it
drop_unready_keys() {
  local ready=$1
  shift
  [ -f "$ready" ] || rm -rf -- "$@"
}

# checkout_commit CHECKOUT prints the commit the checkout's working copy stands on and that
# commit's parents, as `<commit> parents=<parent>,...`. A jj checkout (one that holds .jj) gives
# jj's working-copy commit. Any other checkout gives git's HEAD, with ` dirty` after it when
# `git status --porcelain` lists a change. A git worktree holds no .jj.
checkout_commit() {
  local line head parents status
  if [ -d "$1/.jj" ]; then
    line=$(jj -R "$1" log -r @ --no-graph -T 'commit_id ++ " parents=" ++ parents.map(|c| c.commit_id()).join(",")') || return 1
    printf '%s\n' "$line"
    return 0
  fi
  line=$(git -C "$1" show -s --format='%H %P' HEAD) || return 1
  read -r head parents <<<"$line"
  status=$(git -C "$1" status --porcelain) || return 1
  printf '%s parents=%s%s\n' "$head" "${parents// /,}" "${status:+ dirty}"
}

cmd_profile() {
  [ $# = 2 ] || fail "usage: rig.sh profile <label> <checkout>"
  local label=$1 P co profile=skills-$1
  P=$(label_dir "$label")
  [ ! -e "$P" ] || fail "$P exists; remove it to rebuild label $label"
  co=$(cd "$2" && pwd -P)
  [ -f "$co/scripts/e2e/lib/install-plugin-profile.sh" ] || fail "$co is not a Legion checkout"
  mkdir -p "$P"
  on_exit "$(printf 'drop_unready_keys %q %q' "$P/ready" "$P/model-key")"
  printf '%s\n' "$co" >"$P/checkout"
  printf '%s\n' "$profile" >"$P/profile"
  # What the label runs: the checkout's working-copy commit and a digest of its skills tree, the
  # files the packed plugins stage into their dist/skills, listed in byte order (LC_ALL=C) so the
  # digest is the same in every locale.
  local commit
  commit=$(checkout_commit "$co") || fail "cannot read the commit of $co with jj or git"
  {
    printf 'checkout %s\n' "$co"
    printf 'commit %s\n' "$commit"
    printf 'skills %s\n' "$(cd "$co" && find skills -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum | sha256sum | cut -d' ' -f1)"
  } >"$P/built-from"
  (
    # shellcheck source=/dev/null
    source "$co/scripts/e2e/lib/omp-home.sh"
    make_omp_home "$P/home"
  )
  # Both plugins: a tester-proof worker's pane loads the Legion extension, which exits a Legion
  # session that finds no Envoy plugin, and every plain session gets its Dispatch skills and tools
  # from the Envoy plugin.
  bash "$co/scripts/e2e/lib/install-plugin-profile.sh" --package pi-envoy --profile "$profile" --home "$P/home" --dest "$P/pi-envoy" >"$P/install.log" 2>&1 ||
    fail "install-plugin-profile.sh --package pi-envoy failed; see $P/install.log"
  bash "$co/scripts/e2e/lib/install-plugin-profile.sh" --package pi-legion --profile "$profile" --home "$P/home" --dest "$P/pi-legion" >>"$P/install.log" 2>&1 ||
    fail "install-plugin-profile.sh --package pi-legion failed; see $P/install.log"
  install_model_route "$co" "$profile" "$P" >>"$P/install.log" 2>&1 ||
    fail "the model route (${LEGION_E2E_MODEL_ROUTE:-gateway}) could not be installed; see $P/install.log"
  cp "$co/.omp-pin" "$P/pin"
  [ -z "${SKILL_SCENARIOS_SUPERPOWERS:-}" ] || install_superpowers "$P" "$profile" "$(<"$P/pin")"
  touch "$P/ready"
  trap - EXIT HUP INT TERM
  cat "$P/built-from"
}

# install_superpowers P PROFILE PIN copies SKILL_SCENARIOS_SUPERPOWERS's writing-plans and
# brainstorming skills into the profile's user skill directory, the one the pinned Oh My Pi reads
# for a named profile ($HOME/.omp/profiles/<profile>/agent/skills/<name>/SKILL.md). Then one
# `omp -p` under the profile reads skill://writing-plans, and the step fails when the reply holds
# `Unknown skill`, Oh My Pi's answer for a skill it did not load. It also fails when no tool result
# of the probe's transcript holds the skill's own heading, so a probe that read nothing does not
# pass. It records the plugin's version and each skill's digest in built-from. It marks the label
# with a `superpowers` file, which a spec run needs.
install_superpowers() {
  local P=$1 profile=$2 pin=$3 src=$SKILL_SCENARIOS_SUPERPOWERS skill dest probe version
  dest=$P/home/.omp/profiles/$profile/agent/skills
  mkdir -p "$dest"
  for skill in "${superpowers_skills[@]}"; do
    [ -f "$src/skills/$skill/SKILL.md" ] || fail "SKILL_SCENARIOS_SUPERPOWERS $src has no skills/$skill/SKILL.md"
    cp -RL "$src/skills/$skill" "$dest/$skill"
  done
  version=$(jq -r '.version // "unknown"' "$src/package.json" 2>/dev/null) || version=unknown
  {
    printf 'superpowers %s %s\n' "$src" "$version"
    for skill in "${superpowers_skills[@]}"; do
      printf 'superpowers-skill %s %s\n' "$skill" "$(cd "$dest/$skill" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum | sha256sum | cut -d' ' -f1)"
    done
  } >>"$P/built-from"
  # The read runs as a run does: the label's HOME and profile, no caller variable, an empty cwd, no
  # Dispatch, and an Envoy address nothing listens on.
  probe=$P/superpowers-probe
  mkdir -p "$probe/cwd" "$probe/tmp"
  (
    cd "$probe/cwd" &&
      env -i HOME="$P/home" USER="$USER" TERM=xterm-256color TMPDIR="$probe/tmp" PATH="$PATH" \
        MISE_DATA_DIR="${MISE_DATA_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/mise}" OMP_PROFILE="$profile" \
        PI_NOTIFICATIONS=off PI_NO_TITLE=1 ENVOY_URL=http://127.0.0.1:1 timeout "${SKILL_SCENARIOS_TIMEOUT:-1500}" \
        mise x "$pin" -- omp -p 'Call the read tool once on the path skill://writing-plans, and nothing else. Then reply with the first line of what it returned.' \
        --session-dir "$probe/sessions" </dev/null
  ) >"$probe/out.txt" 2>&1 || fail "the read of skill://writing-plans under profile $profile failed; see $probe/out.txt"
  ! grep -q 'Unknown skill' "$probe/out.txt" "$probe"/sessions/*.jsonl 2>/dev/null ||
    fail "profile $profile does not load skill://writing-plans (Unknown skill); see $probe/out.txt"
  jq -e -s --arg heading "$(grep -m1 '^# ' "$dest/writing-plans/SKILL.md")" \
    'any(.[]; .message.role? == "toolResult" and any(.message.content[]?; (.text // "") | contains($heading)))' \
    "$probe"/sessions/*.jsonl >/dev/null 2>&1 ||
    fail "the probe under profile $profile read no skill://writing-plans; see $probe/out.txt"
  : >"$P/superpowers"
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
# nothing under the agent's HOME) and records the call to $R/calls.jsonl as gh-standin.ts records
# its own. The score reads the tester's run of the CLI from that record. No `legion` stands in: an
# agent reaches the daemon stand-in through its `legion` tool alone, never `legion` from bash.
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

# launch NAME [OMP_ARG]... runs `omp -p "$(cat $R/prompt.txt)"` and then each OMP_ARG in $R/cwd
# under $R/pane.env, followed by $R/system-args (one --append-system-prompt argument, as shell text)
# when it exists, as the tmux session <tag>-<name>. It waits for the session to exit or time out. A
# timeout is the run's result (`exit=timeout`), not a rig failure, so this wait is not an
# until_true. A spec run's later turn passes --continue. Each launch starts out.txt again, so its
# wait reads only its own exit line.
launch() {
  local system='' extra=''
  session=$tag-$1
  shift
  [ $# = 0 ] || extra=" $(printf '%q ' "$@")" extra=${extra% }
  [ ! -f "$R/system-args" ] || system=" $(<"$R/system-args")"
  cat >"$R/launch.sh" <<EOF
#!/usr/bin/env bash
cd $(printf %q "$R/cwd")
mapfile -t vars <$(printf %q "$R/pane.env")
exec env -i "\${vars[@]}" mise x $(printf %q "$pin") -- omp -p "\$(cat $(printf %q "$R/prompt.txt"))"$extra$system --session-dir $(printf %q "$R/sessions") >$(printf %q "$R/out.txt") 2>&1
EOF
  chmod +x "$R/launch.sh"
  : >"$R/out.txt"
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

# skill_reads CHECKOUT SKILL prints the read path of each markdown file of the checkout's skill
# SKILL, in the order a live read reads them: skill://SKILL (its SKILL.md) first, then
# skill://SKILL/<path> for every other .md file under the skill's directory, in byte order.
skill_reads() {
  local dir=$1/skills/$2 file
  [ -f "$dir/SKILL.md" ] || fail "$1 has no skill $2"
  printf 'skill://%s\n' "$2"
  while IFS= read -r -d '' file; do
    printf 'skill://%s/%s\n' "$2" "${file#./}"
  done < <(cd "$dir" && find . -type f -name '*.md' ! -path ./SKILL.md -print0 | LC_ALL=C sort -z)
}

cmd_live_read() {
  [ $# -ge 2 ] || fail "usage: rig.sh live-read <label> <skill>..."
  load_label "$1"
  shift
  local paths=() skill list
  R=$work/live-read/${P##*/}
  on_exit stop_run
  rm -rf "$R"
  mkdir -p "$R/cwd" "$R/sessions"
  for skill in "$@"; do
    list=$(skill_reads "$co" "$skill") || exit 1
    mapfile -t -O "${#paths[@]}" paths <<<"$list"
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
    -e POSTGRES_PASSWORD=ci -p 127.0.0.1::5432 pgvector/pgvector@sha256:7b822b0aac60967beb1ea5e576b8602c94c300a157d187f385ae3e0da199b90a >/dev/null
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
  # The batch's world, seeded once: brainstorm's to-do CLI project, or the project every other
  # scenario's runs seed their own issue into. A batch holds only its own, so a brainstorm run's
  # search finds one project to file in.
  local seed=project
  [ "$batch_scenario" != brainstorm ] || seed=brainstorm
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" "$seed") >>"$S/logs/seed.log" 2>&1 ||
    fail "seed.ts $seed failed; see $S/logs/seed.log"
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

# dispatch_session NAME [CAPTURE] runs $R/prompt.txt as a plain session outside Legion, with
# Dispatch as its own and no Envoy (an address nothing listens on) or NATS, then reads back what it
# left on Dispatch, for the score, with `seed.ts CAPTURE` (default `capture`).
dispatch_session() {
  base_env
  standins
  grep '^DISPATCH_' "$services_env" >>"$R/pane.env"
  echo "ENVOY_URL=http://127.0.0.1:1" >>"$R/pane.env"
  launch "$1"
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$(<"$work/services/dispatch-port") bun "$here/seed.ts" "${2:-capture}" "$R/fixture.json" "$R") \
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

# A tiny to-do CLI in a scratch git repository with no Dispatch configuration and no word of due
# dates. The batch seeded its one Dispatch project (seed.ts brainstorm: TODO, with two unrelated
# issues whose titles name the "todo CLI", so a search for it finds the project), and
# `seed.ts capture-brainstorm` reads back the issue this run's own transcript created, for
# brainstormSurface.
run_brainstorm() {
  local name=$1
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
  dispatch_session "$name" capture-brainstorm
}

# seed_ts MODE ARG... runs `seed.ts MODE ARG...` against the batch's Dispatch, its output appended
# to $R/fixture.log. A failure is the rig's, never the run's.
seed_ts() {
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$(<"$work/services/dispatch-port") bun "$here/seed.ts" "$@") \
    >>"$R/fixture.log" 2>&1 || fail "seed.ts $1 failed; see $R/fixture.log"
}

# judge_ts MODE ARG... runs `judge.ts MODE ARG...` with SKILL_SCENARIOS_WORK at this rig's work
# directory, its output appended to $R/judge.log.
judge_ts() {
  SKILL_SCENARIOS_WORK=$work bun "$here/judge.ts" "$@" >>"$R/judge.log" 2>&1 ||
    fail "judge.ts $1 failed; see $R/judge.log"
}

# next_run SCENARIO LABEL prints one above the highest <n> of a run directory
# $work/runs/<SCENARIO>-<LABEL>-<n>, or 1 when there is none.
next_run() {
  local dir n highest=0
  for dir in "$work/runs/$1-$2-"*; do
    [ -d "$dir" ] || continue
    n=${dir##*/"$1-$2-"}
    [[ $n =~ ^[0-9]+$ ]] || continue
    ((10#$n <= highest)) || highest=$((10#$n))
  done
  printf '%s\n' "$((highest + 1))"
}

# source_dir SCENARIO NAME prints the directory of the source NAME under $work/sources, and fails,
# naming what is missing, when NAME is unset or not one directory name, or the source lacks a file
# seed.ts reads: title.txt, messages.md and asks.json, and old-spec.md for spec-rewrite.
source_dir() {
  local name=$2 dir file files=(title.txt messages.md asks.json)
  [ -n "$name" ] || fail "SKILL_SCENARIOS_SOURCE is unset: name a directory under $work/sources (fetch-spec-sources.ts writes them)"
  [[ $name =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || fail "SKILL_SCENARIOS_SOURCE '$name' is not a directory name under $work/sources"
  dir=$work/sources/$name
  [ -d "$dir" ] || fail "SKILL_SCENARIOS_SOURCE $name: $dir does not exist (fetch-spec-sources.ts writes it)"
  [ "$1" != spec-rewrite ] || files+=(old-spec.md)
  for file in "${files[@]}"; do
    [ -f "$dir/$file" ] || fail "the source $dir has no $file, which $1 needs"
  done
  printf '%s\n' "$dir"
}

# require_superpowers SCENARIO fails unless the label (P) was built with SKILL_SCENARIOS_SUPERPOWERS.
require_superpowers() {
  [ -f "$P/superpowers" ] ||
    fail "$1 needs a label built with SKILL_SCENARIOS_SUPERPOWERS (the superpowers plugin's directory): remove $P and run \`rig.sh profile ${P##*/} <checkout>\` again with it set"
}

# spec_turns NAME ISSUE PROMPT runs a spec run's conversation (the header's "A spec run"): the turn
# loop, at most 8 turns. It reads the open asks and the approval request from each turn's
# state-<turn>.json, which `seed.ts state` writes, and it fails the run when the session directory
# holds more than one transcript after a turn. A turn that does not exit 0 ends the run.
spec_turns() {
  local name=$1 issue=$2 turn=0 kind=first last transcripts
  local answered="The person answered your asks on $issue." approved="The person approved the spec of $issue."
  printf '%s\n' "$3" >"$R/prompt.txt"
  : >"$R/answers.jsonl"
  : >"$R/turns.txt"
  while :; do
    turn=$((turn + 1))
    if [ "$turn" = 1 ]; then launch "$name"; else launch "$name" --continue; fi
    cp "$R/out.txt" "$R/out-$turn.txt"
    last=$(tail -n 1 "$R/out.txt")
    printf '%s %s %s\n' "$turn" "$kind" "$last" >>"$R/turns.txt"
    transcripts=$(find "$R/sessions" -maxdepth 1 -name '*.jsonl' | wc -l)
    [ "$transcripts" -le 1 ] || fail "after turn $turn, $R/sessions holds $transcripts transcripts: --continue did not append to the run's one transcript"
    seed_ts state "$R/fixture.json" "$turn" "$R"
    [ "$kind" != approved ] && [ "$last" = exit=0 ] && [ "$turn" -lt 8 ] || break
    if [ "$(jq -r '.approval != null' "$R/state-$turn.json")" = true ]; then
      seed_ts approve "$R/fixture.json"
      kind=approved
      printf '%s\n' "$approved" >"$R/prompt.txt"
    elif [ "$(jq '.open_asks | length' "$R/state-$turn.json")" != 0 ]; then
      judge_ts answer "$R" "$turn"
      cat "$R/answers-$turn.jsonl" >>"$R/answers.jsonl"
      seed_ts answer "$R/fixture.json" "$R/answers-$turn.jsonl"
      kind=answered
      printf '%s\n' "$answered" >"$R/prompt.txt"
    else
      break
    fi
  done
  note "$name: $turn turns, the last $last"
}

# run_spec SCENARIO NAME: a spec-writing or spec-rewrite run on the source SKILL_SCENARIOS_SOURCE
# names. The source goes to $R/source/ and its name to $R/source.txt. seed.ts seeds the run's issue
# from it, spec_turns runs the conversation, and `seed.ts capture-spec` reads back what the run
# wrote. The prompts name no skill.
run_spec() {
  local scenario=$1 name=$2 source issue prompt
  source=$(source_dir "$scenario" "${SKILL_SCENARIOS_SOURCE:-}") || exit 1
  require_superpowers "$scenario"
  cp -R "$source" "$R/source"
  printf '%s\n' "${source##*/}" >"$R/source.txt"
  seed_ts "$scenario" "$R/fixture.json" "$R/source"
  issue=$(jq -r .issue "$R/fixture.json")
  case $scenario in
  spec-writing) prompt="On Dispatch issue $issue, the people's messages describe a change. Design it with them." ;;
  spec-rewrite) prompt="On Dispatch issue $issue, the newest message asks for a change. Make it." ;;
  esac
  base_env
  standins
  grep '^DISPATCH_' "$services_env" >>"$R/pane.env"
  echo "ENVOY_URL=http://127.0.0.1:1" >>"$R/pane.env"
  spec_turns "$name" "$issue" "$prompt"
  seed_ts capture-spec "$R/fixture.json" "$R"
}

# The judge's two profiles, under $work/judge-profiles/<kind>, each its own HOME made by
# make_omp_home with the model route install_model_route installs and no plugin. `ste` holds one
# user skill, this checkout's skills/asd-ste100. `bare` holds none. The first judge-omp call builds
# both, and every other call waits on the same lock, so parallel calls are safe.
judge_profiles() {
  local dir=$work/judge-profiles kind P commit
  mkdir -p "$dir"
  exec 9>"$dir.lock"
  flock 9
  if [ ! -f "$dir/ready" ]; then
    on_exit "$(printf 'drop_unready_keys %q %q %q' "$dir/ready" "$dir/ste/model-key" "$dir/bare/model-key")"
    [ -f "$root/skills/asd-ste100/SKILL.md" ] || fail "$root has no skills/asd-ste100, the judge's one skill"
    commit=$(checkout_commit "$root") || fail "cannot read the commit of $root with jj or git"
    for kind in ste bare; do
      P=$dir/$kind
      rm -rf "$P"
      mkdir -p "$P"
      (
        # shellcheck source=/dev/null
        source "$root/scripts/e2e/lib/omp-home.sh"
        make_omp_home "$P/home"
      ) >"$P/install.log" 2>&1 || fail "make_omp_home failed for the judge's $kind profile; see $P/install.log"
      install_model_route "$root" "judge-$kind" "$P" >>"$P/install.log" 2>&1 ||
        fail "the judge's model route (${LEGION_E2E_MODEL_ROUTE:-gateway}) could not be installed; see $P/install.log"
    done
    mkdir -p "$dir/ste/home/.omp/profiles/judge-ste/agent/skills"
    cp -RL "$root/skills/asd-ste100" "$dir/ste/home/.omp/profiles/judge-ste/agent/skills/asd-ste100"
    cp "$root/.omp-pin" "$dir/pin"
    {
      printf 'model %s\nthinking %s\npin %s\n' "$judge_model" "$judge_thinking" "$(<"$dir/pin")"
      printf 'commit %s\n' "$commit"
      printf 'asd-ste100 %s\n' "$(cd "$root/skills/asd-ste100" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum | sha256sum | cut -d' ' -f1)"
    } >"$dir/built-from"
    touch "$dir/ready"
    trap - EXIT HUP INT TERM
  fi
  exec 9>&-
}

# judge_keys P AGENT KEYS writes into KEYS (0700) what one judge's container needs to reach the
# model, for the route LEGION_E2E_MODEL_ROUTE names, and nothing any other judge shares writably:
#   anthropic-key  a key command that prints the key, run in the container
#   models.yml     the profile's models.yml, naming /opt/key/anthropic-key in place of the host's
#                  key command
# key-file: the key command reads the key file on each call (the file is mounted read-only at its
# own path), so a key the operator rotates mid-run reaches the judge. It records no call: the
# host's key command appends to a call record every judge of the profile would share writably.
# gateway: the host's key command runs hawk-token, which needs the operator's home and session
# bus, so it runs here, once. Its token goes to KEYS/token (0600), which the key command prints.
# That key command serves a kept token until shortly before it expires, so a token that would
# expire before the judge's deadline is dropped and minted once more. Prints the key file's path
# on the key-file route, for the caller to mount.
judge_keys() {
  local P=$1 agent=$2 keys=$3 host_command token exp deadline payload key_file read_from
  (umask 077 && mkdir -p -- "$keys") || fail "judge-omp: cannot make $keys"
  if [ "${LEGION_E2E_MODEL_ROUTE:-gateway}" = key-file ]; then
    host_command=$P/model-key/anthropic-key
    [ -f "$host_command" ] || fail "judge-omp: the key-file profile $P has no key command $host_command"
    # The key command assigns the key file's path with printf %q: read it as bash reads that line.
    key_file=$(bash -c "$(grep -m1 '^key_file=' "$host_command")"$'\nprintf %s "$key_file"')
    [ -f "$key_file" ] || fail "judge-omp: the key command names '$key_file', which is not a file"
    case $key_file in *[!A-Za-z0-9/._-]*) fail "judge-omp: the key file's path '$key_file' holds a character a key command cannot name plainly" ;; esac
    read_from=$key_file
  else
    host_command=$P/key/hawk-token
    [ -x "$host_command" ] || fail "judge-omp: the gateway profile $P has no key command $host_command"
    token=$("$host_command" 2>/dev/null) && [ -n "$token" ] ||
      fail "judge-omp: the gateway's key command gave no key; see $P/key/hawk-token.log"
    deadline=$((EPOCHSECONDS + ${SKILL_SCENARIOS_TIMEOUT:-1500}))
    payload=${token#*.}
    payload=${payload%%.*}
    payload=$(printf '%s' "$payload" | tr '_-' '/+')
    while [ $((${#payload} % 4)) != 0 ]; do payload+='='; done
    exp=$(printf '%s' "$payload" | base64 -d 2>/dev/null | jq -r '.exp // empty | numbers | floor' 2>/dev/null) || exp=
    if [ -n "$exp" ] && [ "$exp" -lt "$deadline" ]; then
      rm -f -- "$P/cache/hawk-token.key"
      token=$("$host_command" 2>/dev/null) && [ -n "$token" ] ||
        fail "judge-omp: the gateway's key command gave no key; see $P/key/hawk-token.log"
    fi
    (umask 077 && printf '%s\n' "$token" >"$keys/token")
    read_from=/opt/key/token
  fi
  # $(cat) drops trailing newlines and keeps a key whose file has none, as the host's key command
  # does with $(<file).
  (umask 077 && printf '#!/bin/sh\nkey=$(cat %s) && [ -n "$key" ] && printf "%%s\\n" "$key"\n' "$read_from" >"$keys/anthropic-key")
  chmod 0700 "$keys/anthropic-key"
  sed "s#!exec $host_command\"#!exec /opt/key/anthropic-key\"#g" "$agent/models.yml" >"$keys/models.yml"
  grep -q '!exec /opt/key/anthropic-key"' "$keys/models.yml" && ! grep -qF "$host_command" "$keys/models.yml" ||
    fail "judge-omp: $agent/models.yml does not name the key command $host_command"
  [ -z "${key_file:-}" ] || printf '%s\n' "$key_file"
}

# cmd_judge_omp KIND DIR OUTPUT runs one fresh judge session in DIR on DIR/prompt.txt: no plugins,
# the KIND profile's skills alone (ste: asd-ste100; bare: none), the judge's pinned model and
# thinking level, its transcript under DIR/sessions, its output to DIR/out.txt with a last line
# exit=<n>. It exits with omp's status.
#
# The judge runs in a Docker container of judge_image, as the operator's uid, and sees only what it
# needs. A judge with the whole filesystem read the run's transcript and spec versions when its
# directory held no input, and returned rows it made up from them. Mounted:
#   DIR                     read-only, so the judge reads its inputs and nothing beside them
#   DIR/OUTPUT, DIR/sessions  writable: its one answer file and its transcript
#   the pinned omp's install read-only (SKILL_SCENARIOS_JUDGE_OMP names another binary, for tests)
#   the profile's agent directory read-only, copied into a fresh tmpfs HOME at start, where Oh My
#     Pi writes its databases and logs; the operator's native module cache read-only
#   the judge's own key directory read-only at /opt/key (judge_keys): a key command and a
#     models.yml that names it, and on the gateway route the token. It sits beside DIR, is the
#     owner's alone (0700, the token 0600), and is removed when the judge ends. On the key-file
#     route the key file is mounted read-only too, and the key command reads it on each call.
#     Nothing writable is shared between judges: one judge cannot leave a file for another to read.
#   on the gateway route, the gateway's host name resolved here, since a container cannot resolve
#     a tailnet name itself
# The network is Docker's default bridge, for the model's endpoint. SKILL_SCENARIOS_JUDGE_ENV adds
# `NAME=value` words to the container's environment, for tests.
cmd_judge_omp() {
  [ $# = 3 ] || fail "usage: rig.sh judge-omp <ste|bare> <dir> <output file name>"
  local kind=$1 dir output=$3 skills status=0 omp P agent natives keys key_file h=/home/judge
  case $kind in ste) skills=(--skills asd-ste100) ;; bare) skills=(--no-skills) ;; *) fail "judge-omp: '$kind' is ste or bare" ;; esac
  dir=$(cd "$2" && pwd -P) || fail "judge-omp: $2 is not a directory"
  [ -f "$dir/prompt.txt" ] || fail "judge-omp: $dir has no prompt.txt"
  case $output in
  "" | . | .. | */* | prompt.txt | sessions | out.txt | judge-model.txt | .tmp) fail "judge-omp: '$output' is not the name of an answer file in $dir" ;;
  esac
  case "$(omp_ancestor "$dir" || true)" in "") ;; *) fail "judge-omp: $dir is under $(omp_ancestor "$dir"), whose plugins Oh My Pi would load" ;; esac
  case ${LEGION_E2E_MODEL_ROUTE:-gateway} in gateway | key-file) ;; *) fail "judge-omp: LEGION_E2E_MODEL_ROUTE is '$LEGION_E2E_MODEL_ROUTE': it is gateway or key-file" ;; esac
  command -v docker >/dev/null || fail "judge-omp: docker is not on PATH; each judge runs in a container"
  judge_profiles
  P=$work/judge-profiles/$kind
  agent=$P/home/.omp/profiles/judge-$kind/agent
  local route=() host ip
  keys=$dir.key
  rm -rf -- "$keys"
  on_exit "$(printf 'rm -rf -- %q' "$keys")"
  key_file=$(judge_keys "$P" "$agent" "$keys") || exit 1
  route=(-v "$keys:/opt/key:ro")
  if [ -n "$key_file" ]; then
    route+=(-v "$key_file:$key_file:ro")
  else
    host=$(sed -n 's#^ *baseUrl: *https://\([^/:]*\).*#\1#p' "$agent/models.yml" | head -1)
    ip=$(getent hosts "$host" 2>/dev/null | awk '{print $1; exit}') || ip=
    [ -z "$ip" ] || route+=(--add-host "$host:$ip")
  fi
  printf 'model %s\nthinking %s\n' "$judge_model" "$judge_thinking" >"$dir/judge-model.txt"
  mkdir -p "$dir/sessions"
  : >>"$dir/$output"
  omp=${SKILL_SCENARIOS_JUDGE_OMP:-$(mise where "$(<"$work/judge-profiles/pin")")/bin/omp}
  [ -x "$omp" ] || fail "judge-omp: $omp is not an executable omp"
  natives=$(readlink -f "$P/home/.omp/natives")
  local extra=() word
  for word in ${SKILL_SCENARIOS_JUDGE_ENV:-}; do extra+=(-e "$word"); done
  # Pulled here, quietly, so a first run's pull progress never lands in the judge's out.txt, and a
  # failed pull names itself.
  docker image inspect "$judge_image" >/dev/null 2>&1 || docker pull --quiet "$judge_image" >/dev/null ||
    fail "judge-omp: docker could not pull $judge_image"
  docker run --rm --init --user "$(id -u):$(id -g)" --read-only \
    --tmpfs /tmp:exec,mode=1777 --tmpfs "$h:exec,uid=$(id -u),gid=$(id -g)" \
    -v "$(dirname "$omp"):/opt/omp:ro" -v "$natives:/opt/natives:ro" -v "$agent:/opt/profile:ro" \
    "${route[@]}" \
    -v "$dir:$dir:ro" -v "$dir/sessions:$dir/sessions" -v "$dir/$output:$dir/$output" \
    -w "$dir" -e HOME="$h" -e USER=judge -e TERM=xterm-256color -e TMPDIR=/tmp -e OMP_PROFILE="judge-$kind" \
    -e PI_NOTIFICATIONS=off -e PI_NO_TITLE=1 -e ENVOY_URL=http://127.0.0.1:1 "${extra[@]}" \
    "$judge_image" bash -c '
      a=$HOME/.omp/profiles/$OMP_PROFILE/agent
      mkdir -p "$a" && cp /opt/profile/config.yml "$a"/ || exit 125
      cp /opt/key/models.yml "$a"/ || exit 125
      if [ -d /opt/profile/skills ]; then cp -r /opt/profile/skills "$a"/ || exit 125; fi
      ln -s /opt/natives "$HOME/.omp/natives" || exit 125
      exec timeout "$1" "$2" "${@:3}"' - "${SKILL_SCENARIOS_TIMEOUT:-1500}" "/opt/omp/$(basename "$omp")" \
    -p "$(<"$dir/prompt.txt")" --no-extensions "${skills[@]}" \
    --model "$judge_model" --thinking "$judge_thinking" --session-dir "$dir/sessions" </dev/null >"$dir/out.txt" 2>&1 || status=$?
  echo "exit=$status" >>"$dir/out.txt"
  rm -rf -- "$keys"
  return "$status"
}

# cmd_judge RUN... runs judge.ts's prompts on each spec run.
cmd_judge() {
  [ $# -ge 1 ] || fail "usage: rig.sh judge <run dir>..."
  SKILL_SCENARIOS_WORK=$work exec bun "$here/judge.ts" run "$@"
}

# The tester's frozen world under $R: a bare remote whose post-receive hook logs every push, the
# issue workspace cloned from it, the implementer's commit and handoff on legion/<key>, the PR the
# gh stand-in serves, and the architect's assignment. The fixture's content is the same in every
# run; the PR number is 1000 plus <index>, the run's index in its batch, and the commits are dated
# <index> seconds after <start>, the batch's start, so no two runs of a batch share a PR number or
# a head (two runs can meet in a scratch file in the shared /tmp), and each batch's heads carry its
# own start time.
# The CLI must meet every acceptance criterion the seeded spec lists. testerProof fails a run whose
# push changes more than .legion/<key>/test.json, and it cannot tell a tester that pinned a real defect
# with a red test, which roles/core/tester.md asks for, from one that went off-task. A defect left
# in the fixture turns correct testing into a fail.
worker_fixture() {
  local index=$1 start=$2 src C1 C2 body when worker_pr worker_pr_url jj_setup_config
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
  # The implement handoff as the implementer writes it with its write tool: the four stamps
  # (schemaVersion, phase, issue, completed) and the phase's fields, at .legion/<key>/implement.json,
  # the file whose pushed commit the implementer's handoff_complete reports and the tester reads with `read`.
  mkdir -p "$src/.legion/$worker_key"
  jq -n --arg key "$worker_key" --arg head "$C1" --arg completed "$(date -u -d "@$((start + index))" +%FT%TZ)" '
    {schemaVersion:1, phase:"implement", issue:$key, completed:$completed,
     filesChanged:["greet.ts","greet.test.ts"],
     proof:[{criterion:"greet(name) greets by name, trimmed, and `bun greet.ts <name>` prints it; no name is a usage error",
       surface:"the CLI", command:"bun greet.ts Ada", observed:"prints Hello, Ada! and exits 0", headSha:$head,
       negativeControl:"bun greet.ts → exit 2, usage: greet.ts <name> on stderr"}]}' >"$src/.legion/$worker_key/implement.json"
  g add -A && g commit -qm "implement: record handoff ($worker_key)"
  C2=$(g rev-parse HEAD)
  g push -q "$R/remote.git" "legion/$worker_key"
  : >"$R/pushes.log"
  # Isolated from the operator's own jj configuration (fsmonitor.backend = "watchman" on a devbox
  # that runs it): a bare JJ_CONFIG naming only fsmonitor.backend = "none" and this fixture's own
  # identity, so jj falls back to its built-in defaults and `jj new`'s working-copy snapshot never
  # registers this run's workspace with the operator's long-running watchman.
  jj_setup_config=$R/jj-setup.toml
  printf '[user]\nname = "Rig Worker"\nemail = "rig@example.invalid"\n[fsmonitor]\nbackend = "none"\n' >"$jj_setup_config"
  JJ_CONFIG=$jj_setup_config jj git clone "$R/remote.git" "$R/ws" >/dev/null 2>&1
  (cd "$R/ws" && JJ_CONFIG=$jj_setup_config jj bookmark track "legion/$worker_key" --remote origin && JJ_CONFIG=$jj_setup_config jj new "legion/$worker_key") >/dev/null 2>&1
  body="Dispatch: $worker_key

Adds \`greet(name)\` and \`bun greet.ts <name>\`.

## Verification

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
  printf '[user]\nname = "Rig Worker"\nemail = "rig@example.invalid"\n[ui]\npaginate = "never"\n' >"$R/jj.toml"
  # Port 0: the kernel assigns the stand-in's port, which it prints, so concurrent runs never race.
  start_process daemon bun "$here/daemon-standin.ts" 0 "$R/daemon.log" "$state/secrets/boot" "$project" "$worker_key" tester
  await_start daemon "$daemon_pid" 0 30 "the daemon stand-in to listen" grep -q '^listening on ' "$R/logs/daemon.log"
  port=$(sed -n 's|^listening on http://127\.0\.0\.1:||p' "$R/logs/daemon.log")
  base_env
  standins
  # The pane the label's checkout's daemon would give the tester, over the base every agent gets:
  # worker.env and system-args. standins runs first, since worker-pane.ts names its $R/bin/gh, the
  # recording stand-in, as the pane's real gh.
  bun "$here/worker-pane.ts" "$R" "$R/pane.env" "$port" "$profile" "$project" "$worker_key" tester "$co"
  {
    cat "$R/worker.env" "$services_env"
    echo "GIT_CONFIG_COUNT=0"
    echo "JJ_USER=Rig Worker"
    echo "JJ_EMAIL=rig@example.invalid"
    echo "JJ_CONFIG=$R/jj.toml"
    echo "GH_REPO=$worker_repo"
  } >"$R/pane.env"
  launch "$name"
}

cmd_run() {
  [ $# = 5 ] || fail "usage: rig.sh run <ask-on-message|measure-before-ask|tester-proof|brainstorm|spec-writing|spec-rewrite> <label> <n> <index> <start>"
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
  spec-writing | spec-rewrite) run_spec "$scenario" "$name" ;;
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
  case $batch_scenario in ask-on-message | measure-before-ask | tester-proof | brainstorm | spec-writing | spec-rewrite) ;; *) fail "unknown scenario $batch_scenario" ;; esac
  [[ $runs =~ ^[0-9]+$ ]] || fail "run count '$runs' is not a number"
  shift 2
  # A batch's runs share one Dispatch, and every brainstorm run files the same feature: a second run
  # finds the first one's issue and extends it, or Dispatch refuses its own as a duplicate, so its
  # score measures its sibling's timing. Compare labels across batches of one run each. A spec run's
  # agent searches Dispatch as well, and would find a sibling's issue on the same source.
  case $batch_scenario in brainstorm | spec-writing | spec-rewrite)
    [ $((runs * $#)) = 1 ] ||
      fail "a $batch_scenario batch takes one run of one label (got $runs of $#); run each label as its own batch"
    ;;
  esac
  for label in "$@"; do load_label "$label"; done
  # A spec run reads its source and the label's superpowers skills: refused here, before any
  # service starts. Its number is one above the label's highest, so no batch removes an earlier run.
  local first=1
  case $batch_scenario in spec-writing | spec-rewrite)
    source_dir "$batch_scenario" "${SKILL_SCENARIOS_SOURCE:-}" >/dev/null
    require_superpowers "$batch_scenario"
    first=$(next_run "$batch_scenario" "$1")
    ;;
  esac
  lock_services
  on_exit stop_batch
  services_up
  start=$(date +%s)
  for label in "$@"; do
    for n in $(seq "$first" $((first + runs - 1))); do
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
judge) shift && cmd_judge "$@" ;;
judge-omp) shift && cmd_judge_omp "$@" ;;
*) sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//' && exit 2 ;;
esac; exit
