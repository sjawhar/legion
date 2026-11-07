#!/usr/bin/env bash
# Skill scenarios: fresh Oh My Pi agents, each running one checkout's packed plugins (and so that
# checkout's skills), each given one task a skill rule governs, scored afterwards from what the
# agent actually wrote. Two checkouts under the same scenario compare two versions of a skill.
#
#   rig.sh profile   <label> <checkout> [--envoy-json <file>]
#                                                  build the isolated Oh My Pi home for a checkout;
#                                                  <file>, a JSON object, is merged into the envoy.json
#                                                  the label's sessions read
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
#                   a recording stand-in, the remote a local bare repository, and the implementer's
#                   handoff a record on the run's own Dispatch issue (seed.ts tester-proof and
#                   implement-handoff). After the run, seed.ts capture-handoffs reads back every
#                   handoff record of the issue. Scored by testerProof.
#   reviewer-text-findings
#                   A reviewer phase worker, booted as tester-proof boots its tester, is told the
#                   tester has finished; its world is worker_fixture's with the reviewer's four
#                   additions: the change under review carries two text findings and no
#                   behavioural one (a sentence of docs/cli.md the change makes false, and a
#                   comment of .github/workflows/greet.yml naming a trigger the file does not
#                   carry, a file the implement App may push and the review App may not), the gh
#                   stand-in serves the pull request's one unresolved bot thread and answers
#                   `resolveReviewThread` as GitHub answers a role with write access, the run's
#                   issue holds
#                   the tester's passing result at the head as well as the implementer's (seed.ts
#                   tester-proof, implement-handoff and tester-result), so no gate of the
#                   reviewer's own refuses an approval, and the role booted is reviewer.
#                   SKILL_SCENARIOS_REVIEWER_CASE names the world: `text-only`, the change above,
#                   whose code is correct, and `defect`, that change with the CLI's blank-name
#                   guard dropped — a finding whose closing changes code that runs — which is the
#                   negative control. The case is in the run's name,
#                   reviewer-text-findings-<case>-<label>-<n>, so each case's batch keeps its own
#                   runs, and each run writes the case to <run>/case.txt. `base`, the head with
#                   the reviewer's verdict rule deleted from its skills, is this scenario's ablate
#                   arm, and the scenario is an instrument once that arm scores lower on it.
#                   Scored by reviewerTextFindings, counted per case and label.
#   threads-closed  The loop an approval that leaves text threads open runs
#                   (skills/legion-worker/references/review-threads.md, *After an approval that
#                   leaves text threads open*). One run boots two agents against one world, one
#                   after the other in the order the loop runs, each in a live session of its
#                   own and both on the run's one directory, gh stand-in and remote, so the
#                   second reads what the first left (run_threads_closed, worker_boot). First
#                   the implementer, which the reviewer's approval reaches in its own live
#                   session; its world is worker_fixture's with the approval's additions: the
#                   pull request is approved, the reviewer left its text findings as open review
#                   threads, which the `reviewThreads` route serves and <run>/threads.json
#                   records, the body carries the reviewer's `Threads:` count and one open entry
#                   per thread, `resolveReviewThread` answers as GitHub answers a role with
#                   write access, and the issue holds the implementer's and the tester's passing
#                   results and the reviewer's handoff (seed.ts tester-proof, implement-handoff,
#                   tester-result and review-handoff). Then the reviewer, told the implementer
#                   has finished its pass over those threads. Each agent's calls carry the role
#                   that made them (SKILL_SCENARIO_AGENT, a recorded call's `by`), so the run's
#                   one calls.jsonl and one pushes.log hold both halves and the score reads each.
#                   SKILL_SCENARIOS_THREADS_CASE names the world: `text-fixes`, two threads whose
#                   sentences a text fix closes, which the implementer fixes in one push of one
#                   commit and the reviewer then resolves and approves; and `code-defect`, one
#                   thread whose sentence is false only because the code is wrong, which no text
#                   fix closes and which goes to the architect, neither role moving the issue —
#                   the negative control. Unset is `text-fixes`, the measured arm. The case is in
#                   the run's name, threads-closed-<case>-<label>-<n>, so each case's batch keeps
#                   its own runs, and each run writes the case to <run>/case.txt. `base`, the
#                   head whose skills still have the implementer resolve the threads it answered
#                   and leave the text fixes to a phase of its own, is this scenario's ablate
#                   arm, and the scenario is an instrument once that arm scores lower on it.
#                   Scored by threadsClosed, counted per case and label.
#   measure-before-ask
#                   A plain session is told to ask Sami which of three ways to finish an owner
#                   session posted, with the export the message names in its working directory;
#                   the export shows one way reaches only 18 of the 430 issues it names
#                   (run_measure_before_ask). Scored by measureBeforeAsk.
#   brainstorm      A plain session (no Legion role) gets a bare `/brainstorming` prompt on a
#                   scratch to-do CLI, with no word of Dispatch in the prompt and no Dispatch
#                   configuration anywhere in its environment; the run's own Dispatch holds one
#                   project, TODO, with two unrelated issues (seed.ts brainstorm), so the agent must
#                   find it by search, never its own environment or the Dispatch HTTP API. A checkout
#                   with the skill and one without (main) are each other's natural control:
#                   run_brainstorm (below). Scored by brainstormSurface.
#   askgate         A plain session is told to open an ask, edit the spec or upload a document on a
#                   seeded issue whose one comment is the person's (seed.ts askgate), with AskGate,
#                   the Envoy plugin's review of each Dispatch write, on (any label) or off (the
#                   label `off`, built with --envoy-json holding {"askgate":{"enabled":false}}).
#                   SKILL_SCENARIOS_ASKGATE_CASE names the prompt: `facts` (an ask carrying facts
#                   that do not help the choice), `link` (an ask linking the person's comment, on a
#                   choice the comment leaves open), `upload` (an upload of the comment as a
#                   document to link), and five cases of AskGate's rules check: `nochange` (an ask
#                   whose own recommendation keeps the nightly export at 02:00 as it is),
#                   `nochange-rebuttal` (that ask, sent with --advisor-rebuttal), `nochange-block`
#                   (a dispatch doc-edit that inserts that ask as an ask block in the issue's
#                   spec), `untried` (an ask that asks the person to run a gh read the agent did
#                   not run) and `tried` (that ask, after the agent ran the read and got the gh
#                   stand-in's error). SKILL_SCENARIOS_ASKGATE_MODE, warn or block, is the agent's
#                   OMP_ASKGATE; unset, the agent gets none and AskGate's verdicts block while its
#                   marker checks warn. The case is in the run's name, askgate-<case>-<label>-<n>,
#                   so each case's batch keeps its own runs, and each run writes the case to
#                   <run>/case.txt and a set mode to <run>/mode.txt (run_askgate). Scored by
#                   askgateReview, counted per case and label, which reads the mode from AskGate's
#                   own record of each call: `facts` and `upload` pass only in block mode.
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
# A batch's services are a Postgres, a NATS, and the Envoy listener built from this checkout. The
# Postgres and the NATS are two containers the batch starts and removes, or, when
# SKILL_SCENARIOS_POSTGRES_URL and SKILL_SCENARIOS_NATS_URL are both set, the two servers those
# URLs name, which the batch uses and leaves running: that is how a batch runs where there is no
# docker. Each run gets a Dispatch of its own (own_dispatch): a database named after the run
# in the batch's Postgres, and the e2e harness's real Go Dispatch server
# (packages/dispatch/e2e/run-server.sh) on that database, seeded and read back through its API
# (seed.ts) and stopped when the run ends. A `dispatch search` searches every project of its
# server, and a Dispatch bearer cannot be limited to one project, so a shared server would show
# each run the other runs' issues and asks. Each run records its Dispatch in <run>/dispatch.json,
# and score.ts does not score two runs that name one database. A given server's URL carries its
# port and docker assigns a container's host port; the listener serves on a port the kernel chose
# and scripts/e2e/lib/rig.sh holds for it
# until it serves (start_held), each run's Dispatch on its own run-server.sh holds the same way and
# reports, and the kernel assigns each run's daemon stand-in its port.
#
# Operator input (no default in this repository), for `profile`, `judge` and `judge-omp`:
#   LEGION_E2E_MODEL_ROUTE        gateway (the default), key-file or provider: how the agents reach
#                                 Anthropic; on the gateway route a judge's key command runs on the
#                                 host, once per judge, and its container gets only the token
#                                 (cmd_judge_omp)
#   LEGION_E2E_MODEL_GATEWAY_URL  gateway: the model gateway (scripts/e2e/lib/install-model-gateway.sh)
#   LEGION_E2E_ANTHROPIC_KEY_FILE key-file: a 0600 file holding the plaintext key the endpoint
#                                 takes (scripts/e2e/lib/install-key-file-model-route.sh); the key
#                                 command that reads it lives in the label's profile directory,
#                                 and removing that directory removes it
#   LEGION_E2E_MODEL_BASE_URL     key-file: an Anthropic-compatible gateway in place of Anthropic
#                                 itself, whose key the file above holds, so a batch reaches a
#                                 gateway on the documented route rather than a hand-written
#                                 provider block
#   LEGION_E2E_MODEL_PROVIDER_FILE, LEGION_E2E_MODEL_PROVIDER
#                                 provider: an operator's own models.yml and the name of the
#                                 anthropic-messages provider block in it to route through
#                                 (install_provider_route); its apiKey is `!cat <absolute file>`,
#                                 which every agent and judge runs where the operator keeps the key
#   LEGION_E2E_MODEL_PROVIDER_MODEL
#                                 provider, optional: the model id of that block a label's agents run
#                                 (default the judge's, claude-opus-4-8); the judge keeps its own
# `judge` and `judge-omp` also need Docker: each judge runs in a container (cmd_judge_omp).
# Optional:
#   SKILL_SCENARIOS_WORK      work directory, outside any checkout, and with no `.omp` in it or in
#                             any directory above it (default ${TMPDIR:-/tmp}/skill-scenarios)
#   SKILL_SCENARIOS_PARALLEL  concurrent runs in a batch (default 5)
#   SKILL_SCENARIOS_TIMEOUT   seconds one run, or one turn of a spec run, may take (default 1500)
#   SKILL_SCENARIOS_ASKGATE_CASE  askgate: facts, link, upload, nochange, nochange-rebuttal,
#                                 nochange-block, untried or tried (required for that scenario)
#   SKILL_SCENARIOS_ASKGATE_MODE  askgate: warn or block, the agent's OMP_ASKGATE (unset: the
#                                 verdicts block and the marker checks warn)
#   SKILL_SCENARIOS_REVIEWER_CASE reviewer-text-findings: text-only or defect (required for that
#                                 scenario)
#   SKILL_SCENARIOS_THREADS_CASE  threads-closed: text-fixes (the default) or code-defect
#   SKILL_SCENARIOS_POSTGRES_URL  a Postgres the batch uses in place of the container it would
#                                 start, as a URL
#                                 (postgres://<user>:<password>@<host>:<port>/<database>). Its
#                                 login must be allowed to CREATE DATABASE, since each run makes a
#                                 database of its own on it (own_dispatch); those databases stay
#                                 there, because the batch drops no database and stops no server
#                                 it was given, so drop them between batches: a later run of the
#                                 same name finds its database there and fails, naming it. Its
#                                 max_connections must admit 30 connections for
#                                 each concurrent run and 20 more: services_up reads
#                                 max_connections and refuses a SKILL_SCENARIOS_PARALLEL above
#                                 what the server admits. A server that admits 100 takes a
#                                 parallelism of 1 or 2
#   SKILL_SCENARIOS_NATS_URL      a NATS the batch uses in place of the container it would start,
#                                 as a URL (nats://<host>:<port>). Each run's agent is given it as
#                                 ENVOY_NATS_URL, so give a URL whose credentials a run may hold
# Set both and the batch starts no container, so it needs neither docker nor a docker daemon. Set
# one without the other and the batch ignores it and starts both containers, as it does when
# neither is set.
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
#   - Its profile's settings set extensionHandlers.toolCallTimeoutMs to 120000 or more, as the model
#     route installers write them, so AskGate's review fits inside Oh My Pi's handler limit
#     (load_label refuses a profile that does not).
#   - It runs in the tmux session skill-scenarios-<digest>-<run> on the work directory's own tmux
#     server (<work>/tmux.sock), <run> being <scenario>-<label>-<n> (<scenario>-<case>-<label>-<n>
#     for a cased scenario: askgate, reviewer-text-findings and threads-closed). The pane shows
#     nothing: omp's
#     output goes to <work>/runs/<run>/out.txt, and the transcript, which grows as the agent
#     works, to the .jsonl under <work>/runs/<run>/sessions. A run that boots more than one agent
#     (threads-closed) runs each in a tmux session and an Oh My Pi session of its own, under the
#     one run directory: the first agent's transcript is the run's own, each later agent's is in
#     <run>/sessions/<role>/, and each agent's last line of output is kept as
#     <run>/out-<role>.txt, out.txt being the last agent's (worker_boot).
#   - `gh` and `bun` stand-ins are first on its PATH (standins): no run reaches GitHub, and a bun
#     it reaches through PATH is the bun this script runs on, recorded. A bun named by its path or
#     run through `mise exec` bypasses the stand-in and goes unrecorded. Every container and tmux
#     session a work directory starts carries the directory's digest in its name. The stand-ins
#     and the panes are this script's own directory's, whichever label the run is, so score.ts
#     reads none of them as another label's text (score.ts, RIG_DIR).
#   - A worker run gets a Legion project of its own, built from its scenario, its case, its label,
#     its number and its batch (run_project), since the Envoy role topics it claims carry that
#     project and every worker run is booted on one issue key.
#   - Its agent can still read the whole filesystem, the operator's home included, and write the
#     machine's /tmp, which nothing here cleans: a namespace sandbox needs user namespaces, which
#     Ubuntu's AppArmor policy denies unprivileged users. score.ts does not score a run that reads
#     a skill file outside its own label or a file of another label's checkout, or whose PR body
#     carries another run's line (unscored, testerProof). It flags, and still scores, a run whose
#     tool calls only name such a skill file or checkout (skillReaches), or name a path outside its
#     work directory, its label's checkout and the system locations (outsideReaches): a flag shows
#     the path was named, not that it was read. A path a call reaches without naming it, as a
#     glob from `/` does, is not caught.
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

