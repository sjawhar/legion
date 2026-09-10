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

## Your issue

Every session works on an issue. Legion pre-fills `issue` from `LEGION_ISSUE`: use a native issue key
such as `LEGION-3`, an external `owner/repo#n` reference, or a bare positive number (resolved
against the cwd repository). Otherwise pass the issue to every issue-scoped tool as its native key
or an external `owner/repo#n` reference. On first use, an external reference creates its native issue in the
project configured for that repository in Dispatch Settings, then falls back to `DISPATCH_DEFAULT_PROJECT`.

Architects create newly tracked child work with:
```ts
dispatch_issue({ project, title, parent?, external?, spec? })
```
It returns `details` `{ issue, topic }`. Use `dispatch_issue` only to create an issue; never use
it to park a question.

## Asking

Open a decision with:
```ts
dispatch_ask({
  issue,
  question,
  options?: { label, description? }[],
  multiple?,
  urgency?,
  anchor?: { artifact, quote, occurrence? },
})
```
It returns `details` `{ issue, topic, ask }`. Options are buttons: never enumerate choices in
prose. Put the recommendation in `question`, and put each selectable choice in `options`.
Anchor a document question with `anchor: { artifact, quote, occurrence? }`; `occurrence` is
zero-based and selects a repeated quote. An anchor whose quote disappears becomes orphaned but
remains readable against its original document version. An ask stays open until a human answers;
you cannot withdraw it. Ask once and well. The host steers you when a human answers.

An ask is a thread, not a dead end: a human can reply to it before or after answering, and you
(the asker) can reply too — e.g. acknowledging a clarifying question, or following up after the
answer. Use `reply_to_ask` on `dispatch_comment` to reply under your own ask; it is mutually
exclusive with `reply_to`.

## The spec is where narrative goes

Read the current document before changing it:

```ts
dispatch_doc_read({ issue?, artifact?, version?, ref? })
```
It returns live or versioned markdown with open marks and `details` `{ issue }`; omit `artifact`
with `issue` to read the primary document. Then write narrative with:

```ts
dispatch_doc_edit({ issue, artifact, ops, summary? })
```
It returns `details` `{ issue, topic, applied, version? }`. `ops` is an array of this exact
`EditOp` shape:

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

Target edits by quote: `replace` requires `find` and `with`; `delete` requires `find`; `insert`
requires `markdown` and exactly one of `after` or `before`. An insert anchor is a quote,
`"start"`, `"end"`, or `"heading:Title"`. Use zero-based `occurrence` for a repeated `find`.
When a decision lands, pass `summary` to name the resulting version. Never paste progress into a
message.

## Comments and suggestions

Add feedback with:

```ts
dispatch_comment({ issue, artifact?, quote?, occurrence?, body, reply_to?, reply_to_ask? })
```

It returns `details` `{ issue, topic, comment }`. `quote` requires `artifact`; omit both for a
floating issue comment. Use `reply_to` to continue a comment thread; use `reply_to_ask` to reply
directly under a question asked with `dispatch_ask`. The two are mutually exclusive.

Propose an exact replacement instead of describing it:

```ts
dispatch_suggest({ issue, artifact, quote, replace_with, body?, occurrence? })
```

It returns `details` `{ issue, topic, comment }`. A human accepts or rejects a suggestion. On
`TARGET_AMBIGUOUS`, add zero-based `occurrence`. On `TARGET_NOT_FOUND`, re-read the document
before retrying. `INVALID_OP` names a malformed edit; `CAP_EXCEEDED` never truncates;
`ISSUE_CLOSED` rejects a write. `ACTOR_KIND`, `ROUTE_INVALID`, and `PRIMARY_NOT_DOC` reject an
invalid actor, route, or primary artifact.

## Artifacts

Attach an image, diagram, or local file with:

```ts
dispatch_artifact({ issue, name, path, primary?, summary? })
```

Or, when the text is already in the call, post a Markdown document directly:

```ts
dispatch_artifact({ issue, name: "spec.md", content: "# Design\n...", primary: true })
```

Exactly one of `path` and `content` is required. The inline form sends JSON with
`Content-Type: application/json`. It returns `details` `{ issue, topic, artifact, version }`.
Uploading the same `name` creates its next version. Use `content` for an architect's primary spec;
set `primary: true` only for Markdown documents.

## Messages

Use the escape valve only for a note that fits nowhere else:

```ts
dispatch_message({ issue, body })
```

It returns `details` `{ issue, topic, message }`. `body` is capped at 2,000 characters. Your
message does not wake anyone. Do not use it for status, a decision, or document feedback.

## What comes back

A write result's `details.topic` subscribes the host to the issue. Events render as:

```text
dispatch <KEY> · <type> · by <actor>
```

The issue topic carries every Dispatch event; `notify` only controls agent wake and routed delivery.
After a restart, catch up with:

```ts
dispatch_read({ issue?, ref? })
```

With an issue ref, it returns the issue summary, open asks, and recent events with `details`
`{ issue }`. With an ask ref, it returns that ask's question, options, state, answer, and its
reply thread. With a comment ref, it returns that comment and its quoted reply chain. Use
`dispatch_doc_read` for document contents.

## References

Use these in document, ask, comment, and message bodies; Dispatch unfurls them:

```text
dispatch://KEY
dispatch://KEY/spec
dispatch://KEY/artifact/<slug>[@vN]
dispatch://KEY/ask/<id>
dispatch://KEY/comment/<id>
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
