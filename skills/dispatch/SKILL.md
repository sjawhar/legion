---
name: dispatch
description: "Use before your first dispatch command in a session: a message, comment, ask, artifact, document edit, claim or status change."
---

# Dispatch

Dispatch is a record for the people who decide, never a log of your work.

- Use the `dispatch` command; the HTTP API only for what no command does. `dispatch <command> --help` gives its rules and an example.
- Put long text on stdin: `--<field>-file -` with a quoted here-document (`<<'EOF'`). Give object and list fields as JSON.
- Exit 1 is a refusal, 2 a usage error. A refusal lists each problem: fix them all, then call once.
- `DOC_SERVICE_UNAVAILABLE`: the document is reloading, nothing was written; retry in a few seconds.

## Asks and ask blocks

- A design question a document records is an ask block (a decision block) at the end of its section, at each phase:

```md
:::ask{urgency="med"}
The problem and why it matters. Recommendation: the option and its reason.

- Option one: what it costs.
- Option two: what it costs.
:::
```

- A to-do or permission only a person can give, or a decision with no document, is a `dispatch ask`. A Legion phase worker sends its decision to its architect.
- In Legion, file no issue for an outside finding. Fix a clear defect in code your tree uses. Hand others to the controller. Ask about decisions (`skill://legion-worker/references/findings.md`).
- An open ask is the request. Do not raise it again in each message.
- Change an open ask's block with `dispatch edit-ask`, or `doc-edit`'s `replace` or `move`. An answered ask is fixed: ask anew.
- Answer a person's reply to an open ask with `dispatch comment --reply-to-ask`; `--turn agent` for a note that needs nothing from them.
- When an answer's option and text disagree, the text wins.
- An ask block has a block id, for `doc-edit`, and an ask id, for the ask commands. `dispatch read --ref dispatch://KEY/ask/<id>` takes either.
- An ask reaches the issue's assignee. On an unassigned issue, name who must answer.

## Choosing what to work on

- When you finish an issue, take the top ready one: `todo`, by priority then board rank, per project.
- `todo` means specced and waiting on nothing; one that waits goes in `backlog`, saying on what.
- Hold at most three issues in flight; move the rest to `backlog`, saying where each is.
- Never reorder the board: one agent keeps each project's order (`issue-update --before`/`--after`). If the top looks wrong, tell `notifications.role.legion-<project>-controller`.

## Issue status is yours to move

Outside Legion, claim your issue (`dispatch claim`) and move it and your children: `in_progress` at implementation, `testing` on a production-like surface, `needs_review` while its PR waits, `done` with `--reason` once driven in production. An issue with the `legion` label, or under one: never write its status, and claim it only as a Legion role of its tree, even when its claim reads `· not running`, as a suspended architect's does: taking it takes the tree out of the workflow.

A claim delivers no comments: that turn, `dispatch issue-update --route session:<your id>` (`--route ''` on release) or `envoy_subscribe notifications.dispatch.issue.<KEY>.>`. Never route such an issue.

## Close what you opened

- An ask you opened is yours until answered or resolved. If it is answered another way or made moot, `dispatch resolve-ask` it that turn.
- Resolve each open ask a later write names that is no longer needed; retract one a new ask replaces.

## Structure over stream

- One issue per deliverable, its content the issue's document. Decisions are asks. A message only announces.
- A message replies to a person or tells them what they must know now.
- Text a person must read to decide belongs in the spec. An artifact is a real file.

## Architecture components

Attach each architectural issue to its components (`--components-json`), the root before decomposing it. Change code and `.dispatch/architecture/<id>.md` in the same review.

## References

Link each item you name:

```text
dispatch://KEY
dispatch://KEY/spec
dispatch://KEY/artifact/<slug>[@vN]
dispatch://KEY/ask/<id>
dispatch://KEY/comment/<id>
dispatch://KEY/message/<id>
dispatch://PROJECT/artifact/<slug>[@vN]
dispatch://PROJECT/artifact/<slug>/ask/<id>
```

A bare id or filename is not a reference. An ask or comment id can be an 8-hex prefix.

## Setup and the HTTP API

- Use a personal token from Dispatch **Settings → Agent tokens**, as `DISPATCH_TOKEN` or `dispatch.token` in the session's `envoy.json`.
- `GET /api/v1` lists every route; never guess one. Branch on an error's `code`, not its text.