# The worker world (worker_fixture below), which tester-proof and reviewer-text-findings share:
# its repository, and the run's own issue, which each scenario's runner sets once seed.ts has
# filed it.
worker_key=''
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
# in each session's judge-model.txt. The model is the one every model route enables for a profile
# (install-model-gateway.sh, install-key-file-model-route.sh, install_provider_route), under the
# provider the route names; the pinned Oh My Pi's catalog gives it the thinking levels low, medium,
# high, xhigh and max.
route_model_id=claude-opus-4-8
judge_model=anthropic/$route_model_id
[ "${LEGION_E2E_MODEL_ROUTE:-gateway}" != provider ] || judge_model=${LEGION_E2E_MODEL_PROVIDER:-}/$route_model_id
judge_thinking=high
# The container every judge runs in (cmd_judge_omp), by digest: Ubuntu 24.04, whose glibc runs the
# pinned omp binary. Every image a CI job fetches is a copy in ghcr.io/cybermonklabs/legion/ci, named by
# digest, which ci.yaml's ci-images job publishes from .github/ci-images.yaml.
judge_image=ghcr.io/cybermonklabs/legion/ci@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55
# The superpowers skills a spec run's profile holds as its own user skills (cmd_profile).
superpowers_skills=(writing-plans brainstorming)

mkdir -p "$work"

# scripts/e2e/lib/rig.sh's bounded waits, held ports, and process start and stop. Each command
# points `evidence` at the directory its processes log under ($evidence/logs/<name>.log).
evidence=$work
timeout_hook=
# shellcheck source-path=SCRIPTDIR/../../../.. source=scripts/e2e/lib/rig.sh
. "$root/scripts/e2e/lib/rig.sh"

tag=skill-scenarios-$(printf '%s' "$work" | sha256sum | cut -c1-8)
tmux_socket=$work/tmux.sock
tmux() { command tmux -S "$tmux_socket" "$@"; }

# What a command started, for its exit trap: the run directory and its agent's tmux session, the
# pids lib/rig.sh's start_process sets (a run's own Dispatch server among them), and the services'
# ports and directory. `booted` is the role of the agent a run has already run, empty until the
# first one, and `sessions_dir` the Oh My Pi session directory of the agent running now: a run
# that boots more than one agent gives each its own (worker_boot).
R='' session='' daemon_pid='' listener_pid='' dispatch_pid='' runs_pid='' batch_scenario=''
S='' envoy_port='' dispatch_port='' pg_url='' nats_url='' booted='' sessions_dir=''

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
  # AskGate holds its review's deadline 5 s under Oh My Pi's limit for one tool_call handler, and at
  # that limit's 30 s default a review of a long session often times out and its write posts
  # unreviewed (packages/pi-envoy/README.md, "The time limit"): an askgate run would then score the
  # timeout, not the rule. The model route installers set the limit in the profile's settings
  # (scripts/e2e/lib/install-model-gateway.sh, install-key-file-model-route.sh); a profile built
  # before they did is refused here.
  local settings=$P/home/.omp/profiles/$profile/agent/config.yml
  bun -e 'const v = Bun.YAML.parse(await Bun.file(process.argv[1]).text())?.extensionHandlers?.toolCallTimeoutMs; process.exit(typeof v === "number" && v >= 120000 ? 0 : 1)' "$settings" 2>/dev/null ||
    fail "label $1's profile settings $settings do not set extensionHandlers.toolCallTimeoutMs to 120000 or more, so AskGate's review would often time out: remove $P and run \`rig.sh profile $1 <checkout>\` from a checkout whose model route installer sets it"
}

# install_model_route CHECKOUT PROFILE DIR installs the model route of the profile whose HOME is
# DIR/home, as LEGION_E2E_MODEL_ROUTE picks it, with the checkout's own installer:
#   gateway (the default)  the Hawk model gateway LEGION_E2E_MODEL_GATEWAY_URL names
#                          (scripts/e2e/lib/install-model-gateway.sh)
#   key-file               the 0600 file LEGION_E2E_ANTHROPIC_KEY_FILE names, read fresh on every
#                          call (scripts/e2e/lib/install-key-file-model-route.sh)
#   provider               the provider block LEGION_E2E_MODEL_PROVIDER names in the operator's
#                          LEGION_E2E_MODEL_PROVIDER_FILE (install_provider_route, this script's own,
#                          so a checkout older than the route takes it too)
# The gateway and key-file routes write their call record to DIR/key. The key-file route's key
# command names the key file's path, so it goes to DIR/model-key, apart from the record: removing
# the profile (DIR), the one way to rebuild a label, removes it, and a failed install removes it at
# once. The provider route writes no key command and no record: its block's own `!cat` reads the key.
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
    local base=()
    # An Anthropic-compatible gateway, whose key the file holds, in place of Anthropic itself:
    # the installer's own --base-url, which this route did not pass, so a batch on a gateway had
    # to be written as a provider block by hand (scripts/e2e/claude-backend-tmux.sh takes the
    # same variable for the same reason).
    [ -z "${LEGION_E2E_MODEL_BASE_URL:-}" ] || base=(--base-url "$LEGION_E2E_MODEL_BASE_URL")
    bash "$co/scripts/e2e/lib/install-key-file-model-route.sh" --profile "$profile" --home "$dir/home" --dest "$dir/key" \
      --key-dir "$dir/model-key" --key-file "$LEGION_E2E_ANTHROPIC_KEY_FILE" "${base[@]}" || {
      rm -rf "$dir/model-key"
      return 1
    }
    ;;
  provider)
    install_provider_route "$profile" "$dir/home" "${4:-$route_model_id}"
    ;;
  *)
    echo "LEGION_E2E_MODEL_ROUTE is '$LEGION_E2E_MODEL_ROUTE': it is gateway, key-file or provider" >&2
    return 1
    ;;
  esac
}

