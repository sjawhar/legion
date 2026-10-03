# Documents: typed blocks, comments, suggestions, and artifacts

`skill://dispatch` sends you here when you write a typed block (an `:::ask` or a callout), comment on
or suggest a change to a document, upload an artifact, request a document's approval, or get
`DOC_SCHEMA`, `INVALID_ASK_BLOCK` or `DOC_SERVICE_UNAVAILABLE` back. Changing a document's text,
tables included, is [Editing a document](skill://dispatch/references/document-edits.md).

## Typed blocks

The server declares typed document blocks at `GET /api/v1/schema/blocks`. Write one only with the
container-directive form `:::name{#block-id key="value"}` on its own line, ordinary block children,
and a closing line of as many colons at the same nesting. A typed block directly inside another needs the outer
one's fence a colon longer (`::::callout{…}` around a `:::callout{…}`), and so does one whose code holds a `:::` line;
Dispatch writes its fences that way. An unclosed typed block at document level is rejected. An
opening line that continues a paragraph instead of standing on its own (indented four or more
columns under the paragraph's text) is refused, naming the line's number and text - `INVALID_OP` on an edit's `markdown` or `with`, `INVALID_MARKDOWN` on any other write - since it
would be stored as the paragraph's text; start the block on its own line. An opening written inside
a line is stored as text, and `dispatch_issue` and `dispatch_artifact` quote it back as a
typed-block opening that is text, not a block. For
a new typed block, omit `#block-id`; Dispatch mints it. When editing an existing typed block, retain
its id and every rendered attribute. Never copy an existing block's id into new markdown: an id
names one block, so an insert, upload or suggestion whose markdown names an id the document holds
outside the text it replaces is refused naming the id: `INVALID_OP` for an insert,
`INVALID_MARKDOWN` for any other write. To rewrite such a block whole, `delete` it and then
`insert` the new one carrying its id, anchored on the block before or after it, in that order and
in one batch: an insert carrying an id the document still holds is refused. An `ask` block whose ask
is still open cannot be rewritten that way: the tools refuse the `delete`, so reword it with
`replace`, relocate it with `move`, or change its question, options, urgency or `multiple` with
`dispatch_edit_ask` if you asked it ([Editing a document](skill://dispatch/references/document-edits.md)).

Use only the type names, content rule, attributes, and enum values returned by the schema. Values are
quoted: `:::callout{kind="warning" title="Risk"}`. Do not write Pandoc-style `::: {.callout}`, leaf
`::name` directives, or text `:name` directives; those strings are literal when quoted inside a code
block. Do not set attributes the schema marks `server: true`; the server ignores them and reasserts
its authoritative value at settlement.

Questions about a document must be `ask` blocks, never an `Open questions` prose section. An ask
body is one or more question paragraphs followed by an optional bullet list of options, where each
item is `Label: description`. A spec, an uploaded document or an uploaded version holding an ask that
breaks that shape - a code block, heading or quote in it, a paragraph after its options, a second
list - is refused with `INVALID_ASK_BLOCK`; so is an edit that writes one. An ask someone left
unreadable in the browser refuses nothing it is carried through unchanged by. For example:

```md
:::ask{urgency="high" multiple="false"}
Today's release is blocked by a database migration. The maintenance window closes in two hours;
whether production data needs an index rebuild is unknown. How should we complete the migration?
Recommendation: rehearse on a production snapshot, then apply in the window, because it finds the
unknown cost before production while keeping today's release possible.

- Apply now: Meets today's release, but recovery may be slower if the index rebuild is needed.
- Rehearse then apply: Costs rehearsal time, but exposes the rebuild and rollback cost before production.
- Defer the release: Avoids migration risk today, but leaves the release and its fixes unavailable.
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
because replying to an ask makes you one of its followers (see [Following](skill://dispatch/references/asks.md)).
`ref` names the owner (an issue or project-document reference) in place of `issue`/`project`.
`quote` requires `artifact`; its anchor is pinned to the containing block while retaining the quote
for display. Omit both for a floating issue comment. A reply (`reply_to`/`reply_to_ask`) takes no
`quote`; it belongs to its parent's anchor. Reply to any comment in a thread; the server keeps
threads flat. A reply to a resolved thread reopens it. Use `reply_to_ask` to reply directly under a
question asked with `dispatch_ask`; `turn` (only with `reply_to_ask`) says who holds the turn after
the reply — `agent` for a progress note that keeps the ask waiting on you, `human` (the default) when
the human needs to act; see "Asking" in `skill://dispatch`. Comments are edited only by their author from the
dashboard. A delivered `comment.created` event carries the comment `id`; reply to it with
`dispatch_comment({ reply_to: <id> })`.

Resolve your own review comment once you have addressed it:

```ts
dispatch_resolve_comment({ comment })
```

`comment` is the comment id, or a `dispatch://KEY/comment/<id>` or `dispatch://PROJECT/artifact/<slug>/comment/<id>` reference (an
8+ character id prefix is resolved against the owner's comments). It returns the owner details plus `comment`, and takes no reason —
say what you did in a `reply_to` first if the thread needs it. The server lets any session or human resolve any open comment, so
resolve only threads you opened or were asked to close; reopening a resolved thread is human-only (from the dashboard), though your
reply to it reopens it. Asks are closed with `dispatch_resolve_ask` instead.

An exact replacement for document text is a suggestion (`dispatch_suggest`), never a comment; a
comment is for a question or a note the human answers in words. A human accepts a suggestion with
one click and cannot accept a comment, so propose the replacement instead of describing it:

```ts
dispatch_suggest({ issue?, project?, artifact, ref?, quote, replace_with, body?, occurrence? })
```

It returns issue or project-document owner details plus `comment`. A human accepts or rejects a suggestion.
Errors: `TARGET_AMBIGUOUS` (add `occurrence`), `TARGET_NOT_FOUND` (re-read first), `INVALID_ANCHOR`/`ANCHOR_MISSING`/`ANCHOR_ORPHANED`
(bad, unwritten, or stale quote), `INVALID_MARKDOWN`/`DOC_SCHEMA` (malformed content), `CAP_EXCEEDED`, `ISSUE_CLOSED`.
Suggest only what the quoted block can hold. The accept, not the suggestion, checks that: an
accept whose `replace_with` would break an ask block that was readable before it is refused with
`INVALID_ASK_BLOCK` - a question given a code block, text after an ask's options, a second option
list, or an emptied question; one that names an id the document holds outside the text it
replaces, an ask under a held id included, with `INVALID_MARKDOWN`; and one no part of the
document can hold where it sits (a code block over a table cell's whole text), or whose quote runs
into an ask or callout from the text before it, with `INVALID_OP`. Either changes nothing: the human sees the reason with no Retry, and the suggestion
stays open until someone rejects or replaces it.

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
with your text. Never do that: the spec is edited in place with `dispatch_doc_edit` (see [Editing a document](skill://dispatch/references/document-edits.md)). Address an existing
artifact by the slug shown in the upload result or by its filename, and a project document by its artifact id, slug, or filename; the
slug also arrives on `artifact.created` events. Dispatch suffixes a slug two documents would share, so one document's
filename can be another's slug (`plan v2` takes `plan-v2`, then a document named `plan-v2` takes `plan-v2-2`): a bare
`artifact` that names both is refused with each one's id, while a `dispatch://` reference's document part is always the slug.

Documents are CommonMark. A bare `<https://example.com|text>` is a CommonMark autolink and is normalised: the angle brackets are
dropped and the URL keeps `|text`. A backslash-escaped `\<https://example.com|text>` displays as `<https://example.com|text>` in the
document but comes back re-escaped (`\<`) from `dispatch_doc_read`. A Slack mrkdwn draft, or any other payload that is not Markdown,
still belongs inside a fenced code block, where it survives verbatim both ways.

A table cell ends at every `|` not written `\|`, inside inline code and links too, so write
`` `x: Promise<void> \| undefined` ``, never `` `x: Promise<void> | undefined` ``, in a cell. A row
holding text in a cell past its table's width is refused rather than stored without it, naming the
row: write a `|` inside a cell as `\|`, or, where the row really has more cells, give the header and
delimiter rows as many. Blank cells past the width are dropped, on every path. A spec, an upload or
a version answers `INVALID_MARKDOWN`, an insert of blocks `INVALID_OP` on `markdown`, and an insert
of bare table rows `TABLE_WIDTH`, which names the cell counts only.

## Approval requests

```
dispatch_request_approval({ issue?, project?, artifact?, summary })
```

The call opens an approval ask with the options `Approve` and `Request changes`, its question
"Approve spec.md (version N)?" followed by `summary`, where N is the document's latest version. It
is refused, with nothing sent, while that version holds a decision block open, and the refusal
names each block and its ask. An answer or a `dispatch_resolve_ask` closes the ask at once but
reaches a version only when the document settles, about two seconds later, or with your next
`dispatch_doc_edit`: fold the answer into the text (or, for a waiver, write the human's decision
in) before you request. A block written in the last few seconds counts as open before Dispatch has
opened its ask. A document holds one open request. A new version moves it to that version, keeping
its thread and summary, and leaves it waiting on you, as a human's reply in its thread does; a move
your own edit made sends you no event, and `dispatch_read` and `dispatch_doc_read` show it as
`Approval: awaiting, waiting on agent`. Call again when "Approval of a spec" in `skill://dispatch`
allows: that hands the same request back to the human, reworded first when `summary` is new. While
it waits on the human, a call with the same `summary`, or none, changes nothing, and one with a
different `summary` is refused, since it would rewrite the card they are reading. The answer
reaches you as `artifact.approved` or `artifact.changes_requested` with the pinned `version` and
closes the request, so the next call opens a new one; `changes_requested` carries the reason,
which is your next piece of work. Those reads show the document's approval state; `stale` means
it was approved and then edited.

## A document that is reloading

These calls can answer `DOC_SERVICE_UNAVAILABLE` (HTTP 503), because each writes a document inside its
transaction: `dispatch_doc_edit`; `dispatch_ask` and `dispatch_comment` on a quote; a `dispatch_comment` reply
in a thread whose first comment is anchored; `dispatch_suggest`; `dispatch_resolve_comment` on an anchored
comment; `dispatch_artifact` replacing a document that already exists; and, on an ask that lives in a `:::ask`
block, `dispatch_edit_ask` and `dispatch_resolve_ask`, which write that block. Creating an issue with a spec,
uploading a new document, `dispatch_request_approval` and `dispatch_message` never answer it, and neither do
`dispatch_edit_ask` and `dispatch_resolve_ask` on an ask that has no block. It means that document's live room
failed and is reloading from its durable copy, so the server refused rather than wait for it; your call wrote
nothing and the document is intact. Nothing retries it for you: the Dispatch client hands a 503 straight back.
Wait a few seconds and make the same call again. A second refusal in a row is worth telling your human about,
with the document's reference.

`dispatch_doc_read` can answer it too, though it writes nothing: a read never opens a live room, and waits out
a room that is reloading, so it is refused only when the document's durable copy cannot be read or decoded.
Retry it the same way.

