# Reading Dispatch back, and the reference forms

Read this to catch up after a restart, read a node, trace what cites it, or write a `dispatch://` reference.

## What comes back

- After a restart, catch up with `dispatch read --issue <KEY>` (or `--artifact` with `--issue` or `--project`, or `--ref`). It prints the issue's summary, open asks, references and last 10 events, a document's summary (its issue, reference, versions and approval), or an ask's, comment's or message's thread. Reads do not subscribe. Use `dispatch doc-read` for document text.
- To audit an issue's history (who asked what, which ask was answered), add `--events N` for the last N events or `--all-events` for every one. Both work on `--ref dispatch://KEY/log` too.
- An anchored comment or ask prints `Position:`, where its quote's block stands. `unavailable (DOC_SERVICE_UNAVAILABLE)` means try again shortly. `unavailable (DOC_SCHEMA)` means the document needs repair.
- The API gives the same facts as `anchor_block` (or `anchor_block_error`) on `GET /api/v1/comments/{id}` and `GET /api/v1/asks/{id}`. `GET /api/v1/artifacts/{id}/blocks/{block_id}` gives them for any block.
- Each read ends with `Referenced by:` (what cites the node) and `Links:` (what it cites), across projects. "What led to this decision" is one read on the ask.
- A whole list is one block, so every issue named in one list previews the list's first item. Give each issue its own paragraph when each backlink should read right.
- Cite with `dispatch://` whenever you name a node. A bare issue key and a `#<question id>` count too ([Reference forms](#reference-forms)). A bare UUID or a title is invisible to the graph.

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

- A reference renders as a link titled by its target: an issue as its key and title, a document as its name, an ask as its question, a comment or message as its text. A reply or message that is only references, each word one reference, gets a card per reference in the margin, the Inbox thread and the Conversation.
- A bare issue key (`CORE-12`) links and is indexed when that issue exists when the text is written. It stands alone: it starts the text or follows a space or one of `( [ " ' * _ > ~ |` or a backtick. Trailing `.` `,` `:` `;` end it, so `CORE-12.` and `CORE-12's` cite CORE-12. `CORE-12/spec`, `CORE-12#3`, `CORE-12-3`, `CORE-12.5` and `xCORE-12` cite nothing. A key written before its issue exists is indexed only when that text is next written.
- `#<question id>` (`#q2`) links and is indexed when a question block of that id is in the same document as the text: the document's own text, or a question, comment, reply or issue message on it. An issue's comments and messages belong to its spec. It stands alone as a key does; `{#q2}`, `owner/repo#1` and a URL's `#q2` are not one. A copied question block's id cites the original ask. In a direct message `#q2` stays text.
- Inside code the dashboard shows a key, `#id` or `dispatch://` reference as code, but the graph still indexes it.
- A bare UUID is not a reference. `dispatch read` also takes a dashboard URL, also on the public dashboard when `DISPATCH_PUBLIC_URL` names one. An issue read prints its `Link:`: hand that to a person.
- An ask or comment id can be a unique 8+ hex prefix. A message id is always the full UUID.
- An ask or comment on an issue's document is `dispatch://KEY/ask/<id>` or `dispatch://KEY/comment/<id>`, never written through its document.
