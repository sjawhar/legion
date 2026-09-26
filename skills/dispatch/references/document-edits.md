# Editing a document

Every document a Dispatch tool writes — an issue's spec, a project document — is edited in place
with `dispatch_doc_edit`, never re-uploaded. [The Spec](../SKILL.md#the-spec) sends you here for
the tool's shape, how to target the text you mean, what each operation costs a block, and how to
reject a stale edit.

```ts
dispatch_doc_edit({ issue?, project?, artifact, ops, precondition?, summary? })
```
It returns issue or project-document owner details plus `applied`, optional `version`, `changed`, and
`unchanged_ops`. `ops` is an array of this exact `EditOp` shape:

```ts
type EditOp = {
  op: "replace" | "delete" | "insert" | "retype" | "move" | "delete_row" | "delete_column";
  find?: string;
  with?: string;
  occurrence?: number;
  markdown?: string;
  after?: string;
  before?: string;
  block?: string;
  index?: number;
  type?: string;
  attributes?: Record<string, unknown>;
};
```

An operation takes only these keys. A key it does not declare is refused before the call leaves
your process, naming the operation and its keys, because every key but `op` is optional: a
misspelled `with` would otherwise be dropped and the `replace` would delete the text you meant to
rewrite.

Target `replace`, `delete`, and quote insert anchors by a block's text as rendered: write inline
code without backticks, bold without asterisks, and link text without link syntax. A table-cell
anchor is its cell text. Quote code-block contents without their Markdown fences. A quote must stay
within one textblock; split changes that span separate blocks into separate operations.

`replace` requires `find` and `with`; `delete` requires `find` or `block`; `insert` requires `markdown` and exactly one of `after` or
`before`; `move` requires `block` and exactly one of `after` or `before`; and `delete_row` / `delete_column` each require a table
`block` plus a zero-based `index`. An insert or move anchor is a quote, `"start"`, `"end"`, `"heading:Title"`, or `"block:<id>"`.
Ordinary inserts create a sibling block before or after the quote, heading, or block's enclosing document block, and a move lands the
block at that same boundary; `"start"` and `"end"` select the document edges. At a table-cell quote, a body-row fragment (no header or
delimiter rows) extends that table before or after the matched row instead; short rows are padded, wider rows are rejected, and deleting
a cell's quoted text removes only that text. `delete_row` / `delete_column` instead mutate their named table in place, keeping the
table's block id. A row index includes the header: row `0` is the header and its deletion promotes the first body row. The last body
row and any row's last column cannot be deleted. An index is required. A missing, non-integer, negative, or out-of-range index is
`INVALID_OP` on `index`, naming the supplied value and the table's actual dimensions before making any change. Markdown parsing
canonicalizes short ragged rows by padding missing cells, so column deletion preserves every non-selected cell in the canonical table.
`GET /api/v1/artifacts/<artifact UUID>/blocks` reports a table's own references plus its descendant cell anchors. A row or column
deletion that would remove an open ask or unresolved comment anchor is `INVALID_OP` on `index`, naming the axis and anchor ids;
answered asks and resolved comments are history and do not block it. A `find` or quote anchor tolerates inline Markdown
(`**bold**`, `` `code` ``) and a leading `# ` selects a heading by its text; a miss names the quote and the three nearest blocks so
the next quote lands, and a `find` cut before a closing `**` or `` ` `` is refused as an unbalanced inline mark rather than reported
as a miss. A `heading:` anchor matches the whole heading text exactly — a prefix of a longer heading is a miss, naming the anchor and
the nearest headings. `replace` is inline: `with` is the new text of the matched span inside its block, so a marker of a *different*
kind from the block's own (`4. Design` written into a heading, `# Title` into a paragraph) stays literal text and never turns the
block into a list or heading. A `with` that opens with a marker of the *same* kind as the matched block's own would write it twice and
is rejected (`INVALID_OP` on `with`) — including prose that merely looks like a marker (`1999. was a year` into an ordered item),
which is written as text with a backslash escape (`1999\. was a year`) — omit the marker to replace the block's text, or use `insert`
plus `delete` to change the block's kind, level or number. The one exception is a heading rename whose `find` carried a heading
marker: `replace(find="## Old", with="## New")` gives `## New`. A different level in `with` applies only when `find` named the
heading's actual level — `find="## Old"`, `with="### New"` retitles and makes it an h3 — because `# ` is the level-blind selector,
so `find="# Old"` renames the text and keeps whatever level it selected. `with` that forms more than one
paragraph is rejected (`INVALID_OP` on `with`) — see the recipe for a multi-paragraph rewrite below; so is any non-empty `with` that
renders to no text, which a line indented four spaces or a tab does (markdown reads that as a code block), as does whitespace
alone. An empty `with` deletes the matched text on purpose; where the block holding it cannot be written without that
paragraph, the replace is `INVALID_OP`, and the refusal names the `delete` that removes it instead. Inside a code
block none of this applies: `with` is the code's literal text, written as sent, whitespace, markdown syntax and
references included, except that line breaks at the end of the code's text, and a line holding only whitespace in a
list item's code, do not survive the next read; and a line of three or more colons in code inside a typed block,
indented less than four columns from where the typed block's lines start, is `INVALID_OP`, since the browser editor
ends the typed block there - indent it four or more spaces (a tab reaches only the next tab stop, which inside a list
item or a blockquote can be two columns away), or move the code block out of the typed block. Text a `replace` writes
that would read as block syntax at a line start is stored escaped and reads back as the characters you sent: `---` over
a paragraph is stored `\---`, not a rule, so to add a rule, `insert` it beside the paragraph (`insert` with markdown
`***`). Use zero-based
`occurrence` for a
repeated target; re-read a missing or ambiguous target before retrying. Pass `summary` to name the version when recording a decision.

**Rewriting several paragraphs is one `replace` per paragraph, then a read-back.** `replace` is inline:
each `with` is the new text of one paragraph, and outside a code block a `with` that forms two paragraphs is
refused whatever the text says. Give each paragraph you rewrite its own `replace`, which keeps that paragraph's block id and every
anchor outside the text you rewrite. A comment or ask anchored to the text you rewrite loses its quote but keeps
its pin to the block, so the dashboard still shows it beside that paragraph; a delete (below) loses both. An
anchor that straddles the boundary keeps its mark over the words you left alone, with its quote shortened to
them: rewriting `charlie delta.` under a comment on `bravo charlie` leaves that comment quoting `bravo `. When
the new text has more paragraphs than the old, write them as **one** `insert` whose `markdown` holds them all,
anchored on the last paragraph you rewrote with `after` quoting it exactly as it now reads (the operations in
one batch apply in order): each separate `insert` at the same anchor lands directly after it, so two of them
come out in the reverse of the order you wrote. An insert lands after the top-level block that holds the quote,
so beside a paragraph inside a list item or a typed block it goes after the whole list or block. When it has fewer,
`delete` each leftover paragraph, with its whole text as `find` or its id as `block`. All of that holds while
the new text is paragraphs: `replace` keeps a block's kind, so any block that is not a paragraph — a heading,
list, table, blockquote, rule, code fence or typed block — cannot be replaced into place. Its marker is written
as literal text, except a backtick fence, which becomes an inline code span whose text is everything inside it,
info string and line breaks included (```` ```go\nx := 1``` ```` becomes the code span `go` + a line break +
`x := 1`), and a tilde fence, which stays literal.

Outside a code block, HTML is not written as text at all, and a `replace` whose HTML would open a block where it lands is refused
(`INVALID_OP` on `with`), because the schema carries no block HTML. `<div>x</div>` and `<!-- note -->` open one
at the start of a paragraph or a list item and on the line after a hard break; a tag such as `<br>` opens one
only when it stands alone as a paragraph or a list item. The same HTML inside a line, in a table cell or in a
heading is inline HTML and is kept as written.

A hard line break in `with` (two trailing spaces or a backslash before a newline) is refused in a heading or a
table cell (`INVALID_OP` on `with`): both are written on one line, so the break would end the block there. Write
the text without the break, or `insert` a new block after this one.

When the new text adds a block that is not a paragraph beside paragraphs, `insert` it beside the
paragraph you replaced, which keeps that paragraph's id; only when no paragraph of the new text is left to take
the old block's place is it a `delete` of the old block and then an `insert` of the new one, anchored on the
block before or after it. The delete is what costs the id (below); a typed block keeps its id when the insert
carries it, which works only in that order, because an insert carrying an id the document still holds is refused.
Then read the document back with
`dispatch_doc_read` and read the passage and its neighbours, not a grep for the words you added: an empty
`with` deletes the matched text on purpose where the block allows it, so a `replace` whose `with` you meant to fill
empties that paragraph — the block and its id stay, holding nothing — and only a read shows what the document now says.

A batch that leaves the document's semantic identity unchanged — including its inline anchor marks, so an edit that only orphans a
comment or ask anchor still mints its version — mints no version, named or not: the response carries
`changed: false` with `unchanged_ops` naming each operation that did nothing, and the tool result says nothing changed. A `summary`
does not force a version for such a batch; `POST /api/v1/artifacts/<id>/versions`, which names the current state on purpose, still does.

A `delete` whose `find` is a block's entire text removes the block itself — the bullet, paragraph, or heading, not just its words — and
a list emptied of every item disappears with it; a partial match keeps the block with its remaining text. Deleting the text of a bullet
that holds a nested list hoists that list's items into the bullet's place (as an outliner does); a bullet with any other content
(paragraphs, code, tables) is refused with `INVALID_OP` naming `delete {block:"<item id>"}`, which removes the item with its content.
`delete` with `block` removes any block by id (paragraph, heading, list, list item, table, or typed block; deleting an open `ask` block
retracts its ask, while an answered one keeps its answer as the record), and `move` with `block` relocates one, keeping its id and
attributes — a moved `ask` keeps its ask and answer. **A block loses its id only when it is removed**, and its anchors go with it:
`delete` by text or by id removes the block and any container it empties; `delete_row` / `delete_column` remove their cells' ids,
which is why they are refused while an open ask or unresolved comment sits on them; and a `move` that takes the last block out of a
blockquote or list item removes that emptied container, the list too when no item remains, and each enclosing container that
held nothing else (`> - Only.` loses the blockquote as well as the item and the list). The moved block itself keeps its id,
as do `replace` (an accepted empty `with` and a heading-level change included), `insert` and `retype`; `retype` carries the
paragraph's id onto the typed block it becomes. Block ids are the `#id` a typed block renders
(`:::ask{#5467e5ce-…}`) and, for every block including untyped ones, the `id` rows from
`GET /api/v1/artifacts/<artifact UUID>/blocks` (or `/api/v1/issues/{key}/artifacts/{slug}/blocks`), each with its `type` and byte range
in canonical markdown; the UUID route does not accept a slug. A later operation in the same atomic batch that names a block removed by
an earlier `delete {block}` fails as `INVALID_OP` naming the earlier operation and the parent block that cascaded the removal. A move
whose anchor lies inside the moved block, or a delete that would leave a typed block without the body its content rule requires, is
`INVALID_OP` naming the field and the rule.

`GET /api/v1/artifacts/<artifact UUID>/blocks` includes a full-state `token` on every block, including
inline marks. To reject a stale edit, pass `precondition` with exactly one of
`{ document: "<token from dispatch_doc_read>" }` or
`{ blocks: [{ id: "<block id>", token: "<block token>" }] }`. The server resolves the whole batch before
mutation: a block guard must cover every content block it changes, or Dispatch returns
`400 INVALID_PRECONDITION` without applying anything. Use a document token for insert and move because they
depend on document order. A block token lets other sections change concurrently; a new anchored ask or comment
changes the relevant token. A stale guard returns `409 PRECONDITION_FAILED` with each mismatch and current
token; Dispatch applies no part of that batch. It is the hashline `#TAG` property applied to stable block ids,
not line numbers: canonical Markdown lines shift under concurrent edits and rendering changes, while block ids
survive moves and retyping.

`retype` turns the paragraph or typed block with `block` into the named typed `type` in place. It keeps the
block id, keeps a typed block's body, and uses `attributes` for client-owned typed attributes. Use it when
an existing paragraph is the question that should become a decision.
