# Editing a document

Edit every Dispatch document (an issue's spec, a project document) in place with `dispatch doc-edit`. Never upload it again. `dispatch doc-edit --help` has the full rules for each operation: markers, task boxes, hard breaks, anchors and preconditions.

```bash
dispatch doc-edit --issue <KEY> --artifact spec --summary '<names the version>' --ops-json-file - <<'EOF'
[{"op": "replace", "find": "old sentence", "with": "new sentence"}]
EOF
```

- Ops: `replace`, `delete`, `insert`, `retype`, `move`, `delete_row`, `delete_column`. Keys: `op`, `find`, `with`, `occurrence`, `markdown`, `after`, `before`, `block`, `index`, `type`, `attributes`. An unknown key is refused before sending. A misspelled `with` would turn the `replace` into a delete.
- The output names the version, `unchanged_ops`, `lost_ops`, and a `Document token:`.
- Use `--summary` to name a version that records a decision.

## Targeting text

- Quote text as rendered: inline code without backticks, bold without asterisks, link text without syntax, a cell by its text, code without fences. One quote stays in one textblock. Split changes that span blocks.
- A miss names the three nearest blocks. A `heading:` anchor must match the whole heading. Use zero-based `occurrence` for a repeat. Re-read before you retry a missing or ambiguous target.
- `dispatch doc-read --blocks` lists each live block's id, type, token, anchor count and opening words, and each table row and cell index. Only live ids address an edit.

## Rewriting paragraphs

- **One `replace` per paragraph, then a read-back.** A `with` of two paragraphs is refused. Each `replace` keeps the block id and the anchors outside the text you change. An anchor on changed text loses its quote but keeps its pin.
- More new paragraphs than old: add the rest as **one** `insert` holding them all, `after` the last rewritten paragraph as it now reads. Two inserts at one anchor come out reversed. An insert lands after the top-level block that holds the quote.
- Fewer: `delete` each leftover paragraph by its whole text or `block` id.
- `replace` keeps a block's kind. When the new text adds a non-paragraph block (heading, list, table, quote, rule, fence or typed block) beside paragraphs, `insert` it beside the paragraph you replaced. Only when no paragraph of the new text stays, `delete` the old block, then `insert` the new one, anchored on the block before or after it.
- An empty `with` deletes the matched text on purpose and leaves an empty block. Read the passage and its neighbours back with `dispatch doc-read`. A grep for your new words is not a check.
- Dispatch refuses block HTML (`<div>`, `<!-- -->`, a lone `<br>` paragraph) in `with`. Inline HTML stays.
- To add a rule, `insert` `***`. A `---` in `with` is stored as `\---`.

## Blocks and ids

- A block loses its id only when deleted: by `delete`, by `delete_row` / `delete_column` (its cells), or by a `move` that empties its container. Its anchors go with it. `replace`, `insert`, `move` and `retype` keep ids.
- `delete` of a block's whole text deletes the block. A partial match keeps it. A bullet with a nested list hoists the list. A bullet with other content needs `delete {block:"<item id>"}`.
- Dispatch refuses a `delete` or `retype` that takes out an open ask's block, delete-then-insert included. Reword with `replace`, relocate with `move`, or use `dispatch edit-ask`. Moving an open block out of a container and deleting the container takes two calls.
- `retype` turns a paragraph or typed block into a typed block in place, keeping its id. Use it when a paragraph is the question that should become a decision.
- An ask block has two ids. The block id (`:::ask{#<id> …}`) goes bare in `block` for `move` / `delete` (`block:<id>` only in `before` / `after`). `dispatch read --ref dispatch://KEY/ask/<id>` takes either id, whole or as a unique prefix of 8 or more hex characters, and prints the ask id as `Ask id:`. `dispatch open-asks` lists ask ids. `edit-ask`, `resolve-ask`, `follow` and `--reply-to-ask` take only the ask id.
- A block id you write as `{#<id>}` holds at most 256 bytes. Dispatch refuses a longer one you add, and names the bound. Give the block a shorter id, or omit `{#…}` and Dispatch mints one.

## Tables

- `delete_row` / `delete_column` take the table `block` and a zero-based `index`. Row 0 is the header. The last body row and a row's last column cannot go. Open asks or unresolved comments on the cells block it.
- Inserting bare body rows (no header or delimiter) at a cell quote extends that table. A wider row is `TABLE_WIDTH`. A full table with a header pasted there becomes a second table.

## Stale and concurrent edits

- Guard with `--precondition-json`: `{"document": "<token>"}` or `{"blocks": [{"id": "<id>", "token": "<token>"}]}`. Use a document token for `insert` and `move`. A block guard must cover every block the batch changes.
- Chain guarded edits by passing each output's token to the next. Read once, not once per edit.
- `409 PRECONDITION_FAILED` or `409 EDIT_LOST_TO_CONCURRENT_CHANGE`: nothing written. Re-read and decide again. Non-empty `lost_ops` on success: a browser deleted that text after the write. Re-read before you build on it. A result that could not check survival: re-read.
- A batch that changes nothing mints no version, even with `--summary`. `POST /api/v1/artifacts/<id>/versions` names the current state as a version on purpose.

## Edge cases

- Inside a code block, `with` is the code's literal text. Trailing line breaks, and whitespace-only lines in a list item's code, do not survive a read.
- A CR LF or lone CR in any text you write is stored as a line feed.
- Replacing a non-paragraph block writes its marker as text. A backtick fence becomes an inline code span. A tilde fence stays literal.
- An anchor that straddles the rewritten text keeps its mark on the words you left, with a shorter quote.
- Emptying a task item's first paragraph while another block follows it is `INVALID_OP`.
- A later op in one batch that names a block an earlier `delete {block}` took out fails as `INVALID_OP`. So does a move anchored inside the moved block, or a delete that leaves a typed block without its required body.
- An op that only takes text out is never reported lost.
