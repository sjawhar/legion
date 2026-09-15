---
name: dispatch
description: "Use when asking Sami a question, updating the spec, commenting on a document, attaching an artifact, or calling a dispatch_* tool."
---

# Dispatch

Dispatch is your issue's or project document's living spec, asks, comments, and artifacts — a high-signal record for the humans who
decide, never a log of your work. The transcript is your scratch pad; progress and status stay there. Anything meant for a human
goes through a `dispatch_*` tool.

The server enforces high signal: an ask question is at most 800 characters with at most eight options; comment and message bodies are at
most 2,000 characters; an artifact is at most 25 MiB. It refuses over-limit input with the number to trim (`question is 50 characters over
the 800-character limit (850/800)`); it never truncates it. A tool call with several problems is refused once, every problem listed
(`<tool> was not called: N problems`), so one corrected call lands. GitHub threads and markers no
longer exist.

## Writing for the human

Sami, 2026-09-12, on what Legion had been producing: "It's completely incomprehensible. It's just
compressed jargon nonsense. I have no idea what the fuck it's saying." Every spec, ask, comment,
message, and PR body is read by a person who has not read the code, does not share this session's
vocabulary, and is often on a phone. Write for that person.

- Plain English, full sentences, one idea per sentence. Never repo shorthand or nouns you coined:
  not "fix 8c", "READY-target", "PR B", "spec@v3", "the pair", "the packet" — say what the thing is.
- Expand every identifier the first time it appears: an issue key gets its title, a PR number its
  title, a file what it is for, a session id who it is. Link a URL rather than pasting a bare id.
- Frame a request as current state → desired state → proposed change, with at least two options,
  what each costs, and your recommendation with its reason.
- Before posting, test it: could Sami, reading only this text on his phone, know what he is being
  told or asked? If not, rewrite it. Length is not the problem; density is.

## Agent authentication

Use a personal Dispatch token: a human mints it in Dispatch **Settings → Agent tokens** and supplies
it to the agent through `dispatch.token` in `~/.config/opencode/envoy.json` or `DISPATCH_TOKEN`.
The server records the minting human as the owner of that session's writes. `DISPATCH_AGENT_TOKEN`
is the shared devbox fallback; do not configure it for an individual agent.

The deployed Dispatch server's browser origin is configured separately with
`DISPATCH_SERVER_URL` in the deployment `compose/.env`. Do not change an
agent's `envoy.json` to set the GitHub OAuth callback origin: the value must
be the exact URL humans type in their browser, and the GitHub App callback is
`<DISPATCH_SERVER_URL>/auth/callback`.

### Finding a route

The tools cover the everyday surface. For anything else, ask the server: `GET /api/v1` (no
credential) returns every route as `{method, path, auth, description}` sorted by path — `auth`
is `public`, `any` (a human or a bearer), `human` (a bearer gets `403 HUMAN_ONLY`), or `bearer`.
A path Dispatch does not serve under `/api` or `/v1` answers
`404 {"code":"NOT_FOUND","error":"no route for GET /v1/issues","hint":"GET /api/v1 lists every
route"}`; when you see that, you typed the path wrong — read the index rather than guessing. Every
`/api/v1` error body carries a `code`; branch on the code, never on the text.

## Writing a spec

A spec has two readers: the human who decides reads the top; the implementer who builds reads the
rest. Use these headings in this order.

| Section | Required content | Form |
| --- | --- | --- |
| **Summary** | The problem, what changes for whom, and how we will know it worked — in plain words. | Three sentences at most. |
| **Decisions needed** | Only decisions that need human authority, taste, or risk appetite. Each is one plain question, two or three options with what each costs, and your recommendation with its reason — understandable without opening anything else. Each is a `dispatch_ask`; anchor it only when it concerns a document passage. An answered item moves into Requirements with its provenance. If there is nothing to decide, write `None: this records what was agreed.` and do not ask for a review. | One decision per line. |
| **New since we talked** | Every design point the human did not settle in conversation, marked `inferred:` with the reasoning. Empty is fine. | One plain sentence per point. |
| **Acceptance** | Each outcome names what a user will observe and the check that proves it (browser scenario, API call, or command). An outcome without a check is not acceptance. | Numbered lines. |
| **Requirements** | What must hold, and where each came from: a quoted human sentence, or `inferred:` plus the reasoning. Readers treat inferred requirements as hypotheses. | `requirement \| where it comes from` table, or prose if the reader follows it more easily. |
| **Design** | The files, components, routes, and data flow that change. | Prose or tables; a diagram only for real structure. |
| **Errors** | The behaviour for every error condition. Never a silent fallback. | `condition \| behaviour` table. |
| **Testing** | Which proof exercises each acceptance line. | One line per acceptance item. |
| **Rejected** | Each alternative considered and why it was rejected, so it is not proposed again. | One alternative per line. |

