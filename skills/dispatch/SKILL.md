---
name: dispatch
description: "Use before your first dispatch command in a session: a message, comment, ask, artifact, document edit, claim or status change."
---

# Dispatch

Dispatch is a record for the people who decide, never a log of your work.

- Use the `dispatch` command; the HTTP API only for what no command does. `dispatch <command> --help` gives its rules and an example.
- Put long text on stdin: `--<field>-file -` with a quoted here-document (`<<'EOF'`). Give object and list fields as JSON.
- Exit 1 is a refusal, 2 a usage error. A refusal lists each problem: fix them all, then call once.

## Asks and ask blocks

- A design question a document records is an ask block (a decision block) at the end of its section, at each phase:

```md
:::ask{urgency="med"}
Which machines, files or people, the numbers, then "A happens to X, or B to Y?" Recommendation: the option and its reason.

- Option one: what it costs.
- Option two: what it costs.
:::
```

- In Legion, fix a clear defect in code your tree uses, hand others to the controller, and ask about decisions (`skill://legion-worker/references/findings.md`).
- An open ask is the request. Do not repeat it in each message.
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

Outside Legion, claim your issue (`dispatch claim`) and move it and your children: `in_progress` at implementation, `testing` on a production-like surface, `needs_review` while its PR waits, `done` with `--reason` once driven in production. An issue with the `legion` label, or under one: never write its status or route it, and claim it only as a Legion role of its tree, even when its claim reads `· not running`, as a suspended architect's does. An outside claim can take the tree out of the workflow; a status write is set back and reported.

A claim, or `dispatch issue-update --route session:<your id>` (`--route ''` on release), brings a person's answers, comments and messages; for agents' activity, `envoy_subscribe notifications.dispatch.issue.<KEY>.>`, which shows no event Dispatch marks quiet.

## Close what you opened

- An ask you opened is yours until answered or resolved. `dispatch resolve-ask` it that turn once anything makes it needless: another answer, a later write, work dropped.
- Retract an ask a new one replaces.

## Structure over stream

- One issue per deliverable, its content the issue's document.
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
dispatch://KEY/comment/<id>#b-<block>[@vN]
dispatch://KEY/message/<id>
dispatch://KEY/message/<id>#b-<block>
dispatch://PROJECT/artifact/<slug>[@vN]
dispatch://PROJECT/artifact/<slug>/ask/<id>
dispatch://PROJECT/artifact/<slug>/comment/<id>#b-<block>[@vN]
```

A bare id, filename or local path is not a reference, and outside Dispatch neither is `dispatch://`: give the `https://` link `dispatch` printed. Write each id whole, from `dispatch read`'s `Ask id:` or the page URL: the command takes a prefix, a person's link does not.

## Setup and the HTTP API

- Use a personal token from Dispatch **Settings → Agent tokens**, as `DISPATCH_TOKEN` or `dispatch.token` in the session's `envoy.json`.
- `GET /api/v1` lists every route; never guess one. Branch on an error's `code`, not its text.
