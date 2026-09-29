# Changelog

## [Unreleased]

### Added

- The planner, tester, reviewer and implementer role texts each say that under the Go daemon the
  issue branch is pushed with `legion push`, which runs the worker skill's push procedure and
  decides whether the push skips CI. Before, only the Go worker prompt said so, and a tester that
  followed its own role text pushed every handoff with the skill's commands, so each ran full CI
  (LEGION-208).
- The session publishes its own conversation for Dispatch's agent conversation view (LEGION-232):
  every turn, tool call and streamed update becomes a frame on `agentstream.<session id>.frames`
  over core NATS, a subject family the notification stream does not capture, so the bus retains
  none of it. It publishes only while a viewer is attached — the Dispatch relay asks on
  `agentstream.<session id>.control`, and a session that hears nothing for thirty seconds goes
  quiet — and a viewer's replay is answered from a bounded in-memory ring (200 messages, 512 KiB)
  that never leaves the process. Nothing about it is written to Dispatch's database.

- The run-end silent self-check now runs on every normal settle of an eligible session, including
  sessions that already hold open asks. Its one prompt names the first line of up to five open ask
  questions (or says there are none), truncates each at about 120 characters, and normalizes
  Unicode line separators and C0/C1 controls so a question cannot add prompt lines. It asks
  whether the agent is waiting on a human for something those asks do not cover. A WAITING verdict
  produces the same one `dispatch-ask-reminder` steer for every session; PROCEEDING remains silent.
  The existing normal-settle, UI-host, Legion-managed/task-subagent, period, budget,
  stale-generation, open-mid-check abort, and one-check-at-a-time guards are unchanged.
