# Working an issue: choosing, claiming, status, priority, and the backlog

Read this to choose, claim or release an issue, or to handle `409 ISSUE_CLAIMED`. Read it to move status or priority, reorder a board, reassign, sync an architecture model, or audit a backlog.

## Choosing what to work on

- When you finish an issue, or are told to take the next thing, take the top ready issue of the backlog, across every project. Ready means status `todo`, sorted by priority, then board rank. No areas: a standing role, a product owner and a lane take the top issue too.
- `todo` means ready: specced, unblocked, waiting on no deploy and no decision. An issue that waits on one goes in `backlog`, with what it waits on said on the issue.
- Hold at most three issues in flight (`in_progress`, `testing`, `needs_review`, `retro`), of any kind, per agent. A child counts on its own. Priorities decide only what you pull next. Past three: push unfinished work, say where in one comment, move it to `backlog` and clear its route.
- An in-flight issue with no owner goes back to `backlog`. No owner means no claim or route held by a live session, no Dispatch activity in a day, and no PR moving on GitHub. The order keeper sweeps those.
- Never write the status of an issue with the `legion` label or under one. A status you write on a running Legion root is set back and its architect is told. Only a person in the dashboard, the controller's `set_status`, or the operator stops that tree. On a child, a status out of the flow parks it and stops its workers.
- Set priority yourself. Never reorder the board: one agent keeps each project's order, and in a Legion deployment that agent is the controller, at `notifications.role.legion-<project>-controller`. When the top looks wrong, or a priority's next step is not a ready issue, publish it there, and say it on the issue where no Legion controller runs.
- The keeper moves a card with `dispatch issue-update --issue <KEY> --before <OTHER>` or `--after <OTHER>`.

## Claim the issue before you work it

```bash
dispatch claim --issue LEGION-234             # I am implementing this
dispatch claim --issue LEGION-234 --release   # I have stopped; it is free
```

- A claim records your own session. Reads show it (`Claimed by:`). `· not running` means the holder is gone and you can take it. `· liveness unknown` says nothing. Find free work with `dispatch issues` and the dashboard's **Unclaimed** filter.
- Never claim an issue with the `legion` label or under one unless you are a Legion role of that issue's tree, even one whose claim reads `· not running`. The Legion daemon suspends a root's architect while the root waits on a person, so its claim reads `not running` for hours while its tree is live, and a claim another session takes there takes the tree out of the workflow.
- `409 ISSUE_CLAIMED` from a live session: never work it in parallel. Message that session (`envoy_send`) or take other work, and tell the human if you think it should be yours. From a human: ask them with `dispatch ask`. Never assume a human's claim lapsed. Only a human releases or forces it.
- `409 CLAIM_CONTENDED`: nothing applied. Read the issue and decide again.
- Release when you stop. The holder, any human, or any agent once the holder's session ended can release it.
- Claiming moves no status. Do both when you start.

## Issue status is yours to move

- Move it as a person moves a card, root and owned children: `in_progress`, `testing`, `needs_review`, then `done` once driven in production. A merge is not `done`. An issue left in `triage` while work runs is a defect. Waiting for the deploy lane is not a status and is never announced. Never the status of an issue with the `legion` label or under one: the daemon writes those, and a status you write there is undone.
- `--status done` needs `--reason`. Dispatch posts it as a message, then closes. If the close fails after the post, read the status first. A retry points its reason at the posted message.
- Clears differ: `--clear-priority`, `--parent ''` / `--route ''` / `--repo ''`, `--clear-external-links` / `--clear-blocked-by`. See `dispatch issue-update --help`.
- Repository: own `--repo`, else nearest ancestor's, else the project default, else none. Unmapped is `REPO_NOT_MAPPED`. `dispatch read` shows `Repository:`. `Repository: unknown (…)` is an older server: a `--repo` write there fails.
- Link the delivering PR with `--external-link` when you open it.
- `… is still in triage …` on a write: move the status once work starts. It never appears on an issue with the `legion` label or under one, since the daemon moves those out of triage itself.

## Priority is yours to set

- `0` (P0, highest) to `3` (P3), `null` clears. Set it on creation and on grooming passes over issues with none. Say what you set and why. The human overrides from the dashboard.
- A closed issue takes only a board move (`--before` / `--after`), `components`, a reopening status, and an external-links write that adds no link. Reopen first, then set priority, in two commands (`409 ISSUE_CLOSED`).
- Labels are optional. Dispatch trims them, keeps their case, and drops case-insensitive duplicates.

## Board rank, priority, and assignee

- Reads carry `rank`, nullable priority, and `assignee`: a lowercase email, or `null`. `dispatch read` prints `Assignee: unassigned` for `null`.
- Move a card with `dispatch issue-update --issue <KEY> --before <OTHER>`, which puts it above that issue, `--after <OTHER>`, which puts it below, or both together, which land it between them. The neighbour is an issue of the same project and never the issue itself, and the server refuses a pair the board does not hold in that order. The move rides along with any other change in the same call, and a closed issue takes it.

## Reading a project's backlog

`dispatch issues --project <KEY>` lists every issue with status, priority, parent, labels, open asks and last change. It is not search. Filters and paging are in `--help`. A page walk is exact only while the list holds still.

### The owner audit

As a surface's owner, run `dispatch issues --project <KEY> --priority 0 --priority 1 --limit 250` at each checkpoint of a coordinator's loop. These shapes show up only when you run the check.

- Each unclaimed row in `triage`, `icebox`, `backlog` or `todo` is a decision: someone builds it, or it closes. Rows in flight or claimed are not re-staffed.
- A close that says the defect cannot happen cites the code that prevents it. A close by rewrite names the file and line that forecloses it. Without one, the issue is unread, not foreclosed: a rewrite reaches parity most cheaply by porting the defect.
- Find four shapes:
  - **Unstaffed work**: a plan or measurement exists, nobody builds it.
  - **Unrecorded delivery**: a merged PR names an issue not yet in `testing` or `done`. Check it live, then move it.
  - **Unrecorded practice**: someone does the issue's work by hand more than once. The tell is your own messages. Fix the status.
  - **Unreachable route**: the priority filter above never finds it, whatever its priority. List it on its own with `dispatch issues --project <KEY> --route-status no_holder --limit 250`. A route is unowned only when two reads ten minutes apart both list it, since a restarting session is absent for minutes. Check with the second `dispatch issues`, not `envoy_role_get`, which releases an absent holder's claim. Then staff the role, re-route to a live holder, or clear the route and assign. `unknown` means the listener did not answer.

## Syncing a project's architecture model

A project imports its model from one or more source repositories, each on its own branch. `dispatch architecture-sync --project CORE` imports every source now and prints a line for each: `Synced CORE architecture from <repo>@<branch>: commit <sha> (<time>).`, or `Sync failed for CORE (<repo>@<branch>): <error>` followed by what of that source still shows: its commit, its component count when the source was re-saved on another branch and has not imported from it yet, or that none ever imported. A failed source keeps its previous components; every other source is untouched. `--repo <owner/name>` imports that one source alone. No source configured, or no source from that repository, answers `SOURCE_NOT_FOUND`. A credential or branch problem is that source's failed line, or with `--repo` an error, `SOURCE_ACCESS`.
