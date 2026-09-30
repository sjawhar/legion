# Working an issue: choosing, claiming, status, priority, and the backlog

`skill://dispatch` sends you here when you pick your next issue, claim or release one, get
`409 ISSUE_CLAIMED`, move an issue's status or priority, reorder a board, reassign an issue, sync a
project's architecture model, or list or audit a project's backlog.

## Choosing what to work on

When you finish an issue, or are told to work on the next thing, take the top ready issue of the
whole backlog, across every project: status `todo`, highest priority first, then board rank. There
are no areas: a standing role, a product owner and a lane each take the top issue like everyone
else (Sami, 2026-09-27, dispatch://AGENTC-34/ask/01ed2956-73cc-48d2-8ed4-7a86c6d439b1). `todo`
means ready: specced, unblocked, and waiting on neither a deploy nor a decision. An issue that
waits on one belongs in `backlog`, with what it waits on said on the issue.

Hold at most three issues in flight (`in_progress`, `testing`, `needs_review` or `retro`), of any
kind (Sami, 2026-09-27, answering dispatch://AGENTC-34/ask/1aeb8f2e-0950-4eaa-aaac-24286c9dd3ca;
the question proposed two, and his answer set three). The limit is per agent and has nothing to do
with the week's priorities (Sami, 2026-09-28, reply a7647eb0 on
dispatch://AGENTC-393/ask/b773d9f6): the priorities decide only what you pull next. Past three:
push any unfinished work, say where in one comment on the issue, move it to `backlog` and clear
its route. Each issue counts on its own; a child does not ride under its parent's slot.
In-flight issues with no owner at all go
back to `backlog` as well: no claim or route held by a live session, no Dispatch activity in the
last day, and no pull request moving on GitHub (an owner working there leaves no Dispatch trace).
The order keeper sweeps those. Never write the status of an issue that carries the `legion`
label, or of any issue under one: the Legion daemon writes those statuses, and moving one of its
admitted roots out of its flow parks the tree and stops its workers.

One agent keeps the backlog's order against those priorities, with Sami
(dispatch://AGENTC-34/ask/f6780f9e-8b96-49eb-9be7-7c7f2036d5cc). Setting an issue's priority
stays yours ([Priority is yours to set](#priority-is-yours-to-set)); reordering the board does not.
When the top of the backlog looks wrong, or a priority's next step is not yet a ready issue,
publish it to `notifications.role.backlog-order`, which the order keeper holds, instead of
reordering the board yourself.

## Claim the issue before you work it

Two sessions once spent a night implementing the same issue, because nothing on it said who was
on it (Sami, 2026-09-24, verbatim: "It seems like we need a better way of tracking what's already
in progress"). So before you start implementing an issue, claim it:

```ts
dispatch_claim({ issue: "LEGION-234" })                  // I am implementing this
dispatch_claim({ issue: "LEGION-234", release: true })   // I have stopped; it is free
```

A claim records **your** session — the one making the call, never another — and shows on every
read of the issue: the dashboard header, the issue list and board, `dispatch_read` (a
`Claimed by:` line) and `dispatch_issues` (a claim on the row). A session's claim there ends
`· not running` when the live agent registry does not list that session, as the dashboard's chip
does — that claim is free to take — and `· liveness unknown` when the registry could not be read,
which says nothing either way. `dispatch_issues` plus the dashboard's **Unclaimed** filter is how
you find work nobody is on.

- **`409 ISSUE_CLAIMED` means someone else holds this issue.** When it is another session, the
  refusal names it and says it is still running: do not work the issue in parallel — message
  that session (its id is in the message; `envoy_send` reaches it) or pick up something else,
  and tell the human if you believe the work should be yours. When a **human** holds it, the
  refusal names the person and says nothing about a session running, because there is none to
  message: ask them with `dispatch_ask` instead, so the open ask appears in their Inbox, and
  never assume their claim has lapsed — only a human releases or forces a human's claim.
- **`409 CLAIM_CONTENDED` means the issue changed hands twice while your call ran**, so nothing
  was applied and nobody's liveness was checked. Read the issue and decide again; it is not a
  refusal by a live holder.
- **A claim whose session has ended is yours to take.** If the Envoy listener no longer lists
  the holder, your claim simply succeeds; the takeover is recorded on the issue and the session
  that lost it is told.
- **Release it when you stop** — finished, handing over, or moving to something else. A claim is
  released by its holder or any human — and by any agent once the holder's session is no longer
  running, the same rule that lets you take it. Closing the issue releases it for you.
- A claim is intent to implement, not contact: reading the issue, commenting, asking, or gating
  its pull request claims nothing, so a coordinator never collides with an implementer.

**Claiming and moving the status are two separate actions, and you do both.** A claim says which
session is on the work; the status says where the work has got to, and humans use it to track
that too (Sami, 2026-09-24, verbatim: "Keep them separate — Separate because humans might be
using them to keep track of work"). So when you start: `dispatch_claim({ issue })` **and**
`dispatch_issue_update({ issue, status: "in_progress" })`.

## Issue status is yours to move

The issue's status is how a human sees delivery without asking a session. Outside Legion (where
the daemon writes it), the session doing the work moves it, the way a person moves a card:
`in_progress` when implementation starts, `testing` when the change is being proven on a
production-like surface, `needs_review` when its pull request is open and waiting on the merge
queue, `done` when the change has been driven in production (a merge is not `done`). Move child
issues you own as well as the root. An issue left at `triage` while work is underway is a defect:
Sami, 2026-09-15, on the roadmap he could not read — "I'm not even sure what their development
status is." Waiting for the deploy lane is not a status and is never announced.

```ts
// PATCH /api/v1/issues/{key} — status, title, labels, priority, external_links (merged by URL), route, parent
dispatch_issue_update({ issue: "AGENTC-175", status: "testing" })
dispatch_issue_update({ issue: "AGENTC-175", status: "done", reason: "Shipped in owner/repo#7; verified on the production dashboard." })
dispatch_issue_update({ issue: "AGENTC-175", priority: 1 }) // 0–3; see Priority is yours to set
dispatch_issue_update({ issue: "AGENTC-175", external_links: ["https://github.com/owner/repo/pull/7"] })
dispatch_issue_update({ issue: "AGENTC-175", parent: "AGENTC-170" }) // same-project key; "" clears the parent
```

Closing takes a `reason`, and the tool refuses `status: "done"` without one: it posts the reason on
the issue as a message, then closes it, because a closed issue refuses messages, comments, and
artifacts, so a reason left for later has nowhere to go. When the close fails after the post, the
error names the posted message; after a timeout or a server error it also says the close may have
landed, so read the issue's status first. A retry points its reason at the posted message rather
than repeating it.

The two clears differ: `priority` clears with `null`, while `parent` and `route` clear with `""`.
Guessing the other one is a refusal either way.

Link the pull request that delivers the issue in `external_links` when you open it; the issue page
renders its state and checks from that link, and `dispatch_read` lists it under `External links:`.
The call is authenticated with the same bearer as every
other `dispatch_*` tool: a Legion pane reads it from the `DISPATCH_TOKEN_FILE` path the daemon sets on
the pane; an OMP session outside Legion reads `dispatch.token` from `~/.config/opencode/envoy.json`.

A write to an issue still in `triage` answers once with `… is still in triage …`; move the status
when work has started.

## Priority is yours to set

Priority is the coarse bucket a backlog is read by: `0` is P0, the highest, through `3`, P3, the
lowest, and `null` clears it. Sami ruled on 2026-09-24, answering "may agents set issue priority
(P0–P3), or only propose it for you?" on `dispatch://LEGION/artifact/issue-status-conventions-md`:
**"Agents may set"**. So set it — on creation, and on a grooming pass over issues that have none —
and say what you set and why; he overrides anything he disagrees with from the dashboard. A closed
issue takes only `rank`, `components`, and a reopening `status` (any status but `done`);
everything else, `priority` included, waits for the reopen (`409 ISSUE_CLOSED`). So reopen it
first, then set the priority — the two cannot go in one call. `rank` itself is not a tool field:
reorder it the way [Issue reads](#board-rank-priority-and-assignee-on-a-read) describes, through `PATCH /api/v1/issues/{key}`.

## Board rank, priority, and assignee on a read

Issue reads include `rank`, the server-owned ordering key used by project boards; reorder through `PATCH /api/v1/issues/{key}` with `{"rank": {"before": "<key>", "after": "<key>"}}`, either neighbor optional and both in the issue's project. A bearer caller also names its own session in that body, `"actor": {"kind": "session", "id": "<your session id>"}`, or the server refuses with `ACTOR_KIND`. They also include nullable coarse priority (`P0` highest through `P3` lowest) and `assignee`: the lowercase GitHub login of the human who answers the issue's asks, or `null` when nobody holds it. `dispatch_read` of an issue prints it as `Assignee: <login>` or `Assignee: unassigned`.

## Reading a project's backlog

To see the shape of a project rather than find a phrase, list its issues:
```ts
dispatch_issues({ project, status?, parent?, label?, priority?, route_status?, updated_since?, limit?, offset? })
```
Each row carries the issue key, title, status, priority, parent, labels, its open-ask count, and
when it last changed — a roadmap or backlog pass without opening every issue. Filter with `status`
(a lifecycle status), `parent` (one issue's children), `label`, `priority` (a list of `0`–`3`, with
`null` for an issue with no priority: `[0, 1]` is every P0 and P1), `route_status` (below), or
`updated_since` (an RFC3339 timestamp, for "what moved this week"). `limit` is the page size, 50 by
default and 250 at most, and `offset` is where the page starts; the answer says how many issues
match. Repeating with the next offset walks every matching issue only while the list does not
change: an issue that enters or leaves what your filters match, or whose status or rank changes,
between two pages shifts rows across a page boundary, and the walk then shows one issue twice and
misses another.

This is not search: it matches no text. Use `dispatch_search` for a keyword or phrase, and
`dispatch_issues` when you want every issue in a project and its current state.

### The owner audit

As the owner of a surface, list the project's P0 and P1 issues and staff or close each one nobody
has started:
```ts
dispatch_issues({ project, priority: [0, 1], limit: 250 })
```
Every unclaimed row in `triage`, `icebox`, `backlog` or `todo` is a decision: someone takes it and
builds it, or it closes. A row in `in_progress`, `testing`, `needs_review` or `retro`, or one that
carries a claim, is work under way ([Issue status is yours to move](#issue-status-is-yours-to-move))
and is not re-staffed. A todo with a finished spec reads as queued work that nobody is doing
(LEGION-173 sat in todo for two weeks with a complete spec; AGENTC-1010's v4 plan sat in backlog
with nobody building it).

A close that says the defect cannot happen cites the code that makes it impossible. An issue
closed because a rewrite forecloses it names the file and line in the rewrite that does so; a
close that cannot name one is not foreclosed, it is unread. The cheapest way for a rewrite to reach
parity is to port the code, defect included: LEGION-211's bare `git worktree prune`, filed against
the TypeScript daemon, had been ported into the Go coordinator and was live in production.

The audit finds four shapes:

- **Unstaffed work.** A plan or measurement exists, and no one is building it.
- **Unrecorded delivery.** An issue not yet in `testing` or `done`, claimed or not, has a merged PR
  naming it. Check the change live, then move the issue (AGENTC-1033 sat at `triage` after its fix,
  agent-c #20367, merged).
- **Unrecorded practice.** Someone does the issue's work by hand, more than once, while the issue
  sits in backlog (OPS-132, done by hand on every migration merge). It leaves no plan and no PR to
  find; the tell is your own messages. Doing something by hand more than once means an issue is
  wearing the wrong status.
- **Unreachable route.** An open issue whose route names a role nobody holds, or a session that is
  not running, reaches nobody, whatever its priority, and the priority filter above never finds
  it. List it on its own:
  ```ts
  dispatch_issues({ project, route_status: "no_holder", limit: 250 })
  ```
  Each row reads `route role:sre (nobody holds it right now)` or `route session:<id> (that session
  is not running right now)`. That is one read of the listener, and one read is a restart gap as
  often as a vacancy: an agent box that restarts or resumes keeps the session id, but the session
  is absent from the listener for minutes, and its role with it. On 2026-09-27, 58 of 63 session
  routes one read showed as unreachable pointed at a single session that was moving between boxes.
  So a route is unowned only when it is `no_holder` on two reads at least ten minutes apart: list
  again after ten minutes and act on the issues both lists name. Confirm with the second
  `dispatch_issues` read, not `envoy_role_get`: a role lookup releases the claim of a holder whose
  session is absent from the registry as it answers. Then staff the role, re-route the
  issue to a live holder, or clear the route and assign it (AGENTC-1065, a P2 production listener
  503, sat routed to an unheld `role:sre` with no assignee). `route_status: "unknown"` means the
  listener did not answer, so a route could not be judged; a `no_holder` filter refuses rather
  than answer an empty list then.

Run the audit as a step of a coordinator's loop, at each checkpoint, not as a habit: these shapes
are found by running the check, not by noticing them.

## Syncing a project's architecture model

Import a project's architecture model from its configured source repository now (a human configures the source in Settings):
```ts
dispatch_architecture_sync({ project: "CORE" })
```
It returns the imported commit, or the recorded error when the model was rejected — the previous model stays up. Without a configured source it answers 404 `SOURCE_NOT_FOUND`.

