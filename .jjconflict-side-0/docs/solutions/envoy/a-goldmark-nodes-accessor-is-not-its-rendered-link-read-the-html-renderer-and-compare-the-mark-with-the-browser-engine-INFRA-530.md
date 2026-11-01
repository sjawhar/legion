---
title: "A goldmark node's accessor is not its rendered link: read the HTML renderer and compare the mark with the browser engine"
category: envoy
tags:
  - pmdoc
  - goldmark
  - remark-gfm
  - autolink
  - parser-parity
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "INFRA-530"
  - "sjawhar/legion#2307"
---

# A goldmark node's accessor is not its rendered link: read the HTML renderer and compare the mark with the browser engine

When you turn a goldmark node into a pmdoc mark or attribute (`parse.go`), do not use the AST
accessor's value as-is:

- **Read what goldmark's own HTML renderer writes for that node.** goldmark sometimes changes the
  value only when it writes HTML. `ast.AutoLink.URL()` gives an email autolink's bare address
  (`ast/inline.go:589`). `renderer/html/html.go:515` adds `mailto:` only in the HTML output. An
  accessor value that skips the renderer's change is a different link from the one every HTML
  reader and the browser editor make.
- **Compare the mark with the browser editor's engine, not only with a Go expectation.** A Go
  table that you write from Go's output (`linkify_test.go`'s `link alice@example.com` rows) locks
  in the divergence. Make each row check that `Parse(md)` equals `engineReadings(t, …)`
  (`render_readback_test.go:14`). Run it with `bun` on `PATH` and `bun install` done at the
  repository root: without them `engineReadings` skips outside CI, and a skip is not a red run.
- **Record the shapes you leave to another issue as `{markdown, agree: false}` rows.** Name the
  decision and the issue that owns them in the test's comment. When that issue edits the
  neighbouring test file, put the rows in a new file, so the owner can change each flag to true
  without a merge conflict.
- **Lock the stored bytes for both the old and the new form.** A changed reading must not mint a
  new version of documents already stored. Add rows that hold each form (`[a@b.test](a@b.test)` and
  `[a@b.test](mailto:a@b.test)`) to `TestRenderKeepsStoredMarkdownThatReadsBack`
  (`escape_test.go`).

## Evidence

Before INFRA-530, Go's `case *ast.AutoLink` used `current.URL(source)`. An agent's `first@b.test`
was therefore stored as `[first@b.test](first@b.test)`, and remark-gfm reads the same markdown as
`mailto:first@b.test`. At the tests-only commit 5158fd92, `TestAnEmailAutolinkIsAMailLinkInGoAndTheEngine`
failed on all 7 rows (`Go reads … link a@b.test …, the engine … link mailto:a@b.test`). The fix
in 8a431a76 prefixes `mailto:` for `ast.AutoLinkEmail`. goldmark's email table holds no `:`, so
the address never already has a scheme. The seven shapes where the two readings still differ
went to dispatch://INFRA-531 as `agree: false` rows in `email_readback_test.go`, a new file,
because INFRA-531 edits `render_readback_test.go`.