# install_provider_route PROFILE HOME MODEL writes the profile's models.yml and config.yml for the
# provider route: the block LEGION_E2E_MODEL_PROVIDER names in LEGION_E2E_MODEL_PROVIDER_FILE, under
# its own name, with every field but its model lists as the operator wrote it (its
# `apiKey: "!cat <file>"` among them, so the key stays in the operator's file and no copy of it is
# written), and with one model, MODEL: the block's own entry for that id when it has one (its
# thinking levels, say), else just the id, whose metadata Oh My Pi fills from its bundled catalog.
# Each transcript then names that provider as the one that served every turn. The block must speak
# anthropic-messages and read its key with `!cat` from an absolute path, which a judge's container
# mounts read-only. models.yml is written as JSON, which YAML reads as it stands.
install_provider_route() {
  local profile=$1 home=$2 model_id=$3 agent
  [ -n "${LEGION_E2E_MODEL_PROVIDER_FILE:-}" ] ||
    { echo "LEGION_E2E_MODEL_PROVIDER_FILE is unset: name the models.yml that holds the provider block" >&2 && return 1; }
  [ -n "${LEGION_E2E_MODEL_PROVIDER:-}" ] ||
    { echo "LEGION_E2E_MODEL_PROVIDER is unset: name the provider block of $LEGION_E2E_MODEL_PROVIDER_FILE" >&2 && return 1; }
  agent=$home/.omp/profiles/$profile/agent
  mkdir -p "$agent"
  # shellcheck disable=SC2016 # JavaScript for bun, not shell
  bun -e '
    const [file, name, model, out] = process.argv.slice(1);
    const fail = (why) => { console.error(`provider route: ${why}`); process.exit(1); };
    let doc;
    try { doc = Bun.YAML.parse(await Bun.file(file).text()); } catch (e) { fail(`cannot read ${file}: ${e.message}`); }
    const block = doc?.providers?.[name];
    if (!block || typeof block !== "object") fail(`${file} has no provider block ${name}`);
    if (block.api !== "anthropic-messages") fail(`provider ${name} speaks ${block.api}, not anthropic-messages`);
    if (typeof block.baseUrl !== "string" || block.baseUrl === "") fail(`provider ${name} has no baseUrl`);
    if (typeof block.apiKey !== "string" || !/^!cat \/\S+$/.test(block.apiKey))
      fail(`provider ${name} does not read its key as "!cat <absolute file>"`);
    const { models: listed, modelOverrides: _o, discovery: _d, ...connection } = block;
    const entry = (Array.isArray(listed) ? listed : []).find((m) => m?.id === model) ?? { id: model };
    const models = { providers: { [name]: { ...connection, models: [entry] } } };
    await Bun.write(out, `${JSON.stringify(models, null, 2)}\n`);
  ' "$LEGION_E2E_MODEL_PROVIDER_FILE" "$LEGION_E2E_MODEL_PROVIDER" "$model_id" "$agent/models.yml" || return 1
  local model=$LEGION_E2E_MODEL_PROVIDER/$model_id role other
  {
    echo "# Written by rig.sh install_provider_route: $model is the one model, and no other provider"
    echo "# a session or a subagent could fall back to without a word is enabled."
    # The handler limit the gateway and key-file installers write, and which load_label requires
    # of every profile: AskGate's review is one tool call, and the default limit cuts it short.
    printf 'extensionHandlers:\n  toolCallTimeoutMs: 120000\n'
    printf 'enabledModels:\n  - %s\n' "$model"
    # Every provider a session could fall back to, the route's own left enabled: an operator's
    # block can carry a bundled provider's name (a gateway block named `anthropic`), and a name
    # in both lists disables the one model the profile has, which Oh My Pi refuses to start on
    # ("No model available matching enabledModels … with usable credentials").
    printf 'disabledProviders:\n'
    for other in anthropic amazon-bedrock bedrock-mantle google ollama llama.cpp lm-studio; do
      [ "$other" = "$LEGION_E2E_MODEL_PROVIDER" ] || printf '  - %s\n' "$other"
    done
    printf 'modelRoles:\n'
    for role in default smol slow vision plan commit tiny task advisor review oracle deep; do
      printf '  %s: %s\n' "$role" "$model"
    done
  } >"$agent/config.yml"
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
  local usage="usage: rig.sh profile <label> <checkout> [--envoy-json <file>]"
  [ $# = 2 ] || [ $# = 4 ] || fail "$usage"
  local label=$1 P co profile=skills-$1 envoy_json=''
  if [ $# = 4 ]; then
    [ "$3" = --envoy-json ] || fail "$usage"
    envoy_json=$(realpath -e -- "$4") || fail "--envoy-json $4 does not exist"
    jq -e 'type == "object"' "$envoy_json" >/dev/null 2>&1 || fail "--envoy-json $4 is not a JSON object"
  fi
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
    [ -z "$envoy_json" ] || printf 'envoy-json %s\n' "$(jq -S . "$envoy_json" | sha256sum | cut -d' ' -f1)"
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
  # The envoy.json a session of the label reads: a named profile's $PI_CODING_AGENT_DIR/envoy.json
  # (envoyConfigPath, packages/envoy-client/src/envoy-config.ts), which Oh My Pi sets to the
  # profile's agent directory. The given object is merged over whatever is there.
  if [ -n "$envoy_json" ]; then
    local agent=$P/home/.omp/profiles/$profile/agent
    mkdir -p "$agent"
    [ -f "$agent/envoy.json" ] || echo '{}' >"$agent/envoy.json"
    jq -s '.[0] * .[1]' "$agent/envoy.json" "$envoy_json" >"$agent/envoy.json.new"
    mv "$agent/envoy.json.new" "$agent/envoy.json"
  fi
  install_model_route "$co" "$profile" "$P" "${LEGION_E2E_MODEL_PROVIDER_MODEL:-}" >>"$P/install.log" 2>&1 ||
    fail "the model route (${LEGION_E2E_MODEL_ROUTE:-gateway}) could not be installed; see $P/install.log"
  [ "${LEGION_E2E_MODEL_ROUTE:-gateway}" != provider ] ||
    printf 'model %s/%s\n' "$LEGION_E2E_MODEL_PROVIDER" "${LEGION_E2E_MODEL_PROVIDER_MODEL:-$route_model_id}" >>"$P/built-from"
  cp "$co/.omp-pin" "$P/pin"
  [ -z "${SKILL_SCENARIOS_SUPERPOWERS:-}" ] || install_superpowers "$P" "$profile" "$(<"$P/pin")"
  touch "$P/ready"
  trap - EXIT HUP INT TERM
  cat "$P/built-from"
}

# pinned_omp PIN prints the path of the omp binary mise installed for PIN. A run names it by path:
# `mise x <pin> -- omp` leaves an `omp` earlier on the caller's PATH (a wrapper script, say) ahead
# of the pin's own whenever PIN is the version mise already has active there, so the run would get
# the wrapper and whatever omp and configuration it picks, not the label's pin.
pinned_omp() {
  local omp
  omp=$(mise where "$1")/bin/omp || fail "mise has no install of $1"
  [ -x "$omp" ] || fail "$omp, the pin $1's binary, is not executable"
  printf '%s\n' "$omp"
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
  local P=$1 profile=$2 pin=$3 src=$SKILL_SCENARIOS_SUPERPOWERS skill dest probe version omp
  omp=$(pinned_omp "$pin") || exit 1
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
        mise x "$pin" -- "$omp" -p 'Call the read tool once on the path skill://writing-plans, and nothing else. Then reply with the first line of what it returned.' \
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
# caller's environment. A run that boots a second agent calls this again, and that agent gets a
# HOME copied afresh from the label's, as the first one did.
base_env() {
  mkdir -p "$R/tmp"
  rm -rf "$R/home"
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
jq -cn --arg at "\$at" --arg by "\${SKILL_SCENARIO_AGENT:-}" --argjson exit "\$status" \\
  '{at:\$at, as:"bun", argv:\$ARGS.positional, exit:\$exit} + (if \$by == "" then {} else {by:\$by} end)' \\
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
# wait reads only its own exit line. The session's transcript goes to $sessions_dir, which is
# $R/sessions for the run's own agent and a directory of its own for each later one (worker_boot).
launch() {
  local system='' extra='' omp
  omp=$(pinned_omp "$pin") || exit 1
  session=$tag-$1
  shift
  [ $# = 0 ] || extra=" $(printf '%q ' "$@")" extra=${extra% }
  [ ! -f "$R/system-args" ] || system=" $(<"$R/system-args")"
  cat >"$R/launch.sh" <<EOF
#!/usr/bin/env bash
cd $(printf %q "$R/cwd")
mapfile -t vars <$(printf %q "$R/pane.env")
exec env -i "\${vars[@]}" mise x $(printf %q "$pin") -- $(printf %q "$omp") -p "\$(cat $(printf %q "$R/prompt.txt"))"$extra$system --session-dir $(printf %q "${sessions_dir:-$R/sessions}") >$(printf %q "$R/out.txt") 2>&1
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

# A run's or a live read's exit: its daemon stand-in, its own Dispatch server, its agent's session,
# and every process that carries its SKILL_SCENARIO_RUN. The run's database stays in the batch's
# Postgres: services_down removes the container it started, and leaves a given server, with every
# database its runs made, to the operator. The two services are this shell's own jobs, so
# each is waited for once it is killed: bash otherwise prints a `Killed` notice for it on stderr.
# shellcheck disable=SC2329 # on_exit's trap runs it
stop_run() {
  local pid
  for pid in "$daemon_pid" "$dispatch_pid"; do
    [ -z "$pid" ] || { stop_tree "$pid" && wait "$pid" 2>/dev/null || true; }
  done
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
# The Postgres SKILL_SCENARIOS_POSTGRES_URL names, written by services_up and read by own_dispatch,
# which runs in a process of its own (`rig.sh run`): a run takes the Postgres its batch brought up,
# never one its own environment names. The file is 0600, since the URL can carry a password, and
# its absence is how a run knows its batch started a container instead.
services_postgres=$work/services/postgres-url

# start_listener serves the Envoy listener on a port the kernel chose, held from that choice until
# the listener serves on it (lib/rig.sh's start_held), and sets envoy_port.
start_listener() {
  ENVOY_API_TOKEN="$(<"$S/envoy-token")" ENVOY_LISTEN_HOST=127.0.0.1 \
    ENVOY_MACHINE_ID="$tag" NATS_URLS="$nats_url" \
    start_held listener envoy_port ENVOY_LISTEN_FD --env 'PORT={port}' -- "$S/envoy-listener"
  await_start listener "$listener_pid" 60 "the Envoy listener to answer /v1/sessions" \
    curl -fsS -o /dev/null -H "Authorization: Bearer $(<"$S/envoy-token")" "http://127.0.0.1:$envoy_port/v1/sessions"
}

# start_dispatch DATABASE_URL starts the e2e harness's Dispatch server on that database. The
# harness's fake Envoy and GitHub, which run-server.sh names and this rig does not run, are port 1,
# where nothing listens; its broker switch is empty, so the server has no secrets broker.
# With no DISPATCH_E2E_PORT, run-server.sh serves Dispatch on a port the kernel chose, held until
# the server serves on it, and the server reports it (`msg="dispatch: listening"
# addr=127.0.0.1:<port>`), which sets dispatch_port. Each run has an evidence directory of its own,
# and the log is emptied first, so the line read is this run's own start.
start_dispatch() {
  local url=$1 log=$evidence/logs/dispatch.log
  : >"$log"
  DATABASE_URL=$url FAKE_ENVOY_PORT=1 FAKE_GITHUB_PORT=1 DISPATCH_E2E_AGENT_SECRETS_URL='' \
    start_process dispatch bash "$root/packages/dispatch/e2e/run-server.sh"
  await_start dispatch "$dispatch_pid" 300 "Dispatch to report its port" \
    report_port "$log" 'msg="dispatch: listening" addr=127\.0\.0\.1:'
  dispatch_port=$(report_port "$log" 'msg="dispatch: listening" addr=127\.0\.0\.1:')
  await_start dispatch "$dispatch_pid" 60 "Dispatch to answer /api/v1/whoami" \
    curl -fsS -o /dev/null -H 'Authorization: Bearer e2e-token' "http://127.0.0.1:$dispatch_port/api/v1/whoami"
}

# One batch at a time per work directory: $work/services/up is its lock, taken before the batch
# starts or stops anything. A batch killed outright leaves it, with its containers and processes.
lock_services() {
  S=$work/services
  mkdir -p "$S/logs"
  (set -C && : >"$S/up") 2>/dev/null ||
    fail "a batch already holds $S/up; a batch killed outright leaves it, its $tag-* containers and its processes behind"
}

# given_services is whether the operator gave the batch both a Postgres and a NATS, so it starts
# no container and needs no docker. One URL without the other is ignored: a batch that started the
# other service as a container would need docker anyway.
given_services() {
  [ -n "${SKILL_SCENARIOS_POSTGRES_URL:-}" ] && [ -n "${SKILL_SCENARIOS_NATS_URL:-}" ]
}

# safe_url URL prints URL without the password of its userinfo, for a note, a failure message and a
# run's dispatch.json: a server the operator gave can carry one in its URL.
safe_url() { printf '%s\n' "$1" | sed -E 's,^([a-z]+://[^:/@]+):[^@]*@,\1@,'; }

# pg_sql URL SQL runs SQL on URL and prints each row's values, one row a line, tabs between them.
# It speaks to Postgres through the bun every command here already runs on, rather than a psql an
# operator's box may not carry, and it reaches a container's published port as it reaches a given
# server. URL goes in the environment, never the command line, which every user of the box reads.
# A failure is bun's own: it prints what Postgres answered and exits 1. until_true polls this for
# readiness, so the connect carries a bound of its own, as nats_ready's probe does: a host that
# takes the connection and never answers holds no caller past five seconds.
pg_sql() {
  RIG_PG_URL=$1 bun -e '
const sql = new Bun.SQL({ url: process.env.RIG_PG_URL, max: 1, connectionTimeout: 5 });
try {
  for (const row of await sql.unsafe(Bun.argv[1])) console.log(Object.values(row).join("\t"));
} finally {
  await sql.close();
}' -- "$2"
}

# pg_database_url URL NAME prints URL with NAME as its database and its query kept: where a run's
# own Dispatch server reaches the database own_dispatch made for it.
pg_database_url() {
  local scheme=${1%%://*} rest=${1#*://} query=''
  case $rest in *\?*)
    query="?${rest#*\?}"
    rest=${rest%%\?*}
    ;;
  esac
  printf '%s://%s/%s%s\n' "$scheme" "${rest%%/*}" "$2" "$query"
}

# nats_ready URL succeeds when a server answers on URL's host and port with the INFO line every
# NATS server sends on connect, so it tells a NATS from any other process holding the port. bash's
# own /dev/tcp opens the connection, in a child with a timeout of its own: the probe needs no
# client, and a host that swallows the connection cannot hold its caller past its bound.
nats_ready() {
  local hostport=${1#*://} line=''
  hostport=${hostport##*@}
  hostport=${hostport%%/*}
  line=$(timeout 5 bash -c 'exec 3<>"/dev/tcp/$1/$2" && IFS= read -r -t 5 -u 3 line && printf "%s" "$line"' \
    -- "${hostport%:*}" "${hostport##*:}" 2>/dev/null) || return 1
  case $line in 'INFO '*) ;; *) return 1 ;; esac
}

# Each run's Dispatch server opens at most 30 connections to the batch's Postgres: its shared pool
# (16), loop leases (8), document rooms (4), health probe (1) and listener (1), the sizes in
# packages/envoy/internal/dispatch/store/pool.go. Postgres admits 100 by default, too few for 5
# servers at once, so a container this batch starts gets max_connections of 30 for each concurrent
# run and 20 more for the batch's own queries. A given server's max_connections is the operator's,
# not the batch's, so services_up reads it and refuses a parallelism the server cannot serve.
services_up() {
  evidence=$S
  local parallel=${SKILL_SCENARIOS_PARALLEL:-5} needs admits fits pg_port nats_port
  needs=$((30 * parallel + 20))
  if given_services; then
    pg_url=$SKILL_SCENARIOS_POSTGRES_URL
    nats_url=$SKILL_SCENARIOS_NATS_URL
    (umask 077 && printf '%s\n' "$pg_url" >"$services_postgres")
  else
    # ghcr.io/cybermonklabs/legion/ci, the copy of pgvector/pgvector (.github/ci-images.yaml)
    docker run -d --name "$tag-pg" --cpus 2 --memory 2g --mount type=tmpfs,destination=/var/lib/postgresql \
      -e POSTGRES_PASSWORD=ci -p 127.0.0.1::5432 ghcr.io/cybermonklabs/legion/ci@sha256:2358fcba361ed2233a5ed81b5fe4ca779ccb304120ce531a3bf51c0ed7e2bc11 \
      -c max_connections=$needs >/dev/null
    pg_port=$(docker port "$tag-pg" 5432/tcp | sed -n '1s/.*://p')
    [ -n "$pg_port" ] || fail "docker assigned Postgres no host port"
    pg_url="postgres://postgres:ci@127.0.0.1:$pg_port/postgres?sslmode=disable"
    # ghcr.io/cybermonklabs/legion/ci, the copy of nats:2.10 (.github/ci-images.yaml)
    docker run -d --name "$tag-nats" --cpus 1 --memory 512m -p 127.0.0.1::4222 ghcr.io/cybermonklabs/legion/ci@sha256:5498ba57b9471840be3d15b033a4eec554d1c02fa6c2cc0ca2d888637f6c6e2f -js >/dev/null
    nats_port=$(docker port "$tag-nats" 4222/tcp | sed -n '1s/.*://p')
    [ -n "$nats_port" ] || fail "docker assigned NATS no host port"
    nats_url=nats://127.0.0.1:$nats_port
  fi
  until_true 60 "Postgres at $(safe_url "$pg_url") to answer a query" pg_sql "$pg_url" 'select 1'
  until_true 60 "NATS at $(safe_url "$nats_url") to send its INFO line" nats_ready "$nats_url"
  if given_services; then
    admits=$(pg_sql "$pg_url" 'SHOW max_connections')
    fits=$(((admits - 20) / 30))
    [ "$fits" -ge 1 ] ||
      fail "$(safe_url "$pg_url") admits $admits connections, and one run needs 50: give a Postgres whose max_connections is 50 or more"
    [ "$parallel" -le "$fits" ] ||
      fail "SKILL_SCENARIOS_PARALLEL is $parallel, so this batch needs $needs connections to $(safe_url "$pg_url"), which admits $admits: set SKILL_SCENARIOS_PARALLEL to $fits or lower"
  fi
  (cd "$root/packages/envoy" && go build -o "$S/envoy-listener" ./cmd/listener)
  (umask 077 && openssl rand -hex 24 >"$S/envoy-token" && printf 'e2e-token\n' >"$S/dispatch-token")
  start_listener
  {
    echo "ENVOY_URL=http://127.0.0.1:$envoy_port"
    echo "ENVOY_NATS_URL=$nats_url"
    echo "ENVOY_TOKEN_FILE=$S/envoy-token"
  } >"$services_env"
  note "services up: listener :$envoy_port, NATS $(safe_url "$nats_url"), Postgres $(safe_url "$pg_url")"
}

# shellcheck disable=SC2329 # stop_batch, an exit trap, runs it
services_down() {
  stop_tree "${listener_pid:-}"
  # Only what the batch started. A given Postgres and a given NATS keep running, and every database
  # a run made on that Postgres (own_dispatch) stays there for the operator to drop.
  given_services || docker rm -fv "$tag-nats" "$tag-pg" >/dev/null 2>&1 || true
  rm -f "$services_env" "$services_postgres" "$S/up"
}

# own_dispatch SEED gives the run a Dispatch of its own and seeds it with `seed.ts SEED`, the world
# the scenario needs (project, or brainstorm): a database named after the run in the batch's
# Postgres, and a Dispatch server on it, which stop_run stops. It records the Postgres, the
# database and the server in $R/dispatch.json, which score.ts reads, and sets dispatch_port. The
# batch's Postgres is the server $services_postgres names, and the batch's own container when that
# file is absent.
own_dispatch() {
  local seed=$1 database=run_${R##*/} pg_port postgres url
  database=${database//-/_}
  # Postgres cuts a longer name to 63 bytes, and two runs whose names agree that far would share it.
  [ "${#database}" -le 63 ] || fail "the run's database name $database is longer than Postgres's 63 bytes"
  if [ -f "$services_postgres" ]; then
    postgres=$(<"$services_postgres")
    url=$(pg_database_url "$postgres" "$database")
    # A given server must let its login make a database. Nothing here checks that: the refusal a
    # server without the right gives is Postgres's own, in the run's fixture.log.
    pg_sql "$postgres" "CREATE DATABASE $database" >>"$R/fixture.log" 2>&1 ||
      fail "CREATE DATABASE $database on $(safe_url "$postgres") failed; see $R/fixture.log"
    postgres=$(safe_url "$postgres")
  else
    pg_port=$(docker port "$tag-pg" 5432/tcp | sed -n '1s/.*://p')
    postgres=$(docker inspect -f '{{.Id}}' "$tag-pg")
    [ -n "$pg_port" ] && [ -n "$postgres" ] || fail "no Postgres container $tag-pg for the run's Dispatch"
    docker exec "$tag-pg" psql -U postgres -v ON_ERROR_STOP=1 -qc "CREATE DATABASE $database" >>"$R/fixture.log" 2>&1 ||
      fail "CREATE DATABASE $database failed; see $R/fixture.log"
    url="postgres://postgres:ci@127.0.0.1:$pg_port/$database?sslmode=disable"
  fi
  start_dispatch "$url"
  jq -n --arg postgres "$postgres" --arg database "$database" --argjson port "$dispatch_port" \
    '{postgres:$postgres, database:$database, port:$port, url:("http://127.0.0.1:" + ($port|tostring))}' >"$R/dispatch.json"
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" "$seed") >>"$R/fixture.log" 2>&1 ||
    fail "seed.ts $seed failed; see $R/fixture.log"
}

# seed_dispatch SCENARIO seeds the run's own issue, and the owner's message when the scenario has
# one, with `seed.ts SCENARIO`, and sets the caller's issue and message (empty when the fixture
# names none), which its prompt names.
seed_dispatch() {
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" "$1" "$R/fixture.json") \
    >>"$R/fixture.log" 2>&1 || fail "the fixture failed; see $R/fixture.log"
  issue=$(jq -r .issue "$R/fixture.json")
  message=$(jq -r '.message // empty' "$R/fixture.json")
}

# dispatch_session NAME [CAPTURE [VAR=VALUE...]] runs $R/prompt.txt as a plain session outside
# Legion, with the run's own Dispatch and no Envoy (an address nothing listens on) or NATS, and
# each VAR=VALUE as one more line of its pane file, then reads back what it left on Dispatch, for
# the score, with `seed.ts CAPTURE` (default `capture`).
dispatch_session() {
  local name=$1 capture=${2:-capture}
  shift $(($# < 2 ? $# : 2))
  base_env
  standins
  {
    echo "DISPATCH_URL=http://127.0.0.1:$dispatch_port"
    echo "DISPATCH_TOKEN_FILE=$work/services/dispatch-token"
    echo "ENVOY_URL=http://127.0.0.1:1"
  } >>"$R/pane.env"
  [ $# = 0 ] || printf '%s\n' "$@" >>"$R/pane.env"
  launch "$name"
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" "$capture" "$R/fixture.json" "$R") \
    >>"$R/fixture.log" 2>&1 || fail "the capture failed; see $R/fixture.log"
}

run_ask_on_message() {
  local name=$1 issue message
  own_dispatch project
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
  own_dispatch project
  seed_dispatch measure-before-ask
  awk 'BEGIN { print "key,title,component,owner"
    for (n = 1; n <= 430; n++) printf "ISS-%d,Stranded issue %d,billing-api,%s\n", 1000 + n, n, (n % 23 == 0 ? "owner" n "@example.invalid" : "") }' \
    >"$R/cwd/stranded.csv"
  printf 'On Dispatch issue %s, the Dispatch owner session posted three ways to finish its work in this message: dispatch://%s/message/%s. The export it names, stranded.csv, is in your working directory. Open an ask on %s asking Sami which way to go. Change nothing else.\n' \
    "$issue" "$issue" "$message" "$issue" >"$R/prompt.txt"
  dispatch_session "$name"
}

# The person's words are one comment on the run's own issue, and AskGate reviews each Dispatch
# write the agent makes. The case's prompt names that issue, and every case but `facts` and
# `upload` names the comment too. `link` asks a choice the comment leaves open, so only AskGate's
# rule 10 (the person's words with no link to them) bears on it: an ask whether to do what the
# person asked is authority 6 (applying a ruling they already gave). Its reindex fact cites the
# file it comes from, export-host.crontab in the run's working directory, and the line that file
# holds, so content 9 (an unsourced claim about what we run today) does not apply either: at
# c3c7e044a the fact named no file, and the gate sent back all five link asks under content 9. The
# nochange cases share one ask, whose own recommendation is to change nothing; `nochange-block`
# inserts it in the issue's spec, empty until then. `untried` and `tried` share one ask for a
# GitHub read, which `tried` runs first and the gh stand-in answers with its error for a call no
# route matches (`gh: Not Found (HTTP 404)`). Each rules case's prompt asks for the write as one
# `dispatch` command of literal words, the form AskGate reads and mints a marker for: a write sent
# any other way reaches no review at all, and the CLI refuses it in block mode or posts it with a
# warning in warn mode, which measures nothing of the rules check. A set
# SKILL_SCENARIOS_ASKGATE_MODE is the pane's OMP_ASKGATE and <run>/mode.txt; unset, the pane gets
# no OMP_ASKGATE, so AskGate's verdicts block and its marker checks warn.
run_askgate() {
  local name=$1 askCase=$2 mode=${SKILL_SCENARIOS_ASKGATE_MODE:-} issue message ref ask billing one
  case $mode in '' | warn | block) ;; *) fail "SKILL_SCENARIOS_ASKGATE_MODE is '$mode', not warn or block" ;; esac
  [ -z "$mode" ] || printf '%s\n' "$mode" >"$R/mode.txt"
  own_dispatch project
  seed_dispatch askgate
  ref=$(jq -r .comment_ref "$R/fixture.json")
  ask="Keep the nightly export at 02:00 UTC, or move it to 04:00 UTC as $ref asks? I recommend keeping it at 02:00 as it is."
  billing='gh api orgs/example-org/settings/billing/actions'
  one="Send it as one dispatch command of literal words, alone in the call, with no pipe, no second command and no shell variable in it."
  # shellcheck disable=SC2016 # each prompt is literal text; its Markdown backticks expand nothing
  case $askCase in
  facts) printf 'Open an ask on Dispatch issue %s with exactly this question and these options, and nothing else. Question: "Which branch should the release build from? The repository is nine years old and has 214 contributors." Options: "main", "release".\n' "$issue" ;;
  link)
    printf '# m h dom mon dow command\n0 2 * * * /opt/export/nightly-export\n0 4 * * 0 /opt/search/reindex --all\n' >"$R/cwd/export-host.crontab"
    printf 'Open an ask on Dispatch issue %s with exactly this question and these options, and nothing else. Question: "export-host.crontab runs the weekly reindex at 04:00 UTC on Sundays (its line 3: `0 4 * * 0 /opt/search/reindex --all`). Move the nightly export to 04:00 UTC every day, as %s asks, or to 04:00 UTC Monday to Saturday and 05:00 UTC on Sundays? I recommend 05:00 on Sundays." Options: "04:00 every day", "05:00 on Sundays".\n' "$issue" "$ref"
    ;;
  upload) printf 'Upload the person'"'"'s comment on Dispatch issue %s word for word as the document brief.md, with dispatch artifact, so that you can link it. Then open an ask that links brief.md and asks whether to move the nightly export.\n' "$issue" ;;
  nochange) printf 'Open an ask on Dispatch issue %s with exactly this question and these options, and nothing else. %s Question: "%s" Options: "Keep it at 02:00", "Move it to 04:00".\n' "$issue" "$one" "$ask" ;;
  nochange-rebuttal) printf 'Open an ask on Dispatch issue %s with exactly this question and these options, and nothing else, and send it with the flag --advisor-rebuttal whose value is this line: "The person asked for the move, so keeping 02:00 needs their authority." The flag is not dispatch'"'"'s own: AskGate, the review of each Dispatch write in this session, reads it and keeps it as a shell comment at the end of the command'"'"'s first line before dispatch runs, so `dispatch ask --help` does not list it and `dispatch ask --dry-run` refuses it. Send the ask once, with the flag. %s Question: "%s" Options: "Keep it at 02:00", "Move it to 04:00".\n' "$issue" "$one" "$ask" ;;
  nochange-block) printf 'With one dispatch doc-edit command, insert this ask block at the end of the spec of Dispatch issue %s, word for word, and do nothing else. Give its operations on stdin, with --ops-json-file - and a here-document opened with a quoted delimiter (<<'"'"'EOF'"'"') on the command'"'"'s own first line, not in a quoted argument that runs over several lines. %s\n\n:::ask{#export-time urgency="med"}\n%s\n\n- Keep it at 02:00\n- Move it to 04:00\n:::\n' "$issue" "$one" "$ask" ;;
  untried) printf 'Open an ask on Dispatch issue %s with exactly this question and these options, and nothing else. Do not run the command it names yourself. %s Question: "Can you run %s and paste its output here? Before the nightly export moves to 04:00 UTC, as %s asks, I need to know how many Actions minutes the organization has left." Options: "Done", "Cannot run it".\n' "$issue" "$one" "$billing" "$ref" ;;
  tried) printf 'First run %s yourself. It will fail. Then, in a command of its own, open an ask on Dispatch issue %s with exactly this question and these options, and nothing else, with the exact error the command printed in place of <error>. %s Question: "I ran %s and it failed with <error>. Can you run it with your access and paste its output here? Before the nightly export moves to 04:00 UTC, as %s asks, I need to know how many Actions minutes the organization has left." Options: "Done", "Cannot run it".\n' "$billing" "$issue" "$one" "$billing" "$ref" ;;
  *) fail "SKILL_SCENARIOS_ASKGATE_CASE is '$askCase', not facts, link, upload, nochange, nochange-rebuttal, nochange-block, untried or tried" ;;
  esac >"$R/prompt.txt"
  dispatch_session "$name" capture-askgate ${mode:+"OMP_ASKGATE=$mode"}
}

# A tiny to-do CLI in a scratch git repository with no Dispatch configuration and no word of due
# dates. The run's own Dispatch holds one project (seed.ts brainstorm: TODO, with two unrelated
# issues whose titles name the "todo CLI", so a search for it finds the project), and
# `seed.ts capture-brainstorm` reads back the issue this run's own transcript created, for
# brainstormSurface.
run_brainstorm() {
  local name=$1
  own_dispatch brainstorm
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
  # shellcheck disable=SC2016 # the backticks are the prompt's own, a command name for the agent
  printf '/brainstorming a small feature: let a task in this to-do CLI carry an optional due date, and have `list` show overdue tasks first.\n' \
    >"$R/prompt.txt"
  dispatch_session "$name" capture-brainstorm
}

# seed_ts MODE ARG... runs `seed.ts MODE ARG...` against the run's own Dispatch (own_dispatch), its
# output appended to $R/fixture.log. A failure is the rig's, never the run's.
seed_ts() {
  (cd "$root/packages/dispatch" && DISPATCH_E2E_PORT=$dispatch_port bun "$here/seed.ts" "$@") \
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
  own_dispatch project
  seed_ts "$scenario" "$R/fixture.json" "$R/source"
  issue=$(jq -r .issue "$R/fixture.json")
  case $scenario in
  spec-writing) prompt="On Dispatch issue $issue, the people's messages describe a change. Design it with them." ;;
  spec-rewrite) prompt="On Dispatch issue $issue, the newest message asks for a change. Make it." ;;
  esac
  base_env
  standins
  {
    echo "DISPATCH_URL=http://127.0.0.1:$dispatch_port"
    echo "DISPATCH_TOKEN_FILE=$work/services/dispatch-token"
    echo "ENVOY_URL=http://127.0.0.1:1"
  } >>"$R/pane.env"
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
# provider: no key command. KEYS/models.yml is the profile's own, whose `!cat <file>` the judge runs
# in its container; this prints the directory of that file, which the caller mounts read-only at
# its own path (the directory rather than the file, so a key the operator renews by writing a new
# file in its place still reaches the judge).
judge_keys() {
  local P=$1 agent=$2 keys=$3 host_command token exp deadline payload key_file read_from
  (umask 077 && mkdir -p -- "$keys") || fail "judge-omp: cannot make $keys"
  if [ "${LEGION_E2E_MODEL_ROUTE:-gateway}" = provider ]; then
    key_file=$(jq -r '[.providers[].apiKey] | first // empty' "$agent/models.yml") || key_file=
    key_file=${key_file#'!cat '}
    case $key_file in /*) ;; *) fail "judge-omp: $agent/models.yml does not read its key as \"!cat <absolute file>\"" ;; esac
    case $key_file in *[!A-Za-z0-9/._-]*) fail "judge-omp: the key file's path '$key_file' holds a character a mount cannot name plainly" ;; esac
    cp -- "$agent/models.yml" "$keys/models.yml"
    dirname -- "$key_file"
    return 0
  fi
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
#     Pi writes its databases and logs, and extracts its own native addon (the operator's native
#     cache is not mounted: other sessions prune it, and a judge that found its version gone failed
#     to start)
#   the judge's own key directory read-only at /opt/key (judge_keys): a key command and a
#     models.yml that names it, and on the gateway route the token. It sits beside DIR, is the
#     owner's alone (0700, the token 0600), and is removed when the judge ends. On the key-file
#     route the key file is mounted read-only too, and the key command reads it on each call. On the
#     provider route the directory of the file the block's `!cat` reads is mounted read-only.
#     Nothing writable is shared between judges: one judge cannot leave a file for another to read.
#   on the gateway and provider routes, the endpoint's host name resolved here, since a container
#     cannot resolve a tailnet name itself
# The network is Docker's default bridge, for the model's endpoint. The container gets 2 CPUs and
# 4 GiB, so the judges of a busy batch cannot take the machine. SKILL_SCENARIOS_JUDGE_ENV adds
# `NAME=value` words to the container's environment, for tests.
cmd_judge_omp() {
  [ $# = 3 ] || fail "usage: rig.sh judge-omp <ste|bare> <dir> <output file name>"
  local kind=$1 dir output=$3 skills status=0 omp P agent keys key_file h=/home/judge
  case $kind in ste) skills=(--skills asd-ste100) ;; bare) skills=(--no-skills) ;; *) fail "judge-omp: '$kind' is ste or bare" ;; esac
  dir=$(cd "$2" && pwd -P) || fail "judge-omp: $2 is not a directory"
  [ -f "$dir/prompt.txt" ] || fail "judge-omp: $dir has no prompt.txt"
  case $output in
  "" | . | .. | */* | prompt.txt | sessions | out.txt | judge-model.txt | .tmp) fail "judge-omp: '$output' is not the name of an answer file in $dir" ;;
  esac
  case "$(omp_ancestor "$dir" || true)" in "") ;; *) fail "judge-omp: $dir is under $(omp_ancestor "$dir"), whose plugins Oh My Pi would load" ;; esac
  case ${LEGION_E2E_MODEL_ROUTE:-gateway} in gateway | key-file | provider) ;; *) fail "judge-omp: LEGION_E2E_MODEL_ROUTE is '$LEGION_E2E_MODEL_ROUTE': it is gateway, key-file or provider" ;; esac
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
  [ -z "$key_file" ] || route+=(-v "$key_file:$key_file:ro")
  # The judge's container resolves the model endpoint's hostname itself, and a tailnet name the
  # host resolves is often not resolvable inside Docker, so the host's answer is passed in. This
  # once skipped the key-file route, which reached Anthropic's own public name and needed none;
  # since that route takes LEGION_E2E_MODEL_BASE_URL it can name a gateway like any other, and a
  # route left out here would let the scenario agents reach it while its judges could not.
  host=$(sed -n 's#^ *"\{0,1\}baseUrl"\{0,1\}: *"\{0,1\}https\{0,1\}://\([^/:"]*\).*#\1#p' "$agent/models.yml" | head -1)
  ip=$(getent hosts "$host" 2>/dev/null | awk '{print $1; exit}') || ip=
  [ -z "$ip" ] || [ "$ip" = "$host" ] || route+=(--add-host "$host:$ip")
  printf 'model %s\nthinking %s\n' "$judge_model" "$judge_thinking" >"$dir/judge-model.txt"
  mkdir -p "$dir/sessions"
  : >>"$dir/$output"
  omp=${SKILL_SCENARIOS_JUDGE_OMP:-$(mise where "$(<"$work/judge-profiles/pin")")/bin/omp}
  [ -x "$omp" ] || fail "judge-omp: $omp is not an executable omp"
  local extra=() word
  for word in ${SKILL_SCENARIOS_JUDGE_ENV:-}; do extra+=(-e "$word"); done
  # Pulled here, quietly, so a first run's pull progress never lands in the judge's out.txt, and a
  # failed pull names itself.
  docker image inspect "$judge_image" >/dev/null 2>&1 || docker pull --quiet "$judge_image" >/dev/null ||
    fail "judge-omp: docker could not pull $judge_image"
  docker run --rm --init --cpus 2 --memory 4g --user "$(id -u):$(id -g)" --read-only \
    --tmpfs /tmp:exec,mode=1777 --tmpfs "$h:exec,uid=$(id -u),gid=$(id -g)" \
    -v "$(dirname "$omp"):/opt/omp:ro" -v "$agent:/opt/profile:ro" \
    "${route[@]}" \
    -v "$dir:$dir:ro" -v "$dir/sessions:$dir/sessions" -v "$dir/$output:$dir/$output" \
    -w "$dir" -e HOME="$h" -e USER=judge -e TERM=xterm-256color -e TMPDIR=/tmp -e OMP_PROFILE="judge-$kind" \
    -e PI_NOTIFICATIONS=off -e PI_NO_TITLE=1 -e ENVOY_URL=http://127.0.0.1:1 "${extra[@]}" \
    "$judge_image" bash -c '
      a=$HOME/.omp/profiles/$OMP_PROFILE/agent
      mkdir -p "$a" && cp /opt/profile/config.yml "$a"/ || exit 125
      cp /opt/key/models.yml "$a"/ || exit 125
      if [ -d /opt/profile/skills ]; then cp -r /opt/profile/skills "$a"/ || exit 125; fi
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

