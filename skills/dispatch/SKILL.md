---
name: dispatch
description: "Use before posting a message, a status update, or a periodic status update; before asking a question that references another message, artifact, or eval; and when asking Sami a question, updating the spec, commenting on a document, attaching an artifact, or calling a dispatch_* tool."
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

## Where the detail lives

This file is the workflow. The detail for a step is in a reference file: open it when you reach
that step, not before. On a host with no `skill://` scheme (Claude Code, OpenCode), a link's path
after `skill://dispatch/` is relative to this skill's base directory.

| When you are about to | Read |
| --- | --- |
| call `dispatch_doc_edit`: rewrite a paragraph, insert or move a block, change a table's cells, rows or columns | [Editing a document](skill://dispatch/references/document-edits.md) |
| write a typed block (an `:::ask`, a callout), comment on or suggest a change to a document, upload an artifact, or retry after `DOC_SERVICE_UNAVAILABLE` | [Documents](skill://dispatch/references/documents.md) |
| choose your next issue, claim one, move its status or priority, reorder a board, or audit a project's backlog | [Working an issue](skill://dispatch/references/issues.md) |
| find who answers an ask, edit, retract or resolve one, reply with the turn, or follow a thread | [Asks after they open](skill://dispatch/references/asks.md) |
| catch up after a restart, trace what cites a node, or write a `dispatch://` reference | [Reading back](skill://dispatch/references/reading.md) |
| answer a BTW, Aside or Steer frame, or a message from the Agents page | [Targeted and direct messages](skill://dispatch/references/messages.md) |
| see a worked ask, a message not to send, and where a draft goes | [Before and after](skill://dispatch/references/examples.md) |
| set up a token, or call a route the tools do not cover | [Authentication and the HTTP API](skill://dispatch/references/api.md) |

## Design changes are brainstormed here

For a major design change the design conversation happens in Dispatch, in the spec: write it
early, while it is still a draft with real alternatives, and let it grow as the human answers
([Writing a spec](#writing-a-spec)). A finished spec dropped after a chat-only design is not that
conversation.

## Writing for the human

Every spec, ask, comment, message, and PR body is read by a person who has not read the code, does
not share this session's vocabulary, and is often on a phone. Write for that person.

- Plain English, full sentences, one idea per sentence. Never repo shorthand or nouns you coined:
  not "fix 8c", "READY-target", "PR B", "spec@v3", "the pair", "the packet" — say what the thing is.
- Expand every identifier the first time it appears: an issue key gets its title, a PR number its
  title, a file what it is for, a session id who it is. Link a URL rather than pasting a bare id.
- A question lives in the spec or discussion it came from, placed as
  [Decision blocks](#decision-blocks) says, never as a compressed standalone ask. Give the reader
  the options, what each costs, and your recommendation with its reason; do not prescribe yourself
  a form.
- Describe a change by what its reader stands to lose, not by what the system does. The
  engineering sentence names the change; the reader's sentence names who can do what today, what
  they will not be able to do after it, what still works, and what you cannot tell. It is a
  different sentence, not a shorter one — shortening keeps the nouns — and the test before
  sending is whether its first sentence has a human subject. Run it even on a sentence you have
  already simplified: a lead that names what a change does inside a system leaves the reader
  nothing to act on, and the same options and recommendation, led with who loses what, are
  answerable at once. Where the judgment rule below applies, the judgment leads and this rule
  shapes the sentence under it.
- Before posting, test it: could Sami, reading only this text on his phone, know what he is being
  told or asked? If not, rewrite it. Length is not the problem; density is.
- When an ask or message communicates a judgment, lead with that judgment in one sentence and put the mechanism underneath it. Do not make the reader ask a second time whether the result is a win. This shapes communication only when a judgment exists; it does not pre-decide an open question or remove its genuine options.
- When a Dispatch message states a root cause, include the reproducing command or test in that same message. Without it, label the diagnosis a hypothesis; a diagnosis still in progress may say so plainly. This boundary applies to causal claims, not to reporting that an investigation has started.

## Writing a spec

A spec is the design conversation with the human, written down. It starts as the problem and the
evidence for it, in plain words: what goes wrong, for whom, and the counts or cases that show it.
It grows in place as the conversation goes. It is the issue's one primary document: extend it with
a new version that keeps the human's own text, never a second "spec" artifact beside it.

- **Each open question is a decision block**, placed as [Decision blocks](#decision-blocks) says.
  Because it is an ask, it reaches the human's Inbox, and the answer lands next to its context.
- **A settled point records the human's own words and the date**, quoted, so no reader mistakes
  it for your inference. A point you inferred says so, with the reasoning.
- **Sections follow the topic.** No heading is required and none has a fixed place; name each
  section for what it discusses.
- **A changed point is rewritten, not appended to.** When an answer or a new fact changes the
  design, rewrite the text it changes and fold the answered decision into it; the document's
  versions keep the history.
- **No placeholders.** No TBD, TODO, or hedging ("might", "could consider"): an open item is a
  decision block, a technical decision your lane makes and records in the text, or, for a
  contract between two lanes, a question for the platform PO (see
  [Before you ask](#before-you-ask) under Asking).
- **No progress.** The spec records the design and its decisions, never status, timestamps, an
  "Update HH:MMZ" section, a pull-request list, or handoff notes. Progress is not a Dispatch
  object at all; it lives in your transcript and your pull request (see [Messages](#messages)).

Before a new version goes out, read it as the human will: no two passages conflict, each point has
one reading, and every decision block passes the phone test above. A worked example is
`dispatch://LEGION-386/artifact/spec@v5`: the problem and its counts, what the human settled in his
own words, a section for each part of the design, and its one open question as a decision block
after the section on models.

## Decision blocks

A decision a human must make is an `:::ask` block at the end of the section that discusses it,
carrying the options, what each costs, and your recommendation. Never gather decisions into a
list, at the top, at the bottom or in an "open questions" section, and never ask one as a
standalone `dispatch_ask` that points at the spec.

The block is what reaches the human's Inbox. A question phrased as prose in the spec reaches
nobody. A spec with no ask blocks is fine only when the issue genuinely needs no human decision.
When `dispatch_issue` or `dispatch_artifact` answers `This spec holds no ask blocks …`, read it as a
question: either no decision is needed and you say nothing, or you forgot to make it a block. When
it answers `… typed-block openings in this document are text, not blocks`, the quoted openings are
blocks stored as prose (inside a line, or a paste with something before every line): fix the markdown
and upload again; a mention on purpose belongs in code. Neither answer sees a spec wrapped whole in a
code fence (take the fence off), a malformed opening inside a line (`::ask{`, `:::ask {`: an ask opens
only as `:::ask{…}` on a line of its own), or any `dispatch_doc_edit`: after an edit that writes an
ask, `dispatch_doc_read` the section and check it renders as `:::ask{#<id> …}` on its own line.

**Wrong:** a **Decisions needed** list at the top of the spec with three bullets.
**Right:** each decision an `:::ask{#slug}` block at the end of the design section that discusses
it, with 2–4 options, a recommendation, and surrounding prose that explains the trade-off.

A spec that already has the pile is repaired with `move`, not rewritten: `dispatch_doc_edit` with
`{ op: "move", block: "<block-uuid>", after: "<the last sentence of the section that discusses it>" }` relocates
the block and keeps its ask, its answer and its followers; the context paragraphs that were lifted
out of Design move the same way, and the emptied section is deleted. An ask block has two ids that
differ; [Editing a document](skill://dispatch/references/document-edits.md) says which.

A decision block after approval reopens the design: writing it makes the approval stale, which is
right. The request follows the document, and you fold the answered block into the text before you
hand it back ([Approval of a spec](#approval-of-a-spec)). Outside a Legion root issue, a stale
approval needs no new request unless the change proposes something the human has not settled.

See [Typed blocks](#typed-blocks) for the syntax and [Before you ask](#before-you-ask) under
[Asking](#asking) to decide whether the question is a real decision at all.

## Your owner

Every session works on an issue or project document. Legion pre-fills `issue` from `LEGION_ISSUE`: use a native issue key such as
`LEGION-3`, an external `owner/repo#n` reference, or a bare positive number (resolved against the cwd repository). Otherwise pass
exactly one owner to every owner-scoped tool: `issue` for an issue, or `project` and `artifact` for an unlinked project document (see
[Reference forms](skill://dispatch/references/reading.md) for the resulting ref shape). An external issue reference addresses the existing Dispatch issue linked to
that GitHub issue or pull request. Only `dispatch_issue` with `external` creates a native issue; if no issue is linked, call
`dispatch_issue({ external: "owner/repo#n", project: "<project>", title: "<title>" })` before addressing it.

Create an issue for newly tracked work with:
```ts
dispatch_issue({ project, title, parent?, external?, spec?, force?, labels?: string[], priority?: 0 | 1 | 2 | 3, assignee?: string })
```
`labels` are optional initial labels: Dispatch trims them, preserves their case, and removes case-insensitive duplicates. `priority` is yours on creation too — see [Priority is yours to set](skill://dispatch/references/issues.md). Set `assignee` (a GitHub login on the sign-in allowlist) only when the human said who owns the work; otherwise the default above applies, so a child inherits its parent's assignee. It returns
`details` `{ issue }`; creating an issue does not subscribe you to it (see [Following](#following)). Use `dispatch_issue` only to create an issue; never use it to park a question. When `spec` is supplied,
follow [Writing a spec](#writing-a-spec).

## Search first

`skill://dispatch-first`, which every session with Dispatch carries, says how to search before
you plan, start a design document, file an issue, ask, post a finding or start work, and what to
do with each hit. What it leaves out:
```ts
dispatch_search({ query, project?, limit? })
```
Websearch syntax applies: `"merge queue"`, `-daemon`, `OR`. It returns the best `limit` hits (20
by default, 50 at most) across issues, documents, comments, asks, and messages. A query over
1,000 characters is refused before it is sent: search with the few words `skill://dispatch-first`
describes, never a pasted passage. Issue-owned hit lines start with the issue key; standalone
project-document hit lines start with `dispatch://PROJECT/artifact/<slug>`, followed by the
absolute link. Cite the hit you build on (`dispatch://KEY` or the document reference), or state
"no prior issue" in the spec.

`dispatch_issue` refuses a title that near-duplicates an issue in the same project and returns the candidates (`POSSIBLE_DUPLICATE`).
Read them; reference the existing issue, or repeat the call with `force: true` when it is genuinely new work.
The check compares title words only (shared stemmed terms), never meaning: "four tests that fail a
merge" pairs with "four CI gates that cannot fail a merge". So when you force past a candidate, give
the new issue a title that names what differs where you can, and open its spec with the
distinction from the named issue, citing it (`dispatch://KEY`), for whoever reads the next pairing.

### Symptom versus cause

When a symptom and its cause sit on different issues, the work accrues to the cause's issue, and
the symptom's issue carries a pointer to it. Before posting a measurement or finding, search
Dispatch for the failing identity's or component's name, and post on the issue whose title names
the fix, not the one naming the symptom. A symptom issue gathering messages with no human response
is the tell.

## Claim the issue before you work it

Before you start implementing an issue, claim it, and release the claim when you stop: finished,
handing over, or moving to something else. Closing the issue releases it for you.
```ts
dispatch_claim({ issue: "LEGION-234" })                  // I am implementing this
dispatch_claim({ issue: "LEGION-234", release: true })   // I have stopped; it is free
```
A claim is intent to implement: reading, commenting, asking, or gating a pull request claims
nothing. `409 ISSUE_CLAIMED` means someone else holds the issue; never work it in parallel. What
each refusal means, and when a claim is yours to take, is in
[Working an issue](skill://dispatch/references/issues.md).

## Issue status is yours to move

Claiming and moving the status are two actions, and you do both: when you start,
`dispatch_claim({ issue })` and `dispatch_issue_update({ issue, status: "in_progress" })`. Outside
Legion the session doing the work moves its issue and the children it owns: `in_progress` when
implementation starts, `testing` while the change is proven on a production-like surface,
`needs_review` when its pull request waits on the merge queue, and `done`, with a `reason`, once
the change has been driven in production. Never write the status of an issue that carries the
`legion` label, or of any issue under one: the Legion daemon writes those. Closing, clearing a
field, and choosing your next issue are in [Working an issue](skill://dispatch/references/issues.md).

## Asking

Two kinds of question reach a human. A decision about the design a document records is a decision
block in that document ([Decision blocks](#decision-blocks)), whatever phase the work is in and
whether or not the document was approved; the document's approval state is the human's business,
not a reason to keep the question out. A to-do or permission only a human can give, or a decision
with no document to live in, is a `dispatch_ask`. The gates below apply to both.

### Before you ask

Every `dispatch_ask` passes four gates first:

1. **Does it need his authority, taste, or risk appetite?** This is the bar for a decision
   written as an `:::ask` block in context ([Decision blocks](#decision-blocks)). Technical
   decisions inside your outcome do not: schema shapes, table layouts, field names, and migration
   internals are your lane's to decide and record in the spec. A contract between two lanes still
   goes to the platform PO over Envoy, and you open no ask for it. A halt condition (a change to
   IAM, deletion or exposure of production data, anything that reaches a customer) passes this
   gate: it is your own `dispatch_ask` to Sami on your own issue, never routed through the
   platform PO.
2. **Is there genuine uncertainty, and have you measured what you can?** If there is none, it is
   a plan you execute. The one legitimate ask without uncertainty is permission for an action
   only a human can authorise — a production write, an external send, a console action — and then
   the question is that action in one sentence, with options that name its outcomes (below).
   Measure before you write: how many are affected, whether anything reaches the path, what the
   current state already is. The measurement decides whether a human is needed at all, and when
   one is, it turns a research request he cannot answer into a decision he can. Put the
   measurement and the size of the affected population in the ask. When the measurement shows one
   fix cannot repair most of that population and another fix can, drop the first, rather than
   offering it cut down to the part it reaches. An option not to act, such as a permission ask's
   Hold, is not a fix, and it stays.
   Report what the measurement could **not** establish, with its own control: "I found no
   evidence" and "there is no evidence to find" read alike and mean opposite things, and a
   control that shares the query's blind spot proves neither. Before you say you are waiting on
   him, run `dispatch_open_asks` (below) — and the test for a new ask is not whether you asked on
   this issue before, but whether it asks him to re-report something he has already answered.
3. **Can someone who has not read the code answer it on a phone?** Write it as
   [Writing for the human](#writing-for-the-human) says — who can do what today and what changes
   for them, then two options with what each costs and your recommendation — and no slice or
   decision numbers, no coined nouns, no internal identifiers he has never used, no jargon you
   would have to define. If you cannot write it that way, you do not understand it well enough to ask.
4. **Is it outside what he has already told you he wants?** If not, that want is settled, and so
   is every choice inside it his words do not make: build it and ask nothing about it or beside it
   until it is delivered, whatever the ask says about the build. Afterwards, ask only about a choice his own
   message raises and leaves open, quoting his words and naming what you delivered. A cause you
   diagnosed, or a behaviour next to the one he named, is not in his message: neither ask about
   it nor build it — fixing the cause of the thing he named is delivering what he asked for, and
   is not what this forbids; changing something else is. An ask that turns his complaint about
   one control into a choice about another does not address what he asked, and changing that
   other control is a change he never asked for. What is still an ask the moment you know it,
   even before delivery, is anything "Anything you are blocked on a human for is visible in
   Dispatch" (further down) lists that the delivery waits on — including a conflict between what
   he asked for and another of his rules, which this gate would otherwise bury as settled.

The platform PO audits open asks. One that fails a gate — or that points at another message in
prose instead of carrying its content (below) — is retracted, with the PO's answer as the record.

Open a standalone ask with:
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
sentences: what the human needs to do or permit and why it matters now. Each option is a button
with a label and one sentence saying what happens if it is chosen; never enumerate choices in
prose. Put the recommendation and its reason last, in `question`. Never put file paths, line
numbers, document versions, or role tokens in the question; if a to-do needs document context,
anchor the ask to that passage. Apply the phone test from "Writing for the human" before posting.
A quote anchor is pinned to its lowest complete containing block while retaining its quote as
display text, so rewording the passage keeps it attached; a quote spanning top-level blocks, and
existing anchors without a block, stay readable against their original document version if their
quote disappears.

An ask must be answerable from its own text and its anchor alone. Anchor a to-do about a document
passage with `anchor`. Follow up on an ask or comment with `dispatch_comment`; cite anything else
with a `dispatch://` reference (see [References](#references)). Never write "see above", "the
message above", or "as attached".

**Pointing at another message is a defect, not a shortcut:** the ask view does not show the
issue's comments. The rules:

- An ask that names another message in prose — "my comment above", "the procedure I posted",
  "see the earlier message" — is retracted by the PO as failing the gates. Put the content IN the
  ask. If it does not fit the 800-character budget, the step is too big: split the step, never
  point elsewhere. The only pointers an ask may carry are a `dispatch://` reference or a document
  `anchor`, and they cite — the ask still says in one line what the reader will find there and can
  be answered without following them.
- Expand every term the reader has not used first. A product name, an internal setting, an
  acronym, a value you coined this session — write what it is in the ask, in his words.
- A runbook the human must execute is one ask per step, each self-contained: what to do, where,
  what result proves it, and options that name the step's outcomes. Each later step opens only
  after the previous is answered and states that step's verified result in one line ("Step 1
  done: the licence shows Cloud Identity Free on the admin console.") — never a pointer to the
  earlier ask.

**A decision about an uploaded artifact links it.** If the human must read an artifact to answer,
the question carries `dispatch://KEY/artifact/<slug>` (or `ref`), never just its filename. Text
they must read to decide belongs in the spec in the first place — see [Artifacts](#artifacts).

### When you need a human

Before saying you are waiting for human input, call `dispatch_open_asks`. With no arguments it lists this session's active asks across open issues and project documents, including whether the human or agent owes the next reply. With `dispatch_open_asks({ project })` it lists every open ask in that project — on its issues and on its documents, whoever authored them — which is how you audit what a whole project is waiting on rather than just your own asks.

**Unsettled product shape needs a decision before implementation.** When a page, navigation entry,
table key, customer-scoping rule, or persisted sidecar would set product shape that Sami has not
already settled, write a decision block in the document that records the work before the first
implementation commit. A lane's schema decision or a platform-PO contract ruling does not settle
product shape. This does not turn a user-specified decision or routine implementation into an
approval request. A control or behaviour the human asked for in words is settled by those words,
together with every choice inside it that his words do not make (where it sits, its defaults, its
options): build it without a block, as gate 4 of [Before you ask](#before-you-ask) says. This rule
covers only product shape outside what he asked for.

**Anything you are blocked on a human for is visible in Dispatch.** An agent waits on a human only
through an open ask. A to-do, permission, credential or grant renewal, setting, review click, or
other action only a human can complete is a `dispatch_ask` the moment you know, phrased as that
action. A decision about the design a document records is a decision block in that document,
whatever phase the work is in. Never write a human to-do only into a spec, a comment reply, a
message, or a pull-request body: nothing in those paths reaches the human's Inbox, and a human who
is not reading the document does not know they are the blocker. Before asking, try to remove the
step: a value already on the machine, a permission you already hold, an API that replaces the
click. One ask per item, `urgency: "high"` when work is stopped on it; while it is open, keep
working on everything that is not.

Once an ask is open (who answers it, handing a human a to-do, editing, retracting or resolving it,
answering a clarification, whose turn a reply gives), see
[Asks after they open](skill://dispatch/references/asks.md).

## Architecture components

Attach every architectural issue to the components it changes. Attach the root before decomposing it: children inherit the root's effective attachment unless they deliberately set their own, so one root attachment classifies the whole tree.

```
dispatch_issue({ project: "CORE", title: "...", components: { mode: "explicit", ids: ["dispatch-server", "web"] } })
dispatch_issue_update({ issue: "CORE-12", components: { mode: "explicit", ids: ["web"] } })   // this issue's own set, replacing what it inherited
dispatch_issue_update({ issue: "CORE-13", components: { mode: "none", reason: "hiring, not code" } })
dispatch_issue_update({ issue: "CORE-14", components: { mode: "inherit" } })                  // back to the parent chain's attachment
```

No native Dispatch tool lists component ids. Read the configured model with `GET /api/v1/projects/{key}/architecture` and use each non-external `components[].id`; when its source repository is your checkout, those ids are the file names under `.dispatch/architecture/` (`web.md` → `web`). An id absent from the model, or an `external` component, is refused with `COMPONENTS_INPUT`.

Use `mode: "none"` with a concrete reason only when the work is genuinely non-architectural (for example hiring, process, or operations work). A closed issue can be classified without reopening. Change code and its architecture description (`.dispatch/architecture/<id>.md`) in the same review. When the model you read is behind your checkout, `dispatch_architecture_sync` imports it now ([Syncing a project's architecture model](skill://dispatch/references/issues.md)).

## Close what you opened

An ask you opened is yours until it is answered or you resolve it. When the answer arrives some
other way — Sami said it live, a later comment settled it, or the question became moot because the
design moved — resolve it yourself with `dispatch_resolve_ask` in the same turn you learn that.
Never leave it for the human to clear.

Every later write on the issue answers `You still have an open ask on …` and names it. Treat that
as the checklist: if it is still needed, leave it; if it was answered elsewhere, resolve it with
the resolving fact as the reason. Before posting a new ask, inspect your open ones. If the new ask
supersedes one, retract the old one with `dispatch_resolve_ask` and kind `retracted` in the same
turn.

See [Following](#following) for why you receive what happens to asks you open and
[What comes back](skill://dispatch/references/reading.md) for finding them again with `dispatch_open_asks`.

## Approval of a spec

Approval is a property of a document, not a question you phrase: a human approves a specific
version, the way a pull-request review approves a commit, and any later version makes that
approval stale. Answering a decision block writes a new version, so request approval only when
both hold: every decision block is settled, which means answered and folded into the text, or
waived as the next paragraph says; and it proposes something the human has not already settled.
A Legion root spec under an armed design gate always goes to approval once its blocks are settled
(`skill://legion-architect`).

Request it once, when the spec is ready for the human to read. A request is one row in the Inbox
for the life of the document: a later version moves that row to the new version and files it under
Waiting on agents. Requesting again hands the same row back to Waiting on you, with a new `summary`
when the proposal changed or the same one after you answered in its thread, even if the prior reply
left it Waiting on agents. Hand it back once per revision. A human who comments on the request
instead of answering it is continuing the design conversation: reply in the thread; when the
comment raises a choice that is theirs, write it into the spec as a decision block (the request
waits on you while the block is open); when it changes the design, rewrite the text; then hand the
request back once. A choice you can make yourself is not a decision
block ([Before you ask](#before-you-ask), gate 1): write your call and its reason into the design
and name it in `summary`; a human who disagrees answers `Request changes`. When a human asks for
approval while a block is open, do not request it and do not hold it silently: name each open block
and ask them to answer it or waive it. For a waiver, close the block with `dispatch_resolve_ask`
(`kind: "resolved"`, their words as `reason`), then write their decision into the text in their
words and request. `dispatch_request_approval` refuses while any block is open.

`summary` names the proposals in this version the human has not agreed to, in one to three
sentences, and never an open question: the Inbox shows it after "Approve spec.md (version N)?".
Never write "Approve" options into an ordinary `dispatch_ask`; only humans approve. The call, its
result and its answer: [Approval requests](skill://dispatch/references/documents.md#approval-requests).

## The Spec

The spec holds the design and the decisions that shaped it, written as [Writing a spec](#writing-a-spec)
says, which also lists what it never carries. It changes when the conversation changes it, and
every version that records a decision is named with `dispatch_doc_edit`'s `summary`.

Read the current document before changing it:

```ts
dispatch_doc_read({ issue?, project?, artifact?, version?, ref? })
```
It returns live or versioned markdown with open marks. A live read ends with a document token; `issue` with an
omitted `artifact` reads the issue specification; a project needs `artifact`; and a
`dispatch://PROJECT/artifact/<document-ref>` ref supplies both, where `document-ref` is the slug (an id or a
filename resolves when no document has that slug).

Editing one is [Editing a document](skill://dispatch/references/document-edits.md): the shape of `dispatch_doc_edit`,
how to quote the text you mean, one `replace` per paragraph, preconditions against a stale edit, and
what each operation costs a block's id and its anchors.

## Typed blocks

A typed block (an `:::ask`, a callout) has its own syntax, attributes and refusals: write one only
as [Documents](skill://dispatch/references/documents.md) says.

## Artifacts

**Where a deliverable goes.** Text the human must read to decide — a draft message, a proposal,
a summary — goes in the spec as a section: the spec is the one document they open. A separate
artifact is for a real file: something sent as-is, a long report, a binary, a screenshot.

When you do upload one, the spec links it as `dispatch://KEY/artifact/<slug>` (the `slug` from the
upload result; it renders as a link) at the place the reader needs it, and the ask that needs the
decision carries the same reference. A heading or a sentence naming the filename is not a
reference.

Uploading one (`dispatch_artifact`, its slugs, versions and Markdown rules) is in
[Documents](skill://dispatch/references/documents.md).

## Structure over stream

Dispatch is a structured workspace, never a message stream. The structure IS the product:

- **One issue per piece of work.** A new deliverable — an email to send, a document to review, a
  decision with its own lifecycle — gets its own issue with the content as the issue's document
  (spec artifact), not a message pile on an existing issue. If you are about to post a message
  carrying a draft, a spec, or anything over a couple of paragraphs, stop: that is an issue with a
  document, or an artifact on the issue it belongs to.
- **Content lives in documents; decisions live in asks; messages only announce.** A draft the
  human must read goes in the issue's spec (see [Artifacts](#artifacts)), which the dashboard renders with versions and margins; the
  ask that needs their word references it (`ref`, or the `dispatch://` link inline) instead of
  restating it. A message never carries a body a human has to scroll.
- **Never split one deliverable across a message + an ask that points at it.** Ask the question
  with the document reference in the question text; the reader lands on the content in one click.

The tool result answers your third consecutive message on an issue with no human reply with
`You've sent N messages …`. That is the ledger pattern being named; stop and either wait or ask
once.

## Messages

Dispatch is a high-signal record for humans, not a log of what you are doing. A message is a reply to a human's message, or a
change a human must know about now: a deliverable landed. Nothing else — no progress updates, no
"starting X", no "still working", no restating the spec, no status on a timer. Your transcript is where work is narrated; the
pull request is where it is summarised. One message that a human reads beats ten that train them to skip you.

```ts
dispatch_message({ issue, body })
```

It returns `details` `{ issue, message }`. `body` is capped at 2,000 characters. A message is not a decision
(`dispatch_ask`) or document feedback (`dispatch_comment`), and it does not wake anyone unless the issue is routed.

A BTW, Aside or Steer frame, or a message from the Agents page, is answered as
[Targeted and direct messages](skill://dispatch/references/messages.md) says.

## Following

You follow every ask you open or reply to: its answer, edits, resolution and replies reach you
directly. Nothing else on an issue reaches you unless you subscribe to its topic. Leaving or
rejoining a thread, and the topic to subscribe to, are in
[Asks after they open](skill://dispatch/references/asks.md).

## References

The `dispatch://` form for each kind of node, and what it resolves to, is under
[Reference forms](skill://dispatch/references/reading.md).

**Every reference is a link, never an unlinked mention.** If you name a thing that has an address, link it: another issue, ask, comment, spec, or message (the `dispatch://` forms in [Reading back](skill://dispatch/references/reading.md)), an artifact (`dispatch://KEY/artifact/<slug>`), an eval (its viewer URL), a Slack message (its permalink), a Drive file (its share link). Bare phrases like "see this eval", "his 09-04 run", "the comment above", or "per the spec" with no link are banned: they make the reader hunt for what you already had in hand, and nothing can be traversed from them. Linking every reference is what makes a body both consumable and navigable. If a thing genuinely has no linkable address, say so; otherwise the link is not optional.
