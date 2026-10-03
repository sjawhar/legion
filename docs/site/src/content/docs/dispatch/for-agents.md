---
title: Dispatch for agents
description: How a coding agent uses Dispatch - its tools, when to ask, comment or message, documents, and whose turn it is.
sidebar:
  order: 6
---

An agent works in Dispatch through a set of `dispatch_*` tools. The Envoy plugin for the agent's
host (Oh My Pi, Claude Code, or OpenCode) adds them once Dispatch is configured. The `dispatch`
skill in this repository, `skills/dispatch/SKILL.md`, teaches the workflow. The
[tool reference](/legion/dispatch/reference/tools/) lists every tool's arguments.

Dispatch is a record for the people who decide, not a log of the agent's work. Progress belongs in
the agent's own transcript and its pull request. Anything a person must read, answer, or approve
goes through a tool.

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
agent's name followed by `(for <login>)`. `dispatch_whoami` tells the agent who Dispatch takes it
for.

## The tools

| Area | Tools |
| --- | --- |
| Issues | `dispatch_issue` creates one. `dispatch_issue_update` changes status, title, labels, priority, parent, links, or components. `dispatch_claim` claims an issue before work starts. `dispatch_issues` lists a project's issues. |
| Asks | `dispatch_ask` opens one. `dispatch_edit_ask` rewords an open one. `dispatch_resolve_ask` retracts or resolves one. `dispatch_follow` follows or leaves an ask's thread. `dispatch_open_asks` lists open asks. |
| Comments | `dispatch_comment` comments on a document or replies in a thread. `dispatch_suggest` proposes replacement text. `dispatch_resolve_comment` resolves a thread. |
| Messages | `dispatch_message` posts a message or replies to one. |
| Documents | `dispatch_doc_read` reads a document. `dispatch_doc_edit` edits one in place. `dispatch_artifact` uploads a file or a new document. `dispatch_request_approval` asks a person to approve a document. |
| Reading | `dispatch_read` reads an issue, ask, comment, message, or document summary. `dispatch_search` searches everything. `dispatch_whoami` reports who Dispatch takes the agent for. |
| Architecture | `dispatch_architecture_sync` imports a project's architecture model now. |

A tool call with several problems is refused once, with every problem listed, so the next call can
fix them all.

## Ask, comment, suggest, or message

Each kind of write reaches people differently. Pick by what you need from them.

| You need | Use | Where it lands |
| --- | --- | --- |
| A decision only a person can make | An ask (`dispatch_ask`, or a decision block in the spec) | The assignee's Inbox, under **Waiting on you** |
| To point at a passage in a document | A comment with a quote (`dispatch_comment`) | The document's margin and the issue's Conversation |
| To change specific text | A suggestion (`dispatch_suggest`) | The margin, where a person accepts or rejects it |
| To tell people a deliverable landed, or answer a person's message | A message (`dispatch_message`) | The issue's Conversation |

Never post progress, "starting work", or status on a timer as a message. If you are blocked on a
person, that is an ask, and nothing else reaches their Inbox.

The server holds every write to a size, and refuses rather than truncates:

- an ask's question is at most 800 characters, with at most eight options;
- a comment or message body is at most 2,000 characters;
- a Markdown document is at most 1 MiB, and any other file at most 25 MiB.

Write every ask for a person reading on a phone who has not read the code. Put the problem, what
constrains the answer, and your recommendation in the question, and what each option costs in its
description.

## Documents

Every issue has one spec, its main document. Read it with `dispatch_doc_read` and change it in
place with `dispatch_doc_edit`. Do not upload a file named `spec.md` to change it: that replaces the
whole spec with your text. Use `dispatch_artifact` for real files, such as a report, an image, or a
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
agreed to, call `dispatch_request_approval` with a short summary of what is new. The person
approves that version, or requests changes with a reason. Any later version makes the approval
stale.

## Whose turn it is

Every open ask is waiting on a person or on an agent, and the Inbox files it by that.

- Opening an ask puts it in the person's **Waiting on you**.
- When the person replies without answering (**Ask back**), the turn is yours. Answer in the same
  thread with `dispatch_comment` and `reply_to_ask`, or reword the question with
  `dispatch_edit_ask`. Either puts the ask back in front of them. Do not open a second ask.
- A progress note on an ask, such as "checking the release branch, back shortly", goes with
  `turn: "agent"`. The ask stays under **Waiting on agents** and the person is not asked to act.

Close what you opened. When an ask's answer arrives some other way, or the question no longer
matters, resolve it with `dispatch_resolve_ask`, giving the reason.

## What reaches you

You follow every ask you open or reply to. Its answer, edits, resolution, and replies reach you
directly. Nothing else on an issue reaches you unless you subscribe to the issue's Envoy topic,
`notifications.dispatch.issue.<KEY>.>`. Each write's result names that topic.

A person can also message you from the Agents page:

- On Oh My Pi, their **Send** or **Aside** arrives as their own message in your conversation.
  Answer it there, as you would anything typed at your terminal.
- A **BTW**, a broadcast, or any message to a host that takes no such turn (Claude Code) arrives as
  a Dispatch notice with a ready-made reply call. Answer with `dispatch_message` and its
  `in_reply_to`.

## Claiming and status

Before you implement an issue, claim it with `dispatch_claim`, and release the claim when you
stop. A refusal that names another holder means someone else is working on it; do not work it in
parallel. Move the issue's status as the work moves, with `dispatch_issue_update`. When an issue,
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

`dispatch_read` on any of them shows what it is and everything that references it.

## The HTTP API

The tools cover everyday work. For anything else, the [HTTP API reference](/legion/dispatch/reference/api/)
lists every route, and so does `GET /api/v1` on the server, with no credential: each route's
method, who may call it, and what it does. An agent calls the API with its token as
`Authorization: Bearer <token>`.
