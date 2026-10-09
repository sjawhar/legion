---
title: Dispatch for agents
description: How a coding agent uses Dispatch - the dispatch command, when to ask, comment or message, documents, and whose turn it is.
sidebar:
  order: 6
---

An agent works in Dispatch through one shell command, `dispatch`. The Envoy plugin for the agent's
host (Oh My Pi, Claude Code, or OpenCode) puts it on the agent's `PATH` and tells it which session
it acts for. `dispatch --help` lists the commands, `dispatch <command> --help` gives a command's
flags and an example, and `--dry-run` prints what a command would send without sending it. The
`dispatch` skill in this repository, `skills/dispatch/SKILL.md`, teaches the workflow. The
[command reference](/legion/dispatch/reference/tools/) lists every command's flags.

Dispatch is a record for the people who decide, not a log of the agent's work. Progress belongs in
the agent's own transcript and its pull request. Anything a person must read, answer, or approve
goes through a `dispatch` command.

## Connecting an agent

1. A person signs in to Dispatch and opens **Settings → Agent tokens**.
2. They type a label and choose **New token**. The token is shown once; copy it then.
3. They give it to the agent, either in `~/.config/opencode/envoy.json`:

   ```json
   {
     "dispatch": {
       "enabled": true,
       "serverUrl": "https://dispatch.internal.example",
       "token": "<token>"
     }
   }
   ```

   or as the environment variables `DISPATCH_URL` and `DISPATCH_TOKEN`.

Everything the agent writes is attributed to the person who made the token: Dispatch shows the
agent's name followed by `(for <login>)`. `dispatch whoami` tells the agent who Dispatch takes it
for.

## The commands

| Area | Commands |
| --- | --- |
| Issues | `dispatch issue` creates one. `dispatch issue-update` changes status, title, labels, priority, parent, links, or components. `dispatch claim` claims an issue before work starts. `dispatch issues` lists a project's issues. |
| Asks | `dispatch ask` opens one. `dispatch edit-ask` rewords an open one. `dispatch resolve-ask` retracts or resolves one. `dispatch follow` follows or leaves an ask's thread. `dispatch open-asks` lists open asks. |
| Comments | `dispatch comment` comments on a document or replies in a thread. `dispatch suggest` proposes replacement text. `dispatch resolve-comment` resolves a thread. |
| Messages | `dispatch message` posts a message or replies to one. |
| Documents | `dispatch doc-read` reads a document, or the text of an uploaded file. `dispatch doc-edit` edits one in place. `dispatch artifact` uploads a file or a new document. `dispatch request-approval` asks a person to approve a document. |
| Reading | `dispatch read` reads an issue, ask, comment, message, or document summary. `dispatch search` searches everything. `dispatch whoami` reports who Dispatch takes the agent for. |
| Architecture | `dispatch architecture-sync` imports a project's architecture model now. |

A command with several problems is refused once, with every problem listed, so the next run can
fix them all. It exits 0 when it wrote or read, 1 when Dispatch or the flags refused it, and 2 on a
usage error such as an unknown command.

Long text, such as a message body or a document, goes on stdin through a quoted here-document, so
the shell expands nothing in it:

```sh
dispatch message --issue LEGION-1 --body-file - <<'EOF'
Shipped in owner/repo#7.
EOF
```

## Ask, comment, suggest, or message

Each kind of write reaches people differently. Pick by what you need from them.

| You need | Use | Where it lands |
| --- | --- | --- |
| A decision only a person can make | An ask (`dispatch ask`, or a decision block in the spec) | The assignee's Inbox, under **Waiting on you** |
| To point at a passage in a document | A comment with a quote (`dispatch comment`) | The document's margin and the issue's Conversation |
| To change specific text | A suggestion (`dispatch suggest`) | The margin, where a person accepts or rejects it |
| To tell people a deliverable landed, or answer a person's message | A message (`dispatch message`) | The issue's Conversation |

Never post progress, "starting work", or status on a timer as a message. If you are blocked on a
person, that is an ask, and nothing else reaches their Inbox.

The server holds every write to a size, and refuses rather than truncates:

- an ask's question is at most 800 characters, with at most eight options;
- a comment or message body is at most 2,000 characters;
- a Markdown document is at most 1 MiB, and any other file at most 25 MiB.

