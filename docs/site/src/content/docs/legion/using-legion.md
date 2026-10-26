---
title: Using Legion
description: Hand an issue to Legion, answer its questions and approve its spec, review and merge its pull request, and read what each Dispatch status means.
sidebar:
  order: 3
---

This page is for the person whose issue Legion works: you write the issue, answer the architect's
questions, approve the spec, and review the pull request. Everything happens in
[Dispatch](/legion/dispatch/) and on GitHub; you never need the `legion` command.

## Before you start

- **The issue must be in a Dispatch project a Legion daemon runs.** Each daemon runs the projects its
  operator configured; ask your operator which ones.
- **Assign the issue to the person who will answer for it.** The assignee's Inbox gets the
  architect's questions and the spec approval for the whole tree. An issue an agent files without
  one is not always unassigned: it takes its parent's assignee, else the project's default
  assignee where a person set one in Settings. An issue that ends up unassigned still runs, but
  its questions reach nobody's Inbox list and nobody's **Needs you** count until someone takes
  it: they are found through the Inbox's **Assignee** filter, under **Unassigned**, or on the
  issue's own page.
- **Write the outcome, not the steps.** A title that says what someone can do afterwards, and a spec
  with the problem, the acceptance criteria and how you would check them, give the architect what it
  needs. It extends your spec rather than replacing it, and it asks about anything it cannot decide.
  Child issues you have already created are adopted as part of the work.

## Hand an issue to Legion

1. Open the issue in Dispatch and add the label **`legion`** from the issue header.
2. Set its status to **`todo`**.

That is the whole handover. When a slot is free the daemon admits the issue and the status changes
to `in_progress`; when every slot is taken, the issue waits in `todo` and takes the next free slot
in the board's order. A labelled issue in `triage` goes to the controller first, which admits it,
parks it, or leaves it for you.

The controller never takes an issue on its own, whatever slots are free: Legion works what a person
hands it, and the clear defects its own agents report. You can also ask the controller to hand an
issue over for you, in a message or in Slack. An agent that finds a defect outside its own issue
whose fix needs no choice of yours fixes it in its own tree's pull request when that tree touches
or depends on the code. Otherwise the controller gives it to the tree that owns the code, and files it labelled
`legion` only when no tree does. When the problem
needs a decision, the agent asks you in a Dispatch ask whether to file one, rather than filing it.
Answer with `File it for Legion` and the controller files it labelled `legion`.

Do not label child issues: a child runs under its tree once the root is admitted.

## Ask Legion in Slack

In a Slack channel your operator connected to the project, mention the Legion app (`@Legion`) and
the project's controller answers in that thread:

- A question about what Legion is doing ("what are you working on?", "where is ACME-12?") is
  answered from the daemon's state and Dispatch.
- A request for work is filed as a Dispatch issue in the project, and the reply names it with its
  link. The issue carries the `legion` label, so Legion takes it, only when you ask Legion to do the
  work; otherwise it waits for a person to hand it over.
- A question an issue's architect owns is passed to that architect, and the reply says so; its
  answer comes on the issue.

The controller answers only the channels the operator listed, and not while it is stopped: a
mention made then gets no reply, so ask again once it runs.

## Answer the questions and approve the spec

Shortly after admission the architect claims the issue (the issue shows **Claimed by** Legion's
session), then grows your spec into a design: how the work splits, how each outcome is proven, and
the decisions it needs from you. Each open question is a **decision block** in the spec, with
options and a recommendation, and it reaches your Inbox.

1. Answer each decision block. The architect reads every answer and revises the spec.
2. When nothing is open, the architect asks you to approve the spec, with a short summary of what
   the work will do that you have not already agreed to. Approve it from the approval question in
   your Inbox or with **Approve** in the document's header.
3. If it is not right, choose **Request changes** and say why. The architect revises the spec and
   asks again.

No code is written until you approve. The approval is for that version of the spec: if the spec
changes afterwards, you are asked again, and Legion will not send `READY` for the change until you
approve the new version. (A deployment can turn the design gate off with `gates.design: off`; then
the work starts as soon as the spec is written, and nobody is asked.)

If a phase gets stuck, or the architect hits something only a person can decide, it asks you the
same way: a question in your Inbox that says what is blocked, the options, and what it recommends.

## Follow the work

The issue's status tells you which phase is running (see [What each status means](#what-each-status-means)).
The issue's events show each status change, and the architect's messages there say what changed
and why. The pull request appears under the issue's external links, and its body carries the line
`Dispatch: <KEY>` pointing back to the issue.

## Review the pull request

You can read the pull request at any time. Legion's implementer writes its description from a fixed
template, and the later phases fill in their own lines:

