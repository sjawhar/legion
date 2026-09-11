---
name: dispatch
description: "Use when asking Sami a question, updating the spec, commenting on a document, attaching an artifact, or calling a dispatch_* tool."
---

# Dispatch

Dispatch is your issue's living spec, asks, comments, and artifacts. The transcript is your
scratch pad. Anything meant for a human goes through a `dispatch_*` tool.

The server enforces high signal: an ask question is at most 800 characters with at most eight
options; comment and message bodies are at most 2,000 characters; an artifact is at most 25 MiB.
It refuses over-limit input; it never truncates it. GitHub threads and markers no longer exist.

## Writing a spec

A spec is a decision record for the human who decides and the implementer who builds, not a
transcript of your thinking. Use exactly these document headings in this order.

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
- Update the spec in place as decisions land. The spec is the record; comments are the discussion.

### Self-review

- [ ] No placeholders remain.
- [ ] No sections conflict.
- [ ] The spec covers one implementation plan's worth of work.
- [ ] Every requirement has exactly one reading.

## Your owner

Every session works on an issue or project document. Legion pre-fills `issue` from
`LEGION_ISSUE`: use a native issue key such as `LEGION-3`, an external `owner/repo#n`
reference, or a bare positive number (resolved against the cwd repository). Otherwise pass
exactly one owner to every owner-scoped tool: `issue` for an issue, or `project` and
`artifact` for an unlinked project document. A project key such as `CORE` with artifact
`design-notes` identifies `dispatch://CORE/artifact/design-notes`. On first use, an external
issue reference creates its native issue in the project configured for that repository in
Dispatch Settings, then falls back to `DISPATCH_DEFAULT_PROJECT`.

