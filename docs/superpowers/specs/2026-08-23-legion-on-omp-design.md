# Legion on oh-my-pi — Design

Replace Legion's opencode-serve substrate (daemon-spawned worker sessions, deterministic
session IDs, label-driven polling recovery) with oh-my-pi's native primitives: task/hub
subagents, in-process parking and revival, envoy/NATS delivery, and a thin daemon. The
goals from the current system carry over unchanged: keep work moving without polling,
artifacts remain the source of truth, and wakes are advisory.

## Topology

One resident omp process per **root issue**. Inside it:

- **Parent session = the architect.** It owns the issue tree end to end: decomposition,
  orchestration, adjustment, integration verification, and final sign-off. Its toolset is
  restricted vibe-style — read, task/hub, envoy, and GitHub issue operations (create
  sub-issues, labels, comments); no edit/write and no general bash — it delegates all
  code work.
- **Phase workers (planner, implementer, tester, reviewer, merger) are keepAlive task
  subagents** of the architect. They park between events (session disposed, transcript
  retained) and revive automatically when a message or routed event arrives. Workers on
  sub-issues run in `isolated` worktrees via the task tool's native support.
- **Sub-architects are in-process subagents** of the parent architect (see Recursion).

Process residency is cheap: parked subagents hold no session memory. The process exits
when the root issue closes (plus a post-close linger, below).

### Roles and spawning

Only architects spawn. This is enforced **structurally**: phase-worker agent definitions
omit the `task` tool, so a planner physically cannot spawn (verified: the same mechanism
the recursion cap uses strips `task` from agents at the depth cap). The issue-process
config raises `task.maxRecursionDepth` so architect nesting is never depth-limited;
depth is not the enforcement mechanism, roles are.

## Recursion and decomposition

Architects may split their issue into GitHub sub-issues (native sub-issue relationships)
and handle each child one of two ways, per-child judgment call:

1. **In-process (default):** spawn a sub-architect subagent. Best for shallow or coupled
   subtrees. All coordination rides the in-process bus and task contract (structured
   outputs, job accounting, steering).
2. **Promotion (escape hatch):** file the child as a new **root issue**. It re-enters
   through the normal front door — controller triage, daemon admission, its own process.
   The parent needs no special protocol: it subscribes to the child's gate transitions
   (condensed topic) and receives summaries as envoy messages. Use for heavyweight or
   independent subtrees; also the answer if a tree outgrows its process (crash blast
   radius, binary staleness).

Either way the children are real GitHub issues, so child tracking (below) is identical
for both paths.

**Capacity:** the daemon's admission gate governs root-process spawns only. Intra-tree
fan-out is governed by omp's native per-process subagent concurrency cap (queues excess)
plus architect judgment. No spawn RPC between architect and daemon for subagents.

## Parent lifecycle and child tracking

The daemon reducer maintains each parent's child state from GitHub's sub-issue graph
(webhook events for child add/remove/close) and emits on the **parent's** condensed
topic:

- one event per child closure: which child, completion state, remaining count, final
  comment ref;
- one terminal gate transition when all children are closed.

The architect holds a single subscription — its own children topic — independent of
fan-out and of the in-process/promoted split. "All closed" is computed from the GitHub
graph, never from process memory, so it survives process death.

Lifecycle:

1. **Decompose** — create child issues in waves, not necessarily all upfront.
2. **Children in flight** — architect is parked; each closure wakes it to adjust:
   re-scope open children, cancel obsolete ones, release the next wave. The architect is
   the scheduler; **no dependency/blocked-by encoding exists in v1** — wave scheduling
   via closure wakes subsumes it.
3. **Children complete** — terminal gate transition wakes the architect for the end-game.
4. **Integration verification** — the architect dispatches one fresh tester subagent
   against current main, scoped to the parent issue's own acceptance criteria. Each
   child was tested in isolation against main-at-its-time; this is the only check of the
   assembled whole. Failures become a new child wave (the adjust loop).
5. **Architect sign-off** — the architect's own final gate: scope fully met, integration
   evidence in hand, nothing necessary silently deferred. The architect agent definition
   carries heroic-ownership prompting: deferring necessary work (especially architectural
   work and refactors) is failure; the only legitimate deferral is a new child issue the
   architect creates and owns.
6. **Close** — closing the parent emits the same closure event one level up if this
   issue was itself a promoted child. Recursion closes over itself with no extra
   mechanism.

## Event architecture

Two tiers on NATS/JetStream:

- **Raw** (`notifications.github.<owner>.<repo>.…`) — existing webhook topics.
- **Condensed** (`notifications.legion.state.…`) — daemon-hosted stateful reducers
  consume raw events and emit semantic state transitions. JetStream-durable, so a
  resurrected process replays transitions missed while dead.

**Consumption rule: workers get activity on their own artifact; overseers get verdict
changes only.** The implementer reads full comment text on its own PR; the architect and
controller see only gate transitions (ready/blocked/children events). Raw events never
enter an architect's context.

### Subscription semantics

Subagents self-subscribe at runtime (`envoy_subscribe`) — the natural point, since only
the implementer knows its PR number. The process extension records interests on the
subagent's behalf and owns the NATS side:

- Interests **survive parking**. An event for a parked subagent is delivered over the
  in-process bus, which auto-revives it with the event as its wake message.
