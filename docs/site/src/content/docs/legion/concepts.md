---
title: Concepts
description: The controller, the architect, the phase workers, admission and the legion label, the design gate, handoffs, review, the architect's acceptance, READY, the merge that closes the issue and the production check after it.
sidebar:
  order: 2
---

Legion has a few moving parts. This page names each one and says what it does, so the rest of the
section can use the words without stopping to explain them.

## The pieces

- **The Legion daemon** is one process an operator runs with `legion start`. It reads Dispatch's
  issue events and GitHub's pull request, review and CI events, decides which phase each issue is
  in, and starts and stops the agents that do the work. It keeps its own record in Postgres, so a
  restart picks up where it left off. It never writes code; the merge is the merger agent's request
  under the repository's rules.
- **Agents** are [Oh My Pi](https://github.com/sjawhar/oh-my-pi) sessions. Under the Kubernetes
  runtime every agent the daemon starts runs in a pod of its own, from one published worker image,
  and talks to the daemon over a connection the pod dials back. Under the tmux runtime a phase
  worker's role may run on Claude Code instead (`worker_backends` in `legion.yaml`); it does the same
  work through the same `legion` tool and keeps its conversation the same way.
- **[Dispatch](/legion/dispatch/)** is where people and agents meet: the issue, its spec, the
  questions an agent asks you, the approvals, the messages and the status everyone reads. Legion
  never reads or writes a GitHub issue.
- **GitHub** holds the code: one branch, `legion/<KEY>`, and one pull request per issue, in the
  issue's repository. A project spans every repository Dispatch maps to it (Settings →
  Repositories → Projects); each issue targets one of them, the one Dispatch resolves for it (its
  own, else its nearest ancestor's, else the project's default), which Legion records, with that
  repository's default branch as GitHub names it, when it first records the issue, and keeps.
  Legion acts there through two GitHub Apps, so the account that reviews a change is never the one
  that wrote it. The daemon creates a root's branch at the recorded default branch (`main`,
  `develop` or `release/v7.1` alike) when it admits the root, before its architect starts, and a
  child's when the child starts planning in its tree, so no agent's push is the one that creates a
  branch: GitHub can refuse that push on a large repository. An issue's workspace with no branch to
  start from starts at that default branch too. A pull request or push names its repository, and
  counts for an issue only in the issue's own.

## Several repositories

One Legion deployment can work issues across several GitHub repositories, under several owners,
with exactly one repository per issue — never two. A change that spans repositories is a parent
issue with one child per repository, each child's own `--repo` fixed when the daemon records it.
Every pane and pod holds a write token, minted from the role's GitHub App, for its own issue's
owner alone, and a read-only token for every other owner the project maps (`legion gh`, the `gh`
wrapper every pane and pod runs, picks the directory by the owner the command names); `git` reaches
the same bundle through a credential helper keyed the same way, so a push to another owner's
repository is refused there, not merely discouraged.

## The controller

The controller is the one Legion agent a person talks to directly. By default (`controller:
operator`) the operator starts it with `legion controller start` in a terminal on their own machine,
and it runs there as an interactive Oh My Pi session; closing that terminal leaves the project
without a controller until someone runs the command again. Under `controller: daemon` (Kubernetes
only) the daemon launches it as a pod in the cluster and relaunches it when it dies; a person
reaches it through Dispatch. The daemon wakes it when something needs a judgment no tree owns:

- a new `triage` issue labelled `legion` to admit, park, or leave alone;
- a tree its own architect escalated, or one whose architect ran out of launches.

The daemon also wakes it when an admission slot frees and on a timer, every
`controller_wake_interval_seconds` (an hour by default), and on its first turn of each UTC day the
controller posts a report on what Legion finished, closed and is running. A free slot is never a
reason for the controller to take an issue: it starts no work a person did not hand over.

No Legion agent files an issue for a problem it finds outside its own issue. It opens a Dispatch ask
on Legion's daily report issue instead, naming the issue it came from and where the fix belongs
(the repository or layer, and saying so when it is not Legion), with its evidence and a
recommendation, and you decide whether it needs an issue. The controller files that issue, with the
`legion` label only when your answer asks Legion to work it. The issues an agent still files on
its own are an architect's children of its own tree.