- The package ships the task agents Legion's skills dispatch, in `agents/`: `oracle` (`legion-oracle`) and `thermonuclear-deep-review` and `thermonuclear-code-quality` (the reviewer's pair in `legion-worker`), copied from the operator's definitions. Oh My Pi finds an installed plugin's `agents/` in a pane and an explicit extension root's in a pod, so a worker's `task(agent="…")` no longer depends on agent files in the operator's profile. Before, a worker without them got `Unknown agent … Available: scout, reviewer, …` and carried on, usually by substituting `reviewer` (LEGION-200). Each declares an Oh My Pi model role the operator's `modelRoles` maps: `@review` for the pair, `@oracle` for `oracle`.
- The skills those prompts load ship with the others under `dist/skills`: the pair's rubrics (`thermonuclear-deep-review`, `thermonuclear-code-quality`) and the implementer's simplify pass (`ce-simplify-code`, Legion's copy of the MIT-licensed Compound Engineering skill, dispatching its personas to the bundled `reviewer`). Before, the pair loaded a rubric only an operator profile carrying the dotfiles had, and reviewed from a five-step outline without it (LEGION-200).
- Under the Go daemon (`LEGION_DAEMON_API=go`), the operator-launched controller (`legion controller start`) runs as a controller session instead of being refused: it registers on `/legion/v1/claims/register` with its controller capability, claims `legion-<project>-controller`, and mints each bash command's grant from the `/grants` controller-session form with the secret its registration was issued.
- Under the Go daemon, the controller subscribes to its project's controller topic (`notifications.legion.<project>.controller`, `legionControllerNoticeSubject` in `@legion/contracts`, the project from `LEGION_PROJECT`) once it claims its role, for as long as it holds the role; what the daemon publishes there is listed at the Go daemon's `notify.ControllerTopic`. A controller that a later `legion controller start` replaces closes the subscription when its next heartbeat finds the role held by the new session, so it stops taking wakes within one heartbeat, and a dropped connection's retries do not reopen it once the role has ended; a `/new` or `/resume` keeps it open whichever extension handles the switch first; the subscription is never registered with the listener, so the replaced session resumed later does not get it back. Before registering, the controller reads the daemon's project from `GET /legion/v1/state` and refuses a `LEGION_PROJECT` that is unset or names another project's controller role, since a registration replaces the running controller. The subscription is a live wake: an Oh My Pi session subscribes over core NATS, so a notice published while no controller runs never reaches one, and the controller skill reads `legion state` at every start for held issues and failed tree architects, and Dispatch's triage listing for roots created while none ran. A controller on an earlier release never runs against this daemon: the daemon refuses its registration with 409 (contract 7).
- Under the Go daemon, the architect's `legion` tool gains `park_child` and `rerun_child` (`POST /legion/v1/children/park` and `/rerun`). `park_child` takes a running child of the architect's tree out of the workflow by moving it to `backlog`; `rerun_child` runs a parked or signed-off child again from planning by moving it to `todo`. Each is the Dispatch move a human would make, and the daemon reacts to it as to the human's: the child's workers are suspended, or its next run starts under the tree. Neither takes the tree root.
- Phase workers end their phase with the `legion` tool: `handoff_write`, `handoff_read`, and `handoff_complete` run the daemon's own `legion handoff` commands (the TypeScript-daemon tool is now registered for every worker), and the role prompts, the worker skills, and the Go daemon's per-role prompt parts say so. A phase worker whose run settles with its phase still open gets one follow-up in its own session (run `handoff_complete`, or reply WAITING), which names a tool call the model wrote as text (LEGION-208 4b.15).
- The Legion extension refuses `legion handoff complete` outside the `legion` tool in a phase-worker pane (a sub-architect's included, and any `task` subagent's) and in a root architect's: a `bash` command in any position of a chain, `eval` code, and a `hub` process start, by the same tokenised and plain-text rules as the jj operation-log guard. A completion run from the shell never reached the phase stall, which then asked the worker to complete again. `legion handoff write` and `read` stay open to the shell. The tool's `handoff_write` sends its payload on stdin, so a handoff over the 128 KiB cap on one argv string no longer fails with E2BIG; the Go CLI's `legion handoff write` reads stdin when `--data` is omitted, as the TypeScript CLI does. The tool carries no message action: a question for another live role goes to its role topic with `envoy_publish` (LEGION-208 4b.15b).
- The Legion extension refuses `jj undo`, `jj abandon`, and `jj op restore|revert|abandon|undo` in every phase-worker pane before they run — a `bash` command in any position of a pipeline or `&&` chain, with or without `-R`, judged on each `jj` invocation's whole argument list (so `jj --repository <path> undo` and `jj operation restore` count); `eval` code; and a `hub` process start, both by a plain-text rule (the text mentions `jj` with one of the words) — from the worker's own tool calls and from any `task` subagent it spawns, the one gate that binds a subagent (it runs in the same pane, against the same log). Every Legion issue workspace is a `jj workspace` of one clone, so those commands rewrite the operation log for every tree at once (LEGION-45: on 2026-09-12 one worker's `jj undo` rewrote nine of another tree's commits). `jj restore <paths>`, `jj op log`, and `jj op show` stay allowed; the refusal names the command, says the log is shared, and gives the recovery rule. The root architect and controller panes are unaffected.
- Added the nine native Dispatch tools and automatic subscriptions to each mutation result's issue topic.
- Reviewer, implementer, and merger role prompts (and the `legion-worker` skill) name the three thread reply forms — `Accepted: fixed in <commit> — <one line>`, `Accepted: not a defect — <reason>`, `Still open: <what remains>` — and `legion threads resolve --pr <n> --repo <owner>/<repo>`, which the implementer runs before every push that answers a review and the merger before READY to resolve the threads the reviewer accepted (the review App cannot; LEGION-34).
- The implementer's core role prompt says its reply on a review thread names what changed (`Fixed in <commit>: <one line>` or `Declined: <reason>`) and never begins with `Accepted:`. `legion threads resolve` resolves a thread whose newest reply is an `Accepted:` from the opener's account, so when the implementer and the reviewer post as one account, an implementer's `Accepted:` closed a thread the reviewer never answered (LEGION-208 4b.15b). It also says to write nothing further on a thread its opener has answered `Accepted:`, since a later reply becomes the newest comment and leaves the thread open.

### Changed

- `legion.goDaemonApiVersion` is 11. Contract 11 adds `legionAppLogins` to the Go daemon's `POST /legion/v1/gh-token` answer, each Legion role App's login keyed by its App role (`{implement, review}`), which the Go client's strict `LegionGoGitHubTokenResponse` now accepts (LEGION-208). Nothing in the extension calls `githubToken`, but the credential shapes are part of the contract.
- `legion threads resolve` also resolves a thread a bot account opened that is none of Legion's
  role Apps once the Legion review App's `Accepted:` is its newest submitted comment. A CI bot
  never posts `Accepted:`, so the merger's zero-open-threads check could never pass on a
  repository whose CI bot opens review threads; the reviewer, the independent party, now decides
  such a finding. GitHub cannot tell a CI bot from a person whose `gh` is routed to an App, so the
  reviewer may accept a finding such a person raised, and each `resolved <url>` line says whose
  acceptance closed the thread. The pull request author's reply closes nothing. A Legion App's
  thread and a person's still close only on the opener's `Accepted:`. Legion's role Apps are the
  logins the daemon now names on `/legion/v1/gh-token`, keyed by App role (`legionAppLogins`,
  optional, in both daemons). Without them (`--gh`, which has no grant, or a daemon that could not
  read every App) no thread counts as a bot's, and a bot's left-open line says the session cannot
  identify the review App. The implementer, reviewer and merger role texts say so (LEGION-208).
- `legion.goDaemonApiVersion` is 10. Contract 9 adds the daemon's own agent-secrets machine login
  state (`agentSecretsLogin`) to `GET /legion/v1/state` (AGENTC-393). Contract 10 adds
  `POST /legion/v1/roots/close`, the Go `legion` tool's `close_root`: a tree root's own architect
  ends its admitted tree before any phase has started, and the daemon posts the architect's reason
  on the issue before it writes `done` (LEGION-208).
- The worker skill gives a reviewer round that writes a handoff one order: write, commit and push
  the handoff, submit the review of that head by its SHA, then complete. An approval waits for the
  CI verdict to settle green at that head first, and a red that settles there makes the round a
  request for changes naming the failing checks; a request for changes does not wait. A review of a
  head the handoff push then replaces named a head the pull request no longer had (LEGION-208).
- The worker skill says which GitHub App each role pushes as, and it, the retro skill and the
  implementer and reviewer role texts each say once that every path they cite is in sjawhar/legion
  (LEGION-208).
- The `envoy` skill says a `pr.<n>.checks` settlement is published for every commit of the pull
  request whose checks settle and names its `sha`, so an agent waiting on CI compares it with its
  head (LEGION-208).
- `legion.goDaemonApiVersion` is 8. Contract 8 adds `NATS_NKEY_SEED_FILE` to the Go daemon's pane environment when the daemon has the `legion-pane` NATS nkey seed (LEGION-279): a 0600 file of the pane's own under tmux, the providers Secret's `NATS_NKEY_SEED` file on a Sandbox pod, and the operator file's `nats_nkey_seed_file` under `legion controller start`. The extension's Envoy connections authenticate with it. A Go daemon at 8 refuses to boot beside a plugin at 7.
- Under the Go daemon, a claim no longer subscribes to an issue's notice topic (`notifications.legion.<project>.<issue>`): the architect's tree root and each phase worker's issue used to be subscribed. The Go daemon now sends every notice to the owning architect's role topic, which the architect already claims as its Envoy role, and a phase worker receives none. A plugin from this release beside a Go daemon that still publishes notices on issue topics hears none of them.
- `legion.goDaemonApiVersion` is 7. Contract 7 adds `holdReason` to an issue on `/legion/v1/state` (`escalated` while an escalated issue stays held in a tree that runs; absent while its tree lingers or is closed), which the controller skill reads at every start. A plugin at 6 refuses a state response carrying it, and a Go daemon at 7 refuses to boot beside a plugin at 6.
- A targeted Dispatch BTW and the stop-time self-check run on the extension context's `runEphemeralTurn` (Oh My Pi 18.3) when the host has it, with the question in the /btw prompt that `pi.askEphemeral` added itself. `pi.askEphemeral` stays the fallback for fork releases before 18.3, which Legion pins (18.2.9, `packages/daemon/src/daemon/omp-pin.ts`). The check now runs on the host's managed timer after `agent_end` returns: on 18.3 a side turn started inside a running handler inherits that handler's abort signal, which the host fires at its 30 s handler budget. A host with only the upstream call, which is every upstream release from 18.3.0, now advertises and answers BTW; before, it advertised `aside` and `steer` only.
- `legion.goDaemonApiVersion` is 6. Contract 6 adds `phase` to a claim's pending delivery on
  `/legion/v1/state` (the issue phase the task was queued for; absent for an operator's or an
  architect's task, which belong to none) and `unrecorded` as the phase and status the state route
  reads for an issue the workflow does not record. The phase-backward request takes the workflow's
  phases alone, so `unrecorded` is not one of its values. The handoff completion request is
  unchanged; the daemon now attributes each completion to the run of the task the worker took and
  refuses one whose run the issue has left, or one from a claim that has taken no task.
- Every role pushes its own commits to the issue branch (LEGION-285): the planner its plan handoff, the tester the red tests it writes for each defect it finds and its handoff, the reviewer its handoff. The role prompts and `legion-worker` said only the implementer pushes, and that other roles' commits rode its next push; the review App can push, and Sami's answer on LEGION-200 is that its roles should. Thread resolution does not move: GitHub grants resolving a review thread to the pull request's author's App, and the implementer opens every Legion pull request, so the implementer and the merger still run `legion threads resolve`. The App permissions, and those readings, are stated once, in `packages/daemon/src/daemon/AGENTS.md`. Every role pushes with one procedure, which refuses unless `@-` descends from `legion/<KEY>@origin`. Without that check, `--allow-backwards` moves the remote branch sideways over another role's push, which the shared clone makes visible at once (measured on jj 0.45.1). A rebase, a retarget, or a squash into a pushed commit records the pushed tip first; the same push then accepts that tip and refuses any other, so a rewritten chain goes out only when no role pushed in between.
- `legion.goDaemonApiVersion` is 5. Contract 5 adds `LEGION_GRANT_FILE` to the Go daemon's pane environment (tmux panes and Sandbox pods alike); the extension no longer sets it itself once the claim registers. Install this release before starting a Go daemon that requires contract 5; its boot gate refuses a plugin that declares 4, and this release on a pane a daemon at 4 launched refuses every bash command and every call Oh My Pi serves with `gh` with `LEGION_GRANT_FILE is not set on this pane`, until the pane is relaunched from a daemon at 5 (a restarted daemon re-adopts a live pane as it was launched).
- `legion.goDaemonApiVersion` is 4. Contract 4 adds the Go daemon's operator-launched controller: `POST /legion/v1/controller/secret`, the controller registration on `claims/register`, the `/grants` controller-session form, and `controllerLocator` on `/legion/v1/state`. Install this release before starting a Go daemon that requires contract 4; its boot gate refuses a plugin that declares 3.
- `legion.daemonApiVersion` is 7. Contract 7 removes `/gh-token` merge intent because Legion never
  merges; install this release before starting a daemon that requires contract 7.

### Fixed

- The pane guard (LEGION-121) models a subshell as bash runs one, everywhere it sees one: a command
  or process substitution, `( … )`, `( … ) &`, each part of a pipeline, and a coprocess. A subshell
  starts with none of the parent's traps and runs the handlers it sets itself at its own end; the
  parent's run once, at the parent's end. Before, a substitution ran every handler set before it
  against the state at that line, and the other four walked a handler they set into a state that
  was then discarded. So `( trap 'rm -rf "$HOME/y"' EXIT; echo hi )`, its background and piped
  forms, and the coprocess form were allowed, and bash deletes the target in each. `! command`, a
  pipeline of one, is walked in this shell, as bash runs it, so `! cd "$HOME"; rm -rf x` is refused.
- `trap - <condition>` removes only the conditions it names: `trap - INT` leaves the EXIT handler,
  which bash still runs, and `trap -` naming none resets nothing. Before, any `trap -` dropped every
  handler, so `trap 'rm -rf "$HOME/y"' EXIT; trap - INT` was allowed. A condition the guard cannot
  read (`"$(…)"`, a variable read from input, a positional parameter) could be any: a removal naming
  one resets nothing, and a handler set for one stays and is walked. A handler whose text the guard
  cannot read (`trap "$c" EXIT` with `c` from input) is refused, since it cannot check what runs at
  exit; a double-quoted handler that expanded only pids from `$!` (`trap "kill $pid" EXIT`) reads
  them back and is judged. A handler the guard can read that runs a command it cannot
  (`trap 'eval "$c"' EXIT`) is judged as any such command is.
- A handler set inside an `if`, `case` or loop body stays set after it, as bash keeps it, judged
  with the variables that body gave it: `if true; then trap 'rm -rf "$HOME/y"' EXIT; fi` was
  allowed, and `if true; then t=$(mktemp); trap 'rm -f "$t"' EXIT; fi` still is. A name assigned
  after the construct takes its new value, which is what a single-quoted handler reads at exit, so
  `…; fi; t="$HOME/y"` is refused; a double-quoted handler keeps the values it expanded when it was
  set. A function an `if`, `case` or loop body defines is every definition a path may have left: a
  call runs each, and the command itself where a path defined none. Before, a definition inside a
  branch was dropped, so `if true; then f() { rm -rf "$HOME/y"; }; fi; f` was allowed, and one that
  replaced an earlier definition was judged by the earlier one. The positional parameters merge as a
  variable does: a `shift` or `set --` inside a branch makes them unknown after it, where before the
  guard kept the arguments from before the branch, so `set -- "$LEGION_WORKSPACE/a" "$HOME/y"; if
  true; then shift; fi; rm -rf "$1"` was allowed. A sourced file's `set --` or `shift` changes the
  caller's arguments, as bash does; before, the guard kept the caller's, so `set --
  "$LEGION_WORKSPACE/a"; . lib.sh; rm -rf "$1"` with `lib.sh` running `set -- "$HOME/y"` was
  allowed. With operands, bash restores the caller's arguments afterwards, undoing a `shift` and
  keeping a `set --`; the guard does not tell those apart, so a list the file touched is unknown,
  even one set to the same values (`set -- "$@"`), which bash keeps. `. file` with no operands runs
  the file with the caller's arguments, not none, so `set -- "$HOME/y"; . lib.sh` with `lib.sh`
  running `rm -rf "$@"` is refused. A backgrounded command (`f &`) is walked in a subshell, so its
  `trap - EXIT` no longer clears the parent's handler, and nothing else it changes reaches the
  parent.
- The guard walks up to 100,000 nodes of one command before refusing it as too large to judge,
  from 10,000. The repository's largest tracked script, Stage 4b's driver, needs about 20,700 to
  reach its first refusal, and 10,000 refused it for size alone. The limit still refuses and never
  allows unread.
- Tracked scripts the guard now judges on their real first refusal:
  `packages/envoy/scripts/e2e-api.sh`, whose cleanup kills only the child whose pid it wrote, runs.
  Stage 2, Stage 3 and the 4b.13b
  acceptance are refused at their gateway key command's write, and Stage 4b and the controller
  proof at the plugin unpack. The allow-list test records each refused script's `file:line`, derived
  by `src/legion/pane-guard-scripts.ts`, where it had recorded a phrase several refusals share.
- The pane guard resolves more of what a script computes before it refuses a target it cannot
  (LEGION-300). A script or function run with arguments the guard knows has them as `$1`, `$#` and
  `${1:-…}`, so an argument loop (`while [ $# -gt 0 ]; do case "$1" in --dest) dest=$2; shift 2`)
  is walked pass by pass, a `case` on a known word takes its one matching item, and a `shift` past
  the last argument shifts nothing, as in bash. A word that may be several arguments or none (an
  unquoted `$v` holding a space, `$*`, a glob) leaves the arguments unknown, `"$@"` of no arguments
  is none (and assigned, `x="$@"`, the empty string, one argument when quoted), `unset` leaves a
  name unset rather than empty, and in a shell whose arguments the guard does not know `${1-…}` and
  `${1+…}` stay unknown. It also evaluates pattern replacement and removal of a known ASCII value
  (`${v//a/b}`, `${v#*:}`, `${v%/*}`), `printf -v`, a function whose output passes through
  `(umask 077 && …)`, and a script a brace group writes from here-documents and `printf` before
  running it. A target it cannot resolve is still refused, and some stay
  unknown on purpose: `$(git rev-parse --show-toplevel)`, whose answer the repository's config
  decides and an earlier command in the same line can rewrite; an operand the parser splits
  differently from bash (`${v///tmp//etc}`); text outside ASCII, which bash counts by the locale; a
  `case` whose word matches no item, which walks every branch. A pattern is matched in time its
  value and itself bound, never by a backtracking regular expression (`*a*a*a*b` over a run of
  `a`s held a regex for hours), and the work is charged to the walk budget. No value the guard
  builds is longer than 65,536 characters: a replacement of a replacement reached 134 million in a
  tenth of a second and held the pane for seconds on each read, so past the bound a value is
  unknown. A script a command writes is read whole up to the 1 MiB the guard reads of one on disk,
  and past it is refused as one it cannot read: a 176 KB brace group rendering 655 MB held the pane
  for 30 s. So is a script whose code holds a value the guard cannot know: `printf %s`, `printf %q`
  and `echo` write the value into it, and bash parses what the value holds, where a `;`, a quote or
  a newline reaches out of any position (an operand of `echo`, a quoted string, a comment), and a
  value containing a newline makes `%q` select `$'…'`, which closes a single- or double-quoted
  position. So is one `echo` writes with an option first, since the option changes what it prints:
  a harmless `echo -e 'ls' > f; bash f` is refused as well, and writing the file stays allowed.
  `printf %d` writes only digits and a sign, so there its value stays one unknown word. The Stage
  2, 3, 4b.13b, 4b and controller drivers are refused for killing the processes a query selects
  (`$(run_processes)`, `first_child`), all but the controller's first for running the gateway key
  command `install-model-gateway.sh` writes, whose `command=(…)` line takes a value built from
  `$(command -v hawk-token)`, and the five manual smokes (`smoke-delivery.sh`, `smoke-btw.sh`,
  `smoke-channel.sh`, `smoke-clear-rebind.sh`, `omp-roundtrip.sh`) run their sessions on their own
  tmux server, where a `kill-session` can end only the session each started.
  `src/legion/pane-guard-walk.ts` prints every refusal a script meets, not only the first.
- A pane whose `HOME` sits under `/tmp` keeps it (LEGION-300). The guard counted every directory
  below `/tmp` except the socket families as the pane's scratch, so with `HOME` at
  `/tmp/<run>/omp-home` and no `TMUX_TMPDIR` in the same directory, `rm -rf ~`,
  `rm -rf "$HOME/.ssh"`, `find /tmp/<run> -delete` and `rm -rf "$LEGION_STATE_DIR"` were allowed.
  The e2e rigs that run guarded panes set `TMUX_TMPDIR` beside their Oh My Pi home, so the
  protection of its directory covered them by that coincidence, which nothing enforced. The
  directory holding `HOME` is now protected as the one holding `TMUX_TMPDIR` is, by exact name; the
  workspace inside it stays writable.
- The pane guard follows the writes to a variable it sees (LEGION-300). It kept values bash had
  changed, so a target built from one afterwards was judged on the stale value: `unset d;
  : "${d:=$HOME/.ssh}"; rm -rf "$d"`, a function's `local d=…` still in force after it returned,
  `printf -v 'd[0]'`, `read -ra d`, `declare "d=$HOME/.ssh"`, a plain `d=x` over an array's other
  elements, arithmetic, `wait -p`, `unset -f` of a function that shadowed a command, and a write
  bash refuses or rewrites for a `readonly`, `-i`, `-l` or `-u` name were all allowed where bash
  deletes outside the roots. Each now assigns as bash does, and a write the guard cannot model (a
  variable named at run time, `eval "$(tool)"`) leaves every variable unknown, so a target built
  from one afterwards is refused. A nameref (`declare -n`) is refused. Still not followed: a
  `source` of a path the guard cannot read at check time, such as a process substitution, which it
  takes as sourcing nothing (LEGION-332), and an assignment to `IFS`, since it splits an unquoted
  value on whitespace alone.
- The pane guard reads no unquoted here-document that bash expands (LEGION-300). `cat > f <<EOF`
  and `tee f <<EOF` stored the text as written, so `x='rm -rf ~'; cat > f <<EOF` with `$x` in the
  body, then `bash f`, was allowed while bash wrote and ran the expanded line; a shell or
  interpreter reading one as its program (`bash <<EOF`, `python3 - <<PY`) was judged on the same
  unexpanded text. A here-document whose unquoted text holds `$`, a backquote, or a backslash
  before one of them or a newline is now a script the guard cannot read, and a shell or
  interpreter reading it is refused, as a rendered brace group already treated one.
- The pane guard gives an array's literal the elements bash gives it (LEGION-300). It took one
  element per word, where bash makes `("$@")` one per argument, `("${arr[@]}")` none for an empty
  array, and a word that may split several, so `Y=("$@"); rm -rf "${Y[1]}"` with a second argument
  outside the roots was allowed. A word that may be several elements now leaves every element
  unknown.
- The Go `legion` tool's `register_gate` takes the spec document as the Dispatch tools name it
  (`spec` for the primary document, or its id, slug or filename) and registers its id, where it
  passed any reference to the daemon, which refused one that was not an id. A Dispatch it cannot
  reach refuses the call naming the lookup and the reference, and a call from anyone but the tree
  root's own architect for its own issue is refused before any lookup (LEGION-208).
- The Dispatch tools refuse a bare document reference that is one document's slug and another's
  filename, on an issue or a project, naming both ids, where they took the slug's document; on a
  project an id also outranks another document's slug. A `dispatch://` document reference, or a
  dashboard document URL, still resolves by its slug.
- A phase worker's or root architect's pane, and any `task` subagent it spawns, no longer runs a
  command that would delete, move, truncate, overwrite an existing file, or recursively `chmod`
  or `chown` a path outside its issue workspace or a permitted directory below `/tmp`. `/tmp`
  itself, a glob over it, and its tmux and ssh socket directories remain out of bounds; the guard
  cannot identify which other `/tmp` directory belongs to the pane. It also refuses a signal to a
  process the pane did not start (LEGION-121). On 2026-09-13 a worker pane's probe script ended in
  `rm -rf "$work" "$HOME"` and deleted the operator's SSH and commit-signing keys, stopping every
  agent on the machine; the same day a subagent's `pkill -x sleep` killed other agents' processes.
  The `tool_call` hook parses each `bash` command, `eval` code, and `hub` process start
  (`src/legion/pane-guard.ts`, with the bundled `unbash` parser), resolves every target through
  `$HOME`, `~`, earlier variables, `cd`, and the scripts the command runs (`bash <file>`, `sh -c`,
  `source`, heredocs, python/node/bun scripts), refuses a target it cannot resolve and a command
  it cannot parse, refuses `pkill`, `killall`, and `fuser -k`, resolves `tmux kill-*` to its actual
  socket path and requires that path to stay inside the pane roots, and lets `kill` reach only
  descendants of the pane's Oh My Pi process. Each refusal names the target, where it resolved,
  and the rule.
- A `task` subagent's `envoy_whoami`, `/whoami`, `envoy_send`, and `envoy_publish` now name the session that spawned it as the reply address, instead of reporting an empty id and sending an empty `source_session` the listener erased — which reached the recipient as `from: agent` with no reply address, and a reply attempt as `no live session`. A subagent still registers nothing (no listener registration, no agent-subject subscription, no heartbeat); every top-level instance publishes its own session on a process-wide record keyed by its transcript path (`src/envoy-session.ts`), dropping its previous key whenever its id or transcript changes, and a subagent resolves its own by walking OMP's transcript layout up, a nested subagent included. An ACP host running several top-level sessions in one process therefore answers each subagent with the session that actually spawned it. A session is recorded from its `session_start` even before the host mints its id, and dropped at `session_shutdown`, so a subagent never names a session that has no id yet or has already deregistered. `envoy_whoami` reports that address as `session_id` and the subagent's own host session id under `subagent`. Where the walk matches nothing and more than one top-level session is recorded, there is no reply address: `session_id` is empty, the `subagent` note says why, and `@legion/envoy-client`'s transport now omits `source_session` rather than sending an empty one. The `envoy_publish` result also names the delivery a subagent cannot get: the listener drops a message whose source session is its recipient, so a publish to a role the parent holds reaches nobody, and hub is that hop.
- The TypeScript daemon keeps a reviewer's `changes_requested` decision across the reviewer's own handoff-only push (`.legion/review.json`), so its completion returns the issue to `in_progress` rather than `retro`, and the corrective implementer's completion writes `testing` (LEGION-285). A new head that changes anything outside `.legion/`, or whose push cannot be classified, still ends the round unless the review App pushed it (none of its commits answers a request made of the implementer; resync keeps a range whose every commit GitHub attributes to the review App), and a head whose push webhook never arrives, or that resync's read finds first, is settled by the reviewer's next review of that head or by resync from GitHub's compare of the round's head against it (a compare it cannot read drops the decision); an approval is still dropped on every new head. The reviewer's clean COMMENT round at a later head ends its own request, and a late changes-requested review of an earlier commit is settled from that commit. A push by the review App, a tester's red tests included, is never a fix attempt, and neither is the implementer's fix after the red those red tests earned, so tester rounds consume no `max_fix_attempts`; after the tester's handoff-only push onto the implementer's red, the implementer's next push still counts. The Go daemon counts the same way (it reads the pusher and stores `planned_red`), and both daemons refuse to boot when one GitHub App is configured for both roles. Daemon state is v34 (`PrState.reviewDecisionUnsettledFrom`, `changesRequest`, `plannedRed`, `pendingPush.before` and `byReviewApp`); a v33 file migrates on load and is kept as `<state>.v33.bak`.
- The refusal on a pane without `LEGION_GRANT_FILE` now names its remedy: relaunch the pane from a daemon on the matching release, since a daemon restart keeps a live pane as it was launched.
- A `read`, `grep`, `glob`, `ast_grep`, or `ast_edit` of a `pr://` or `issue://` URL (alone, in a `;`-, `,`- or whitespace-separated path list, or in one pair of double quotes: every shape Oh My Pi's path pipeline hands to `gh`), and every call of Oh My Pi's `github` tool, now mints the pane's grant first, as a bash command does: Oh My Pi serves them by running `gh`, which on a Legion pane is the shim that runs `legion gh`, and a grant lives 60 seconds, so one made long after the pane's last bash command failed its redemption. Under the Go daemon they failed every time with `LEGION_GRANT_FILE is missing`, because Oh My Pi copies its environment for `gh` when it starts and the extension set the variable only after the claim registered; a root architect signed off without the pull request read it tried (LEGION-262). The grant file's directory is created 0700 when absent, as it is in an Agent Sandbox pod's empty state volume.
- The empty receipt on the direct agent subject now goes only to a role-lane frame (envelope `topic` other than the direct subject — the shape the listener forwards to a role holder). Every other publish to that subject with a reply inbox is a JetStream publish whose inbox belongs to the server's PubAck; the receipt landing there made the publisher fail with `nats: invalid jetstream publish response` (31 Dispatch outbox `publish author route` failures in 24 h in production).
- `envoy_unsubscribe` now leaves the session's role claim intact, so cleaning up normal notification topics cannot stop the registration heartbeat from restoring a lost role.
- A phase worker or root architect whose registration the daemon refuses with the same-agent 409 (`Worker respawn must resume the same agent session` at `/worker/started` or `/process/started` — a resumed process that arrived as a different session, under a database session store Oh My Pi having started fresh at a path whose row is gone) now exits like a 403 boot-token refusal does, so the daemon counts the launch failure and retires the role instead of a live pod sitting unregistered under the boot watchdog forever; a 5xx or transport failure still propagates without exiting (LEGION-81).
- Phase workers, the root architect, and the controller pane now receive their one-time grant through a 0600 file named by `LEGION_GRANT_FILE`, written by the extension before each bash command and read first by `legion credential`, `legion gh`, and `legion handoff complete` — replacing both the prepended command text the model imitated with stale ids (LEGION-12; `legion gh`, `legion handoff complete`, and `jj git push` no longer fail with 403 as a session goes on) and the 1.17.1 `env` delivery (the `secretsd` plugin's bash replacement discarded it, so every `legion` command failed with `LEGION_GRANT is missing`). The static `GH_CONFIG_DIR`, worker-bin `PATH`, and cleared `GH_TOKEN`/`GITHUB_TOKEN`/`GH_HOST` now come from the daemon's pane environment; the daemon strips any inherited `worker-bin` PATH entry at its own boundary, so a daemon started from a Legion pane never resolves its `gh` to the shim (LEGION-54).
- Envoy roles now survive `omp --resume` — including after stale-session cleanup — and follow `/fork`, `/branch`, or other transcript-carrying switches to the new session id until released.
- Legion agents now refresh their Envoy registration before claiming a role, so claims made after registration expiry are accepted.

### Removed

- `before_agent_start` no longer injects the authored-ask summary (`dispatch-open-asks`) into
  every turn. It reads open asks only to arm the run-end self-check, while an agent reads its own
  open asks with `dispatch_open_asks`.
- Removed the NATS-based `{type:"shutdown"}` Legion control directive (`LegionControlDirective`, `requestShutdown`) — the daemon now gracefully stops every process, including the root architect, over its own `legion worker-shim` unix socket instead of publishing a control-subject directive.
