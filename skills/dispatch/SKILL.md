---
name: dispatch
description: "Use before your first dispatch command in a session: a message, comment, ask, artifact, document edit, claim or status change."
---

# Dispatch

Dispatch holds each issue's spec, asks, comments and documents. It is a record for the people who decide, never a log of your work.

- Use the `dispatch` command; the HTTP API is only for what no command does. `dispatch <command> --help` gives each command's rules, limits and an example.
- Put long text on stdin: `--<field>-file -` with a quoted here-document (`<<'EOF'`). Give object and list fields as JSON.
- Exit 1 means Dispatch refused, 2 a usage error. A refusal lists each problem and what to fix: fix them all, then call once.
- File work only in a project `dispatch projects` lists, never one from the environment.
- `DOC_SERVICE_UNAVAILABLE` means the document is reloading and nothing was written. Wait a few seconds and repeat the call.

## Asks and ask blocks

- A question about a design a document records is an ask block at the end of the section it concerns, at each phase:

```md
:::ask{urgency="med"}
The problem and why it matters. Recommendation: the option and its reason.

- Option one: what it costs.
- Option two: what it costs.
:::
```

- A to-do or permission only a person can give, or a decision with no document, is a `dispatch ask`. A Legion phase worker sends its decision to its architect.
- An ask stands alone: its view does not show the issue's comments.
- An open ask is the request. Do not raise it again in each message.
- Change an open ask's block with `dispatch edit-ask`, or `doc-edit`'s `replace` or `move`. An answered ask is fixed: ask again in a new block.
- A person's reply to an open ask asks you to clarify: answer with `dispatch comment --reply-to-ask`. Use `--turn agent` for a note that needs nothing from them.
- When an answer's option and its typed text disagree, the text is the answer.
- An ask block has a block id, for `doc-edit`, and an ask id, for the ask commands. `dispatch read --ref dispatch://KEY/ask/<id>` takes either.
- An ask reaches the issue's assignee. On an unassigned issue, name who must answer.
- After `doc-edit` adds a block, read it back with `dispatch doc-read`.

## Choosing what to work on

- When you finish an issue, take the top ready issue: status `todo`, by priority and then board rank, in each project.
- `todo` means specced and waiting on no deploy or decision. An issue that waits goes in `backlog`, saying what it waits on.
- Hold at most three issues in flight. Past three, move unfinished work to `backlog`, saying where it is.
- Never reorder the board. When the top looks wrong, tell the project's order keeper: `notifications.role.legion-<project>-controller`.

## Issue status is yours to move

Outside Legion, claim your issue (`dispatch claim`) and move it and the children you own: `in_progress` at implementation, `testing` on a production-like surface, `needs_review` while its PR waits to merge, and `done` with `--reason` once you drove it in production. Never write the status of an issue with the `legion` label, or under one.

## Close what you opened

- An ask you opened is yours until it is answered or resolved. When the answer comes another way, or the design makes it moot, `dispatch resolve-ask` it in the same turn.
- Each later write names your open asks. Resolve each one no longer needed, and retract each one a new ask replaces.

## Structure over stream

- One issue per deliverable, with its content as the issue's document. Decisions are asks. A message only announces.
- A message replies to a person, or tells them a change they must know now. Never progress or status.
- `You've sent N messages …` means three with no reply. Wait, or ask once.
- Text a person must read to decide is a section of the spec. An artifact is a real file.

## Architecture components

Attach each architectural issue to the components it changes (`--components-json`), the root before you decompose it. Use `"mode":"none"` only for work that is not architectural, with a reason. Change code and `.dispatch/architecture/<id>.md` in the same review.

## References

Link each issue, ask, comment, document or message you name:

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

A bare id or filename is not a reference. An ask or comment id can be a unique prefix of 8 hex characters or more.

## Setup and the HTTP API

- Use a personal token from Dispatch **Settings → Agent tokens**, as `DISPATCH_TOKEN` or `dispatch.token` in the session's `envoy.json`.
- `GET /api/v1` lists every route. Never guess a path. Branch on an error's `code`, never its text.
- No command reorders the board: `PATCH /api/v1/issues/{key}` with `{"rank": {"before": "<key>", "after": "<key>"}, "actor": {"kind": "session", "id": "<your session id>"}}`.
