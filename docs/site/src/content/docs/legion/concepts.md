---
title: Concepts
description: The controller, the architect, the phase workers, admission and the legion label, the design gate, handoffs, review, the architect's acceptance, the merge that closes the issue and the production check after it.
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
reason for the controller to take an issue: it takes only what a person hands over and the clear
defects Legion's agents report.

No Legion agent files an issue for a problem it finds outside its own issue. A defect with a clear
fix in code the agent's own tree touches or depends on is fixed in that tree's pull request, while
the agent is there. Any other goes to the controller with its evidence, and the controller gives it
to the running tree whose code it is. Only when no tree owns that code does it file the issue with
the `legion` label, so Legion works it without asking you. A problem that needs a decision of
yours goes into a Dispatch ask on
Legion's daily report issue instead, naming the issue it came from and where the fix belongs (the
repository or layer, and saying so when it is not Legion), with its evidence and a recommendation,
and you decide whether it needs an issue. The controller files that issue, with the `legion` label
only when your answer asks Legion to work it. An architect also files the children of its own
tree.

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

The daemon starts each phase's worker itself, one phase at a time, in the order below. A role stays
live and idle between its assignments until its issue leaves the workflow, so its next task reaches
the same session.

| Phase | Worker | What it does | GitHub identity |
| --- | --- | --- | --- |
| `awaiting_person` | none | The root returned its admission slot while its live architect waits for and reads a person's answer to a question on the root. | none |
| `planning` | planner | Writes the implementation plan into its handoff and the issue's document. | review App |
| `implementing` | implementer | Writes the change, proves it works, pushes `legion/<KEY>` and opens the pull request. | implement App |
| `testing` | tester | Runs the changed surface for real, tries to break it, judges whether the tests would catch what breaks, and reads CI for everything static; reports `pass` or `fail`. | review App |
| `reviewing` | reviewer | Reviews the pull request and submits an approval of the head, or changes requested. | review App |
| `accepting` | the architect | Reads the tester's handoff, the pull request's two `E2E` lines and the reviewer's approval of the head, and accepts the issue (on to retro) or sends it back to the implementer or the tester. Issues still running beneath it do not hold the acceptance: a root's close waits for them instead. No worker runs here; the issue's Dispatch status stays `needs_review`. | review App |
| `retro` | implementer | Writes down what the work taught, as notes in the repository and one message on the issue, and brings up to date whatever the repository's instructions require the pull request description to say about the paths it changes, those notes included. | implement App |
| `merging` | merger | Reads the pull request and merges it itself, at the head it read and under a message it writes, which GitHub completes under the base branch's own rules. The merge closes the issue; a root whose tree still runs records the merge and stays here until the last other issue of its tree is done or parked. | implement App |

