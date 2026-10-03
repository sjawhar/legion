---
title: Walkthrough
description: One issue going through Legion from handover to production check, as you see it in Dispatch, on GitHub and in the operator's terminal.
sidebar:
  order: 6
---

This page follows one issue through Legion, step by step. Each step names what you see; the
screenshots, the two terminal recordings and the narrated video are recorded by the site's media
scripts, which replace each `TODO(media)` marker below.

<!-- TODO(media) video legion-issue-journey
Narrated walkthrough, about three minutes, recorded against the disposable Dispatch project and
smoke repository, example data only: an issue labelled `legion` and set to `todo`; admission and the
architect's claim; the architect's decision block and the spec approval in the Inbox; the status
moving through in_progress, testing, needs_review and retro; the pull request and its body on
GitHub; the READY message; the merge; the production check and the architect's sign-off; done.
Narration follows the steps on this page. No browser chrome or address bar in frame. -->

:::note[Video to come]
A narrated video of the whole journey replaces this note.
:::

## 1. Hand the issue over

The issue has a title that states the outcome and a spec with acceptance criteria. You add the
`legion` label from the issue header and set the status to `todo`.

<!-- TODO(media) screenshot legion-handover
Dispatch issue page, example project: the issue header with the label picker open and `legion`
selected, status `todo`. -->

## 2. Admission and the architect's claim

With a slot free, the daemon admits the issue: the status becomes `in_progress`, and moments later
the architect claims it, so the issue shows it as claimed by Legion's session.

<!-- TODO(media) screenshot legion-admitted
The same issue after admission: status `in_progress`, the claim line naming the architect's
session, and the status change in the issue's events. -->

## 3. Decisions and the spec approval

The architect grows the spec into a design and writes each open question as a decision block, which
reaches the assignee's Inbox. Once every block is answered, it asks for approval with a summary.

<!-- TODO(media) screenshot legion-decision-block
The spec document with one decision block: its question, options and the architect's
recommendation. -->

<!-- TODO(media) screenshot legion-spec-approval
The Inbox's approval question for the spec, with Approve and Request changes, and the architect's
summary. -->

## 4. The phases run

After approval the daemon starts the planner, then the implementer, which opens the pull request,
then the tester and the reviewer. The issue's status follows: `in_progress`, `testing`,
`needs_review`, then `retro` once the reviewer approves.

<!-- TODO(media) screenshot legion-status-events
The issue's events showing the daemon's status changes from `in_progress` to `testing`,
`needs_review` and `retro`, and the pull request under the issue's external links. -->

## 5. The pull request

The pull request's body follows Legion's template: the outcome, why, the change, how it was
proven, what is not proven, and later the production check. Legion's reviewer approves it as the
review App.

<!-- TODO(media) screenshot legion-pull-request
The smoke repository's pull request: the body's Outcome, Proven by and E2E lines, and the review
App's approval of the head. -->

## 6. READY, and your merge

The merger checks the approved head and the required checks, and the daemon posts `READY` on the
Dispatch issue. You merge the pull request on GitHub.

<!-- TODO(media) screenshot legion-ready
The READY message on the Dispatch issue: the first line with the pull request, head and approved
commits, then the outcome and risks. -->

## 7. The production check and sign-off

After the merge the implementer checks the change in production and records it on the pull request
and the issue. The architect signs off with a comment summarizing the evidence, and the issue moves
to `done`.

<!-- TODO(media) screenshot legion-signed-off
The issue in `done`, with the implementer's production record and the architect's sign-off
comment. -->

## The operator's view

The same journey from the operator's terminal: the daemon's state while the tree runs, and the
controller's first turn.

<!-- TODO(media) cast legion-state
Terminal recording, narrated: `legion status <PROJECT>`, `legion state --config legion.yaml`, and
`legion state --config legion.yaml --json | jq '.admission, .issues["<KEY>"].phase'` while a tree
runs. Example project and placeholder hosts only. -->

<!-- TODO(media) cast legion-controller
Terminal recording, narrated: `legion controller start --config controller.yaml`, the controller's
first turn (its start procedure, filling a free slot), and a message typed to it. -->

<!-- TODO(media) screenshot legion-daily-report
The project's `Legion daily report` issue, in icebox, with one day's report message. -->

:::note[Recordings to come]
Two terminal recordings, of `legion state` and of the controller starting, replace this note.
:::
