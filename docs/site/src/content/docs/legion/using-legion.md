---
title: Using Legion
description: Hand an issue to Legion, answer its questions and approve its spec, review and merge its pull request, and read what each Dispatch status means.
sidebar:
  order: 3
---

This page is for the person whose issue Legion works: you write the issue, answer the architect's
questions, approve the spec, and merge the pull request. Everything happens in
[Dispatch](/legion/dispatch/) and on GitHub; you never need the `legion` command.

## Before you start

- **The issue must be in a Dispatch project a Legion daemon runs.** Each daemon runs the projects its
  operator configured; ask your operator which ones.
- **Assign the issue to the person who will answer for it.** The assignee's Inbox gets the
  architect's questions and the spec approval for the whole tree. An unassigned issue still runs, but
  its questions wait in every Inbox's Unassigned band until someone picks the issue up.
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

You do not have to hand work over yourself. When a slot is free, the controller takes the
highest-priority `todo` issue that has no children and that nobody else is working, adds the
`legion` label, and posts a comment saying it took the issue, who will be asked to approve the
spec, and how to undo it. An issue that is claimed by someone, linked to a pull request, or still
being designed (an open question, a spec awaiting approval) is left alone.

Do not label child issues: a child runs under its tree once the root is admitted.

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
- **Production**: written by the implementer after the merge.

Legion's reviewer posts its review as the review App. A review you submit on GitHub while the issue
is in `needs_review` counts the same way: **Request changes** sends the work back to the
implementer with your comments, and an approval of the head can end the round. A plain comment
moves nothing. The pull request also carries a `.legion/` directory of handoff files; the operator
removes it from the default branch after the merge.

## Merge

When the change is tested, reviewed, approved and has passed every check the repository requires,
the merger sends **READY**, which appears on the Dispatch issue as one message:

```text
READY #<pull request> at <head sha> (approved at <approved sha>) for <KEY> (<pull request url>)
```

followed by the pull request's outcome and its known risks. That is your cue: read it, and merge the
pull request on GitHub as you would any other, under the repository's branch protection and
code-owner rules. Legion never merges, and nothing in Legion approves a pull request on your behalf
or changes who may merge.

After the merge, the implementer checks the change in production and records what it saw on the
pull request and the issue. The architect then signs off with a comment summarizing the evidence,
and the issue moves to `done`. The tree's workspace is kept for three days (`linger_hours`, 72 by
default) in case the work resumes.

## Stop, park or rerun

- **To stop Legion working an issue**, move it to `backlog` (or `icebox`) from the Dispatch
  dashboard. A person's move takes the tree out at once: its agents stop and its slot goes to the
  next issue. To keep Legion off it for good, also take the `legion` label off; taking the label off
  alone does not stop a tree that has started.
- **To run it again**, set it back to `todo` with the label on. The tree starts over as a new
  generation, from its architect.
- **To close it without a change**, tell the architect at the design gate that no change is needed;
  it closes the issue with that reason. You can also close the issue yourself from the dashboard.

A status written by an agent session on a running issue is set back by the daemon, and the
architect is told who wrote it; only a person's move in the dashboard, or the controller's and
operator's `legion status`, takes a running tree out.

## What each status means

| Status | Set by | What it means for a Legion issue |
| --- | --- | --- |
| `triage` | whoever files the issue | New. With the `legion` label, the controller decides whether to admit it now, park it, or leave it to you. |
| `icebox` | a person or the controller | Parked for the long term. Legion does not run it. |
| `backlog` | a person or the controller | Should not run now: it waits on a decision or a deploy. Legion does not run it. |
| `todo` | a person or the controller | Ready. A labelled root is admitted when a slot is free and waits in line until then. A child in `todo` runs under its tree. |
| `in_progress` | the daemon | Admitted: the architect is writing the spec or waiting for your approval, or the planner or implementer is working. Work sent back by a failed test, red CI or a review returns here. |
| `testing` | the daemon | The implementer opened the pull request and finished; the tester is checking the change. |
| `needs_review` | the daemon | The tester passed; Legion's reviewer is reviewing. Your GitHub review counts now. |
| `retro` | the daemon | The reviewer approved. The issue stays here through retro, `READY`, your merge, and the production check. |
| `done` | the daemon at sign-off, or a person | Finished. The tree lingers, then closes. |