- Interests are **reaped at terminal states** (abort/release). Unsubscribe-on-done is
  derived from lifecycle, never from worker discipline; a worker that forgets cannot
  leak. NATS subscriptions are dropped when no live interest references a topic.

### Delivery rendering

The wire stays JSON (reducers, dispatcher, and daemon parse it). The envoy-omp extension
renders payloads to **TOON** at the point of injection into a session's context —
condensed events, check lists, and replayed transitions are uniform arrays, TOON's
sweet spot. No TOON on the wire.

### CI reducer contract

Per PR, the reducer tracks only the **current head commit** and emits:

- **Eager first red:** the first failing check of the head, immediately.
- **Settled red batch:** the complete failing list once the burst quiets (at most two
  red events per push).
- **Settled green:** only when the full set settles green.

Suppressed by construction: queued/running noise, per-check transitions, and any result
for a superseded commit. A new push resets the edge detector. Overseers receive only
derived gate transitions ("PR ready: green + approved", "PR blocked: red after N fix
attempts"), never CI events.

## Ingress and the controller

- New human-created issue → controller triages → daemon spawns a root process.
- Human-created epic with existing sub-issues → triage the **children**; never
  auto-dispatch the parent as a work unit.
- System-created child issues carry a marker (label/creator identity) so ingress never
  double-triages them; in-process children are owned by their tree, promoted children
  re-enter deliberately.
- The controller remains a wake-driven session for triage, escalations, and mentions.
  It does the thinking; it never routes raw events (the daemon and extensions do) and
  never performs worker work.

Future ingress channels (e.g. Slack environment-issue intake from contractors) enter
here: external report → controller triage → root issue. Externally-triggered work
crosses an authorization boundary and must not drive privileged agents without a gate.

## Handoff and persistence

- **Live path:** phase subagents return structured output via task `outputSchema` —
  parsed, validated JSON straight to the architect, with `agent://` artifacts for bulk.
- **Durable path:** `.legion/$MODE.json` committed on the branch, as today. Local reads
  for every phase (no GitHub round-trips in the handoff path), useful PR telemetry,
  survives process death via the workspace.
- **Never merged to main:** the merge phase's final commit deletes `.legion/`; under the
  standard squash-merge the add+delete nets to zero, so the files appear in neither
  main's tree nor its history while the PR retains them. (Rebase-merge would leave
  add/remove pairs in history — acceptable degradation, squash is the standard.)

## GitHub identities and session attribution

Exactly two GitHub App identities, because only real identities change GitHub behavior:
**author** (planner/implementer/tester/merger) and **reviewer** (GitHub voids a PR
approval from the PR's own author). No per-session identities.

Session attribution rides metadata:

- **Commits:** author email plus-addressing (`implementer+<session-id>@…`, committer
  stays the app) and a `Legion-Session: <id>` trailer carrying the transcript reference
  (machine-readable via `git interpret-trailers`).
- **Comments/reviews:** a structured footer per comment (visible one-liner or invisible
  `<!-- legion: {session, issue, phase} -->` HTML comment).
- The daemon parses both and maintains the reverse index: commit SHA or comment →
  session → transcript.

## The daemon

Thin, and its whole job list:

1. NATS/JetStream.
2. Condensation reducers (CI edges, gate transitions, children aggregation) and the
   attribution reverse index.
3. Root-process spawn with admission control (global capacity cap; queue excess).
4. Resurrection: an event on a durable topic whose owning root process is dead →
   re-exec omp against the process's session files; the revived process replays missed
   condensed transitions from JetStream.
5. Resync healing: the existing low-frequency artifact-driven pass, unchanged in spirit
   — labels and GitHub state remain the truth that heals missed pushes.

Gone: opencode-serve, worker port registry, per-worker HTTP env endpoint, polling state
passes as the primary loop.

## Failure and recovery

- **Within a tree:** parking/revival is automatic and in-process. Nothing to heal.
- **Root process death:** the whole tree's revival machinery dies at once. Recovery
  substrate: session files on disk, `.legion` artifacts on branches, JetStream replay,
  resync. Documented limitation: blast radius is the tree, and a long-lived process runs
  the omp binary it started on — resurrection is also the binary-upgrade path (kill,
  resurrect on the new build).
- **Post-close linger (default, overridable):** after the root issue closes, the process
  lingers parked for **72 hours**, still routing PR/issue comments to revivable workers
  with full context ("this broke X" follow-ups). After that, events on closed issues
  reach only the controller's standing subscriptions (mentions) and resync.

## Out of scope (v1)

- Inter-child dependency encoding (wave scheduling subsumes it).
- Per-session GitHub identities (attribution solves the actual need).
- TOON on the wire (model-boundary rendering only).
- A parent↔child protocol beyond gate transitions + envoy messages for promoted
  children.
- Migration tooling from opencode-Legion state (trees start fresh; the old daemon runs
  until drained).

## Flagged defaults (overridable at review)

- Post-close linger: 72h.
- Promotion guidance: promote when a subtree is independent enough to deserve its own
  capacity slot, crash boundary, and binary freshness — otherwise stay in-process.
- `task.maxRecursionDepth`: raised generously (e.g. 8) in issue-process config rather
  than disabled, as a runaway-decomposition backstop.