- **Outcome** and **Why**: what changes for a user, and the issue it answers.
- **Proven by** and **E2E (implementer)** / **E2E (tester)**: the command or run that exercised the
  change on a production-like surface, what it showed, the commit it ran at, and one negative
  control.
- **Look at first** and **Not proven / risk**: written by Legion's reviewer, naming where a wrong
  decision would hurt and every claim left unproven.
- **Production**: written by the implementer after the merge closes the issue, once your deploy carries the change; `pending deploy of <sha>` until then.
- Any line your repository's own instructions derive from the paths the change touches, such as a
  checklist named for each kind of path: after the approval, the retro recomputes it for the whole
  change, its notes included, the way those instructions say, before it pushes the notes.

Legion's reviewer posts its review as the review App. A review from anyone with write access to the
repository, submitted on GitHub while the issue is in `needs_review`, counts the same way:
**Request changes** sends the work back to the implementer with your comments, and an approval of
the head can end the round. A review from an account without write access decides nothing, since
any GitHub account can review a public repository's pull request, and a plain comment moves
nothing. The approved head also carries the tree's handoff files under `.legion/<issue>/`. Retro's
last commit removes them, so the head that merges carries none and the merge brings no handoff onto
the default branch; the pull request's file list shows them only until that commit.

## Merge

When the change is tested, reviewed and approved, the merger sends **READY**. The daemon accepts it
on the approved head plus retro's notes, reading no check of that head, and posts it on the
Dispatch issue as one message:

```text
READY #<pull request> at <head sha> (approved at <approved sha>) for <KEY> (<pull request url>)
```

followed by the pull request's outcome and its known risks. The daemon then submits the merge
itself, and the repository's branch protection and code-owner rules decide when it lands: a review
only you can give is waited for. A pull request the project's `merge.by_hand` paths match is
merged by a person instead, and its READY says so. Nothing in Legion approves a pull request on
your behalf or changes who may merge.

The merge closes the issue: it moves to `done` the moment GitHub reports the pull request merged.
A root with work still running beneath it is the exception: its merge is recorded and the issue
waits until the last other issue of its tree is done or parked, so no child's finished work is cut
off, and then it closes. The architect accepted the change before that, when the reviewer approved
it, with a comment on the issue summarizing the evidence; Legion refuses that acceptance while any
issue beneath it is still running, so a parent is accepted after its children. After the close the
implementer checks the change in production, once your deploy carries it, and records what it saw
on the pull request's `Production:` line; a closed issue takes no further write, so look there.
The tree's workspace is kept for three days (`linger_hours`, 72 by default) in case the work
resumes.

## Stop, park or rerun

- **To stop Legion working an issue**, move it to `backlog` (or `icebox`) from the Dispatch
  dashboard. A person's move takes the tree out at once and its slot goes to the next issue; its
  agents stop, each once the turn it is in ends, however long that takes, so a turn already under
  way finishes what it was doing. To keep Legion off it for good, also take the `legion` label off;
  taking the label off alone does not stop a tree that has started.
- **To run it again**, set it back to `todo` with the label on. The tree starts over as a new
  generation, from its architect.
- **To close it without a change**, tell the architect at the design gate that no change is needed;
  it closes the issue with that reason. You can also close the issue yourself from the dashboard.

A status written by an agent session on a running issue is set back by the daemon, and the
architect is told who wrote it; only a person's move in the dashboard, the controller's
`set_status`, or an operator's `legion status`, takes a running tree out.

## What each status means

| Status | Set by | What it means for a Legion issue |
| --- | --- | --- |
| `triage` | whoever files the issue | New. With the `legion` label, the controller decides whether to admit it now, park it, or leave it to you. |
| `icebox` | a person or the controller | Parked for the long term. Legion does not run it. |
| `backlog` | a person or the controller | Should not run now: it waits on a decision or a deploy. Legion does not run it. |
| `todo` | a person or the controller | Ready. A labelled root is admitted when a slot is free and waits in line until then. A child in `todo` runs under its tree. |
| `in_progress` | the daemon | Admitted: the architect is writing the spec or waiting for your approval, or the planner or implementer is working. Work sent back by a failed test, red CI or a review returns here. |
| `testing` | the daemon | The implementer opened the pull request and finished; the tester is checking the change. |
| `needs_review` | the daemon | The tester passed; Legion's reviewer is reviewing, and once it approves the architect reads the evidence and accepts the change or sends it back. Your GitHub review counts now. |
| `retro` | the daemon | The architect accepted. The issue stays here through retro, `READY` and the wait for the merge — and, for a root whose tree still runs, through the wait between its merge and its close. |
| `done` | the daemon at the merge, or a person | Finished: the pull request merged. A root whose tree is still running waits in `retro` until the last of it is done or parked, then closes. The implementer's production check follows on the pull request; the tree lingers, then closes. |
