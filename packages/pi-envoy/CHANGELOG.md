# Changelog

## [Unreleased]

### Changed

- A spec is the design conversation (LEGION-387). The `dispatch` skill's "Writing a spec" drops
  the eight required headings: a spec starts as the problem and its evidence, puts each open
  question in a decision block at the end of the section that discusses it, records a settled
  point in the human's words with the date, and is rewritten in place as it changes. "Approval of
  a spec" says to request approval only once no decision block is open and the spec proposes
  something the human hasn't settled. The `dispatch_issue` and `dispatch_doc_edit` descriptions
  point at that section instead of listing headings.
- `dispatch_request_approval` requires `summary`: the proposals in the document's latest version
  the human hasn't already agreed to, in one to three sentences. The Inbox shows it after "Approve
  spec.md (version N)?", and the result text quotes the question the human sees. It needs a
  Dispatch server that accepts `summary`; an older one refuses the call.
- `dispatch_request_approval` is refused while the document holds an open decision block, even
  when a human asked for approval, and the refusal names each block. "Approval of a spec" says
  what to do instead: name the open block and ask the human to answer or waive it; a waived block
  is closed with `dispatch_resolve_ask`. It also says to request approval in the pass that
  finishes a spec whose remaining choices are the agent's own, rather than making each of them a
  decision block.
- The root architect's role text, its Go-daemon part and `legion-architect` settle the spec's
  decision blocks first, then request approval with a summary of what the tree will do.
- The run-end nudge that tells an agent to open an ask no longer offers
  `dispatch_request_approval` as a way to wait on a human.

### Added

- The implementer orchestrates its change rather than writing it (LEGION-415). The package ships
  `deep-worker` in `agents/`, an autonomous coding agent on the deployment's `deep` model role
  (`@deep`): given a goal, the workspace and files in scope, the skills to follow and the checks
  that must pass, it makes the change, runs the checks and reports what it changed. Its prompt
  tells it to make no commit, push or GitHub write. Nothing enforces that: the implementer's
  review of each result can catch a worker's commit, but not a push or a GitHub write
  (LEGION-428). The implementer's role text plans the change as todos,
  hands each coding task (the plan's change and each review round's fixes) to
  `task(agent="deep-worker")` one at a time, and verifies every result itself, running the plan's
  checks and reading the diff, before it builds on or commits it; the commits, pushes, pull
  request, review-thread answers and production check stay the implementer's. The Go daemon's
  boot gate resolves `deep-worker` as it does the other shipped agents, so an operator route that
  gives `deep` no model (`modelRoles.deep`, or a `task.agentModelOverrides` entry) refuses the
  boot, naming `deep-worker`; the TypeScript daemon has no such gate, and there Oh My Pi runs an
  unmapped `deep-worker` on the implementer's own model. Map `deep` in the operator route first,
  then install the worker image built from this release, then the Go daemon build; the refusal
  comes from that daemon build's own role prompts, which dispatch `deep-worker`, so a new daemon
  build on an older image is refused, since that image's plugin ships no `deep-worker`, and an old
  daemon build on the new image boots.
- Every session with the Dispatch tools, Legion panes and `task` subagents included, now carries the
  `dispatch-first` skill in every model request: search Dispatch before planning, filing, asking or
  starting work; extend the issue that already tracks the work; cite the decision a human already
  made; close duplicates naming the survivor; write asks and messages that stand on their own; and
  read `skill://dispatch` before writing a spec (LEGION-386). The extension inserts it as a user
  message after any compaction summaries on each request, so it survives turn 2 and compaction;
  a message that quotes its marker never switches it off, and a copy is looked for only where the
  extension inserts one. It is read once at load, so a package without
  `dist/skills/dispatch-first/SKILL.md` fails to load naming the file. A session without Dispatch
  configured gets nothing.
- The planner, tester, reviewer and implementer role texts, and the worker skill's push procedure
  they point to, each say that under the Go daemon the issue branch is pushed with `legion push`,
  which runs that procedure and decides whether the push skips CI: a handoff push that a later push
  follows starts no CI run, and every other push runs CI in full. A worker that pushed with the
  skill's commands by hand would run full CI on every handoff. Under the TypeScript daemon, whose
  `legion` has no `push` command, the commands stay hand-run (LEGION-208).