The only statuses the controller sets are `todo`, `backlog` and `icebox` (`triage` is set by
whoever files an issue). It reads pull requests as the review App, and never touches a tree's work.

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
6. accepts the issue once the reviewer approves it, reading the tester's and the implementer's evidence, or sends it back; the merge then closes it.

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
| `testing` | tester | Runs the changed surface for real, tries to break it, judges whether the tests would catch what breaks, and reads CI for everything static; reports `pass` or `fail`. | review App |
| `reviewing` | reviewer | Reviews the pull request and submits an approval of the head, or changes requested. | review App |
| `accepting` | the architect | Reads the tester's handoff, the pull request's two `E2E` lines and the reviewer's approval of the head, and accepts the issue (on to retro) or sends it back to the implementer or the tester. The daemon refuses the acceptance (`SIGNOFF_CHILD_IN_WORKFLOW`) while any issue beneath this one is still in the workflow, naming each of them. No worker runs here; the issue's Dispatch status stays `needs_review`. | review App |
| `retro` | implementer | Writes down what the work taught, as notes in the repository and one message on the issue, and brings up to date whatever the repository's instructions require the pull request description to say about the paths it changes, those notes included. | implement App |
| `merging` | merger | Sends `READY` for the current head; the daemon accepts it only on the approved head plus retro's notes, and submits the merge, which GitHub completes under the base branch's own rules. | implement App |
| `awaiting_merge` | none | Waits for the merge to land under the repository's rules: the daemon's submission, or a person's for a by-hand READY. The merge closes the issue; a root whose tree still runs records the merge and waits here until the last other issue of its tree is done or parked. | none |