### Rules

- The spec is the issue's one primary document. Extend it in place — a new version that keeps the
  human's own text — never a second "spec" artifact beside it.
- No hedging ("might", "could consider"). No TBD, TODO, or placeholders: an open item is a
  Decision needed.
- Keep each section to one screen; work that exceeds one screen per section is two specs.
- Update the spec as decisions land: the spec is the record, comments are the discussion.
- Before sending it: no sections conflict, every requirement has exactly one reading, and the
  Summary and Decisions pass the phone test above.

## Your owner

Every session works on an issue or project document. Legion pre-fills `issue` from `LEGION_ISSUE`: use a native issue key such as
`LEGION-3`, an external `owner/repo#n` reference, or a bare positive number (resolved against the cwd repository). Otherwise pass
exactly one owner to every owner-scoped tool: `issue` for an issue, or `project` and `artifact` for an unlinked project document (see
[References](#references) for the resulting ref shape). On first use, an external issue reference creates its native issue in the
project configured for that repository in Dispatch Settings, then falls back to `DISPATCH_DEFAULT_PROJECT`.

Issue reads include `rank`, the server-owned ordering key used by project boards; reorder through `PATCH /api/v1/issues/{key}` with neighboring issue keys. They also include nullable coarse priority (`P0` highest through `P3` lowest).

Architects create newly tracked child work with:
```ts
dispatch_issue({ project, title, parent?, external?, spec?, force?, labels?: string[], priority?: 0 | 1 | 2 | 3 })
```
`labels` are optional initial labels: Dispatch trims them, preserves their case, and removes case-insensitive duplicates. Set `priority` on creation only when the human's intent makes the bucket clear; otherwise priority remains the human's decision. It returns
`details` `{ issue }`; creating an issue does not subscribe you to it (see [Following](#following)). Use `dispatch_issue` only to create an issue; never use it to park a question. When `spec` is supplied,
follow [Writing a spec](#writing-a-spec).

## Search first

Before you create an issue or start a design document, search:
```ts
dispatch_search({ query, project?, limit? })
```
It returns every issue, document, comment, ask, and message that contains the words. Issue-owned hit lines
start with the issue key; standalone project-document hit lines start with
`dispatch://PROJECT/artifact/<slug>`, followed by the absolute link. Cite the hit you build on
(`dispatch://KEY` or the document reference), or state "no prior issue" in the spec. Websearch syntax applies:
`"merge queue"`, `-daemon`, `OR`.

`dispatch_issue` refuses a title that near-duplicates an issue in the same project and returns the candidates (`POSSIBLE_DUPLICATE`).
Read them; reference the existing issue, or repeat the call with `force: true` when it is genuinely new work.

## Asking

Open a decision with:
```ts
dispatch_ask({
  issue?,
  project?,
  artifact?,
  ref?,
  question,
  options?: { label, description? }[],
  multiple?,
  urgency?,
  anchor?: { artifact, quote, occurrence? },
})
```
It returns `details` `{ issue, ask, follows: { ask } }` for an issue or `{ project, artifact, document, ask, follows: { ask } }` for a project document: you follow the ask you opened (see [Following](#following)).

References belong in the question text; `ref` is sugar that appends its `dispatch://` value to the question as a rendered link.

An ask is read on a phone by someone who has not read the code. Open with one or two plain
sentences: what needs deciding and why it matters now. Each option is a button with a label and
one sentence saying what happens if it is chosen; never enumerate choices in prose. Put the
recommendation and its reason last, in `question`. Never put file paths, line numbers, sequence
numbers, document versions, or role tokens in the question; if the human needs that detail, anchor
the ask to the document passage instead. Apply the phone test from "Writing for the human" before
posting. Anchor a document question with `anchor: { artifact, quote, occurrence? }`; `occurrence`
is zero-based and selects a repeated quote. A quote anchor is pinned to its lowest complete
containing block while retaining its quote as display text, so rewording the passage keeps it
attached; a quote spanning top-level blocks, and existing anchors without a block, stay readable
against their original document version if their quote disappears.

An ask must be answerable from its own text and its anchor alone. Anchor a question about a document
passage with `anchor`. Follow up on an ask or comment with `dispatch_comment`; cite anything else
with a `dispatch://` reference (see [References](#references)). Never write "see above", "the
message above", or "as attached".

Before saying you are waiting for human input, call `dispatch_open_asks`. It lists this session's active asks across open issues and project documents, including whether the human or agent owes the next reply.

**Anything that needs the human is an ask, or it does not exist.** An approval, a credential,
a setting only they can change, a review click, a conflict between two of their own rules - if
your work waits on it, open a `dispatch_ask` with `kind: "action"` the moment you know, the
action as the question (the server supplies the fixed `Done` / `Can't` options; `Can't` requires
an explanation). Never write it into a spec, a comment reply, a message, or a
pull-request body: nothing in those paths reaches the human's Inbox, and a human who is not
reading your document does not know they are the blocker. Before asking, try to remove the
step: a value already on the machine, a permission you already hold, an API that replaces the
click. One ask per item, `urgency: "high"` when work is stopped on it; while it is open, keep
working on everything that is not.

Use `kind: "action"` for a to-do handed to a human. It has fixed `Done` / `Can't` options;
`Can't` requires an explanation, while the asker can still correct the action's wording:
```ts
dispatch_ask({ issue: "DSP-42", kind: "action",
  question: "Confirm the deployment is complete." })
```

Correct or refine an open ask in place instead of opening a second question:
```ts
dispatch_edit_ask({
  ask,
  question?,
  options?: { label, description? }[],
  multiple?,
  urgency?,
})
```
At least one field besides `ask` is required. Use this only while the same decision remains open: it keeps the prior text in the event
log and invalidates any answer draft against the prior `edited_at` revision, so the human sees the new wording and explicitly reconfirms.
An answered or resolved ask cannot be edited. If the decision is moot or superseded, retract the old ask and open a new one.

An ask stays open until a human answers, unless its question no longer needs that answer. Retract a moot or superseded question, or
self-resolve one after finding the answer:
```ts
dispatch_resolve_ask({
  ask,
  kind: "retracted",
  reason: "A newer ask supersedes this question.",
})
```
Use `retracted` when the question is obsolete and `resolved` when you found the answer. Include the reason because the question remains
in its Conversation card and reply thread. Resolution is not an answer: it never records a human decision, and an answered ask cannot be
resolved. A human may reply to an open or answered ask; so may you, e.g. after finding the answer — use `reply_to_ask` on
`dispatch_comment` (mutually exclusive with `reply_to`).

A human answers or asks back from the same field; a question-shaped free-text answer is offered as a clarification first. A human
reply while your ask is still open (the delivered `comment.created` carries `ask_state: open`) is a request for clarification, not
an answer: the human did not understand the question or needs more before choosing. The ask now waits on you in their Inbox. Answer
in the same thread with `dispatch_comment({ reply_to_ask })`, or reword the question itself with `dispatch_edit_ask` when the wording
was the problem; either puts the ask back in front of them. Do not open a second ask.

Every reply to an open ask says whose turn it is next, and the Inbox files the ask by that, not by who spoke last. Your plain reply
(`turn` omitted, or `turn: "human"`) hands the turn to the human: the ask returns to their `Waiting on you`. When you are not done
yet — "dispatched two auditors, back with results", "checking the release branch, back shortly", any working-on-it note — reply with
`turn: "agent"`: the note lands in the thread, the ask stays under `Waiting on agents`, and the human is not told to act. Use
`turn: "agent"` for every progress note and `turn: "human"` (the default) only when you need them. A human's reply always hands the
turn to you. The result names the state (`ask now waiting on agent` / `human`), the delivered `comment.created` carries it as
`ask_waiting_on`, and every ask read carries it as `waiting_on`.

## Approval of a spec

Approval is a property of a document, not a question you phrase: a human approves a specific version, the way a pull-request
review approves a commit, and any later edit makes that approval stale. It is the exception, not a step for every issue - reach
for it when a spec departs from what the human already settled, proposes children, or when the project has armed a design gate.

```
dispatch_request_approval({ issue?, project?, artifact? })
```

Opens (or returns the open) approval ask for the document at its latest version - options `Approve` and `Request changes`, in
the human's Inbox like any ask. The answer reaches you as `artifact.approved` or `artifact.changes_requested` with the pinned
`version`; `changes_requested` carries the reason, which is your next piece of work. `dispatch_read` and `dispatch_doc_read` show
the document's approval state; `stale` means it was approved and then edited - request again for the new version. Never write
"Approve" options into an ordinary `dispatch_ask`, and never approve anything yourself: only humans review.

## The Spec

The spec holds requirements, design, acceptance, decisions, and rejected alternatives, structured per [Writing a spec](#writing-a-spec).
It changes only when a decision or requirement changes, and every version that records one is named with `summary`. Never write
progress, status, timestamps, an "Update HH:MMZ" section, a PR list, or handoff notes into the spec. Progress is not a
Dispatch object at all: it lives in your transcript and your pull request (see [Messages](#messages)).

Read the current document before changing it:

```ts
dispatch_doc_read({ issue?, project?, artifact?, version?, ref? })
```
It returns live or versioned markdown with open marks. `issue` with an omitted `artifact` reads the issue specification; a project needs
`artifact`; and a `dispatch://PROJECT/artifact/<document-ref>` ref supplies both, where `document-ref` is the id, slug, or filename.

```ts
dispatch_doc_edit({ issue?, project?, artifact, ops, summary? })
```
It returns issue or project-document owner details plus `applied` and optional `version`. `ops` is an array of this
exact `EditOp` shape:

```ts
type EditOp = {
  op: "replace" | "delete" | "insert" | "retype" | "move";
  find?: string;
  with?: string;
  occurrence?: number;
  markdown?: string;
  after?: string;
  before?: string;
  block?: string;
  type?: string;
  attributes?: Record<string, unknown>;
};
```

Target `replace`, `delete`, and quote insert anchors by a block's text as rendered: write inline
code without backticks, bold without asterisks, and link text without link syntax. A table-cell
anchor is its cell text. Quote code-block contents without their Markdown fences. A quote must stay
within one textblock; split changes that span separate blocks into separate operations.

`replace` requires `find` and `with`; `delete` requires `find` or `block`; `insert` requires `markdown` and exactly one of `after` or
`before`; `move` requires `block` and exactly one of `after` or `before`. An insert or move anchor is a quote, `"start"`, `"end"`,
`"heading:Title"`, or `"block:<id>"`. Ordinary inserts create a sibling block before or after the quote, heading, or block's enclosing
document block, and a move lands the block at that same boundary; `"start"` and `"end"` select the document edges. At a table-cell
quote, a body-row fragment (no header or delimiter rows) extends that table before or after the matched row instead; short rows are
padded, wider rows are rejected, and deleting a cell's quoted text removes only that text. A `find` or quote anchor tolerates inline
Markdown (`**bold**`, `` `code` ``) and a leading `# ` selects a heading by its text; a miss names the three nearest blocks so the next
quote lands.

`replace` is inline: `with` is the new text of the matched span inside its block, so a leading list or heading marker (`4. Design`,
`# Title`) stays literal text and never turns the block into a list or heading; `with` that forms more than one paragraph is rejected
(`INVALID_OP` on `with`) — delete the block and insert new blocks instead. Use zero-based `occurrence` for a repeated target; re-read a
missing or ambiguous target before retrying. Pass `summary` to name the version when recording a decision.

A `delete` whose `find` is a block's entire text removes the block itself — the bullet, paragraph, or heading, not just its words — and
a list emptied of every item disappears with it; a partial match keeps the block with its remaining text. Deleting the text of a bullet
that holds a nested list hoists that list's items into the bullet's place (as an outliner does); a bullet with any other content
(paragraphs, code, tables) is refused with `INVALID_OP` naming `delete {block:"<item id>"}`, which removes the item with its content.
`delete` with `block` removes any block by id (paragraph, heading, list, list item, table, or typed block; deleting an open `ask` block
retracts its ask, while an answered one keeps its answer as the record), and `move` with `block` relocates one, keeping its id and
attributes — a moved `ask` keeps its ask and answer. Block ids are the `#id` a typed block renders (`:::ask{#5467e5ce-…}`) and, for
every block including untyped ones, the `id` rows of `GET /api/v1/artifacts/{id}/blocks` (or `/api/v1/issues/{key}/artifacts/{slug}/blocks`),
each with its `type` and byte range in the canonical markdown. A move whose anchor lies inside the moved block, or a delete that would
leave a typed block without the body its content rule requires, is `INVALID_OP` naming the field and the rule.

`retype` turns the paragraph or typed block with `block` into the named typed `type` in place. It keeps the
block id, keeps a typed block's body, and uses `attributes` for client-owned typed attributes. Use it when
an existing paragraph is the question that should become a decision.

## Typed blocks

The server declares typed document blocks at `GET /api/v1/schema/blocks`. Write one only with the
container-directive form `:::name{#block-id key="value"}` on its own line, ordinary block children,
and a closing `:::` at the same nesting. An unclosed typed block at document level is rejected. For
a new typed block, omit `#block-id`; Dispatch mints it. When editing an existing typed block, retain
its id and every rendered attribute.

Use only the type names, content rule, attributes, and enum values returned by the schema. Values are
quoted: `:::callout{kind="warning" title="Risk"}`. Do not write Pandoc-style `::: {.callout}`, leaf
`::name` directives, or text `:name` directives; those strings are literal when quoted inside a code
block. Do not set attributes the schema marks `server: true`; the server ignores them and reasserts
its authoritative value at settlement.

Questions about a document must be `ask` blocks, never an `Open questions` prose section. An ask
body is one or more question paragraphs followed by an optional bullet list of options, where each
item is `Label: description`. For example:

```md
:::ask{urgency="high" multiple="false"}
Should we ship the migration?

- Ship: Release the verified change.
- Hold: Wait for another review.
:::
```

When a human answers a decision written as an ask block, the answer lives on that ask. Use
`dispatch_resolve_ask` when the decision is resolved without a human response, or preserve the
human's answer; never rewrite the question into its answer or blank its options. An edit that leaves
an ask block without a question or with a blank option is rejected with `INVALID_ASK_BLOCK`.

## Comments and suggestions

Add feedback with:

```ts
dispatch_comment({ issue?, project?, artifact?, ref?, quote?, occurrence?, body, reply_to?, reply_to_ask?, turn? })
```

It returns issue or project-document owner details plus `comment`; a `reply_to_ask` reply also returns `ask` and `follows: { ask }`,
because replying to an ask makes you one of its followers (see [Following](#following)).
`ref` names the owner (an issue or project-document reference) in place of `issue`/`project`.
`quote` requires `artifact`; its anchor is pinned to the containing block while retaining the quote
for display. Omit both for a floating issue comment. A reply (`reply_to`/`reply_to_ask`) takes no
`quote`; it belongs to its parent's anchor. Reply to any comment in a thread; the server keeps
threads flat. A reply to a resolved thread reopens it. Use `reply_to_ask` to reply directly under a
question asked with `dispatch_ask`; `turn` (only with `reply_to_ask`) says who holds the turn after
the reply — `agent` for a progress note that keeps the ask waiting on you, `human` (the default) when
the human needs to act; see [Asking](#asking). Comments are edited only by their author from the
dashboard. A delivered `comment.created` event carries the comment `id`; reply to it with
`dispatch_comment({ reply_to: <id> })`.

Propose an exact replacement instead of describing it:

```ts
dispatch_suggest({ issue?, project?, artifact, ref?, quote, replace_with, body?, occurrence? })
```

It returns issue or project-document owner details plus `comment`. A human accepts or rejects a suggestion.
Errors: `TARGET_AMBIGUOUS` (add `occurrence`), `TARGET_NOT_FOUND` (re-read first), `INVALID_ANCHOR`/`ANCHOR_MISSING`/`ANCHOR_ORPHANED`
(bad, unwritten, or stale quote), `INVALID_MARKDOWN`/`DOC_SCHEMA` (malformed content), `CAP_EXCEEDED`, `ISSUE_CLOSED`.

## Artifacts

Attach an image, diagram, or local file with:

```ts
dispatch_artifact({ issue?, project?, name, path, summary? })
```

Or, when the text is already in the call, post a Markdown document directly:

```ts
dispatch_artifact({ issue?, project?, name: "load-test-results.md", content: "# Load test\n..." })
```

Exactly one of `issue` and `project` is required. A project upload creates an unlinked project document; it must not include `artifact`.
Exactly one of `path` and `content` is required. It returns issue or project-document owner details plus `artifact` and `version`.
Uploading the same `name` creates its next version — so uploading `spec.md` **replaces the issue's own specification**
with your text. Never do that: the spec is edited in place with `dispatch_doc_edit` (see [The Spec](#the-spec)). Address an existing
artifact by the slug shown in the upload result or by its filename, and a project document by its artifact id, slug, or filename; the
slug also arrives on `artifact.created` events.

## Structure over stream

Dispatch is a structured workspace, never a message stream (Sami, 2026-09-13, verbatim: "strange
to me that agents keep trying to use dispatch as a giant stream of messages instead of
high-signal, structured conversation"). The structure IS the product:

- **One issue per piece of work.** A new deliverable — an email to send, a document to review, a
  decision with its own lifecycle — gets its own issue with the content as the issue's document
  (spec artifact), not a message pile on an existing issue. If you are about to post a message
  carrying a draft, a spec, or anything over a couple of paragraphs, stop: that is an issue with a
  document, or an artifact on the issue it belongs to.
- **Content lives in documents; decisions live in asks; messages only announce.** A draft the
  human must read goes in a document artifact the dashboard renders with versions and margins; the
  ask that needs their word references it (`ref`, or the `dispatch://` link inline) instead of
  restating it. A message never carries a body a human has to scroll.
- **Never split one deliverable across a message + an ask that points at it.** Ask the question
  with the document reference in the question text; the reader lands on the content in one click.

## Messages

Dispatch is a high-signal record for humans, not a log of what you are doing. A message is a reply to a human's message, or a
change a human must know about now: a deliverable landed, a blocker only they can clear. Nothing else — no progress updates, no
"starting X", no "still working", no restating the spec, no status on a timer. Your transcript is where work is narrated; the
pull request is where it is summarised. One message that a human reads beats ten that train them to skip you.

```ts
dispatch_message({ issue, body })
```

It returns `details` `{ issue, message }`. `body` is capped at 2,000 characters. A message is not a decision
(`dispatch_ask`) or document feedback (`dispatch_comment`), and it does not wake anyone unless the issue is routed.

### Targeted agent messages

A human — or any bearer caller over HTTP, such as a test rig — can target the issue message at a
live Envoy session or role as **BTW**, **Aside**, or **Steer**. The incoming Dispatch frame names
the issue and includes a `reply_with` hint (`{ tool, args }`, ready to issue on any host); reply on the same open issue with the existing
tool, never a new targeted send:

```ts
dispatch_message({
  issue: "CORE-1",
  in_reply_to: "<targeted-message-id>",
  body: "The requested answer.",
})
```

`in_reply_to` correlates the answer under the asker's message in its Conversation card. A BTW
delivery can post its answer automatically; use this call when the frame asks the primary agent to
reply. `dispatch_message` itself never carries `target` or `delivery`: agent-to-agent traffic goes
through Envoy or the hub. A bearer that targets over HTTP names its own session in `actor`
(`{kind: "session", id}`), and the card shows that session as the author. `GET /api/v1/agents`
(any authenticated caller) lists live sessions with their capabilities (`aside`, `btw`, `steer`);
target only a session that advertises the mode you want. Sending to a session with no issue
(`POST /api/v1/agents/{session_id}/messages`) stays human-only.

## Following

An ask has followers: every session that wrote to it — the session that opened it and every session that replied with
`dispatch_comment({ reply_to_ask })` — plus any session a human adds from the ask card. The ask's answer, edits, resolution, and
every reply on it reach each follower's own agent topic directly, whether or not the writer was a human and whatever the issue's
route. The tool result says so (`You follow this ask: its answer and replies reach you directly.`) and carries `details.follows.ask`;
the host tells you once per ask. Leave a thread you no longer need, or rejoin one, with:

```ts
dispatch_follow({ ask, action: "follow" | "unfollow" })
```

`ask` is the full ask id or a `dispatch://KEY/ask/<id>` reference. A human may also remove you from the ask card; either way you are
told with an `ask.follower_removed` notice, and a human adding you arrives as `ask.follower_added`.

No write subscribes you to an issue or document. Following covers your own asks and the threads you joined; everything else on the
owner — other sessions' asks, comments, messages, status changes — reaches you only if you subscribe to the owner topic yourself.
Every write result names that line: `envoy_subscribe notifications.dispatch.issue.<KEY>.>` for an issue,
`envoy_subscribe notifications.dispatch.document.<PROJECT>.<SLUG>.>` for a project document. The owner topic carries every Dispatch
event; `notify` only controls agent wake and routed delivery. A human may unsubscribe you from the issue or document header; you
are told with a `subscription.removed` notice when that happens.

## What comes back

After a restart, catch up with:

```ts
dispatch_read({ issue?, project?, artifact?, ref? })
```

With an issue ref, it returns the issue summary, open asks, references, and recent events with `details` `{ issue }`. With a project
document owner or ref, it returns a document summary with `details` `{ project, document }`. With an ask ref, it returns that ask's
question, options, state, answer, and its reply thread. With a comment ref, it returns that comment and its quoted reply chain. With a
message ref, it returns that message and its reply chain. Reads do not subscribe; use `dispatch_doc_read` for document contents.

Every read ends with two sections from the reference graph. `Referenced by:` lists what points at the node — every document, ask,
comment, or message that cites it, plus its structure: child issues, attached documents, anchored and owned asks and comments, replies,
followers — and `Links:` lists what it cites. Each row is `- <edge kind> <node kind> dispatch://… (<excerpt> · <when>)`; for a
document source the excerpt is the block containing the mention. Cross-project, always: a message on another project's issue that
cites an ask shows up under that ask. So "what led to this decision" is one `dispatch_read` on the ask, and "who relies on this
document" one read on the document. Cite with `dispatch://` references (below) whenever you name a node in a body — a bare id or
title is invisible to the graph.

## References

Use these in document, ask, comment, and message bodies. In the dashboard, a reference renders
as an inline link whose text is the target's title (an issue's title, an ask's question, a
comment's first line, a document's name) once it resolves; a body that is only a bare reference
still gets an unfurl card instead. Every `ref` argument below (and `issue`/`project`) accepts
either form — an issue key or a project key is never ambiguous, since a project key never
contains a dash:

```text
dispatch://KEY
dispatch://KEY/spec
dispatch://KEY/artifact/<slug>[@vN]
dispatch://KEY/ask/<id>
dispatch://KEY/comment/<id>
dispatch://KEY/message/<id>
dispatch://PROJECT/artifact/<document-ref>[@vN]
dispatch://PROJECT/artifact/<document-ref>/ask/<id>
dispatch://PROJECT/artifact/<document-ref>/comment/<id>
```

A bare UUID or `KEY#seq` is not a reference; the `dispatch://` form is what Dispatch links and records. `dispatch_read` also
accepts the dashboard URL of an issue, spec, artifact, ask, comment, or project document on the configured server (it maps to the
`dispatch://` form above), and an ask or comment id may be a unique prefix of at least 8 hex characters; a message id is always the
full uuid. A non-uuid id on `GET /asks/{id}`, `/comments/{id}`, or `/issues/{key}/messages/{id}` is a 400 `ASK_ID_INPUT` /
`COMMENT_ID_INPUT` / `MESSAGE_ID_INPUT`, never a 500.

## Before / after

Before — a wall of text hides the decision and makes the choices unclickable:

```text
We need to settle the release gate because the deploy branch has the migration and the
dashboard changes, I checked the staging result and it is fine except the release notes are
not reviewed, so should we ship today, wait for docs, or cut the dashboard from this release?
I think waiting is safest but the customer demo is tomorrow and the list above is probably stale.
```

After — anchor the decision and make each option a button:

```ts
dispatch_ask({
  issue: "LEGION-815",
  question:
    "Choose the release gate. Recommendation: ship after release-note review, since the tested deployment is otherwise ready.",
  options: [
    { label: "Review notes, then ship", description: "Keeps the release intact and reviewed." },
    { label: "Ship now", description: "Meets the demo deadline; release notes follow later." },
  ],
  urgency: "high",
  anchor: { artifact: "spec", quote: "Release requires reviewed operator instructions before deployment." },
})
```

Before — a progress note that nobody needs, posted where humans look for decisions:

```ts
dispatch_message({ issue: "LEGION-815", body: "Merged the release PR, moving to docs next." })
```

After — nothing. The merge is visible on the pull request; the docs work shows up as its own deliverable. Post a message only when
a human must act or a deliverable is theirs to use:

```ts
dispatch_message({
  issue: "LEGION-815",
  body: "Release 1.4 is live on the devbox (dispatch://LEGION-815/artifact/release-notes). Nothing needed from you.",
})
```