- The session publishes its own conversation for Dispatch's agent conversation view (LEGION-232):
  every turn, tool call and streamed update becomes a frame on `agentstream.<session id>.frames`
  over core NATS, a subject family the notification stream does not capture, so the bus retains
  none of it. It publishes only while a viewer is attached — the Dispatch relay asks on
  `agentstream.<session id>.control`, and a session that hears nothing for thirty seconds goes
  quiet — and a viewer's replay is answered from a bounded in-memory ring (200 messages, 512 KiB)
  that never leaves the process. Nothing about it is written to Dispatch's database.
- The dispatch skill's documents reference says a `|` in a table cell, inside inline code and
  links too, is written `\|`, and that a row holding text in a cell past its table's width is
  refused on every write path rather than stored short, naming each path's error code. Its
  document-edits reference says how an insert at a table-cell quote decides it holds table rows,
  in three steps: every line yields a cell and an unescaped `|` and none is a delimiter row of
  three hyphens or more a cell; what the rows parse refuses is refused; and the rows are inserted
  only when that parse reads one table holding every line. Any other fragment, such as
  `- | a | b |`, is read on its own as blocks.

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

- The `dispatch` skill arrives whole (LEGION-386): its body is under 500 lines and its detail lives
  in step-linked `skill://dispatch/references/*.md` files, each under Oh My Pi's 51,200-byte spill
  threshold, where the 75 KB single file used to reach agents with its middle cut out.
  `src/skills-guard.test.ts` now holds every skill to that: it fails when any skill file reaches
  the threshold, any skill body reaches 500 lines, a skill's frontmatter name is not its
  directory's, or a `skill://<name>/<path>` link names a missing file or heading.
- The `legion-worker` skill arrives whole (LEGION-386). At 55,020 bytes it was over Oh My Pi's
  51,200-byte spill threshold, so a phase worker read it with its middle cut out. Its body is now
  under 500 lines, and the PR-body template and proofs, review threads, conflicts and fingerprints,
  and the merge gate are references under `skill://legion-worker/references/`, each linked from
  the step that needs it. The reviewer's role prompt approves the `.legion/` deletion head with
  the skill's head-pinned review submission, and the implementer, reviewer and tester role
  prompts point at the reference that now holds each procedure they name. `src/skills-guard.test.ts`
  keeps every legion-worker file under the threshold and every `skill://legion-worker/` link and
  reference resolved, and every change under `skills/` now runs this package's tests in CI.
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

- The Go `legion` tool's `register_gate` takes the spec document as the Dispatch tools name it
  (`spec` for the primary document, or its id, slug or filename) and registers its id, where it
  passed any reference to the daemon, which refused one that was not an id. A Dispatch it cannot
  reach refuses the call naming the lookup and the reference, and a call from anyone but the tree
  root's own architect for its own issue is refused before any lookup (LEGION-208).
- The Dispatch tools refuse a bare document reference that is one document's slug and another's
  filename, on an issue or a project, naming both ids, where they took the slug's document; on a
  project an id also outranks another document's slug. A `dispatch://` document reference, or a
  dashboard document URL, still resolves by its slug.
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

- The filesystem and signal boundary (LEGION-121) is removed. The `tool_call` hook no longer
  parses a phase worker's or root architect's `bash` commands, `eval` code, or `hub` process
  starts (or a `task` subagent's) to refuse deleting, moving, truncating, overwriting, or
  recursively `chmod`/`chown`ing a path outside the issue workspace and `/tmp`, or signalling a
  process the pane did not start (`kill`, `pkill`, `killall`, `fuser -k`, `tmux kill-*`), and the
  bundled `unbash` parser is gone. The pane rules stay: a phase-worker pane still refuses the jj
  operation-log rewrites, and a phase-worker or root-architect pane still refuses
  `legion handoff complete` from the shell.
- `before_agent_start` no longer injects the authored-ask summary (`dispatch-open-asks`) into
  every turn. It reads open asks only to arm the run-end self-check, while an agent reads its own
  open asks with `dispatch_open_asks`.
- Removed the NATS-based `{type:"shutdown"}` Legion control directive (`LegionControlDirective`, `requestShutdown`) — the daemon now gracefully stops every process, including the root architect, over its own `legion worker-shim` unix socket instead of publishing a control-subject directive.
