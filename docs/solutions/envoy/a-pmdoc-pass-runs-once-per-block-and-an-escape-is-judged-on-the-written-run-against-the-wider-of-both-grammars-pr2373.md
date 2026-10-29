---
title: "A pmdoc pass runs once per block, and an escape is judged on the written run against the wider of both grammars"
category: envoy
tags:
  - pmdoc
  - goldmark
  - micromark
  - parser-parity
  - escapes
  - raw-html
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/pmdoc
---

# A pmdoc pass runs once per block, and an escape is judged on the written run against the wider of both grammars

- **A pass in an inline parser's `CloseBlock` treats child blocks as opaque.** goldmark calls
  `CloseBlock` on every block, the innermost first, then each container around it, then the
  document. A pass that walks the whole subtree of `parent` reads a paragraph's text again from its
  list item, its quote, its table cell and the document, and the second read sees the nodes the
  first one made. Skip every node with `node.Type() == ast.TypeBlock` below `parent`, so each block's
  text is read once, by its own close.
- **An escape whose construct can close in a later node is decided on the run as written.** A
  check inside one text node (`angleConstruct`) cannot see a `>` that a link, a split-off address or
  the next text supplies. Record each candidate opener while the run is written, scan the written
  bytes after the run, and write the run once more with the openers that formed a construct
  escaped (`writeInlineRun`). A run that already reads back has no such opener, so its bytes stay.
- **Where goldmark reads less than the browser engine, escape what either reader accepts.**
  goldmark reads a declaration only after `<!` and an uppercase letter, and refuses a control
  character in an unquoted attribute value; micromark (CommonMark 0.31) accepts both. Ask goldmark
  only for the constructs it reads the same or wider (closing tags, comments, processing
  instructions, CDATA), and decide the others by CommonMark's grammar. An extra `\<` reads back as
  text in both readers, so over-escaping by one grammar is safe; under-escaping stores markdown the
  editor reads as HTML or cannot open.
- **A line's first character is also judged by the engine's block rules.** At a block's first
  line, after its container prefix, micromark tries an HTML block (html-flow, seven start kinds,
  kind 7 only where it does not interrupt a paragraph) before any inline rule. Check those start
  conditions on the whole first line as written (`htmlBlockStart`). Take "first" from the writer's
  state, not the byte offset: the text after a task item's `[ ] ` is inside the paragraph, where no
  block opens, so it gets no such escape (`renderer.checkboxEnd`).
- **Prove each of these against the engine, not against goldmark.** A row in a read-back test that
  only Go reads back passes while the editor shows an html node or refuses the document. Assert the
  rendering reads back as the tree in Go and in `engineReadings`, and for a shape main stored,
  assert the bytes are main's.

## Evidence

- Pull request #2373, commit 62dee6e2: `linkTextAddresses` (`autolink_transform.go`) searched
  `parent`'s whole subtree from `linkifyGuard.CloseBlock`; `a\_b@c.dd\.@y.zz` linked `.@y.zz`,
  which the engine leaves as text. The simplify pass found it, not a test. The fix is the `opaque`
  line in `linkTextAddresses`, pinned by the `a\_b@c.dd\.@y.zz` rows of `email_readback_test.go`
  (paragraph, list item, quote, table cell).
- Commits 495f97b1 and 3ff56757: `&lt;javascript:void(a@b.cc)>` was written
  `<javascript:void([a@b.cc](mailto:a@b.cc))>`, one autolink to a `javascript:` address, and
  `Set \<input value=me@x.cc> in the form.` became raw HTML. Each cost a review round. Rows in
  `TestALinkReadFromABareAddressAfterPunctuationIsWrittenBack`.
- Commit 28ae264d: `Set \<!doctype x;a@b.cc> now` and `\<a b=\x01;a@b.cc>` were stored raw because
  `rawHTMLScan` asked goldmark alone; the engine read an html node or no document. `openTagPattern`
  and the declaration rule in `rawHTMLCloser` (`render_inline.go`) carry the wider grammar.
- Commits 7fd13b50 and b80be208: `\<span title=a=b;a@b.cc>` at a block's start opened an HTML block
  in the engine (micromark's `=` quirk in kind 7), and the first fix then escaped
  `- [ ] <!-- todo later`, which main stored as written. `htmlBlockStart` (`escape.go`) matched
  micromark on 200,000 random lines.
