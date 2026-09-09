# Legion relaunch on Oh My Pi

**Date:** 2026-09-09
**Status:** draft, awaiting review
**Supersedes:** the process model in `.superpowers/2026-08-23-legion-on-omp-plan.md`
(in-process keepAlive `task` subagents as phase workers) and the daemon routing rules
that model implied.

## Problem

Legion has not run for real since the OpenCode era. The one launch after the OMP rewrite
(#753, 2026-08-26, project `trajectory-labs-pbc/2`) was visibly broken and hand-patched
for a week (#756–#760); no transcript, state file, or listener log from that window survives.
Investigation of the current code (2026-09-08/09) found the causes, none of them a flaky wire:

1. **The extension did not load at all** on pi-envoy 0.8.0–0.8.2 under OMP 18.1.x
   (`e.ir.desc` Zod-shape mismatch). Fixed by #802. A session with no extension has no
   NATS subscriber, which reads from the operator's chair as "unreliable delivery".
2. **The in-process worker model misuses roles.** All phase workers run as `task`
   subagents inside one OMP process. Every subagent's extension instance overwrites one
   process-global role-claim slot (`envoy.ts:380-391`); a finished subagent *parks*
   (OMP disposes it), which deletes its Envoy interest, so daemon publishes to it get
   `no_holder` and are held forever with nothing to wake it (`events.ts:330-349`); the
   daemon judges every worker alive by probing the root's PID only (`processes.ts:315-323`).
3. **The daemon routes GitHub events to two roles only.** `RoutedRole = "architect" |
   "implementer"` (`reducers.ts:350`); CI settlement and PR comments/reviews always go to the
   implementer, so a running tester or reviewer never receives them. `LegionState.phases`
   records the active phase and is never consulted by routing.
4. **Daemon intake is not durable.** The daemon consumes `notifications.github.>` — every
   repo the listener sees — over core NATS (`events.ts:496`, `index.ts:147`). Anything that
   arrives while the daemon is down is lost; resync only re-converges the board.
5. **Dispatch cannot attach a question to the work's issue.** `ContinueThread` refuses any
   issue without a thread marker in its body (`thread.go:281`), so the only way to ask is a
   new issue. agent-c accumulated ~59 question issues in four days, 53 from Legion's bot.
6. **Nothing makes workers look for the repo's own skills.** Phase prompts never mention
   them; `PlanHandoff.requiredSkills` exists in the schema and no role fills or reads it;
   the tester's charter says "run the changed behavior through its actual surface *where one
   exists*" and the tester decides whether one exists.
7. **Config drift.** `legion.yaml` lacks `nats_urls` and `board_project_ids` (empty list =
   board admission silently dead), carries four dead keys including `runtime: opencode`, and
   the daemon's default OMP pin is ~20 releases behind the installed build.

Positive control: on 2026-09-08 the unrelated `sre` role took 1,637 clean role-lane
deliveries; a throwaway spike on 2026-09-09 showed a real OMP 18.1.15 TUI session with
pi-envoy 0.8.3 claims a role and receives a publish as a steer in ~10 ms, idle or busy. The
transport works; Legion's use of it is what this design changes.

## Decisions

Provenance: **verbatim** quotes Sami's words this session; **inferred** states the reasoning
and is a hypothesis for him to confirm at review.

| # | Decision | Provenance |
|---|---|---|
| D1 | One OMP process per phase worker, in its own tmux window, instead of in-process `task` subagents. | verbatim: "we need OhMyPy to do per-worker processes" |
| D2 | Envoy role per (issue, phase): `legion-<project>-<issue>-<phase>`, the existing `roleToken` grammar. Roles are addresses for inter-agent messaging as much as daemon routing targets. | verbatim: "yes, it should be legion 2 41 tester"; "Roles are not only for receiving messages from the daemon. They're also to make inter-agent messaging easier." |
| D3 | A worker tells the daemon when its phase is done (explicit `phase/complete`). | verbatim: "I would agree with the worker telling the daemon it's done, at least for now." |
| D4 | Workers stay running after their phase, idle, holding their role, so agents can ask each other questions. | verbatim: "I don't think we need to kill the worker, it's fine. Just keep it around. Maybe the agents want to ask each other questions." |
| D5 | Processes are stopped gracefully through OMP's own shutdown, never by killing the tmux window first. | verbatim: "The right way to kill an OMP process is not to just kill the tmux window... There is definitely a much better way to gracefully shut down OMP." |
| D6 | Closing a root issue enters linger, then shuts the tree's processes down. Closing a child never kills work. | verbatim: "we wouldn't want closing the issue to just kill all in-progress work. But if an issue goes through its normal life cycle and then gets closed, we should shut down the Oh My Pie processes" |
| D7 | The daemon stays subscribed after a tree closes; activity on a closed tree wakes the controller, which decides whether to resume the architect and pass the event on. | verbatim: "the daemon should stay subscribed to it... if something is received, it should relaunch, resume the architect and pass the notification to it"; controller path chosen as "the fewest changes from the current state" |
| D8 | The daemon subscribes per configured repository, not to the whole GitHub firehose, and consumes durably. | verbatim on the firehose: "This seems a bit unnecessary"; durability inferred from D7 (staying subscribed must survive a daemon restart) |
| D9 | Dispatch is integrated with the issue lifecycle: a Legion session's question is a comment on its own issue, and the human's answer routes like any other comment. | verbatim: "Since dispatch is part of Legion itself, surely we can somehow integrate it easily with the issue life cycle" |
| D10 | Testing quality comes from workers finding and following the target repo's skills, and from the planner/architect making the work testable — not from mechanical checks of handoffs or a sandbox config file. | verbatim: "I'm not interested in some kind of stupid fucking mechanical check of the handoff... The handoffs are to go between workers"; "we don't need a config.yaml to tell us things that should just be in the repo skills"; "the planner and the implementer and the tester and the reviewer should all have an explicit step one of their phase that is search for relevant skills in this repo" |
| D11 | Worker credentials beyond GitHub are out of scope: workers run on this machine, which is already logged in. | verbatim: "the credentials thing is not really relevant right now because they're just going to be running on this machine and this machine's already logged in" |
| D12 | Legion gets a new, Legion-only GitHub Project board. | verbatim: "we should probably use a new board just for Legion... so I don't have to deal with cruft" |
| D13 | Sami reviews the architect's decomposition before implementation (design gate) and gives final approval before merge (merge gate); both exist today and are kept. | verbatim: "I want to make sure that I still have the ability to review plans after the architect and I get final approval before things merge" |
| D14 | The OpenCode `envoy-plugin` is not retired as part of this work. | inferred: it carries no Legion logic (the swarm half was deleted in #753) and Sami used it interactively in this repo on 2026-09-06 |
| D15 | Fully diagnosed, unobjectionable fixes ship without re-asking. | verbatim: "If fixes are straightforward and unobjectionable, just let them rip" |

## Architecture

```
GitHub ──webhooks──▶ Envoy listener ──JetStream──▶ Legion daemon (durable consumer, per repo)
                          │                              │  reducers: issue/PR event → active phase
                          │ core NATS role lane          │  admission, spawn, probe, held events
                          ▼                              ▼
   ┌── tmux session legion-<project> ───────────────────────────────────────────────┐
   │  controller  │  architect #41  │  planner #41 │ implementer #41 │ tester #43 … │
   │  (one)       │  (one per tree) │  (one process per issue × phase, stays idle)  │
   └───────────────────────────────────────────────────────────────────────────────┘
        each process: omp TUI + pi-envoy (one copy) → claims its role → steer on publish
                          ▲
   dispatch server ◀──────┘ questions are comments on the session's own issue
   dashboard: one card per issue, grouped
```

Components and what changes in each:

| Component | Today | After |
|---|---|---|
| Daemon `processes.ts` | tmux window per root tree and controller | plus a window per worker; per-worker locator, probe, graceful shutdown; worker cap |
| Daemon `reducers.ts` | routes to architect/implementer | routes to the active phase of the issue; controller wake on closed-tree activity |
| Daemon `events.ts`/`index.ts` | core NATS `notifications.github.>` | JetStream durable consumer, `notifications.github.<owner>.<repo>.>` per configured repo |
| pi-envoy `legion.ts` | intercepts `task`, injects `<legion-spawn>`, in-process budget, `ensureLive` revival | worker boot handshake from env; run-state heartbeat; control subscription per process; no `task` interception |
| pi-envoy `agents/legion-*.md` | OMP agent definitions (frontmatter tools/spawns/output) | bodies become `roles/<phase>.md` system prompts; tool restriction moves to the `tool_call` hook keyed on role |
| pi-envoy packaging | plugin ships `envoy.js` only; daemon passes `--extension <repo>/packages/pi-envoy` | plugin ships `legion.js` too; daemon sessions load the installed plugin and nothing else |
| Dispatch server + tool | new issue per question; `thread` requires a marker | adopts a work issue as a thread; Legion sessions default to their own issue; standalone threads guarded |
| Dashboard | flat list, equal-weight cards | grouped by issue, answered asks collapsed, origin shown once |
| Skills / role prompts | no repo-skill step; "where one exists" | step one: find repo skills; planner names skills per role; testability is planned |

### Processes

**Root architect** — unchanged: `spawnTree` opens a tmux window running the resolved `omp`
binary with `--append-system-prompt roles/architect-root.md`; boot handshake
(`processStarted`/`processReady`), architect role claim, control subscription.

**Worker** — a root-shaped process scoped to one issue and one phase.

- *Spawn.* The architect calls `legion({ op: "spawn_worker", issue, role, task })`. The
  daemon checks the worker cap, then opens a tmux window in `legion-<project>` exactly as
  `spawnTree` does: same binary, working directory = the issue's jj workspace,
  `--append-system-prompt` composed from `roles/<role>.md` plus any repo-provided worker
  instructions (see Backlog: per-deployment customization). Environment: `LEGION_ISSUE`,
  `LEGION_ROLE`, `LEGION_TREE`, `LEGION_WORKSPACE`, `LEGION_BOOT_TOKEN`,
  `LEGION_DAEMON_URL`, `LEGION_PROJECT`, `ENVOY_NATS_URL`, `ENVOY_URL`,
  `LEGION_CONTROL_SUBJECT` (per process), `GIT_CONFIG_COUNT=0`, `PATH`. The window id and
  session file are recorded on `state.roles[token]` (a `WorkerLocator`, mirroring
  `TreeState.locator`).
- *Boot.* `session_start` sees `LEGION_ROLE` and runs the same handshake the root runs today
  (`legion.ts:239-268`): `processStarted` → daemon returns the role token, per-session
  secret, and git identity → `claimRole(token)` → control subscription → `processReady`.
  One process, one extension instance, one claimant.
- *Assignment.* The architect's `task` text is the worker's first role message, published by
  the daemon after `processReady` (the path root catch-up uses today, `index.ts:313-317`).
  If it arrives before the claim, it is held and flushed on claim (`redeliverHeldRoleEvents`).
  No prompt rewriting; the `<legion-spawn>` block is deleted.
- *Work.* Unchanged: jj workspace per issue, `gh` shim through `legion gh --`,
  `legion handoff write` committing `.legion/<phase>.json`. Per-role tool restriction
  (reviewer cannot `edit`, architect cannot `bash`, …) moves from agent frontmatter into the
  existing `tool_call` hook (`legion.ts:392-398`), keyed on `LEGION_ROLE`.
- *Completion (D3).* After committing its handoff the worker calls
  `POST /legion/v1/phase/complete` with its secret. The daemon publishes
  `phase-complete { issue, role, summary }` to the tree's architect role and clears
  `phases[issue]`. The architect receives it as a steer; no `task` return value, no `hub`.
- *After completion (D4).* The process stays running and idle, still holding its role. Any
  agent may `envoy_publish` to it — the tester asking the implementer why something was done
  a certain way; review-requested changes go to the same implementer, which already has the
  context. Skills say so explicitly.
- *Run state.* The extension subscribes to OMP's run-state transitions
  (`AgentSession.subscribeRunState`, `agent-session.ts:4280`) and reports
  `running | idle` in its Envoy heartbeat. The daemon therefore knows per worker whether it
  is working, which answers "did it hang" now and is the basis for inferring completion
  later without D3's explicit call.
- *Cap.* `LEGION_WORKER_BUDGET` (an in-process semaphore) is replaced by a daemon-side cap on
  **running** workers per project, queued like tree admission (`admission.active/queue`).
  Idle finished workers do not count. The architect is told `worker-queued` then
  `worker-started`.
- *Sub-architects.* A worker with `role: architect` on a child issue uses the same mechanism.
  Depth is the tree depth the daemon already knows; `LEGION_MAX_RECURSION_DEPTH` stays daemon
  config and OMP's `task.maxRecursionDepth` in the workspace `.omp/config.yml` is removed.
  Workers keep the `task` tool for ordinary oracle/scout/reviewer subagents.

**Shutdown (D5, D6).** The daemon sends the existing `shutdown` control directive
(`control.ts:47-48` → `pi.shutdown()` → `session.dispose()` → `session_shutdown` →
Envoy unregister → clean exit) to every process of a tree when its linger expires, then waits
for the session to disappear from the Envoy registry (`GET /v1/registry/<session>`) or the
pane's PID to exit, and only `kill-window`s after a timeout (default 60 s). Today
`expireLinger` fires the directive and kills the window in the same breath
(`processes.ts:403-406`); that race is removed, and `markProcessDead`/`closeTree` follow the
same graceful order.

**Controller** — the per-project admission judge: triage of new board issues, backlog,
reactivation on reopen, resync anomalies, mentions, cross-tree escalations. Added: the
closed-tree activity wake (D7). Removed: interpreting an ambiguous human comment as gate
approval (`legion-controller/SKILL.md:114-125`); approval is a label or a PR review, nothing
else.

**Deleted:** `<legion-spawn>` injection and the `task`/`before_agent_start` interception,
`worker-budget.ts`, `pi.agents.ensureLive` revival and the `revive-worker` directive, the
`agents/legion-*.md` frontmatter (bodies survive as role prompts), the globalThis role-claim
bridge (one claimant per process makes it unnecessary; if the bridge must stay for the
controller command path it validates the caller's own identity).

### Roles and routing (D2, D8)

Role names are unchanged: `roleToken(project, issue, phase)` for the six phases plus
`controllerToken(project)`.

**Intake.** The daemon becomes a JetStream durable consumer (server-owned, as the listener
already is, `cmd/listener/main.go:112-140`) with one filter per configured repository:
`notifications.github.<owner>.<repo>.>`. Events that arrive while the daemon is down are
delivered on restart. `notifications.slack.*.*.mention` and the role exception lane stay as
they are.

**Routing rule** — replaces `RoutedRole` and every hardcoded `implementer` target:

| Event about issue N (or its PR) | Tree state | Target |
|---|---|---|
| any | active, `phases[N]` set | the active phase's role (`legion-<p>-<N>-<phase>`) |
| any | active, no phase running on N | the tree's architect |
| any | lingering | the tree's architect (still running) |
| any | closed | held on the tree **and** controller wake `closed-tree-activity { issue, root, event }` |
| root reopened | closed | unchanged: controller `reactivation` + probe |
| `.mention` | any | unchanged: controller |

The controller reads the artifact and either `legion admit <root>` (existing admission →
existing resume from the recorded session file → held events flush to the architect on claim)
or dismisses. Held events on a closed tree that the controller does not act on expire after
`lingerHours`.

**Liveness.** `probe()` targets the worker's own pane, not the root's. A `no_holder` or
`delivery_failed` on a worker role → probe that worker → alive: redeliver after its claim
returns; dead: resume it from its session file (the `resurrectDeadTree` resume path, per
worker) and publish `worker-died { issue, role }` to the architect. `redeliverHeldRoleEvents`
splices a held event only after the publish succeeds.

**Registries.** The daemon's `LegionState.roles` and Envoy's role KV are never reconciled
today. With one claimant per process the divergence window is a worker that booted but whose
claim failed; the boot handshake makes `claimRole` a precondition of `processReady`, so the
daemon never records a worker as ready without Envoy holding its role.

### Extension loading

OMP dedupes extensions by resolved path only (`loader.ts:701-707`). A daemon session today
loads both `--extension <repo>/packages/pi-envoy` (source) and the globally installed plugin
bundle — two `envoy.ts` instances, two NATS connections, every inbound message steered twice.
Change: `prepack.sh` stops stripping `legion.js`; the plugin ships both extensions; the daemon
stops passing `--extension` and instead verifies at startup that the installed
`@sjawhar/pi-legion-envoy` satisfies the daemon's contracts version (the same shape as
`verifyOmpAgentsCapability`). One copy, pinned by the plugin manifest.

### Dispatch (D9)

**Legion sessions.** Every root and worker knows its issue. The extension fills
`thread: <own issue>` when the model omits both `subject` and `thread`; `subject` is refused
in a Legion session unless `parent` is given (a genuinely new decision is a sub-issue of the
work). The server adopts a plain issue as a thread on first use — adds the `dispatch-thread`
label and prepends the invisible thread marker to the body — and posts the ask as a
`dispatch:ask` comment. The human's `dispatch:answer` comment is an ordinary issue comment
and routes by the table above: the active phase, else the architect, else the controller. The
existing per-session subscription remains as a fast path; correctness no longer depends on
it, so a dead asker or a restarted daemon loses nothing. The architect's design-gate ask
lands on the root issue, where the `needs-approval` label already lives.

**Every session.** The same server change makes `thread: <work issue>` legal for non-Legion
sessions, which is what Sami's rule for all agents requires ("they should have an issue that's
tracking the work... and add the question to that existing issue"). Two guards on the
standalone path: `subject` without `parent`/`thread` requires the literal argument
`standalone: "no issue tracks this work"`, and the server refuses a second open standalone
thread from the same `origin.sessionId` (`is:open label:dispatch-thread in:body "<id>"`),
naming the existing one.

**Asks on a shared thread.** #799 made a follow-up supersede the thread's earlier unanswered
asks. With one thread per issue and several phases asking, supersession must be scoped to
the asking session, not the thread — otherwise the tester's follow-up silently withdraws the
implementer's open question. Marked as an open question below.

**Dashboard.** Sidebar groups threads by issue (then by parent issue for sub-issues) using the
`parentNumber` and `origin` fields already loaded on every thread (`api.ts:122,144`). Answered
asks collapse; urgency changes and plain comments render subordinate to asks; origin metadata
renders once per thread, not per turn; long bodies collapse past ~12 lines. Two rendering
defects are already fixed (#805 free-text answers as prose; #806 Envoy deliveries in the OMP
TUI as text, not a code block).

### Phase prompts and skills (D10)

Every role prompt (planner, implementer, tester, reviewer; architect for its own work) gets a
mandatory first step: **find this repository's skills.** List the skills the session has
loaded (OMP already discovers `.claude/skills/*/SKILL.md` and `.omp/skills` walking up from
the workspace; agent-c exposes ~80); read the ones whose descriptions match the issue's
domain, the changed area, or testing/smoke/e2e/deploy/infra; read the repo's `AGENTS.md`
for its verification norms; state which you will follow before doing anything else.

- **Planner** fills `PlanHandoff.requiredSkills` per downstream role with what it found and
  one line each on why. The plan states how every acceptance criterion is exercised as a
  user would, using which repo skill. If the repo cannot yet exercise it, building that is in
  the plan — as a task of this issue, or reported to the architect as a prerequisite
  sub-issue when it must exist before implementation starts.
- **Architect** owns testability at decomposition: an issue whose criteria cannot be exercised
  end to end gets a test-infrastructure sub-issue scheduled ahead of it; integration
  verification uses the repo's own testing skills.
- **Implementer, tester, reviewer** read `requiredSkills` from the plan handoff and follow
  those skills; a repo skill's definition of "tested" wins over the worker's own. The
  tester's "where one exists" is removed: a criterion the tester cannot reach is a finding
  handed back to the architect — the work is not testable yet — not a pass.
- **All workers** may message any live role for context (D4) and are told so.

Rules inherited from the merge-queue controller's practice (verbatim source:
`running-the-merge-queue/SKILL.md`; application to Legion inferred): gates are facts read
from GitHub at surfacing time, never from a worker's report; correctness fixes land in the
PR and cleanup is a named fast-follow; bot Minors are not a gate; one independent, read-only
oracle pass over the tester's end-to-end evidence per PR, run by the reviewer before its
approval, then delta checks rather than another review round; the daemon's existing
`pr` `closed`/`merged` event to the architect is the merge notification.

### Gates (D13)

- **Design gate** — unchanged mechanism: architect adds `needs-approval` to the root (the only
  label it may write, server-enforced), asks on the root issue via dispatch, parks until
  `human-approved`. Approval is the label or the dashboard answer that applies it; the
  controller no longer infers approval from prose.
- **Merge gate** — unchanged mechanism: `merge_gate` requires a GitHub review `APPROVED` by a
  non-App login at the exact head; `legion-merger` squash-merges. agent-c `main` is protected
  by ruleset `11527236` (PR required, squash only, code-owner review, `pr-checks-result`
  required, linear history). Whether `legion-human-approval` joins that ruleset's required
  checks is an open question below.

### Configuration and launch state

- `legion.yaml`: add `nats_urls`, `board_project_ids: [<new board id>]`, `project: <new
  board>`; remove `backend`, `runtime`, `reviewer_app_id`, `reviewer_app_login`; set
  `omp_invocation` to the installed build. `resolveDaemonConfig` fails fast when
  `board_project_ids` is empty and the design gate is on.
- Daemon default OMP pin bumped; the startup probe additionally checks the installed
  pi-envoy plugin version.
- New GitHub Project (D12), Legion-only. `Core work` (#2) is no longer the target; `Legion
  Smoke (disposable)` (#24) remains the smoke board.

## Error handling

| Failure | Behaviour |
|---|---|
| Worker process dies mid-phase | probe on next publish or heartbeat gap → resume from session file; `worker-died` to architect if resume fails twice |
| Worker never claims (extension failed to load) | `processReady` never arrives → daemon reports `launch-failed` to the architect and controller after the boot timeout; the startup plugin check makes this unlikely |
| Publish to a role with no holder | held on the tree; flushed on claim; controller wake if the tree is closed |
| Daemon down when a webhook lands | JetStream redelivers on restart (D8) |
| Dispatch answer arrives after the asking worker exited | it is an issue comment → active phase or architect; nothing depends on the dead session's subscription |
| Graceful shutdown ignored | `kill-window` after timeout, logged |
| Held events accumulate on a closed tree | expire after `lingerHours` unless the controller re-admits the tree first |

## Testing

- **Permanent delivery smoke** (replaces the stub in `packages/envoy/scripts/e2e-local.sh`):
  a real `omp` TUI in a scratch tmux session with the installed pi-envoy plugin claims a
  unique role; the script publishes to it and asserts the steer in the session `.jsonl`
  within 5 s; `SIGTERM` then asserts `no_holder`. `-p` mode is not used (no session id).
- **Worker lifecycle smoke** on the `Legion Smoke (disposable)` board: daemon admits a root,
  architect spawns a planner, planner completes, `phase-complete` reaches the architect,
  planner stays idle and answers an `envoy_publish`, root closed → linger → graceful
  shutdown observed in the registry, a comment on the closed issue wakes the controller.
- **Routing unit tests** for the table above, including active-phase lookup and the
  closed-tree wake.
- **Dispatch**: adopt-issue-as-thread round trip (issue without marker → ask comment →
  answer comment → daemon reducer effect), standalone guards, per-session supersession.
- Package-local checks per CI (`biome`, `tsc`, `bun test`, `go test`).

## Rejected designs

- **One `worker` role per issue** (whichever phase is running holds it). Rejected by Sami:
  roles address specific phases for inter-agent messaging.
- **Mechanical enforcement of end-to-end testing** — required evidence fields validated at
  handoff write, `.legion/config.yml` sandbox declaration, a daemon-written commit status.
  Rejected by Sami: handoffs are for workers, skills belong in the repo, the worker's
  judgment is the mechanism.
- **Stopping workers after their phase.** Rejected: keep them for inter-agent questions.
- **`kill-window` as shutdown.** Rejected: use OMP's graceful shutdown.
- **Waking the architect directly on closed-tree activity.** Bypasses admission and spawns a
  process per stray comment; the controller decides instead.
- **Per-issue daemon subscriptions.** The daemon needs a broad subscription to discover new
  board issues and PRs anyway, and per-issue subscribe/unsubscribe races sub-issue creation;
  per-repo plus the reducer's own issue map is the filter.
- **Retiring `packages/envoy-plugin`.** Still used interactively; no Legion logic in it.
- **Worker credential provisioning.** Out of scope per Sami (D11).

## Backlog (Legion PO)

Tracked, not part of this spec's implementation plan:

- **Internal issue tracker in Go**, grown from the dispatch server (which already owns OAuth,
  SSE, per-user KV storage, an MCP endpoint, the ask/answer model and the SPA). Legion's
  GitHub-Issues surface: create/read/close with `state_reason`, four labels, sub-issues
  (GraphQL), comments with the Legion footer, Projects v2 as ingress, `in:body` search for
  dedupe. PRs, reviews and checks stay on GitHub. Sizing: CRUD + storage M, relationships S,
  event emission S, reducer adapter seam M (`reducers.ts` hardcodes GitHub JSON shapes; the
  adapter deleted in #760 covered none of this), ingress replacement S, dashboard client M,
  migration of open trees L. Sequenced after this relaunch: Legion builds it.
- **Per-deployment / per-repo worker customization.** The spawn path already composes
  `--append-system-prompt`; extend it with `legion.yaml` deployment instructions and a
  repo-provided instructions file. Workflow customization already exists via architect
  sub-issue recursion. Design after the core lands.
- **Descendant-count limit** (old `maxDescendants=20`) has no successor; only depth and the
  admission cap exist.
- `~/.config/opencode/envoy.json` is a live OpenCode-era config path read by both Go and TS.
- The Fargate listener exposes no version; add one to `/healthz`.

## Open questions

1. Ask supersession on a shared issue thread: scope to the asking session (proposed) or keep
   per-thread and give each phase its own sub-issue thread?
2. Should `legion-human-approval` be added to agent-c's ruleset as a required check, making
   the merge gate GitHub-enforced, and should the approver be restricted to Sami's login
   rather than any non-App login?
3. Held-event expiry on closed trees: `lingerHours` (proposed) or never?
4. Worker cap default when idle workers do not count: 6 running per project (proposed).
5. Does a re-run phase (review-requested changes → implementer) reuse the idle implementer
   process (proposed) or spawn a fresh one when the idle one has been shut down?
6. Who runs the read-only oracle pass over end-to-end evidence: the reviewer before approval
   (proposed) or the architect before `merge_gate`?

## Launch sequence

1. Merge the fixes already open (#803 docs, #805 dashboard answers, #806 TUI rendering); release
   pi-envoy.
2. Board and config (D12, configuration section) — needs `project` scope on Sami's `gh` token.
3. Implement: extension loading → routing and durable intake → per-process workers and
   graceful shutdown → dispatch integration → role prompts and skills → dashboard.
4. Smoke on the disposable board; then one real tree on the new board against
   `trajectory-labs-pbc/agent-c`.