A failed test, red CI on the head in testing, review, acceptance, retro or merging (a check or workflow the
base branch requires), or a review that requests changes sends the issue back to `implementing`,
and the change goes through the tester again before the reviewer sees it. Two reds are the
exception: one the tester planned, with its own failing tests, and red CI within the fix attempt
that ran out the project's `max_fix_attempts`, which waits for the architect
([review signalling](#review-signalling)). The review round and CI are two rules, each read on its
own: the reviewer's approval of the head ends the round whatever CI says at that commit, since the
reviewer reads the code commit's checks on GitHub before it approves, and a red the daemon reads
later sends the work back to the implementer from wherever the issue stands. Red CI on the head of
an issue in `awaiting_merge` sends it back too, at any fix-attempt count, when the head's own checks
or required workflow runs are red: the work returns through testing, review and `READY`, and the
daemon takes back the merge it submitted, since that submission names a commit about to be
replaced; the issue, and the project's merge-queue role when one is set, are told the `READY` is
withdrawn.

## Admission and the `legion` label

Legion works only the issues handed to it with the Dispatch label **`legion`** (in any case), so it
can share a Dispatch project with people and other agents. A root issue is admitted when it carries
the label, is in `todo`, and a slot is free. The number of slots is `admission_cap` in the daemon's
configuration, four by default; a labelled `todo` root that finds no free slot waits in line, in the
project board's order, and takes the next one.

An issue gets the label only by a person's decision:

- **A person hands it over**, from the issue header in the Dispatch dashboard.
- **A person asks the controller to**, in a message, a Slack request, or the answer to one of its
  asks, and the controller sets the label for them.

A free slot stays free until a person hands Legion an issue: the controller never takes one.

A child issue needs no label: it runs under its tree once the root is admitted. Taking the label
off a waiting root takes it out of line; taking it off a running tree does not stop the tree.

## The design gate

By default (`gates.design: root-issues`), no phase starts until a person approves the root issue's
spec. The architect settles the spec's decision blocks with you first, then asks for approval with a
summary of what the tree will do. You approve from the document's header or from the approval
question in your Dispatch Inbox; **Request changes** sends it back with your reason.

Approval is pinned to a version: a later edit to the spec closes the gate again until someone
approves the new version. Your own answer to a decision block in the approved spec is the one
exception: when that answer is the whole change since the version you approved, Dispatch records
your approval at the new version and the gate reopens, so the architect asks you for nothing; when
the answer means the spec's text changes too, the architect rewrites it and asks you to approve
again, naming what changed since your approval. Work already in flight carries on, but the merger's
`READY` waits for that approval. The daemon only reads approvals; nothing in Legion approves a spec
on a person's behalf, and the approval Dispatch records at your answer is yours, in your name. A
deployment that does not want the gate sets `gates.design: off`, and the tree starts as soon as the
architect has written its spec.

## Handoffs on the issue branch

Each phase ends by writing a **handoff**, a small JSON file committed to the issue's branch under
its own `.legion/<issue>/` directory: `architect.json`, `plan.json`, `implement.json`, `test.json`,
and `review.json`. The next phase reads the ones before it. A handoff is the copy that survives:
when a worker comes back after a restart, or a memory disagrees with a file, the committed handoff
wins. The merger writes none.

Each tree writes only under its own directory, so trees running at the same time never touch the
same file. Handoffs belong to the issue branch, not the default branch: nothing on the default
branch reads one, so retro's last commit removes the tree's `.legion/<issue>/` from the head that
merges, and the merger's `READY` is refused while that head still carries it. The merge
therefore brings no handoff onto the default branch, and no later branch starts with one. The
removal sits above the reviewer's approved head beside retro's notes, so it costs no test or review
round; the handoffs stay on the issue branch below it, where a worker still reads them. Those two
are all that may sit there: the daemon compares the approved head with the head `READY` names, on
GitHub, and refuses `READY` (`READY_UNREVIEWED_CHANGE`) when anything else changed since the
approval, so that change goes back to review.

## Review signalling

Legion uses GitHub's own review mechanisms rather than labels:

- The **reviewer** submits a GitHub review: an approval of the head by its commit, or changes
  requested. A plain comment decides nothing, and the architect is told the round is stuck. The
  daemon hears of a review from GitHub's webhook, and it also reads every open pull request's
  reviews with the implement App every two minutes, so a review whose webhook never arrived still
  counts within one read. Either way the newest deciding review counts, and counts once: a review
  the daemon already applied, by webhook or by read, decides nothing again. A review submitted while
  no round is open (the work is being implemented or tested) decides no later round when the daemon
  reads it before the next round opens. One it first hears of after that round opened can still
  decide it, as a webhook delivered late can.
- **CI** on the pull request's head counts: red checks while testing, reviewing, in retro or
  merging, or once the issue awaits its merge, send the issue back to the implementer, naming the
  failing checks. The tester, a reviewer whose round is not yet completed, or the merger has its
  turn interrupted; from retro the implementer keeps its turn and is handed the implementing task
  after it. The implementer pushes and completes without waiting for CI: a red that settled while
  it was still implementing sends the work back the moment it completes, so no tester round runs
  on code CI already found broken. Once the issue awaits its merge, only a red of the head's own
  counts: a red carried from the head a `.legion/`-only push replaced was already there when
  `READY` was accepted, and says nothing new about the commit the daemon's submission names. Only
  what the base branch requires counts, the set the daemon reads for itself: its rulesets'
  required status checks and required workflows, and its branch protection's required
  status checks. CI is red when one of those checks failed, or when the latest pull-request run of
  a required workflow on the head did not pass (it failed, was cancelled or timed out; a run that
  ended neutral or skipped passes). A required check that was cancelled, or that the head's checks
  settled without, leaves the head with no verdict until a later settlement decides it, since a
  settlement can come before an aggregator job is queued or just after a run is cancelled. The
  check may never run again: a head with no verdict is sent nowhere, and nothing holds `READY`
  back for it, since GitHub decides the merge under the base branch's own rules, where a required
  check that never reported is what keeps the merge waiting. A required workflow still running
  leaves no verdict until the daemon's next read of its runs, and one with no run on the head
  leaves none either. A failing check the base branch does not
  require, such as a lane started by hand or an advisory review check, never makes CI red, and on
  a base branch that requires nothing, nothing does: a base that requires no check and no workflow
  gives the head no verdict (`HeadVerdict` in `packages/daemon/internal/classify/required.go`), so
  nothing there sends work back to an implementer, and Legion submits a merge GitHub completes
  whatever the head's checks say. The daemon judges the check runs GitHub
  reports, so a required check that only a commit status reports never reports to it: the head
  has no verdict and is sent nowhere, and GitHub, which reads that status, holds the merge on it.
  Legion matches a required workflow to a run by its path and the repository that defines it, and
  a run belongs to the repository it ran for, so a required workflow another repository defines
  (an organization ruleset) never matches a run: the head never reads green, so Legion sends the
  work nowhere for it, and whether it holds the merge is GitHub's to say. A head that a push
  changing only
  `.legion/` made carries the checks and workflow runs of the head before it, except a workflow run
  cancelled there, which the new head's own run superseded: it leaves no verdict until that head's
  own CI settles, and a red carried that way is that code's red and sends the work back as the
  code head's own would. The daemon reads each open pull request's required set with the implement
  App as it starts and every two minutes after, with the required workflows' runs on the head;
  until a read succeeds, no verdict stands for the head, and a read GitHub refuses is logged with
  the repository and the HTTP status.
- **Review threads** close one by one, and only once the thread's opener (or, for a thread a bot
  opened, Legion's reviewer) accepts the reply.
- Two limits flag a loop: after `review_round_cap` rounds (three by default), or once
  `max_fix_attempts` pushes fail to turn CI green (three by default), the daemon posts a message on
  the issue and tells the architect; neither stops a worker. At the round cap the architect decides,
  in the turn the notice wakes, whether to narrow the next rounds, split work off (into a sibling
  child, or, on a root, an ask whether the rest needs an issue of its own), or let converging rounds
  run, and posts that decision on the issue. Every move back to an earlier phase counts a round: a
  tester's fail, a review's request for changes, a worker's move back, and the daemon's own move
  when CI turns red or the head starts conflicting with its base. Once `max_fix_attempts` runs out,
  CI red within the attempt that ran it out - the head whose push was counted, and every head pushed
  on top of it until a push lands on a head CI judged green or on a settled head whose base requires
  nothing - no longer sends the work back from testing, review, retro or merging by itself: the
  issue stays in its phase through every red in that attempt until the architect moves it, or a
  worker's own decision does (a tester's fail, a review's request for changes). A head pushed onto
  green CI, or onto a settled head whose base branch requires no check and no workflow, raises no
  count and leaves the attempt, so its red sends the work back as usual. Every other push stays in
  the attempt, whether it is counted or not: one onto a head CI has not judged yet, and one onto the
  held red that counts nothing, such as the tester's red tests or its handoff. The next push onto a
  red raises the count and is told again. A `READY` is withdrawn on red CI at any count (below).

## READY and the merge

When the reviewer has approved, the architect has accepted and retro is done, the merger sends **READY**. The daemon accepts
it only when the pull request's head is the approved one plus only retro's notes; it reads no check
of that head at all, since whether the merge happens is GitHub's to decide under the base branch's
own rules. It posts
the READY on the Dispatch issue as one message:

```text
READY #<pull request> at <head sha> (approved at <approved sha>) for <KEY> (<pull request url>)
```

followed by the pull request's outcome and its known risks. A project can also name a merge-queue
role (`merge_queue_role`) that gets the same packet. Its optional `merge.by_hand` regular-expression
paths keep matching pull requests out of auto-merge: the daemon accepts their `READY` and tells the
merger that a person merges it under the repository's rules, and the packet it posts on the issue,
and publishes to that role, starts with that `MANUAL_MERGE_REQUIRED:` note, its `Merge:` line
replaced, so nobody it reaches arms it. A by-hand READY refuses `READY_DISARM_PENDING` while an
earlier auto-merge disarm remains unfinished, preventing a stale ordinary arm from merging this
person-only head. Ordinary READY does not wait for the disarm. Otherwise, the moment the daemon
accepts READY it submits
the merge itself, as the implement App, with the `READY` head as GitHub's expected head: a base with
a merge queue enqueues it; otherwise a pull request GitHub already reports mergeable merges at once,
one in a repository that allows auto-merge is armed, and in a repository that allows neither the
daemon waits until GitHub reports the pull request mergeable — a required check passing, a required
review a person gives — and then merges it. An arm or a queue entry is not the end: the submission
keeps reading GitHub until GitHub reports the pull request merged at the `READY` head, and submits
again when the arm or entry is gone while the record still awaits that head, as when a disarm sent
before `READY` lands late. The repository's own required reviews and checks decide
when it merges, rules Legion neither reads nor changes nor bypasses, and a person may still merge it
first. The submission reads GitHub, then the record, when it runs, and submits nothing once the
record shows the issue left that `READY` or the pull request closed. While the record still awaits
the `READY` head, GitHub showing the pull request closed or at another head means one of them has
not caught up: the submission waits. It ends and tells the architect only once GitHub's last change
to the pull request is more than 10 minutes old, far longer than a webhook takes to arrive, however
long the submission waited before. A merge that simply waits ends nothing: when GitHub has not
completed one the daemon submitted within an hour of the workflow queueing it at `READY`'s
acceptance, the issue's architect is told once (`merge-waiting`) and the submission goes on.
Nothing is withdrawn and the issue stays in `awaiting_merge`, so what is left to find is on
GitHub: a required check that never ran or failed, a review only a person can give, a merge queue
that has not landed it.
A reopen at the `READY` head takes the submission back from a
run that read the record before the reopen, so it is never lost. A refusal GitHub would repeat on a
retry ends the submission and is told to the architect too. GitHub checks the expected head only when
auto-merge is enabled, so an arm outliving its `READY` would merge an unreviewed head. Arming is right
only for the `READY` head of an issue awaiting its merge, so the daemon queues a disarm on every
other push or reopen, when a child is set back to `todo`, and when an issue leaves the workflow. The
disarm reads GitHub, then the record: it keeps the arm only while the issue awaits its merge at its
`READY` head and that `READY` is not by hand, and otherwise disables auto-merge and takes the pull
request out of a merge queue, as the implement App. `READY` is
withdrawn when a push or reopen moves the head past the one `READY` named, when the head's own CI
turns red, or when it starts conflicting with its base: the
issue goes back to `implementing`, the architect is told, and the daemon disarms the pull request
and posts on the issue, and tells that role, that the `READY` is withdrawn, naming the cause.

## The production check

The merge closes the issue: once the pull request merges, the daemon writes `done`, and nothing is
left of the issue to develop. A root whose tree still runs is the one issue the merge does not
close at once: the daemon records the merge, the root waits in `awaiting_merge` with its workers
and its admission slot while its children reach their own merges, its architect is told once which
issues the close waits for, and the close comes when the last of them is done or parked. What is
left after the close is to see the change work in production, and that is
work after the close: the implementer — the agent that wrote the change — is handed a production
check in its live session as the issue closes. Each look of the check reaches the deploy as your
deployment instructions say (the merge commit published to your deploy role, with no reply
awaited), reads whether production carries the merge, and ends one of three ways: `pass`, once
the implementer drove the change in production through a user's own access path and wrote the
pull request's `Production:` line, naming the merge commit and what woke the look; `pending`,
when production does not carry the merge yet; or `fail`, a defect, which the implementer asks you
about in a Dispatch ask on Legion's daily report issue, naming the issue, the pull request, the
merge commit, what it drove and what failed, so you decide whether it needs an issue.
The record lives on the pull request alone, whether or not the issue is closed.

Nothing waits for the deploy: a `pending` look ends, the implementer's pod goes — and its disk
with it, since an issue's disk belongs to its pod — while its conversation is kept in the session
database, and the daemon wakes the check — a new pod comes back and
the same agent takes the next look — when Dispatch's delivery timeline reports the pull request
deployed (its `issue.deployed` event); when the project names the repository's deploy workflow
(`projects.<KEY>.deploys` in `legion.yaml`: the repository, the workflow file and a step such as
`Apply`), at each completed run of that workflow on the repository's default branch whose step
succeeded after the pull request merged, once per such job; or, for Legion's own repository, when
a Legion daemon boots or Dispatch starts on a build that contains the merge. A run of another
workflow or branch, a run a pull request triggered, a step that was skipped or failed, and a step
that finished before the merge wake nothing. A wake while a look is open is kept for the next look.
A look is bounded at two hours, and the whole check at `production_check_hours` (336, fourteen
days, by default): the wake at that bound is the last look, and a `pending` there closes the check
as not run, which the implementer writes on the pull request. `legion state` prints how many checks
are open and the oldest of them.

A child's close deletes its pod, and the disk goes with it; a root's close frees its admission slot
once no other issue of its tree is still in the workflow, and the finished tree **lingers** for
`linger_hours` (72 by default) in case work resumes, each member's conversation kept in the session
database; then its Sandboxes are deleted, once no member's check is open. Closing a root yourself
from the Dispatch dashboard lingers the tree at once, children and all: your decision outranks
that order. What a stop keeps is the conversation and whatever was pushed to the issue's branch: a
workspace lives as long as the pod it was provisioned in, and the next pod provisions it again
from that branch. Setting a done root back to `todo` runs it again as a new generation, starting
from its architect.

## Supervision

The daemon counts what goes wrong with each agent: launches that fail, deaths while it had work
outstanding, prompts it acknowledged without taking a turn, and the times it was relaunched for
that. When a budget runs out (`launch_failure_limit`, `prompt_failure_limit` and
`prompt_retire_limit` in the configuration) the daemon stops relaunching that agent and **holds**
the issue. A held phase worker is its architect's to retry or escalate to the controller; an
architect that runs out goes to the controller, which can re-admit the tree.
[Troubleshooting](/legion/legion/troubleshooting/) shows how to see each of these.

A launch failure that is charged is spaced from the next try by a backoff: 2 s after the first,
doubling after each further one, each wait drawn between half its step and the whole of it. A
launch the runtime refused is tried again after it, and so is a process that died or was retired at
its registration deadline, so the three tries of `launch_failure_limit`'s default span three
seconds at least, never the moment one fault lasts. The wait logs `supervise: waiting before the
next launch` with its length. The try it waits for is stored, with the charge, before the wait
begins, so a daemon stop during it forgives nothing; the next boot launches the claim again.
