# Reading Dispatch back, and the reference forms

`skill://dispatch` sends you here when you catch up after a restart, read an issue, ask, comment,
message or document, trace what cites a node, or need the exact `dispatch://` form for a reference.

## What comes back

After a restart, catch up with:

```ts
dispatch_read({ issue?, project?, artifact?, ref? })
```

With an issue ref, it returns the issue summary, open asks, references, and recent events with `details` `{ issue }`. With a project
document owner or ref, it returns a document summary with `details` `{ project, document }`. With an ask ref, it returns that ask's
question, options, state, answer, and its reply thread. With a comment ref, it returns that comment and its quoted reply chain. With a
message ref, it returns that message and its reply chain. Reads do not subscribe; use `dispatch_doc_read` for document contents.

An anchored comment or ask also prints `Position:`, where its quote's block stands — `table[3] › row 5 (Red-teamer loop), column Due`
is the fourth top-level block, a table, its row 5 (row 0 is the header; the index `delete_row` takes), labelled by the row's cells
before the anchored one, in the column headed Due; outside a table it is the path of types and child indexes down to the block. The
same facts are `anchor_block` on `GET /api/v1/comments/{id}` and `GET /api/v1/asks/{id}`, and
`GET /api/v1/artifacts/{id}/blocks/{block_id}` answers them for any block id a document holds. When Dispatch could not read the
document, the read still answers and prints `Position: unavailable (document_unavailable)` (try again shortly) or
`Position: unavailable (document_unreadable)` (the document needs repair); the API carries that reason as `anchor_block_error`.

Every read ends with two sections from the reference graph. `Referenced by:` lists what points at the node — every document, ask,
comment, or message that cites it, plus its structure: child issues, attached documents, anchored and owned asks and comments, replies,
followers — and `Links:` lists what it cites. Each row is `- <edge kind> <node kind> dispatch://… (<excerpt> · <when>)`; for a
document source the excerpt is the start of the block holding the mention, and a whole list is one block, so every issue named in
one list previews the list's first item. When a document references many issues and each backlink should read right, give each
issue its own paragraph (or block), not an item of one list. Cross-project, always: a message on another project's issue that
cites an ask shows up under that ask. So "what led to this decision" is one `dispatch_read` on the ask, and "who relies on this
document" one read on the document. Cite with `dispatch://` references (below) whenever you name a node in a body — a bare id or
title is invisible to the graph.

## Reference forms

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