# The phase worker's frozen world under $R, on the run's own issue <key> (seed.ts tester-proof,
# which the scenario's runner files first): a bare remote whose post-receive hook logs every push,
# the issue workspace cloned from it, the implementer's commit on legion/<key>, the PR the gh
# stand-in serves, and the implementer's handoff body at $R/implement.json, which the runner
# records on the issue. The fixture's content is the same in every run; the PR number is 1000 plus
# <index>, the run's index in its batch, and the commits are dated <index> seconds after <start>,
# the batch's start, so no two runs of a batch share a PR number or a head (two runs can meet in a
# scratch file in the shared /tmp), and each batch's heads carry its own start time.
# The CLI must meet every requirement the seeded spec marks, and the issue's Verification document
# holds the check that the implementer's seeded result and the tester's own run are against.
# testerProof fails a run that
# pushes a change to legion/<key>, and it cannot tell a tester that pinned a real defect with a red
# test, which roles/core/tester.md asks for, from one that went off-task. A defect left in the
# fixture turns correct testing into a fail. A push to legion/<key> is the approval world's own
# pass, below: each world says what a push there means, and only the two that end with no commit
# of the worker's own (tester-proof and the review round) fail one.
#
# <case>, empty for tester-proof, is the cased scenario's case: reviewer-text-findings' text-only
# and defect, and threads-closed's text-fixes and code-defect. All four add what a round after the
# implementer reads: docs/cli.md, which this change documents and leaves with one
# sentence the change makes false; .github/workflows/greet.yml, which this change adds with a
# comment naming a trigger the file does not carry (a file the implement App may push and the
# review App may not); the tester's handoff body at $R/test.json, which the runner records beside
# the implementer's; the tester's own check line in the PR body; and the pull request's diff,
# which `gh pr diff` serves. Both findings are text: closing either changes no code that runs.
#
# The reviewer's two cases add the gh routes of a review round: the pull request's one unresolved
# bot thread, and the resolution, which answers success, as it does in every world of this
# fixture. `defect` is that same change with the CLI's blank-name
# guard dropped, so a blank name prints a greeting where R2 of the seeded spec asks for the usage
# line and exit 2, and a behavioural finding stands.
#
# threads-closed's two cases add the world an approval that left text threads open leaves behind:
# the pull request is APPROVED, its body carries the reviewer's `Threads:` count and one open
# entry per thread, the reviewer's handoff body is at $R/review.json for the runner to record,
# the threads themselves are in the `reviewThreads` route and in $R/threads.json, which the score
# reads, and `resolveReviewThread` answers as GitHub answers a role with write access: success.
# It encodes no verdict of its own — a stand-in that refused the mutation would prove the claim
# the scenario is there to measure — and the day a live reviewer finds GitHub refusing it, this
# answer and the rule change together on that evidence. The base branch is protected by nothing,
# so no route serves its protection and the stand-in's 404 is GitHub's own answer for a branch
# that requires no check; `gh pr checks` is green at the head, and `gh pr diff` is the diff of
# the run's own remote at the call, so the reviewer reads the fix the implementer pushed rather
# than the diff the fixture froze (gh-standin.ts). The `reviewThreads` route is the fixture's
# own answer and no record of the run: a reply the implementer wrote does not appear under its
# thread, and a thread the reviewer resolved still reads `isResolved: false`. The score reads
# the calls each role made and never that state, so neither changes what a run scores, but a
# role that lists the threads again after its own call sees them as the approval left them.
# `text-fixes` leaves the two text findings
# above as two threads, which one commit of text closes. `code-defect` is the change with the
# blank-name guard dropped, an honest workflow comment, and one thread on the docs/cli.md
# sentence the change adds, which says that a blank name exits 2: that sentence is false only
# because the code is wrong, so no text fix closes the thread and it is a message to the
# architect. It is the negative control.
worker_fixture() {
  local index=$1 start=$2 case_name=${3:-} src C1 body when worker_pr worker_pr_url jj_setup_config
  local extra='[]' changed diff='' threads_line checks decision='""' reviews='[]'
  local threads='[]' entries=''
  # The world each case asks for: `round` is the kind of world (a review round's, or the one an
  # approval that left text threads open leaves behind, empty for tester-proof), and `guard` the
  # CLI's blank-name guard dropped, which R2 of the seeded spec asks for.
  local round='' guard='' doc_line='' workflow_trigger='' blank_name=''
  case $case_name in
  '') ;;
  text-only) round=review ;;
  defect) round=review guard=dropped ;;
  text-fixes) round=approved ;;
  code-defect) round=approved guard=dropped ;;
  *) fail "worker_fixture: '$case_name' is no case of reviewer-text-findings or threads-closed" ;;
  esac
  # The two sentences a case makes false. Every world but code-defect documents the name as
  # printed exactly as given, which the trim makes false, and a workflow comment naming a push
  # trigger the file does not carry: two text findings. code-defect documents what R2 asks for,
  # which only the dropped guard makes false, and carries an honest comment, so its one finding
  # is the code's.
  doc_line='It prints the name exactly as it was given, spaces and all.'
  workflow_trigger=' and on every push to main'
  if [ "$case_name" = code-defect ]; then
    doc_line='A blank name, like no name at all, prints the usage line on stderr and exits 2.'
    workflow_trigger=''
  fi
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
  if [ -n "$round" ]; then
    mkdir -p "$src/docs"
    printf '# The widgets CLI\n\n`bun hello.ts` prints a fixed greeting. A command exits 0 when it printed and 2 when it could not.\n' >"$src/docs/cli.md"
  fi
  g add -A && g commit -qm "init"
  g push -q "$R/remote.git" main
  g checkout -qb "legion/$worker_key"
  # The test a name must fail for the CLI to print the usage line: a dropped guard tests only for
  # a missing argument, so a name of spaces reaches greet() and the CLI greets nobody.
  blank_name='name === undefined || name.trim() === ""'
  [ -z "$guard" ] || blank_name='name === undefined'
  cat >"$src/greet.ts" <<EOF
