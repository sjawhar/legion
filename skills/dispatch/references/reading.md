# Reading Dispatch back, and the reference forms

Read this to catch up after a restart, read a node, trace what cites it, or write a `dispatch://` reference.

## What comes back

- After a restart, catch up with `dispatch read --issue <KEY>` (or `--artifact` with `--issue` or `--project`, or `--ref`). It prints the issue's summary, open asks, references and last 10 events, a document's summary (its issue, reference, versions and approval), or an ask's, comment's or message's thread. Reads do not subscribe. Use `dispatch doc-read` for document text.
- To audit an issue's history (who asked what, which ask was answered), add `--events N` for the last N events or `--all-events` for every one. Both work on `--ref dispatch://KEY/log` too.
- An anchored comment or ask prints `Position:`, where its quote's block stands. `unavailable (DOC_SERVICE_UNAVAILABLE)` means try again shortly. `unavailable (DOC_SCHEMA)` means the document needs repair.
- The API gives the same facts as `anchor_block` (or `anchor_block_error`) on `GET /api/v1/comments/{id}` and `GET /api/v1/asks/{id}`. `GET /api/v1/artifacts/{id}/blocks/{block_id}` gives them for any block.
- Each read ends with `Referenced by:` (what cites the node) and `Links:` (what it cites), across projects. "What led to this decision" is one read on the ask.
- A whole list is one block, so every issue named in one list previews the list's first item. Give each issue its own paragraph when each backlink should read right.
- Cite with `dispatch://` whenever you name a node. A bare id or title is invisible to the graph.

## Reference forms

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

- A reference renders as a link titled by its target. A body that is only a reference gets an unfurl card.
- A bare UUID or `KEY#seq` is not a reference. `dispatch read` also takes a dashboard URL, also on the public dashboard when `DISPATCH_PUBLIC_URL` names one. An issue read prints its `Link:`: hand that to a person.
- An ask or comment id can be a unique 8+ hex prefix. A message id is always the full UUID.
- An ask or comment on an issue's document is `dispatch://KEY/ask/<id>` or `dispatch://KEY/comment/<id>`, never written through its document.