A failed test, a review that requests changes, or a head that starts conflicting with its base
sends the issue back to `implementing` from testing, review, acceptance, retro or merging,
and the change goes through the tester again before the reviewer sees it. Red CI sends nothing
back from anywhere: the review round and CI are two rules, each read on its own, and the
reviewer's approval of the head ends the round whatever CI says at that commit, since the
reviewer reads the code commit's checks on GitHub before it approves
([review signalling](#review-signalling)). What a red check does is reach the tree's architect,
the worker of the issue's current phase and the implementer, as one message naming each check and
its conclusion; and hold the merge, where GitHub applies the base branch's own required checks.

## Admission and the `legion` label

Legion works only the issues handed to it with the Dispatch label **`legion`** (in any case), so it
can share a Dispatch project with people and other agents. A root issue is admitted when it carries
the label, is in `todo`, and a slot is free. The number of slots is `admission_cap` in the daemon's
configuration, four by default; a labelled `todo` root that finds no free slot waits in line, in the
project board's order, and takes the next one.

An issue gets the label in one of three ways:

- **A person hands it over**, from the issue header in the Dispatch dashboard.
- **A person asks the controller to**, in a message, a Slack request, or the answer to one of its
  asks, and the controller sets the label for them.
- **A Legion agent reports a clear defect** no running tree owns, one whose fix needs no choice of
  yours, and the controller files it with the label.

A free slot stays free until an issue gets the label one of these ways: the controller never takes
one on its own.

A child issue needs no label: it runs under its tree once the root is admitted. Taking the label
off a waiting root takes it out of line; taking it off a running tree does not stop the tree.

A root whose architect asked you a question and now waits only on your answer gives its slot back
rather than holding the project up: once nothing else of its tree runs and the architect has taken
the task it was last given, the root moves to `awaiting_person` and the slot goes to the next root
in line at once. Its architect stays live and reads your comments and replies as they arrive. Its
tree stays live for its children, and `waitingOn` in `legion state` names the question and when the
wait began. Your act on any ask of the root answers it, whichever ask the wait names: your answer,
resolution, or reply that hands an open ask back to the agent; so does your approval or changes
request of the root's spec, and so does the spec's design gate standing open. The wait also ends
when nothing is asked of you any more: Dispatch retracts the question because its decision block
left the spec, a session closes it, or a session hands it back to the agent's turn. The answered
root goes to the front of the waiting line, ahead of every root that never ran, and the next free
slot gives the same architect session a task naming what answered it and what is still with you. An
agent's answer or resolution of another ask answers nothing, and an act of yours from before the
wait began is one the architect already read. Taking the label off, or a person or an outside session
claiming the root on Dispatch, takes it out of the workflow instead: Legion never resumes work
another owner took. A root whose architect requested approval of its spec and registered that version waits the same way, on the design gate, until the person reviews the spec or a newer version hands the request back to the architect. The slot
a waiting root returns is no reason for the controller to label or admit anything: a root you
handed to Legion takes it through the waiting line.

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
again, naming what changed since your approval. Work already in flight carries on, through its
merge: the gate holds a tree's start, and nothing after it. The daemon only reads approvals;
nothing in Legion approves a spec on a person's behalf, and the approval Dispatch records at your answer is yours, in your name. A
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
merges, and the merger reads the head for that directory itself and sends one that still carries
it back to retro rather than merging it. The merge
therefore brings no handoff onto the default branch, and no later branch starts with one. The
removal sits above the reviewer's approved head beside retro's notes, so it costs no test or review
round; the handoffs stay on the issue branch below it, where a worker still reads them.

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
  decide it, as a webhook delivered late can. An approval of a head below the pull request's head
  still counts when everything above it changed only the issue's own `.legion/<issue>/`: the
  daemon learns that from each push's webhook, and the same two-minute read also asks GitHub to
  compare the approved head with the head while the approval does not count, so a push webhook
  lost or late, or an approved head the record never held, sets nothing aside. Any other change
  above the approved head, another issue's `.legion/` included, or a head not built on it, sets the
  approval aside, and only an approval of the head itself or a request for changes ends the round;
  an approval of the head itself ends it as soon as the record reaches that head.
- **CI** on the pull request's head is information, not a verdict. When GitHub's checks settle, the
  daemon names every check GitHub reported, with the conclusion GitHub gave it, and judges none of
  them: it sends one message each to the tree's architect, the worker of the issue's current phase
  and the implementer whose code the checks ran on, and moves the issue nowhere. A red is for
  those roles to decide on — re-run a check that failed on itself, fix code that is at fault, or
  wait — and for GitHub to hold the merge over, under the base branch's own required checks, which
  Legion neither reads nor re-implements. The reviewer reads the head's checks on GitHub itself
  before it approves, and the architect before it accepts; those reads are the ones that decide
  anything. Two settlements of the same head that name the same checks tell nobody twice, and a
  lingering tree is told nothing at all.
- **A head that conflicts with its base** does move the issue. GitHub computes no merge ref for
  such a head and runs no check on it, so no CI result would ever arrive: the daemon takes the
  issue back to `implementing` from `testing`, `reviewing`, `accepting`, `retro` or `merging`,
  starts the implementer with the base to merge forward, and tells the architect.
- **An implementer that completes with no pull request** is refused (`HANDOFF_NO_PULL_REQUEST`)
  until one is open on its branch, and the daemon's read of that issue tells its architect
  `pull-request-missing`, once per handoff commit, so an issue never waits in `implementing`
  unnoticed.
- **Review threads** close one by one, and only once the thread's opener (or, for a thread a bot
  opened, Legion's reviewer) accepts the reply.
- One limit flags a loop: after `review_round_cap` rounds (three by default) the daemon posts a
  message on the issue and tells the architect; it stops no worker. At the round cap the architect
  decides, in the turn the notice wakes, whether to narrow the next rounds, split work off (into a
  sibling child, or, on a root, an ask whether the rest needs an issue of its own), or let
  converging rounds run, and posts that decision on the issue. Every move back to an earlier phase
  counts a round: a tester's fail, a review's request for changes, a worker's move back, and the
  daemon's own move when the head starts conflicting with its base.

## The merge

When the reviewer has approved, the architect has accepted and retro is done, the merger reads the
pull request and merges it itself, as the implement App and at the head it read, which it names as
the head GitHub must match (`gh pr merge <n> --squash --match-head-commit <head>`). On a base with
a merge queue it enqueues the pull request instead, with GraphQL `enqueuePullRequest` and the same
head as `expectedHeadOid`, and watches the pull request until GitHub merges it or takes it out of
the queue. Merging at a named head is how bors and Prow's Tide merge only what they tested: a push
that lands under the merger is refused instead of merged unseen.

The merge carries a message the merger writes itself,
`--subject '<pull request title> (#<n>)' --body ''`. GitHub's own default
(`squash_merge_commit_message` of `COMMIT_MESSAGES`) joins every commit message of the branch into
the squash commit, which would put the tree's Dispatch keys, its `Omp-Session:` trailers and every
`skip-checks: true` line on your default branch. `enqueuePullRequest` can carry no message, so on a
merge-queue base whose repository setting still joins them the merger enqueues nothing and tells
the architect: only a person can change that setting, to `PR_BODY` or `BLANK`.

The merger never turns auto-merge on, and the daemon submits no merge and changes no merge setting
of any pull request. The repository's own required reviews and checks decide when it lands; Legion
reads none of them at the merge, bypasses none, and waits for a review only a person can give.
Nothing holds the head that merges to the head the reviewer approved: a repository that wants its
newest push reviewed says so in its own rulesets. A head that still carries `.legion/<issue>/` is
one retro did not finish, and the merger sends it back to retro rather than merging it.

When a merge fails, the decision is the merger's, on GitHub's own answer: wait for what GitHub
waits for, merge again at the head it re-reads, send the issue back with `request_backward_move`
when the head itself must change, or tell the architect what no worker can do — a repository
setting or an App permission only a person can change. A head that starts conflicting with its
base sends the issue back to `implementing` from `merging` as it does from any later phase.

What closes the issue is the daemon's own reading of the merged pull request, by GitHub's webhook
or by its own read: it writes `done`, whether the merger's completion arrived before that or after
it.

## The production check

The merge closes the issue: the daemon reads the merged pull request, writes `done`, and nothing is
left of the issue to develop. A root whose tree still runs is the one issue the merge does not
close at once: the daemon records the merge, the root stays in `merging` with its workers
and its admission slot while its children reach their own merges, its architect is told once which
issues the close waits for, and the close comes when the last of them is done or parked. What is
left after the close is to see the change work in production, and that is
work after the close: the implementer — the agent that wrote the change — is handed a production
check in its live session as the issue closes. Each look of the check reaches the deploy as your
deployment instructions say (the merge commit published to your deploy role, with no reply
awaited), reads whether production carries the merge, and ends one of three ways: `pass`, once
the implementer drove the change in production through a user's own access path and wrote the
pull request's `Production:` line, naming the merge commit and what woke the look; `pending`,
when production does not carry the merge yet; or `fail`, a defect, naming the issue, the pull
request, the merge commit, what it drove and what failed. A defect with a clear fix goes to the
controller, which gives it to the tree that owns that code and files an issue only when none does.
One that needs a decision is a Dispatch ask on Legion's
daily report issue, so you decide whether it needs an issue.
The record lives on the pull request alone: Dispatch refuses a write on a closed issue.

Nothing waits for the deploy: a `pending` look ends, the implementer's pod goes — and its disk
with it, since an issue's disk belongs to its pod — while its conversation is kept in the session
database, and the daemon wakes the check — a new pod comes back and
the same agent takes the next look — when Dispatch's delivery timeline reports the pull request
deployed (its `issue.deployed` event); when the project names the repository's deploy workflow
(`projects.<KEY>.deploys` in `legion.yaml`: the repository, the workflow file and a step such as
`Apply`), at each completed run of that workflow on the repository's default branch whose step
succeeded after the pull request merged, once per such job; or, for Legion's own repository, when
a Legion daemon boots on a build that contains the merge, or Dispatch announces that every one of
its server processes serves such a build. A run of another
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
