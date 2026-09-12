---
name: dispatch
description: "Use when asking Sami a question, updating the spec, commenting on a document, attaching an artifact, or calling a dispatch_* tool."
---

# Dispatch

Dispatch is your issue's or project document's living spec, asks, comments, and artifacts — a high-signal record for the humans who
decide, never a log of your work. The transcript is your scratch pad; progress and status stay there. Anything meant for a human
goes through a `dispatch_*` tool.

The server enforces high signal: an ask question is at most 800 characters with at most eight options; comment and message bodies are at
most 2,000 characters; an artifact is at most 25 MiB. It refuses over-limit input; it never truncates it. GitHub threads and markers no
longer exist.

## Writing a spec

A spec is a decision record for the human who decides and the implementer who builds, not a transcript of your thinking. Use exactly
these document headings in this order.

| Section | Required content | Form |
| --- | --- | --- |
| **Decisions needed** | Only decisions requiring human authority, taste, or risk appetite. Each states one question, two or three options with tradeoffs, and a recommendation. Every item is an anchored `dispatch_ask`. Answered items move into Requirements with provenance, then leave this section. No other section asks the reader anything. Empty means `None.` | One decision per line; anchor each ask to that line. |
| **Acceptance** | Every outcome names its check and user-facing surface. An outcome without a verification method is not acceptance criteria. | Numbered lines; browser scenario, API call, or CLI command. |
| **Requirements** | Provenance is a verbatim human quote or `inferred: <reasoning>`; readers treat inferred requirements as hypotheses. Do not restate the prompt in prose. | `requirement \| provenance` table. |
| **Design** | State the files, components, routes, and data flow that change. | Facts, not narrative; diagrams only for genuine structure. |
| **Errors** | Name the behaviour for every error condition; never specify a silent fallback. | `condition \| behaviour` table. |
| **Testing** | Map every acceptance line to the proof that exercises it. | Suite or scenario. |
| **Rejected** | Record each considered alternative and why it was rejected so it is not proposed again. | One alternative per line. |

### Rules

- Every sentence is a fact, decision, or risk; delete the rest.
- Use tables over prose and keep one idea per line.
- Do not use Overview, Background, Introduction, Summary, or Conclusion sections.
- Do not hedge with “might” or “could consider.”
- Do not use TBD, TODO, or placeholders; an open item is a Decision needed.
- Keep each section to one screen; work that exceeds one screen per section is two specs.
- Update the spec in place as decisions land: the spec is the record, comments are the discussion.
- Before sending it: no sections conflict, and every requirement has exactly one reading.

## Your owner