To show a picture inline, pass its local path with `--image` on `dispatch message`,
`dispatch comment` or `dispatch ask` (PNG, JPEG, GIF or WebP, at most 25 MiB each), once per
picture. The command uploads it to the issue — to the project for a comment or ask on a project
document, and to the agent's own conversation for a reply to a direct message — and appends one
`![<file name>](dispatch://…@vN)` line per picture, which counts toward the caps above.
`dispatch doc-read` and `dispatch read` write the pictures they return to files and print each
one's path; `dispatch read` returns the pictures the messages, asks and comments it shows embed,
newest first, at most eight and 10 MiB per read. A picture over 3,750,000 bytes (5 MB once
base64-encoded, the model providers' bound), or of another type, is described rather than shown. A
session is shown each picture once: a later `dispatch read`, or an Inbox delivery on Oh My Pi,
names a picture the session was already shown instead of sending it again, because every request
carries the session's history and Anthropic refuses one over 32 MB. `dispatch doc-read` always
shows the picture, which is how an agent gets one back after compaction.

Write every ask for a person reading on a phone who has not read the code. Put the problem, what
constrains the answer, and your recommendation in the question, and what each option costs in its
description.

## Documents

Every issue has one spec, its main document. Read it with `dispatch doc-read` and change it in
place with `dispatch doc-edit`. Do not upload a file named `spec.md` to change it: that replaces the
whole spec with your text. Use `dispatch artifact` for real files, such as a report, an image, or a
draft to send as-is.

A question the spec needs a person to decide is a **decision block**: an `:::ask` block at the end
of the section that discusses it.

```md
:::ask{urgency="high" multiple="false"}
Should we ship the migration this week?

- Ship: Release the verified change now.
- Hold: Wait for another review.
:::
```

The block becomes an ask in the person's Inbox, and their answer lands next to its context.

When every decision block is settled and the spec proposes something the person has not already
agreed to, run `dispatch request-approval` with a short `--summary` of what is new. The person
approves that version, or requests changes with a reason. Any later version makes the approval
stale.

## Whose turn it is

Every open ask is waiting on a person or on an agent, and the Inbox files it by that.

- Opening an ask puts it in the person's **Waiting on you**.
- When the person replies without answering (**Ask back**), the turn is yours. Answer in the same
  thread with `dispatch comment --reply-to-ask`, or reword the question with
  `dispatch edit-ask`. Either puts the ask back in front of them. Do not open a second ask.
- A progress note on an ask, such as "checking the release branch, back shortly", goes with
  `--turn agent`. The ask stays under **Waiting on agents** and the person is not asked to act.

Close what you opened. When an ask's answer arrives some other way, or the question no longer
matters, resolve it with `dispatch resolve-ask`, giving the reason.

## What reaches you

You follow every ask you open or reply to. Its answer, edits, resolution, and replies reach you
directly. Nothing else on an issue reaches you unless you subscribe to the issue's Envoy topic,
`notifications.dispatch.issue.<KEY>.>`. Each write's result names that topic.

A person can also message you from the Agents page:

- On Oh My Pi, their **Send** or **Aside** arrives as their own message in your conversation.
  Answer it there, as you would anything typed at your terminal.
- A **BTW**, a broadcast, or any message to a host that takes no such turn (Claude Code) arrives as
  a Dispatch notice with a ready-made reply command. Answer with `dispatch message` and its
  `--in-reply-to`.

## Claiming and status

Before you implement an issue, claim it with `dispatch claim`, and release the claim when you
stop. A refusal that names another holder means someone else is working on it; do not work it in
parallel. Move the issue's status as the work moves, with `dispatch issue-update`. When an issue,
or an issue above it, carries the `legion` label, Legion moves its status, and an agent never
writes it.

## References

Link everything you mention. Dispatch reads `dispatch://` references in every document, ask,
comment, and message, and records what points where:

```text
dispatch://KEY
dispatch://KEY/spec
dispatch://KEY/artifact/<slug>
dispatch://KEY/ask/<id>
dispatch://KEY/comment/<id>
dispatch://KEY/message/<id>
dispatch://PROJECT/artifact/<document>
```

`dispatch read` on any of them shows what it is and everything that references it.

## The HTTP API

The `dispatch` commands cover everyday work. For anything else, the [HTTP API reference](/legion/dispatch/reference/api/)
lists every route, and so does `GET /api/v1` on the server, with no credential: each route's
method, who may call it, and what it does. An agent calls the API with its token as
`Authorization: Bearer <token>`.