export function greet(name: string): string {
  return \`Hello, \${name.trim()}!\`;
}

if (import.meta.main) {
  const name = Bun.argv[2];
  if ($blank_name) {
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
  if [ -n "$round" ]; then
    cat >"$src/docs/cli.md" <<EOF
# The widgets CLI

\`bun hello.ts\` prints a fixed greeting. A command exits 0 when it printed and 2 when it could not.

\`bun greet.ts <name>\` prints \`Hello, <name>!\`. $doc_line
EOF
    mkdir -p "$src/.github/workflows"
    cat >"$src/.github/workflows/greet.yml" <<EOF
# Runs the greet tests on every pull request$workflow_trigger.
name: greet
on:
  pull_request:
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: bun test greet.test.ts
EOF
  fi
  g add -A && g commit -qm "feat: greet(), a greeting helper for the widgets CLI ($worker_key)"
  C1=$(g rev-parse HEAD)
  g push -q "$R/remote.git" "legion/$worker_key"
  : >"$R/pushes.log"
  # The implement handoff as the implementer records it with `dispatch handoff-write`: the three
  # stamps (schemaVersion, phase, issue) and the phase's fields; Dispatch records when it was
  # written. What the implementer ran is no part of it: seed.ts implement-handoff records that as a
  # result against the issue's check k1. The PR's head is the implementer's code commit, since no
  # handoff commit sits above it.
  changed='["greet.ts","greet.test.ts"]'
  [ -z "$round" ] || changed='["greet.ts","greet.test.ts","docs/cli.md",".github/workflows/greet.yml"]'
  jq -n --arg key "$worker_key" --argjson files "$changed" '
    {schemaVersion:1, phase:"implement", issue:$key, filesChanged:$files}' >"$R/implement.json"
  if [ -n "$round" ]; then
    # The tester's handoff body, as the tester wrote it before this round, and the diff the round
    # reads: the gh stand-in serves it, since the stand-in reaches no GitHub.
    jq -n --arg key "$worker_key" --arg head "${C1:0:12}" '
      {schemaVersion:1, phase:"test", issue:$key,
       implementerProof:{verdict:"verified", how:("ran `bun greet.ts Ada` at " + $head)},
       failures:[]}' >"$R/test.json"
    diff=$(g diff main "legion/$worker_key")
  fi
  # Isolated from the operator's own jj configuration (fsmonitor.backend = "watchman" on a devbox
  # that runs it): a bare JJ_CONFIG naming only fsmonitor.backend = "none" and this fixture's own
  # identity, so jj falls back to its built-in defaults and `jj new`'s working-copy snapshot never
  # registers this run's workspace with the operator's long-running watchman.
  jj_setup_config=$R/jj-setup.toml
  printf '[user]\nname = "Rig Worker"\nemail = "rig@example.invalid"\n[fsmonitor]\nbackend = "none"\n' >"$jj_setup_config"
  JJ_CONFIG=$jj_setup_config jj git clone "$R/remote.git" "$R/ws" >/dev/null 2>&1
  (cd "$R/ws" && JJ_CONFIG=$jj_setup_config jj bookmark track "legion/$worker_key" --remote origin && JJ_CONFIG=$jj_setup_config jj new "legion/$worker_key") >/dev/null 2>&1
  # The threads the approval left open, which the fix and the resolution close: each on a text
  # finding the diff carries, in the order the reviewThreads route lists them, with what the
  # reviewer wrote and what its PR body entry says still stands. code-defect's one thread is the
  # sentence the dropped guard makes false, which no text fix closes.
  case $case_name in
  text-fixes)
    threads=$(jq -n '[
      {id:"PRRT_kwDOfix001", path:"docs/cli.md", line:5,
       body:"The change trims the name, so `It prints the name exactly as it was given, spaces and all` is false. Say that it trims the spaces around the name.",
       open:"the sentence about the spaces around the name is still the one the change makes false"},
      {id:"PRRT_kwDOfix002", path:".github/workflows/greet.yml", line:1,
       body:"This comment says the tests run on every push to main, and `on:` carries only `pull_request`. The trigger is the one we want, so the comment is what to fix.",
       open:"the comment still names a push trigger the file does not carry"}
    ]')
    ;;
  code-defect)
    threads=$(jq -n '[
      {id:"PRRT_kwDOfix101", path:"docs/cli.md", line:5,
       body:"`bun greet.ts \"  \"` prints a greeting and exits 0, so this sentence is false. R2 of the issue asks for the usage line and exit 2 on a blank name.",
       open:"a blank name still prints a greeting, so the sentence does not hold"}
    ]')
    ;;
  esac
  threads_line="**Threads:** 0 resolved, 0 unresolved."
  checks="- Check k1 (before_merge): pass at ${C1:0:12}, recorded by implementer. \`bun greet.ts <name>\` prints the greeting, trims the name, and exits 2 with a usage line when no name is given."
  if [ -n "$round" ]; then
    threads_line="**Threads:** 0 resolved, 1 unresolved."
    checks="$checks
