# Documents: typed blocks, comments, suggestions, and artifacts

Read this to write a typed block, comment, suggest, upload an artifact, request approval, or handle `DOC_SCHEMA`, `INVALID_ASK_BLOCK` or `DOC_SERVICE_UNAVAILABLE`. Text and table edits are [Editing a document](skill://dispatch/references/document-edits.md).

## Typed blocks

- Use only the types, content rules, attributes and enum values from `GET /api/v1/schema/blocks`. Quote values: `:::callout{kind="warning" title="Risk"}`. Never set an attribute marked `server: true`.
- Write the container form only: `:::name{#block-id key="value"}` on its own line, block children, and a closing line with the same number of colons. A block inside another, or one whose code holds a `:::` line, needs the outer fence one colon longer. Never write Pandoc `::: {.callout}`, leaf `::name` or text `:name`.
- Dispatch refuses an unclosed block, and an opening indented four or more columns under a paragraph. An opening inside a line is stored as text.
- For a new block, omit `#block-id`: Dispatch mints it. When you edit one, keep its id and every attribute. Never copy an id into new markdown. To rewrite a block whole, `delete` it, then `insert` it with its id, in one batch. You cannot do this to an open ask's block: use `replace`, `move` or `dispatch edit-ask`.
- Questions about a document are `ask` blocks, never an "Open questions" section. The body is question paragraphs, then an optional bullet list of `Label: description` options. A code block, heading or quote, a paragraph after the options, or a second list is `INVALID_ASK_BLOCK`.

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

- A human's answer lives on the ask. Never rewrite the question into its answer or blank its options. Resolve without a human with `dispatch resolve-ask`.
- An answered or resolved ask's question is fixed. Dispatch writes its block back. To ask something new, write a new block. An open ask's block is its text.
- `This spec holds no ask blocks …`: either no decision is needed, or you forgot the block. `… typed-block openings in this document are text, not blocks`: fix the markdown and upload again. Put an intended mention in code.
- Neither check sees a spec wrapped in a code fence, an opening like `::ask{` or `:::ask {`, or any `doc-edit`. After an edit that writes an ask, `dispatch doc-read` the section and check that it renders `:::ask{#<id> …}` on its own line.

## Comments and suggestions

```bash
dispatch comment --issue <KEY> --artifact spec --quote '<the passage>' --body '<the question or note>'
dispatch suggest --issue <KEY> --artifact spec --quote '<exact text>' --replace-with '<replacement>'
```

- An exact replacement is a suggestion, never a comment: a human accepts it with one click. A comment is for a question or note answered in words.
- Omit `--quote` and `--artifact` for a floating comment. A reply (`--reply-to`, `--reply-to-ask`) takes no quote. Threads are flat. A reply reopens a resolved thread, except a decided suggestion's. `--turn` works only with `--reply-to-ask`.
- `--ref` names the owner only. Dispatch refuses an ask, comment or message link there.
- Resolve your own comment once addressed: `dispatch resolve-comment --comment <id>`. It takes no reason, so reply first if needed. Resolve only threads you opened or were asked to close. Reopening a resolved thread from the dashboard is human-only, but your reply to it reopens it, except a decided suggestion's.
- Suggest only what the quoted block can hold. The accept checks it, and a refused accept stays open.
- Error codes: `TARGET_AMBIGUOUS` (add `--occurrence`), `TARGET_NOT_FOUND` (re-read), `INVALID_ANCHOR` / `ANCHOR_MISSING` / `ANCHOR_ORPHANED` (bad or stale quote), `INVALID_MARKDOWN` / `DOC_SCHEMA`, `CAP_EXCEEDED`, `ISSUE_CLOSED`.

## Artifacts

```bash
dispatch artifact --issue <KEY> --name diagram.png --path ./diagram.png
dispatch artifact --issue <KEY> --name load-test-results.md --content-file - <<'EOF'
# Load test
EOF
```

- Uploading the same `--name` makes its next version. Never upload `spec.md`: it replaces the human's spec. Edit with `dispatch doc-edit`.
- Address an artifact by the slug the upload printed. A bare `--artifact` that names two documents (a slug and a filename) is refused with both ids. A `dispatch://` reference always uses the slug.
- A copy of another document's `:::ask` block (same `#id` and text, same owner) opens no ask. It mirrors the original. Answer the original. Change the copy's text and it opens its own ask.
- A picture from an Agents-page conversation is `dispatch://agent/<session id>/artifact/<slug>@vN`. `dispatch doc-read` reads it by that reference alone.
- Show a picture inline with `--image` on `message`, `comment` or `ask`, not `dispatch artifact`. Each adds about 60 characters that count toward the cap. A plain `dispatch://KEY/artifact/<slug>` link to an image shows a thumbnail.
- `dispatch read` and `dispatch doc-read` write pictures to files and print `- picture: <path>`. Open each with your file-read tool. A session sees each picture once. `dispatch doc-read` shows it again, after compaction too.
- Documents are CommonMark. Put Slack mrkdwn or other non-Markdown in a fenced code block.
- In a table cell, write every `|` as `\|`, inside code and links too. A row wider than its table is refused.

## Approval requests

```bash
dispatch request-approval --issue <KEY> --summary '<what the human is approving>'
```

- It opens an `Approve` / `Request changes` ask on the latest version. It is refused while a decision block is open. A block written in the last few seconds counts as open.
- An answer or resolve reaches a version about two seconds later. Wait for that version, do not edit to force one. A waiver carries no record: write the decision into the text before you request.
- A document holds one open request. A new version moves it and leaves it `waiting on agent`. Run the command again when "Approval of a spec" allows, to hand it back. Your own thread reply hands it back too, unless you reply with `--reply-to-ask` and `--turn agent`. A `--reply-to` reply takes no turn flag, so it takes the default, `human`. Exception: a new version moved the request since your last `dispatch request-approval`. Then no reply hands it back: run `dispatch request-approval` again. While it waits on the human, a different `--summary` is refused.
- The answer arrives as `artifact.approved` or `artifact.changes_requested` with its version. `changes_requested` carries the reason: that is your next work. `stale` means approved, then changed.
- A human's own answer to a block can carry their approval forward. That needs no change outside the blocks, and their answer on every new or answered block. Then `dispatch doc-read` shows `Approval: approved vN`. Anything else leaves it stale, and the card shows a diff: make the next `--summary` one sentence on what changed.

## A document that is reloading

- `DOC_SERVICE_UNAVAILABLE` (503) means the document's live room is reloading. Your call wrote nothing. `dispatch` already asked again under the call's key, a few times over about 15 seconds, and this is the answer it kept getting. Wait a few seconds and repeat the same call.
- A second refusal in a row: tell the human, with the document's reference.
- It comes from calls that write a document. Those are `doc-edit`, a quoted `ask` or `comment`, a reply in an anchored thread, and `suggest`. Also `resolve-comment` on an anchored comment, an `artifact` that replaces a document, and `edit-ask` / `resolve-ask` on a block ask. `doc-read` returns it only when the durable copy is unreadable.