Architects create newly tracked child work with:
```ts
dispatch_issue({ project, title, parent?, external?, spec?, force? })
```
It returns `details` `{ issue, topic }`. Use `dispatch_issue` only to create an issue; never use
it to park a question. When `spec` is supplied, follow [Writing a spec](#writing-a-spec).

## Search first

Before you create an issue or start a design document, search:
```ts
dispatch_search({ query, project?, limit? })
```
It returns every issue, document, comment, ask, and message that contains the words, with the
issue key and a link. Cite the hit you build on (`dispatch://KEY` or the document reference), or
state "no prior issue" in the spec. Websearch syntax applies: `"merge queue"`, `-daemon`, `OR`.

`dispatch_issue` refuses a title that near-duplicates an issue in the same project and returns
the candidates (`POSSIBLE_DUPLICATE`). Read them; reference the existing issue, or repeat the
call with `force: true` when it is genuinely new work.

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
It returns `details` `{ issue, topic, ask }` for an issue or `{ project, artifact, document,
topic, ask }` for a project document. Options are buttons: never enumerate choices in
prose. Put the recommendation in `question`, and put each selectable choice in `options`.
Anchor a document question with `anchor: { artifact, quote, occurrence? }`; `occurrence` is
zero-based and selects a repeated quote. The server writes the resulting mark. The HTTP API also
accepts `{ artifact, mark_id }` from a browser that has already written its mark; Dispatch tools
use the quote form. An anchor whose quote disappears becomes orphaned but remains readable against
its original document version.

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
At least one field besides `ask` is required. Use this only while the same decision remains
open: it keeps the prior text in the event log. An answered or resolved ask cannot be edited.
If the decision is moot or superseded, retract the old ask and open a new one.

An ask stays open until a human answers, unless its question no longer needs that answer. Retract a
moot or superseded question, or self-resolve one after finding the answer:
```ts
dispatch_resolve_ask({
  ask,
  kind: "retracted",
  reason: "A newer ask supersedes this question.",
})
```
Use `retracted` when the question is obsolete and `resolved` when you found the answer. Include the
reason because the question remains in its Conversation card and reply thread. Resolution is not an
answer: it never records a human decision, and an answered ask cannot be resolved.

An ask is a thread, not a dead end: a human can reply to it before or after answering, and you
(the asker) can reply too — e.g. acknowledging a clarifying question, or following up after the
answer. Use `reply_to_ask` on `dispatch_comment` to reply under your own ask; it is mutually
exclusive with `reply_to`.

## The spec is where narrative goes
Write and update the issue specification according to [Writing a spec](#writing-a-spec).

Read the current document before changing it:

```ts
dispatch_doc_read({ issue?, project?, artifact?, version?, ref? })
```
It returns live or versioned markdown with open marks. `issue` with an omitted `artifact`
reads the issue specification; a project needs `artifact`; and a
`dispatch://PROJECT/artifact/<slug>` ref supplies both. Then write narrative with:
```ts
dispatch_doc_edit({ issue?, project?, artifact, ops, summary? })
```
It returns issue or project-document owner details plus `applied`, optional `version`, and its
write `topic`. `ops` is an array of this exact `EditOp` shape:

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

Target `replace` and `delete` by the document's plain text: inline-code and link text match
without Markdown syntax, and a table-cell anchor is its cell text. `replace` requires `find` and
`with`; `delete` requires `find`; `insert` requires `markdown` and exactly one of `after` or
`before`. An insert anchor is a quote, `"start"`, `"end"`, or `"heading:Title"`. Every insert
creates a sibling block before or after the quote or heading's enclosing document block; `"start"`
and `"end"` select the document edges. At a table-cell quote, pipe-table body-row fragments extend
that table before or after the matched row; omit table header and delimiter rows, and do not exceed
the table width. Use `replace` for inline continuation. Use zero-based `occurrence` for a repeated
target. When a decision lands, pass `summary` to name the resulting version. Never paste progress
into a message.

## Comments and suggestions

Add feedback with:

```ts
dispatch_comment({ issue?, project?, artifact?, quote?, occurrence?, body, reply_to?, reply_to_ask? })
```

It returns issue or project-document owner details plus `comment` and, for writes, `topic`.
`quote` requires `artifact`; omit both for a floating issue comment. A reply
(`reply_to`/`reply_to_ask`) takes no `quote`; it belongs to its parent's anchor. Use `reply_to`
to continue a comment thread at its root; a reply to a resolved thread reopens it. Use
`reply_to_ask` to reply directly under a question asked with `dispatch_ask`. The two are
mutually exclusive. Comments are edited only by their author from the dashboard.

Propose an exact replacement instead of describing it:

```ts
dispatch_suggest({ issue?, project?, artifact, quote, replace_with, body?, occurrence? })
```

It returns issue or project-document owner details plus `comment` and its write `topic`. A
human accepts or rejects a suggestion. On
`TARGET_AMBIGUOUS`, add zero-based `occurrence`. On `TARGET_NOT_FOUND`, re-read the document
before retrying. `INVALID_ANCHOR` requires exactly one nonempty anchor `quote` or `mark_id`;
`ANCHOR_MISSING` means a browser mark was not observed in the live tree, and
`ANCHOR_ORPHANED` means its marked text no longer exists. `INVALID_MARKDOWN` and `DOC_SCHEMA`
reject Markdown or a live tree outside the Proof schema. `INVALID_OP` names a malformed edit;
`CAP_EXCEEDED` never truncates; `ISSUE_CLOSED` rejects a write. `ACTOR_KIND` and `ROUTE_INVALID`
reject an invalid actor or route.

## Artifacts

Attach an image, diagram, or local file with:

```ts
dispatch_artifact({ issue?, project?, name, path, summary? })
```

Or, when the text is already in the call, post a Markdown document directly:

```ts
dispatch_artifact({ issue?, project?, name: "spec.md", content: "# Design\n..." })
```

Exactly one of `issue` and `project` is required. A project upload creates an unlinked project
document; it must not include `artifact`. Exactly one of `path` and `content` is required. The
inline form sends JSON with `Content-Type: application/json`. It returns issue or
project-document owner details plus `artifact`, `version`, and its write `topic`. Uploading the
same `name` creates its next version. Use `content` when the text is already in the call.

## Messages

Use the escape valve only for a note that fits nowhere else:

```ts
dispatch_message({ issue, body })
```

It returns `details` `{ issue, topic, message }`. `body` is capped at 2,000 characters. Your
message does not wake anyone. Do not use it for status, a decision, or document feedback.

## What comes back

A write result's `details.topic` subscribes the host to its owner. Issue writes use
`notifications.dispatch.issue.<KEY>.>`; project-document writes use
`notifications.dispatch.document.<PROJECT>.<SLUG>.>`. The owner topic carries every Dispatch
event; `notify` only controls agent wake and routed delivery. After a restart, catch up with:

```ts
dispatch_read({ issue?, project?, artifact?, ref? })
```

With an issue ref, it returns the issue summary, open asks, references, and recent events with
`details` `{ issue }`. With a project document owner or ref, it returns a document summary with
`details` `{ project, document }`. With an ask ref, it returns that ask's question, options,
state, answer, and its reply thread. With a comment ref, it returns that comment and its quoted
reply chain. Reads do not subscribe; use `dispatch_doc_read` for document contents.

## References

Use these in document, ask, comment, and message bodies; Dispatch unfurls them:

```text
dispatch://KEY
dispatch://KEY/spec
dispatch://KEY/artifact/<slug>[@vN]
dispatch://KEY/ask/<id>
dispatch://KEY/comment/<id>
dispatch://PROJECT/artifact/<slug>[@vN]
dispatch://PROJECT/artifact/<slug>/ask/<id>
dispatch://PROJECT/artifact/<slug>/comment/<id>
```

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
    "Choose the release gate. Recommendation: ship after release-note review because the tested deployment is otherwise ready.",
  options: [
    {
      label: "Review notes, then ship",
      description: "Keeps the tested release intact and publishes reviewed instructions.",
    },
    {
      label: "Ship now",
      description: "Meets the demo deadline; release notes follow separately.",
    },
    {
      label: "Remove dashboard changes",
      description: "Narrows the release but requires another deployment test.",
    },
  ],
  urgency: "high",
  anchor: {
    artifact: "spec",
    quote: "Release requires reviewed operator instructions before deployment.",
  },
})
```

Before — a status message loses the durable outcome:

```text
Done, PR merged.
```

After — record the result in the spec and name its version:

```ts
dispatch_doc_edit({
  issue: "LEGION-815",
  artifact: "spec",
  ops: [
    {
      op: "replace",
      find: "## Delivery\n\nRelease pending.",
      with: "## Delivery\n\nRelease merged and ready for deployment.",
    },
  ],
  summary: "Recorded merged release",
})
```