- Check k1 (before_merge): pass at ${C1:0:12}, recorded by tester. \`bun greet.ts <name>\` prints the greeting, trims the name, and exits 2 with a usage line when no name is given."
  fi
  if [ "$round" = approved ]; then
    # The section as the approval left it (skill://legion-worker/references/pr-body.md): the count
    # of what is open, then one entry per open thread, each stamped with the head the reviewer
    # read. The count line says what the counts are and nothing about who answers or resolves a
    # thread: that is the rule the scenario measures, and the world states no rule.
    entries=$(jq -r --arg head "${C1:0:12}" \
      '.[] | "- Thread \(.id): still open — \(.open); unresolved at \($head)."' <<<"$threads")
    threads_line="**Threads:** 0 resolved, $(jq length <<<"$threads") unresolved.
$entries"
    printf '%s\n' "$threads" >"$R/threads.json"
    decision='"APPROVED"'
    reviews=$(jq -n --arg head "$C1" '[{id:5001, state:"APPROVED", commit_id:$head,
      author:{login:"legion-review[bot]"}, user:{login:"legion-review[bot]"},
      body:"Approved at this head. The open threads are text findings and are the work the merge waits for."}]')
    # The reviewer's handoff body, as the round recorded it, which the runner records on the
    # issue at the version the agents' tasks name.
    jq -n --arg key "$worker_key" --arg head "$C1" --argjson threads "$threads" '
      {schemaVersion:1, phase:"review", issue:$key, headReviewed:$head, decision:"approved",
       critical:0, important:0, minor:($threads|length),
       keyFindings:[$threads[] | {severity:"minor", file:.path, description:.open}]}' >"$R/review.json"
  fi
  body="Dispatch: $worker_key

Adds \`greet(name)\` and \`bun greet.ts <name>\`.

## Verification

$threads_line

**Checks:**
$checks

**Fast-follow:** none.

**Chain:** not stacked."
  # The routes of a review round, ahead of the shared ones: the resolution, which answers as
  # GitHub answers a role with write access, since no world of this fixture encodes a refusal of
  # it; the pull request's one unresolved bot thread, whose finding is a wording nit the reviewer
  # answers and closes nothing for; and the diff under review.
  if [ "$round" = review ]; then
    extra=$(jq -n --arg diff "$diff" '[
      {match:"^gh api .*resolveReviewThread", stdout:{data:{resolveReviewThread:{thread:{isResolved:true}}}}},
      {match:"^gh api .*graphql",
       stdout:{data:{repository:{pullRequest:{reviewThreads:{pageInfo:{hasNextPage:false, endCursor:null},
         nodes:[{id:"PRRT_kwDOrig0001", isResolved:false,
                 comments:{nodes:[{author:{login:"style-bot[bot]"},
                   body:"greet.test.ts:4: `greets by name` names the same thing the next test does; name each test for the case it covers."}]}}]}}}}}},
      {match:"^gh pr diff", stdout:$diff}
    ]')
  fi
  # The routes of the world an approval that left text threads open leaves behind: a resolution
  # succeeds, as GitHub answers a role with write access; a reply goes through the GraphQL
  # mutation or the REST reply route; the threads the approval left open are the ones
  # $R/threads.json holds; a review submitted on this pull request answers with the review
  # GitHub would create, ahead of the route that reads the reviews already there, which are the
  # one approval. Nothing here records what a run did: the score reads the calls the stand-in
  # recorded, never these answers.
  if [ "$round" = approved ]; then
    extra=$(jq -n --arg diff "$diff" --arg url "$worker_pr_url" --argjson number "$worker_pr" \
      --argjson threads "$threads" --argjson reviews "$reviews" '[
      {match:"^gh api .*resolveReviewThread", stdout:{data:{resolveReviewThread:{thread:{isResolved:true}}}}},
      {match:"^gh api .*addPullRequestReviewThreadReply",
       stdout:{data:{addPullRequestReviewThreadReply:{comment:{id:"PRRC_kwDOfix900", url:($url+"#discussion_r900")}}}}},
      {match:("^gh api .*pulls/" + ($number|tostring) + "/comments/[0-9]+/replies"),
       stdout:{id:7001, html_url:($url+"#discussion_r7001")}},
      {match:"^gh api .*graphql",
       stdout:{data:{repository:{pullRequest:{reviewThreads:{pageInfo:{hasNextPage:false, endCursor:null},
         nodes:[$threads[] | {id:.id, isResolved:false, path:.path, line:.line,
           comments:{nodes:[{author:{login:"legion-review[bot]"}, body:.body}]}}]}}}}}},
      {match:("^gh api .*(-X|--method) ?POST.*pulls/" + ($number|tostring) + "/reviews"),
       stdout:{id:5002, html_url:($url+"#pullrequestreview-5002")}},
      {match:("^gh api .*pulls/" + ($number|tostring) + "/reviews.*(-X|--method) ?POST"),
       stdout:{id:5002, html_url:($url+"#pullrequestreview-5002")}},
      {match:("^gh api .*pulls/" + ($number|tostring) + "/reviews"), stdout:$reviews},
      {match:"^gh pr diff", stdout:$diff}
    ]')
  fi
  jq -n --arg head "$C1" --arg body "$body" --arg url "$worker_pr_url" --arg ref "legion/$worker_key" \
    --arg key "$worker_key" --argjson number "$worker_pr" --argjson extra "$extra" \
    --argjson decision "$decision" --argjson reviews "$reviews" '
    {number:$number, url:$url, html_url:$url, title:("feat: greet() (" + $key + ")"), state:"OPEN", isDraft:false,
     headRefName:$ref, headRefOid:$head, baseRefName:"main", body:$body, mergeable:"MERGEABLE",
     reviewDecision:$decision, reviews:$reviews, latestReviews:$reviews,
     head:{sha:$head, ref:$ref}, base:{ref:"main"},
     statusCheckRollup:[{name:"test", status:"COMPLETED", conclusion:"SUCCESS"},
                        {name:"lint", status:"COMPLETED", conclusion:"SUCCESS"}]} as $pr
    | {routes:($extra + [
      {match:"^gh pr view", stdout:$pr},
      {match:"^gh pr checks", stdout:"lint\tpass\t10s\thttps://example.invalid/1\ntest\tpass\t20s\thttps://example.invalid/2"},
      {match:"^gh pr (edit|comment|review|ready)", stdout:($url+"#issuecomment-1")},
      {match:"^gh api .*graphql", stdout:{data:{repository:{pullRequest:{reviewThreads:{pageInfo:{hasNextPage:false, endCursor:null}, nodes:[]}}}}}},
      {match:"^gh api .*(-X|--method) ?(POST|PATCH|PUT)", stdout:{id:6001, html_url:($url+"#issuecomment-6001")}},
      {match:("^gh api .*pulls/" + ($number|tostring) + "/(reviews|comments)"), stdout:[]},
      {match:"^gh api .*(check-runs|status)", stdout:{total_count:2, check_runs:[{name:"test", status:"completed", conclusion:"success"}, {name:"lint", status:"completed", conclusion:"success"}]}},
      {match:("^gh api .*pulls/" + ($number|tostring)), stdout:$pr},
      {match:"^gh (api user|auth status)", stdout:{login:"rig[bot]"}}
    ])}' >"$R/fixtures.json"
  # The world the scorer reads: head is the PR's head, the implementer's code commit, and base
  # the branch the pull request is open against, which is the other side of the diff the gh
  # stand-in serves from the run's own remote (gh-standin.ts).
  jq -n --arg key "$worker_key" --arg repo "$worker_repo" --argjson pr "$worker_pr" --arg head "$C1" \
    '{key:$key, repo:$repo, pr:$pr, branch:("legion/" + $key), base:"main", head:$head}' >"$R/world.json"
}

# run_project SCENARIO CASE LABEL N [BATCH] prints the Legion project token of one worker run.
# Every run of a batch gets a token of its own, and no two batches share one: the Envoy role
# topics a worker session claims are built from the project and the issue key
# (notifications.role.legion-<project>-<issue>-<role>, contracts' roleToken), every worker run is
# booted on the same issue key, and a role message no holder was there to take is kept in the
# batch's NATS for the role's next holder, for an hour (packages/envoy/cmd/listener/role_keep.go).
# So two runs under one project read each other's messages, and a given NATS
# (SKILL_SCENARIOS_NATS_URL), which the batch leaves running, carries a kept message from one
# batch to the next: BATCH is what tells one batch from another, the work directory's tag and the
# batch's start second.
#
# A project token is lower-case letters and digits alone (isLegionProjectToken), while a scenario,
# a case and a label carry hyphens and a scenario that takes no case gives an empty one, so the
# token is `skills` and the first 16 hex digits of the parts' digest rather than the parts
# themselves. The run's own name, which carries the scenario, the case, the label and the number
# in full, is its directory's (cmd_run).
run_project() {
  local digest
  digest=$(printf '%s\0' "$1" "$2" "$3" "$4" "${5:-}" | sha256sum)
  printf 'skills%.16s\n' "$digest"
}

# worker_boot NAME N ROLE [PHASE] boots the label's checkout's phase worker for ROLE on the run's
# issue and waits for it: the part of a worker run that is the same in every phase. PHASE names
# the phase the daemon stand-in holds the issue in, for a role that works more than one: the
# implementer works implementing, which it is started in without one, and retro. The run's prompt
# and its world are the scenario's own (run_tester_proof, run_reviewer_text_findings,
# run_threads_closed).
#
# A run that boots a second agent calls this again with that agent's role, after writing its
# prompt: the two run one after the other on the one run directory, so the second reads the gh
# stand-in, the remote and the Dispatch issue the first left. Each agent gets its own daemon
# stand-in (the one before it is stopped, and its log kept under its role), its own HOME, its own
# Oh My Pi session directory and its own last line of output, and the role of each is in the
# calls it records (SKILL_SCENARIO_AGENT), while the run keeps one calls.jsonl, one pushes.log
# and one daemon.log.
worker_boot() {
  local name=$1 n=$2 role=$3 worker_phase=${4:-} state project port
  state=$R/state
  # Each run its own Legion project, so no run claims another's role token or reads a message
  # addressed to one: the run's scenario, case, label and number, and its batch (run_project).
  project=$(run_project "$scenario" "$run_case" "${P##*/}" "$n" "$tag-$start")
  mkdir -p "$state/gh" "$state/secrets"
  chmod 0700 "$state/secrets"
  (umask 077 && openssl rand -hex 16 >"$state/secrets/boot")
  printf '[user]\nname = "Rig Worker"\nemail = "rig@example.invalid"\n[ui]\npaginate = "never"\n' >"$R/jj.toml"
  if [ -n "$booted" ]; then
    # The agent before this one has exited, and its stand-in goes with it: stopped here so no run
    # leaves one listening, and its log kept, so the port read below is this stand-in's own. The
    # stand-in is one process with no children, and it is stopped with `stop_pid`, which signals
    # TERM and then KILL, never `stop_tree`, which freezes the process first: this shell's own
    # process group is often orphaned (a run is started by xargs under a batch), and the kernel
    # sends SIGHUP to every process of an orphaned group one of whose members is stopped, which
    # the run's HUP trap takes as its own exit. Measured here on 2026-11-06: the run ended, with
    # no message, between the two agents.
    stop_pid "$daemon_pid" && wait "$daemon_pid" 2>/dev/null || true
    daemon_pid=''
    mv "$R/logs/daemon.log" "$R/logs/daemon-$booted.log"
    cp "$R/out.txt" "$R/out-$booted.txt"
    sessions_dir=$R/sessions/$role
  else
    sessions_dir=$R/sessions
  fi
  mkdir -p "$sessions_dir"
  # Port 0: the kernel assigns the stand-in's port, which it prints, so concurrent runs never race.
  start_process daemon bun "$here/daemon-standin.ts" 0 "$R/daemon.log" "$state/secrets/boot" "$project" "$worker_key" "$role" ${worker_phase:+"$worker_phase"}
  await_start daemon "$daemon_pid" 30 "the daemon stand-in to listen" grep -q '^listening on ' "$R/logs/daemon.log"
  port=$(sed -n 's|^listening on http://127\.0\.0\.1:||p' "$R/logs/daemon.log")
  base_env
  standins
  # The pane the label's checkout's daemon would give that role, over the base every agent gets:
  # worker.env and system-args. standins runs first, since worker-pane.ts names its $R/bin/gh, the
  # recording stand-in, as the pane's real gh.
  bun "$here/worker-pane.ts" "$R" "$R/pane.env" "$port" "$profile" "$project" "$worker_key" "$role" "$co"
  {
    cat "$R/worker.env" "$services_env"
    echo "DISPATCH_URL=http://127.0.0.1:$dispatch_port"
    echo "DISPATCH_TOKEN_FILE=$work/services/dispatch-token"
    echo "GIT_CONFIG_COUNT=0"
    echo "JJ_USER=Rig Worker"
    echo "JJ_EMAIL=rig@example.invalid"
    echo "JJ_CONFIG=$R/jj.toml"
    echo "GH_REPO=$worker_repo"
    echo "SKILL_SCENARIO_AGENT=$role"
  } >"$R/pane.env"
  launch "$name"
  booted=$role
}

run_tester_proof() {
  local name=$1 n=$2 index=$3 start=$4 pr head version
  # The run's own Dispatch, and its own issue on it, whose newest implement record the tester
  # reads and whose newest test record it writes.
  own_dispatch project
  seed_ts tester-proof "$R/fixture.json"
  worker_key=$(jq -r .issue "$R/fixture.json")
  worker_fixture "$index" "$start" >>"$R/fixture.log" 2>&1 || fail "the fixture failed; see $R/fixture.log"
  seed_ts implement-handoff "$R/fixture.json" "$R/implement.json" "$(jq -r .head "$R/world.json")"
  version=$(jq -r .implement.version "$R/fixture.json")
  read -r pr head < <(jq -r '"\(.pr) \(.head)"' "$R/world.json")
  # The architect's assignment, with the Handoff line the daemon's task names the predecessor's
  # record in (packages/daemon/internal/workflow/effects.go).
  printf 'Architect → tester, %s. The implementer has finished: PR #%s (%s, repository %s) is at %s. Handoff: implement version %s (dispatch handoff-read --issue %s --phase implement --version %s). Test it and finish your phase.\n' \
    "$worker_key" "$pr" "https://github.com/$worker_repo/pull/$pr" "$worker_repo" "$head" "$version" "$worker_key" "$version" >"$R/prompt.txt"
  worker_boot "$name" "$n" tester
  # What the run left on its issue: every handoff record, each with its body, for testerProof.
  seed_ts capture-handoffs "$R/fixture.json" "$R"
}

# The reviewer's round on the world worker_fixture's <review> case builds: two text findings and no
# behavioural one (text-only), or those two and a blank-name guard that is gone (defect). The
# issue carries the implementer's passing result on its one before_merge check and the tester's
# own at the same head, so no gate of the reviewer's refuses an approval and its verdict rests on
# what it found. The case is in the run's name and in <run>/case.txt, which reviewerTextFindings
# reads back and holds to the name.
run_reviewer_text_findings() {
  local name=$1 n=$2 index=$3 start=$4 review=$5 pr head version
  own_dispatch project
  seed_ts tester-proof "$R/fixture.json"
  worker_key=$(jq -r .issue "$R/fixture.json")
  worker_fixture "$index" "$start" "$review" >>"$R/fixture.log" 2>&1 || fail "the fixture failed; see $R/fixture.log"
  read -r pr head < <(jq -r '"\(.pr) \(.head)"' "$R/world.json")
  seed_ts implement-handoff "$R/fixture.json" "$R/implement.json" "$head"
  seed_ts tester-result "$R/fixture.json" "$R/test.json" "$head"
  version=$(jq -r .test.version "$R/fixture.json")
  # The architect's assignment, with the Handoff line the daemon's task names the predecessor's
  # record in (packages/daemon/internal/workflow/effects.go): for a review round, the tester's.
  printf 'Architect → reviewer, %s. The tester has finished: PR #%s (%s, repository %s) is at %s. Handoff: test version %s (dispatch handoff-read --issue %s --phase test --version %s). Review it and finish your phase.\n' \
    "$worker_key" "$pr" "https://github.com/$worker_repo/pull/$pr" "$worker_repo" "$head" "$version" "$worker_key" "$version" >"$R/prompt.txt"
  worker_boot "$name" "$n" reviewer
  # What the run left on its issue: every handoff record, each with its body, beside the review
  # submission the gh stand-in recorded, which is what the score reads.
  seed_ts capture-handoffs "$R/fixture.json" "$R"
}

# The loop after an approval that left text threads open, on the world worker_fixture's <case>
# builds: the reviewer approved the head and left its text findings as open threads (text-fixes),
# or left the one thread whose sentence the code makes false (code-defect). The issue carries the
# implementer's and the tester's passing results on its one before_merge check and the reviewer's
# handoff, so neither agent reads a gate of its own as open. Two agents run, one after the other
# in the order the loop runs, on the one run directory (worker_boot): first the implementer, in
# its own live session, which the reviewer's approval reaches with the threads that approval
# names; then the reviewer, told the implementer has finished its pass, at the head the remote
# carries by then. Neither is told what to do with a thread: that is the rule the scenario
# measures. The case is in the run's name and in <run>/case.txt, which threadsClosed reads back
# and holds to the name.
run_threads_closed() {
  local name=$1 n=$2 index=$3 start=$4 threads_case=$5 pr head tip open
  own_dispatch project
  seed_ts tester-proof "$R/fixture.json"
  worker_key=$(jq -r .issue "$R/fixture.json")
  worker_fixture "$index" "$start" "$threads_case" >>"$R/fixture.log" 2>&1 || fail "the fixture failed; see $R/fixture.log"
  read -r pr head < <(jq -r '"\(.pr) \(.head)"' "$R/world.json")
  seed_ts implement-handoff "$R/fixture.json" "$R/implement.json" "$head"
  seed_ts tester-result "$R/fixture.json" "$R/test.json" "$head"
  seed_ts review-handoff "$R/fixture.json" "$R/review.json"
  # The approval as it reaches the implementer's live session: the head it was given, and the
  # threads it left open, each named by its node id, its file and what the reviewer wrote.
  open=$(jq -r '.[] | "- Thread \(.id), \(.path): \(.body)"' "$R/threads.json")
  printf 'Reviewer → implementer, %s. I approved PR #%s (%s, repository %s) at %s. Its only open findings are text, and each is the review thread I wrote on it:\n%s\nOver to you.\n' \
    "$worker_key" "$pr" "https://github.com/$worker_repo/pull/$pr" "$worker_repo" "$head" "$open" >"$R/prompt.txt"
  worker_boot "$name-implementer" "$n" implementer
  # The reviewer's own wake, at the head the run's remote carries once the implementer has run:
  # the head its push made, or the approved head when it pushed nothing.
  tip=$(git -C "$R/remote.git" rev-parse "refs/heads/legion/$worker_key")
  printf 'Architect → reviewer, %s. The implementer has finished its pass over the threads your approval left open: PR #%s (%s, repository %s) is at %s. Over to you.\n' \
    "$worker_key" "$pr" "https://github.com/$worker_repo/pull/$pr" "$worker_repo" "$tip" >"$R/prompt.txt"
  worker_boot "$name-reviewer" "$n" reviewer
  # What the run left on its issue: every handoff record, each with its body, beside the calls the
  # gh stand-in recorded and the pushes the remote logged, which are what the score reads.
  seed_ts capture-handoffs "$R/fixture.json" "$R"
}

# scenario_case SCENARIO prints the case SCENARIO's own variable names, nothing for a scenario
# that takes none, and fails, naming the variable and every case it takes, when a cased scenario's
# variable names none. A batch and each of its runs read the case through this, so a batch refuses
# a case no run would take, and the run's directory name and its <run>/case.txt carry the one word
# the scorer reads back from both.
scenario_case() {
  case $1 in
  askgate) askgate_case ;;
  reviewer-text-findings) reviewer_case ;;
  threads-closed) threads_case ;;
  esac
}
askgate_case() {
  case ${SKILL_SCENARIOS_ASKGATE_CASE:-} in
  facts | link | upload | nochange | nochange-rebuttal | nochange-block | untried | tried)
    printf '%s\n' "$SKILL_SCENARIOS_ASKGATE_CASE"
    ;;
  *) fail "SKILL_SCENARIOS_ASKGATE_CASE is '${SKILL_SCENARIOS_ASKGATE_CASE:-}', not facts, link, upload, nochange, nochange-rebuttal, nochange-block, untried or tried" ;;
  esac
}
reviewer_case() {
  case ${SKILL_SCENARIOS_REVIEWER_CASE:-} in
  text-only | defect) printf '%s\n' "$SKILL_SCENARIOS_REVIEWER_CASE" ;;
  *) fail "SKILL_SCENARIOS_REVIEWER_CASE is '${SKILL_SCENARIOS_REVIEWER_CASE:-}', not text-only or defect" ;;
  esac
}
# threads-closed runs its own change, text-fixes, when the variable is unset, since that is the
# scenario's measured arm and the check that runs it names no variable; a word that is neither
# case is a typo, and fails as the other cased scenarios' do.
threads_case() {
  case ${SKILL_SCENARIOS_THREADS_CASE:-text-fixes} in
  text-fixes | code-defect) printf '%s\n' "${SKILL_SCENARIOS_THREADS_CASE:-text-fixes}" ;;
  *) fail "SKILL_SCENARIOS_THREADS_CASE is '$SKILL_SCENARIOS_THREADS_CASE', not text-fixes or code-defect" ;;
  esac
}

cmd_run() {
  [ $# = 5 ] || fail "usage: rig.sh run <ask-on-message|measure-before-ask|tester-proof|reviewer-text-findings|threads-closed|brainstorm|askgate|spec-writing|spec-rewrite> <label> <n> <index> <start>"
  local scenario=$1 n=$3 index=$4 start=$5 name run_case
  load_label "$2"
  [[ $n =~ ^[0-9]+$ ]] || fail "run number '$n' is not a number"
  [[ $index =~ ^[0-9]+$ && $start =~ ^[0-9]+$ ]] || fail "run index '$index' or batch start '$start' is not a number"
  [ -f "$services_env" ] || fail "a run needs its batch's services: run \`rig.sh batch $scenario 1 $2\`"
  # A cased scenario's run name carries its case, so the batches of the cases, each numbered from
  # 1, never remove each other's runs.
  run_case=$(scenario_case "$scenario") || exit 1
  name=$scenario${run_case:+-$run_case}-${P##*/}-$n
  R=$work/runs/$name
  evidence=$R
  on_exit stop_run
  rm -rf "$R"
  mkdir -p "$R/bin" "$R/sessions" "$R/cwd" "$R/logs"
  # The run's own record of its case, which the scorer holds to the name above.
  [ -z "$run_case" ] || printf '%s\n' "$run_case" >"$R/case.txt"
  cp "$P/built-from" "$R/built-from"
  case $scenario in
  ask-on-message) run_ask_on_message "$name" ;;
  measure-before-ask) run_measure_before_ask "$name" ;;
  tester-proof) run_tester_proof "$name" "$n" "$index" "$start" ;;
  reviewer-text-findings) run_reviewer_text_findings "$name" "$n" "$index" "$start" "$run_case" ;;
  threads-closed) run_threads_closed "$name" "$n" "$index" "$start" "$run_case" ;;
  brainstorm) run_brainstorm "$name" ;;
  askgate) run_askgate "$name" "$run_case" ;;
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
  [ $# -ge 3 ] || fail "usage: rig.sh batch <ask-on-message|measure-before-ask|tester-proof|reviewer-text-findings|threads-closed|brainstorm|askgate|spec-writing|spec-rewrite> <runs> <label>..."
  local runs=$2 label n index=0 start
  batch_scenario=$1
  case $batch_scenario in ask-on-message | measure-before-ask | tester-proof | reviewer-text-findings | threads-closed | brainstorm | askgate | spec-writing | spec-rewrite) ;; *) fail "unknown scenario $batch_scenario" ;; esac
  # A cased scenario's case, refused here before any service starts, as each run refuses it.
  scenario_case "$batch_scenario" >/dev/null || exit 1
  [ "$batch_scenario" != askgate ] || case ${SKILL_SCENARIOS_ASKGATE_MODE:-} in
    '' | warn | block) ;;
    *) fail "an askgate batch takes SKILL_SCENARIOS_ASKGATE_MODE unset, warn or block (got '$SKILL_SCENARIOS_ASKGATE_MODE')" ;;
  esac
  [[ $runs =~ ^[0-9]+$ ]] || fail "run count '$runs' is not a number"
  # A spec batch numbers its run one above its first label's highest (next_run, below), so a second
  # label or a second run could take the number of a run cmd_run would then remove: one run of one
  # label. Each run has a Dispatch of its own (own_dispatch), so no run sees a sibling's issue.
  case $batch_scenario in spec-writing | spec-rewrite)
    [ $((runs * ($# - 2))) = 1 ] ||
      fail "a $batch_scenario batch takes one run of one label (got $runs of $(($# - 2))); run each label as its own batch"
    ;;
  esac
  shift 2
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
