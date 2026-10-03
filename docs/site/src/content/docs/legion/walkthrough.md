---
title: Walkthrough
description: One issue going through Legion from handover to production check, as you see it in Dispatch, on GitHub and in the operator's terminal.
sidebar:
  order: 6
---

This page follows one issue through Legion, step by step, and each step names what you see. The
screenshots, the narrated video and the two terminal recordings show example data: a Storefront
project in Dispatch and a Legion daemon running on one machine.

<video controls preload="metadata" poster="/legion/media/videos/legion-issue-journey.jpg" style="width: 100%" aria-label="Walkthrough: handing an issue to Legion and getting it back ready to merge">
  <source src="/legion/media/videos/legion-issue-journey.mp4" type="video/mp4">
  <track kind="captions" src="/legion/media/videos/legion-issue-journey.vtt" srclang="en" label="English" default>
</video>

## 1. Hand the issue over

The issue has a title that states the outcome and a spec with acceptance criteria. You add the
`legion` label from the issue header and set the status to `todo`.

![An issue handed to Legion: the label picker open on its header with legion selected, and the status Todo.](/legion/media/legion/handover.png)

## 2. Admission and the architect's claim

With a slot free, the daemon admits the issue: the status becomes `in_progress`, and moments later
the architect claims it, so the issue shows it as claimed by Legion's session.

![The issue after admission: status In progress, claimed by the issue's architect, with the claim in its Conversation.](/legion/media/legion/admitted.png)

## 3. Decisions and the spec approval

The architect grows the spec into a design and writes each open question as a decision block, which
reaches the assignee's Inbox. Once every block is answered, it asks for approval with a summary.

![The spec's design with an open decision block: the architect's question, its recommendation, and two options to answer.](/legion/media/legion/decision-block.png)

![The architect's request to approve the spec in the Inbox, with its summary and the Approve and Request changes options.](/legion/media/legion/spec-approval.png)

## 4. The phases run

After approval the daemon starts the planner, then the implementer, which opens the pull request,
then the tester and the reviewer. The issue's status follows: `in_progress`, `testing`,
`needs_review`, then `retro` once the reviewer approves.

![The issue's Conversation as the phases run: the daemon's status changes, newest first, with the issue now in Retro and its pull request linked.](/legion/media/legion/status-events.png)

## 5. The pull request

The pull request's body follows Legion's template: the outcome, why, the change, how it was
proven, what is not proven, and later the production check. Legion's reviewer approves it as the
review App. In Dispatch, the issue's header links the pull request.

![The issue header with the pull request Legion opened, linked as #42.](/legion/media/legion/pull-request.png)

## 6. READY, and your merge

The merger checks the approved head and the required checks, and the daemon posts `READY` on the
Dispatch issue. You merge the pull request on GitHub.

![The READY message on the issue: the pull request, its head and the approved commit, then the outcome and the risk.](/legion/media/legion/ready.png)

## 7. The production check and sign-off

After the merge the implementer checks the change in production and records it on the pull request
and the issue. The architect signs off with a comment summarizing the evidence, and the issue moves
to `done`.

![The issue closed as Done, with the implementer's production record and the architect's sign-off.](/legion/media/legion/signed-off.png)

## The operator's view

The same journey from the operator's terminal: the daemon's state and the agents it runs, the
controller starting, and the controller's daily report.

<video controls preload="metadata" poster="/legion/media/videos/legion-state.jpg" style="width: 100%" aria-label="Terminal recording: legion status, legion state and legion claims list on a running daemon">
  <source src="/legion/media/videos/legion-state.mp4" type="video/mp4">
  <track kind="captions" src="/legion/media/videos/legion-state.vtt" srclang="en" label="English" default>
</video>

<video controls preload="metadata" poster="/legion/media/videos/legion-controller.jpg" style="width: 100%" aria-label="Terminal recording: legion controller start, and the controller registered with the daemon">
  <source src="/legion/media/videos/legion-controller.mp4" type="video/mp4">
  <track kind="captions" src="/legion/media/videos/legion-controller.vtt" srclang="en" label="English" default>
</video>

![The project's Legion daily report issue, parked in Icebox, with the controller's report for the day: the issue Legion finished and its pull request, nothing running, and the free slots.](/legion/media/legion/daily-report.png)
