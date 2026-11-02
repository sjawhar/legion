---
title: Dispatch for agents
description: How a coding agent uses Dispatch - the dispatch command, when to ask, comment or message, documents, and whose turn it is.
sidebar:
  order: 6
---

An agent works in Dispatch through one shell command, `dispatch`. The Envoy plugin for the
agent's host (Oh My Pi or Claude Code) puts it on the agent's `PATH` and tells it which session
it acts for. `dispatch --help` lists the commands, `dispatch <command> --help` gives a command's
flags and an example, and `--dry-run` checks a command's arguments as the call would and prints
them as given (before the call fills in an issue from `LEGION_ISSUE` or a ref), sending nothing.
The `dispatch` skill in this repository, `skills/dispatch/SKILL.md`, teaches the workflow. The
[command reference](/legion/dispatch/reference/tools/) lists every command's flags.

Dispatch is a record for the people who decide, not a log of the agent's work. Progress belongs in
the agent's own transcript and its pull request. Anything a person must read, answer, or approve
goes through a `dispatch` command.

## Connecting an agent

1. A person signs in to Dispatch and opens **Settings → Agent tokens**.
2. They type a label and choose **New token**. The token is shown once; copy it then.
3. They give it to the agent in the `dispatch` section of its `envoy.json`:

   ```json
   {
     "dispatch": {
       "enabled": true,
       "serverUrl": "https://dispatch.internal.example",
       "token": "<token>"
     }
   }
   ```

   A session under an Oh My Pi profile other than `default` or a Claude Code `CLAUDE_CONFIG_DIR`
   reads the `envoy.json` in that directory and no other. Any other session reads the file
   `ENVOY_CONFIG_FILE` names when it is set, else `~/.config/legion/envoy.json`
   (`$XDG_CONFIG_HOME/legion/envoy.json` when that is set). The `dispatch` command reads the
   file its session's plugin chose, which the plugin hands it in `ENVOY_CONFIG_FILE`; in a
   Claude Code shell that gets nothing the plugin hands on, as a subagent's can, it still reads
   the session's `CLAUDE_CONFIG_DIR`. A token written as `!<command>`, such as
   `!secrets get DISPATCH_TOKEN`, is a command, and what it prints is the token.

   The environment variables `DISPATCH_URL` and `DISPATCH_TOKEN` override the file's other fields,
   but a `dispatch.enabled: false` in the file keeps Dispatch off. The file's token goes only to
   the server written beside it, so a `DISPATCH_URL` naming another server needs `DISPATCH_TOKEN`
   as well. An agent that reaches Dispatch at an address people cannot open, such as a Legion pod's
   in-cluster one, also needs `DISPATCH_PUBLIC_URL`, Dispatch's dashboard as people open it: every
   link the `dispatch` command prints points there, and a pasted dashboard link on either address
   resolves.

   The same file holds the agent's other two sections: `envoy`, the listener its session connects
   to, and `askgate`, the switch for the review each Dispatch write of a main session gets before
   it posts. AskGate is on in every session that loads the Oh My Pi plugin; this turns it off for
   the session that reads this file:

   ```json
   { "askgate": { "enabled": false } }
   ```

   Only that file switches it. A checkout's `.envoy/envoy.json` can carry an `askgate` section,
   and nothing reads it; the session says so in its start notice.
   [AskGate](/legion/dispatch/for-agents/#askgate-reviews-each-write) says what it reviews.

Everything the agent writes is attributed to the person who made the token: Dispatch shows the
agent's name followed by `(for <login>)`, and an issue the agent creates without naming an
assignee goes to that person. An agent on the shared token has no such person, so its issue takes
its parent's assignee instead, and where no parent names one, the project's default assignee, set
on the Settings page. `dispatch issue` names the assignee Dispatch chose. `dispatch whoami`
tells the agent who Dispatch takes it for.

## The commands

| Area | Commands |
| --- | --- |
| Issues | `dispatch issue` creates one; `--blocked-by <key>`, repeated, names up to 20 issues in the same project it waits on. `dispatch issue-update` changes status, title, labels, priority, parent, links, blockers, or components; its `--blocked-by` replaces the list, and `--clear-blocked-by` empties it; `--before <KEY>` and `--after <KEY>` move it on its project's board, above that issue, below it, or between the two. `dispatch claim` claims an issue before work starts. `dispatch issues` lists a project's issues. |
| Projects | `dispatch project` creates a project from a key and a name; a key that is already taken is refused. `dispatch projects` lists every project's key and name. |
| Asks | `dispatch ask` opens one. `dispatch edit-ask` rewords an open one. `dispatch resolve-ask` retracts or resolves one. `dispatch follow` follows or leaves an ask's thread. `dispatch open-asks` lists open asks. `dispatch answers` lists, in full and newest first, every answer, comment and message a person wrote to the session in the last 72 hours. |
| Comments | `dispatch comment` comments on a document or replies in a thread. `dispatch suggest` proposes replacement text. `dispatch resolve-comment` resolves a thread. |
| Messages | `dispatch message` posts a message or replies to one. |
| Documents | `dispatch doc-read` reads a document, or the text of an uploaded file; with `--blocks` it lists the document's block ids, tokens and table rows that `dispatch doc-edit` addresses. `dispatch doc-edit` edits one in place. `dispatch artifact` uploads a file or a new document. `dispatch request-approval` asks a person to approve a document. |
| Reading | `dispatch read` reads an issue, ask, comment, message, or document summary; an issue read lists the issue's last 10 events, `--events N` the last N, and `--all-events` every one. `dispatch search` searches everything. `dispatch related --project <KEY>` runs the related-item check a new issue or ask gets on a `--title` and `--text`, and writes nothing. `dispatch whoami` reports who Dispatch takes the agent for. |
| Handoffs | `dispatch handoff-write` records a phase handoff on an issue as the agent's session: `--phase <phase>` and `--body-file <path>` (`-` reads stdin), one JSON object stored exactly as written, each write the phase's next version. `dispatch handoff-read` prints one line naming the phase, version, id, writing session and time, then the body exactly as stored; `--version <n>` reads an earlier version. |
| Architecture | `dispatch architecture-sync` imports a project's architecture model now, from every source repository with a line for each, or from one with `--repo <owner/name>`. |

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

## AskGate reviews each write

In an Oh My Pi session with the Envoy plugin, one model call reviews each Dispatch write of the
main agent before it posts: `dispatch ask`, `message`, `edit-ask`, `comment`, `doc-edit`,
`request-approval`, an `issue` with a spec, and an `artifact` that uploads a markdown document.
Every other command posts unreviewed. A review that sends the write back carries the reason and
the fix; in the default mode the write does not post until the agent corrects it.

AskGate reads one form of the call, so that the bytes it reviews are the bytes that post:

```
[cd <literal path> && ] dispatch <subcommand> <literal words…> [2>&1] [<<'DELIM' … DELIM] [| head|tail [-n N]]
```

Every word is literal, and nothing else is in the call but one trailing `head` or `tail` stage that
reads its output. A quoted word may run over several lines, and a backslash at the end of a line
continues the command. Text from a file goes in a `--<field>-file <path>` flag or in one
here-document whose delimiter is quoted. A call in any other form, or a write another command would
run (`bash -c '…'`, `eval`, a function), runs as written: AskGate does not read it. The `dispatch`
command then finds no reviewed marker for the write. In the default warn mode it posts the write
with a `dispatch: warning:` line and records it as unreviewed; in block mode (`OMP_ASKGATE=block`)
it refuses the write and says which form to send. Send the write alone, in the form above, so that
it is reviewed.

`{ "askgate": { "enabled": false } }` in the session's own `envoy.json` turns the review off.

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

Every model request of an Oh My Pi session that reaches Dispatch, its `task` subagents' included,
ends with a list of what people wrote to that session in the last 72 hours: answers to its asks,
replies on its asks and in its comment threads, comments on its documents and on issues routed to
it, mentions, and messages to it, each word for word, newest first, under 8,000 characters. A
Claude Code session gets the same list at its start, resume and compaction and when a subagent
starts. When the list leaves older items out, its last line says how many; `dispatch answers`
reads them all. The request waits at most 800 ms for the list and carries the last one read when
Dispatch does not answer.

The server holds every write to a size, and refuses rather than truncates:

- an ask's question is at most 2,000 characters, with at most eight options;
- a comment or message body is at most 32,000 characters;
- a comment anchored to a document is also projected into that document's margin, which is bounded
  in bytes and not by that character limit: 256 KiB per thread, and 1 MiB per document margin. A
  few long anchored comments therefore fill a thread or a margin where many short ones used to
  fit, and a comment or suggestion past either bound is refused `413 CAP_EXCEEDED`; a document
  keeps every comment it has had, so one whose margin is full takes more once it is split;
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
approves that version, or requests changes with a reason. A later version leaves the approval
stale, with one exception: when the person's own answer to a decision block is the whole change
since the version they approved, Dispatch records the approval in their name at the new version, so
request nothing for it. When their answer changes text outside the block, rewrite that text and
request approval again with a one-sentence `--summary` naming what changed since the earlier
approval; the card shows that approval and a **Show diff** control that carries the detail.

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
directly. A person's reply in a comment thread you wrote in reaches you directly. A person's
comment on a document you wrote reaches you directly too: a new comment on a passage, on the whole
document, or as a suggestion, and a reply in a thread on the document. A new comment on your ask
or document that mentions you arrives once, as the mention, when the mention reached you; when the
mention's send fails, the comment still reaches you directly.

While you hold an issue's claim, a person's answers, comments and messages on that issue reach you
directly too: their answer, hand-back or resolution of an ask, their new, edited, resolved or
reopened comment, and their message. A new comment that mentions you arrives once, as the mention,
when the mention reached you; when the mention's send fails, the comment still reaches you
directly. A message a person sends to one session reaches only that session. Dispatch reads the
claim when it sends the event, so whoever holds the claim then receives it. An event that also
reaches you as a follower, an author or the issue's route arrives once.

When an issue has no route and no session holds its claim, a person's comment on it, and its edit,
reach every session that wrote the issue's spec, by the same mention rule.

Two exceptions. When a mention's receipt times out, or its send is stranded for over a minute,
Dispatch records it as failed though it may have reached you, so the comment can arrive twice:
once as the mention and once directly. Treat the second copy as the same comment. And while a new
comment waits on its mention's send, its edit, resolution or reopening is not held back, so it can
reach you before the comment itself; that is not a bug, and the comment follows.

Every other event on the issue reaches you only if you subscribe to the issue's Envoy topic,
`notifications.dispatch.issue.<KEY>.>`: an agent's events, and a person's other changes, such as
an issue update, a new document version, or opening or rewording an ask. A person's changes also
reach the issue's route, when that is your session. Each write's result names that topic.

If your connection to Envoy drops, or your session stops and is resumed, what was sent to you
directly, what was sent to a role you hold, and what was published on an issue topic you follow in
the meantime still arrives once you are back, each message once and in the order it was
published, for up to 72 hours. A person's BTW that arrives late this way starts no side turn, so
answer it with `dispatch message` and its `--in-reply-to` if it still matters. Where your listener
cannot keep a consumer for your session (its NATS user lacks the grant, or it is of an earlier
release), a missed message is recovered from the stream instead, marked as published while your
connection was down: a person's Send, Aside or BTW that arrives that way is shown as a message
only, nothing runs and no reply goes back on its own, and messages to a role you hold are not
recovered.

A person can also message you from the Agents page:

- On Oh My Pi, their **Send** or **Aside** arrives as their own message in your conversation,
  except a message beginning with `/` runs as the session's terminal would run it. A prompt
  template, file command or skill gives you the resulting prompt; an unknown `/name` gives you the
  words themselves. What a command prints, or its note that it did not run because it belongs
  only to the terminal, is already a **Reply via Dispatch**: write nothing for it. Only a command
  that also submits a prompt, such as `/force:<tool> <prompt>`, gives you that prompt to answer.
- A **BTW**, a broadcast, or any message to a host that takes no such turn (Claude Code) arrives as
  a Dispatch notice with a ready-made reply command. Answer with `dispatch message` and its
  `--in-reply-to`.

## Claiming and status

Before you implement an issue, claim it with `dispatch claim`, and release the claim when you
stop. While you hold it, a person's answers, comments and messages on the issue reach you
([What reaches you](#what-reaches-you)). A refusal that names another holder means someone else is
working on it; do not work it in parallel. Move the issue's status as the work moves, with
`dispatch issue-update`. When an issue, or an issue above it, carries the `legion` label, Legion
moves its status, and an agent never writes it.

## References

Link everything you mention. Dispatch reads `dispatch://` references in every document, ask,
comment, and message, and records what points where:

```text
dispatch://KEY
dispatch://KEY/spec
dispatch://KEY/artifact/<slug>
dispatch://KEY/ask/<id>
dispatch://KEY/comment/<id>
dispatch://KEY/comment/<id>#b-<block>[@v<N>]
dispatch://KEY/message/<id>
dispatch://KEY/message/<id>#b-<block>
dispatch://PROJECT/artifact/<document>
dispatch://PROJECT/artifact/<document>/comment/<id>#b-<block>[@v<N>]
```

`dispatch read` on any of them shows what it is and everything that references it.

A comment or message is made of blocks, one per paragraph, list or other top-level block, named
`b1`, `b2` and so on. `#b-<block>` names one of them, and a link in a comment or issue message
starts at the block that holds it. A comment block keeps its id when its author moves, rewords or
splits it in the dashboard (a split block's id stays with the part holding its start), and
`@v<N>` pins it to revision N of the comment, so `dispatch read` shows the text it had then and
whether it has changed or been removed since. A message never changes, so its blocks take no pin.

To answer one block rather than the whole item, pass its reference to
`dispatch comment --reply-to` or `dispatch message --in-reply-to`. Dispatch records the reply as
answering that block, and `dispatch read` of the reply prints `Re: block <id>:` and the block's
text before its body, or `Re: block <id> (removed)` once an edit removed it.

## The HTTP API

The `dispatch` commands cover everyday work. For anything else, the [HTTP API reference](/legion/dispatch/reference/api/)
lists every route, and so does `GET /api/v1` on the server, with no credential: each route's
method, who may call it, and what it does. An agent calls the API with its token as
`Authorization: Bearer <token>`.

Every `POST` and `PATCH` an agent makes through `dispatch` is keyed: each write a command makes
carries an `Idempotency-Key` header of its own, and the command asks again under that key when an
attempt fails on the way, gets no answer for 20 seconds while none of its body is going out,
Dispatch answers 5xx, 408 or 429, or Dispatch is still running an earlier attempt
(`409 KEY_IN_FLIGHT`): at most five attempts, the last starting within 30 seconds of the first, and
the whole command within 60. It asks again only of a Dispatch that judges the key: one whose answer
carried the key back, or whose `/healthz` said `idempotency_keys: true` in the last five minutes,
whatever its status; any other would make a repeated write twice. Dispatch makes the write once per
key and answers a repeat with the first answer, so the command never makes a write twice. When the
write landed but Dispatch has no answer to give for it (`409 KEY_COMMITTED`, or a first answer too
large to keep), the command exits 0 and says it was made: read it back to see it. When no attempt
got an answer, the command says the write may or may not have taken effect: read it back before
you run the command again, since a new command is a new key. Reads, and the `PUT` and `DELETE` of
`dispatch follow` and `dispatch claim --release`, carry no key and go once. A script that calls
the API directly can send its own key on any `POST` or `PATCH`: 1 to 128 letters, digits, `.`,
`_`, `:` and `-`, the same on every retry of one write and new for every new write. Dispatch keeps
each key's answer for 24 hours; the same key with a different request is refused
`422 KEY_REUSED`.
