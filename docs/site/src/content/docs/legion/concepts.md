---
title: Concepts
description: The controller, the architect, the phase workers, admission and the legion label, the design gate, handoffs, review, READY and the production check.
sidebar:
  order: 2
---

Legion has a few moving parts. This page names each one and says what it does, so the rest of the
section can use the words without stopping to explain them.

## The pieces

- **The Legion daemon** is one process an operator runs with `legion start`. It reads Dispatch's
  issue events and GitHub's pull request, review and CI events, decides which phase each issue is
  in, and starts and stops the agents that do the work. It keeps its own record in Postgres, so a
  restart picks up where it left off. It never writes code and never merges.
- **Agents** are [Oh My Pi](https://github.com/sjawhar/oh-my-pi) sessions. Under the Kubernetes
  runtime every agent the daemon starts runs in a pod of its own, from one published worker image,
  and talks to the daemon over a connection the pod dials back.
- **[Dispatch](/legion/dispatch/)** is where people and agents meet: the issue, its spec, the
  questions an agent asks you, the approvals, the messages and the status everyone reads. Legion
  never reads or writes a GitHub issue.
- **GitHub** holds the code: one branch, `legion/<KEY>`, and one pull request per issue. Legion acts
  there through two GitHub Apps, so the account that reviews a change is never the one that wrote
  it.

## The controller

The controller is the one Legion agent a person talks to directly. The operator starts it with
`legion controller start` in a terminal on their own machine, and it runs there as an interactive
Oh My Pi session; closing that terminal leaves the project without a controller until someone runs
the command again. The daemon wakes it when something needs a judgment no tree owns:

- a new `triage` issue labelled `legion` to admit, park, or leave alone;
- a free admission slot to fill (see [Admission](#admission-and-the-legion-label));
- a tree its own architect escalated, or one whose architect ran out of launches.

The daemon also wakes it on a timer, every `controller_wake_interval_seconds` (an hour by
default), and on its first turn of each UTC day the controller posts a report on what Legion
finished, closed and is running.

The only statuses the controller sets are `todo`, `backlog` and `icebox` (`triage` is set by
whoever files an issue). It never reads GitHub and never touches a tree's work.

## Trees and the architect

Every issue Legion admits becomes the root of a **tree**, and an **architect** owns it from
admission to close. The architect:

1. claims the issue in Dispatch, so no other session works it alongside Legion;
2. extends your issue's spec in place, writing each open question as a decision block for you to
   answer;
3. asks you to approve the spec (the [design gate](#the-design-gate));
4. splits large work into child issues and releases them in waves, when one pull request would be
   too much;
5. decides what to do when a phase gets stuck, a review round cap is reached, or CI keeps failing;
6. signs the issue off once the production check is recorded, which closes it.

The architect writes no code. A child issue runs the same phases as its root, with a pull request
of its own, inside the same tree.

## Phase workers

The daemon starts each phase's worker itself, one phase at a time, in the order below. When a phase
ends its worker is suspended: its pod goes away and its conversation is kept, so the same agent
comes back, with everything it knew, the next time its role is needed.

| Phase | Worker | What it does | GitHub identity |
| --- | --- | --- | --- |
| `planning` | planner | Writes the implementation plan into its handoff and the issue's document. | review App |
| `implementing` | implementer | Writes the change, proves it works, pushes `legion/<KEY>` and opens the pull request. | implement App |
| `testing` | tester | Exercises the change against the issue's acceptance criteria and reports `pass` or `fail`. | review App |
| `reviewing` | reviewer | Reviews the pull request and submits an approval of the head, or changes requested. | review App |
| `retro` | implementer | Writes down what the work taught, as notes in the repository and one message on the issue. | implement App |
| `merging` | merger | Checks the approved head and the required checks, then sends `READY`. | implement App |
| `awaiting_merge` | none | Waits for a person to merge the pull request. | none |
| `production_check` | implementer | After the merge, drives the change in production and records what it saw. | implement App |

A failed test, red CI on the head, or a review that requests changes sends the issue back to
`implementing`, and the change goes through the tester again before the reviewer sees it.

## Admission and the `legion` label

Legion works only the issues handed to it with the Dispatch label **`legion`** (in any case), so it
can share a Dispatch project with people and other agents. A root issue is admitted when it carries
the label, is in `todo`, and a slot is free. The number of slots is `admission_cap` in the daemon's
configuration, four by default; a labelled `todo` root that finds no free slot waits in line, in the
project board's order, and takes the next one.

There are two ways an issue gets the label:

- **A person hands it over**, from the issue header in the Dispatch dashboard.
- **The controller takes it.** Whenever a slot is free, the controller labels the
  highest-priority `todo` issue that has no children and that nobody else is working, and says so
  in a comment on the issue.

A child issue needs no label: it runs under its tree once the root is admitted. Taking the label
off a waiting root takes it out of line; taking it off a running tree does not stop the tree.

## The design gate

By default (`gates.design: root-issues`), no phase starts until a person approves the root issue's
spec. The architect settles the spec's decision blocks with you first, then asks for approval with a
summary of what the tree will do. You approve from the document's header or from the approval
question in your Dispatch Inbox; **Request changes** sends it back with your reason.

Approval is pinned to a version: a later edit to the spec closes the gate again until someone
approves the new version. Work already in flight carries on, but the merger's `READY` waits for that
approval. The daemon only reads approvals; nothing in Legion approves a spec on a person's behalf. A
deployment that does not want the gate sets `gates.design: off`, and the tree starts as soon as the
architect has written its spec.

## Handoffs on the issue branch

Each phase ends by writing a **handoff**, a small JSON file committed to the issue's branch under
`.legion/`: `architect.json`, `plan.json`, `implement.json`, `test.json`, and `review.json`. The next
phase reads the ones before it. A handoff is the copy that survives: when a worker comes back after
a restart, or a memory disagrees with a file, the committed handoff wins. The merger writes none.

## Review signalling

Legion uses GitHub's own review mechanisms rather than labels:

- The **reviewer** submits a GitHub review: an approval of the head by its commit, or changes
  requested. A plain comment decides nothing, and the architect is told the round is stuck.
- **CI** on the pull request's head counts: red checks while testing or reviewing send the issue
  back to the implementer, naming the failing checks. Only the checks the base branch requires
  count, the same set the merger's `READY` checks (its rulesets' required status checks and its
  branch protection's): CI is red when one of them failed. A required check that was cancelled, or
  that the head's checks settled without, leaves the head with no verdict until a later settlement
  decides it, since a settlement can come before an aggregator job is queued or just after a run
  is cancelled. The check may never run again, and a head with no verdict never reaches `READY`,
  so when that is the reviewer's own head and its approval is in, the architect is told the round
  is stuck, naming the check. A failing check the base branch does not require, such as a lane
  started by hand or an advisory review check, never makes CI red, and on a base branch that
  requires no check nothing does. The daemon judges the check runs GitHub reports, so a required
  check that only a commit status reports never reports to it: the head never reads green, and an
  approved round is told stuck on it, where `READY` reads the status. A head that a push changing
  only `.legion/` made carries the checks of the head before it. The daemon reads each open pull
  request's required set with the implement App as it starts and every two minutes after; until a
  read succeeds, no verdict stands for the head, and a read GitHub refuses is logged with the
  repository and the HTTP status.
- **Review threads** close one by one, and only once the thread's opener (or, for a thread a bot
  opened, Legion's reviewer) accepts the reply.
- Two limits stop a loop: after `review_round_cap` review rounds (three by default), or once
  `max_fix_attempts` pushes fail to turn CI green (three by default), the daemon posts a message on
  the issue and hands the decision to the architect.

## READY and the human merge

When the reviewer has approved and retro is done, the merger checks that the pull request's head is
the approved one (plus only retro's notes) and that every check the base branch requires has passed.
It then sends **READY**, which the daemon posts on the Dispatch issue as one message:

```text
READY #<pull request> at <head sha> (approved at <approved sha>) for <KEY> (<pull request url>)
```

followed by the pull request's outcome and its known risks. A project can also name a merge-queue
role (`merge_queue_role`) that gets the same packet. Legion never merges, and every merge-shaped
`gh` command an agent tries is refused. A person merges under the repository's own branch
protection and code-owner rules, which Legion neither reads nor changes.

## The production check

After the merge, the daemon starts the implementer once more: the agent that wrote the change
drives it in production through a user's own access path and records what it observed, on the pull
request and on the issue. A defect it finds becomes a new child issue of the tree. Only once that
record exists does the architect sign off, which moves the issue to `done`.

A finished tree **lingers** for `linger_hours` (72 by default) with its workspace kept, in case work
resumes; then its pods and volume are deleted. Setting a done root back to `todo` runs it again as a
new generation, starting from its architect.

## Supervision

The daemon counts what goes wrong with each agent: launches that fail, deaths while it had work
outstanding, prompts it acknowledged without taking a turn, and the times it was relaunched for
that. When a budget runs out (`launch_failure_limit`, `prompt_failure_limit` and
`prompt_retire_limit` in the configuration) the daemon stops relaunching that agent and **holds**
the issue. A held phase worker is its architect's to retry or escalate to the controller; an
architect that runs out goes to the controller, which can re-admit the tree.
[Troubleshooting](/legion/legion/troubleshooting/) shows how to see each of these.
