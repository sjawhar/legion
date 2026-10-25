# Reading Dispatch back, and the reference forms

Read this to catch up after a restart, read a node, trace what cites it, or write a `dispatch://` reference.

## What comes back

- After a restart, catch up with `dispatch read --issue <KEY>` (or `--artifact` with `--issue` or `--project`, or `--ref`). It prints the issue's summary, open asks, references and last 10 events, a document's summary (its issue, reference, versions and approval), an ask's, comment's or message's thread, or one block of a document. Reads do not subscribe. Use `dispatch doc-read` for document text.
- To audit an issue's history (who asked what, which ask was answered), add `--events N` for the last N events or `--all-events` for every one. Both work on `--ref dispatch://KEY/log` too.
- An anchored comment or ask prints `Position:`, where its quote's block stands. `unavailable (DOC_SERVICE_UNAVAILABLE)` means try again shortly. `unavailable (DOC_SCHEMA)` means the document needs repair.
- The API gives the same facts as `anchor_block` (or `anchor_block_error`) on `GET /api/v1/comments/{id}` and `GET /api/v1/asks/{id}`. `GET /api/v1/artifacts/{id}/blocks/{block_id}` gives them for any block.
- Each read ends with `Referenced by:` (what cites the node) and `Links:` (what it cites), across projects. "What led to this decision" is one read on the ask.
- A link written in a document starts at the innermost block that holds it, and its backlink names and previews that block: a list item's own paragraph, or the whole table for a link in a cell. On a document's read, a row at one of its blocks ends `(block <ref>)`: the block cited, or the block holding the link.
- `dispatch read --ref <block ref>` reads one block: `Block:`, `Document:`, `Changed in version:` (the version its text last changed in) and `Text:`, then what cites it and what it cites. A pinned ref adds `Pinned to version:`, `Unchanged since version N.`, `Changed since version N; last changed in version M.` or `Removed in version R.`, and `Text at version N:`. A block the document no longer holds reads `No block <id> in <document>.` unpinned, and as removed when pinned to a version that held it.
- `GET /api/v1/block-links?from=<document ref>&to=<document ref>` lists the links from the blocks of one document to the blocks of another (or of the same one), each with both blocks and the version that first held it.
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
dispatch://KEY/spec#b-<block id>[@vN]
dispatch://KEY/artifact/<slug>#b-<block id>[@vN]
dispatch://PROJECT/artifact/<slug>#b-<block id>[@vN]
```

- A reference renders as a link titled by its target. A body that is only a reference gets an unfurl card.
- A bare UUID or `KEY#seq` is not a reference. `dispatch read` also takes a dashboard URL, also on the public dashboard when `DISPATCH_PUBLIC_URL` names one. An issue read prints its `Link:`: hand that to a person.
- An ask or comment id can be a unique 8+ hex prefix. A message id is always the full UUID.
- An ask or comment on an issue's document is `dispatch://KEY/ask/<id>` or `dispatch://KEY/comment/<id>`, never written through its document.
- A `#b-` reference names one block of a document. `dispatch doc-read --blocks` lists the live ids. Percent-encode every byte of the id except ASCII letters, digits and `-`: `ship_it` is `#b-ship%5Fit`, an `@` is `%40`. A dashboard URL of a live document ending `#b-<id>` names the same block.
- Without a pin, a block reference follows the block's current text. `@vN` after the id pins it to version N of the document: a read shows the block's text at N and whether it is unchanged, changed or removed since. Pin a block when the words you cite matter, not just the place.
- A link to a block counts only while the document holds that block: deleting the block takes its links out of every list, and a version that restores the id brings them back.
- `dispatch read` reads a block reference and `dispatch ask` cites one. `dispatch comment`, `suggest`, `doc-edit` and `doc-read` refuse it, naming the arguments for its document.