Every session works on an issue or project document. Legion pre-fills `issue` from `LEGION_ISSUE`: use a native issue key such as
`LEGION-3`, an external `owner/repo#n` reference, or a bare positive number (resolved against the cwd repository). Otherwise pass
exactly one owner to every owner-scoped tool: `issue` for an issue, or `project` and `artifact` for an unlinked project document (see
[References](#references) for the resulting ref shape). On first use, an external issue reference creates its native issue in the
project configured for that repository in Dispatch Settings, then falls back to `DISPATCH_DEFAULT_PROJECT`.

Architects create newly tracked child work with:
```ts
dispatch_issue({ project, title, parent?, external?, spec?, force? })
```
It returns `details` `{ issue, topic }`. Use `dispatch_issue` only to create an issue; never use it to park a question. When `spec` is
supplied, follow [Writing a spec](#writing-a-spec).

## Search first

Before you create an issue or start a design document, search:
```ts
dispatch_search({ query, project?, limit? })
```
It returns every issue, document, comment, ask, and message that contains the words, with the issue key and a link. Cite the hit you
build on (`dispatch://KEY` or the document reference), or state "no prior issue" in the spec. Websearch syntax applies: `"merge queue"`,
`-daemon`, `OR`.

`dispatch_issue` refuses a title that near-duplicates an issue in the same project and returns the candidates (`POSSIBLE_DUPLICATE`).
Read them; reference the existing issue, or repeat the call with `force: true` when it is genuinely new work.

## Asking

Open a decision with:
```ts
dispatch_ask({
  issue?,
  project?,
  artifact?,
  question,
  options?: { label, description? }[],
  multiple?,
  urgency?,
  anchor?: { artifact, quote, occurrence? },
})
```
It returns `details` `{ issue, topic, ask }` for an issue or `{ project, artifact, document, topic, ask }` for a project document.
Options are buttons: never enumerate choices in prose. Put the recommendation in `question`, and put each selectable choice in
`options`. Anchor a document question with `anchor: { artifact, quote, occurrence? }`; `occurrence` is zero-based and selects a repeated
quote, and an anchor whose quote later disappears becomes orphaned but stays readable against its original document version.

An ask must be answerable from its own text and its anchor alone. Anchor a question about a document passage with `anchor`; thread one
about a comment with `reply_to`; thread a follow-up on your own ask with `reply_to_ask`; cite anything else with a `dispatch://`
reference (see [References](#references)). Never write "see above", "the message above", or "as attached".

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
log. An answered or resolved ask cannot be edited. If the decision is moot or superseded, retract the old ask and open a new one.

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

A human reply while your ask is still open (the delivered `comment.created` carries `ask_state: open`) is a request for
clarification, not an answer: the human did not understand the question or needs more before choosing. The ask now waits on you
in their Inbox. Answer in the same thread with `dispatch_comment({ reply_to_ask })`, or reword the question itself with
`dispatch_edit_ask` when the wording was the problem; either puts the ask back in front of them. Do not open a second ask.

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
`artifact`; and a `dispatch://PROJECT/artifact/<slug>` ref supplies both. Then write with:

```ts
dispatch_doc_edit({ issue?, project?, artifact, ops, summary? })
```
It returns issue or project-document owner details plus `applied`, optional `version`, and its write `topic`. `ops` is an array of this
exact `EditOp` shape:

```ts
type EditOp = {
  op: "replace" | "delete" | "insert";
  find?: string;
  with?: string;
  occurrence?: number;
  markdown?: string;
  after?: string;
  before?: string;
};
```

Target `replace` and `delete` by the document's plain text: inline-code and link text match without Markdown syntax, and a table-cell
anchor is its cell text. Quote code-block contents without their Markdown fences. A quote must stay within one textblock; split changes
that span separate blocks into separate operations.

`replace` requires `find` and `with`; `delete` requires `find`; `insert` requires `markdown` and exactly one of `after` or `before`. An
insert anchor is a quote, `"start"`, `"end"`, or `"heading:Title"`. Ordinary inserts create a sibling block before or after the quote or
heading's enclosing document block; `"start"` and `"end"` select the document edges. At a table-cell quote, a body-row fragment (no
header or delimiter rows) extends that table before or after the matched row instead; short rows are padded, wider rows are rejected,
and deleting a cell's quoted text removes only that text.

Use `replace` for inline continuation. Use zero-based `occurrence` for a repeated target; re-read a missing or ambiguous target before
retrying. Pass `summary` to name the version when recording a decision.

## Comments and suggestions

Add feedback with:

```ts
dispatch_comment({ issue?, project?, artifact?, ref?, quote?, occurrence?, body, reply_to?, reply_to_ask? })
```

It returns issue or project-document owner details plus `comment` and, for writes, `topic`.
`ref` names the owner (an issue or project-document reference) in place of `issue`/`project`.
`quote` requires `artifact`; omit both for a floating issue comment. A reply (`reply_to`/`reply_to_ask`)
takes no `quote`; it belongs to its parent's anchor. Use `reply_to` to continue a comment thread at
its root; a reply to a resolved thread reopens it. Use `reply_to_ask` to reply directly under a
question asked with `dispatch_ask`. Comments are edited only by their author from the dashboard. A
delivered `comment.created` event carries the comment `id`; reply to it with
`dispatch_comment({ reply_to: <id> })`.

Propose an exact replacement instead of describing it:

```ts
dispatch_suggest({ issue?, project?, artifact, ref?, quote, replace_with, body?, occurrence? })
```

It returns issue or project-document owner details plus `comment` and its write `topic`. A human accepts or rejects a suggestion.
Errors: `TARGET_AMBIGUOUS` (add `occurrence`), `TARGET_NOT_FOUND` (re-read first), `INVALID_ANCHOR`/`ANCHOR_MISSING`/`ANCHOR_ORPHANED`
(bad, unwritten, or stale quote), `INVALID_MARKDOWN`/`DOC_SCHEMA` (malformed content), `CAP_EXCEEDED`, `ISSUE_CLOSED`.

## Artifacts

Attach an image, diagram, or local file with:

```ts
dispatch_artifact({ issue?, project?, name, path, summary? })
```

Or, when the text is already in the call, post a Markdown document directly:

```ts
dispatch_artifact({ issue?, project?, name: "spec.md", content: "# Design\n..." })
```

Exactly one of `issue` and `project` is required. A project upload creates an unlinked project document; it must not include `artifact`.
Exactly one of `path` and `content` is required. It returns issue or project-document owner details plus `artifact`, `version`, and its
write `topic`. Uploading the same `name` creates its next version. Address an existing artifact by the slug shown in the upload result
or by its filename; the slug also arrives on `artifact.created` events.

## Messages

Dispatch is a high-signal record for humans, not a log of what you are doing. A message is a reply to a human's message, or a
change a human must know about now: a deliverable landed, a blocker only they can clear. Nothing else — no progress updates, no
"starting X", no "still working", no restating the spec, no status on a timer. Your transcript is where work is narrated; the
pull request is where it is summarised. One message that a human reads beats ten that train them to skip you.

```ts
dispatch_message({ issue, body })
```

It returns `details` `{ issue, topic, message }`. `body` is capped at 2,000 characters. A message is not a decision
(`dispatch_ask`) or document feedback (`dispatch_comment`), and it does not wake anyone unless the issue is routed.

## What comes back

A write result's `details.topic` subscribes the host to its owner, and the first such subscription on an issue or document also tells
you so (an already-subscribed write stays quiet — no repeat notice). Issue writes use `notifications.dispatch.issue.<KEY>.>`;
project-document writes use `notifications.dispatch.document.<PROJECT>.<SLUG>.>`. The owner topic carries every Dispatch event; `notify`
only controls agent wake and routed delivery. A human may unsubscribe you from the issue or document header; you are told with a
`subscription.removed` notice when that happens. After a restart, catch up with:

```ts
dispatch_read({ issue?, project?, artifact?, ref? })
```

With an issue ref, it returns the issue summary, open asks, references, and recent events with `details` `{ issue }`. With a project
document owner or ref, it returns a document summary with `details` `{ project, document }`. With an ask ref, it returns that ask's
question, options, state, answer, and its reply thread. With a comment ref, it returns that comment and its quoted reply chain. With a
message ref, it returns that message and its reply chain. Reads do not subscribe; use `dispatch_doc_read` for document contents.

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
dispatch://PROJECT/artifact/<slug>[@vN]
dispatch://PROJECT/artifact/<slug>/ask/<id>
dispatch://PROJECT/artifact/<slug>/comment/<id>
```

A bare UUID or `KEY#seq` is not a reference; the `dispatch://` form is what Dispatch links and records.

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
