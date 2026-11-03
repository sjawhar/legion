---
title: "Code that copies the Postgres lexer is tied to one major, so diff scan.l on an upgrade"
category: database-issues
tags:
  - postgres
  - postgres-18
  - lexer
  - migrations
  - go-regexp
date: 2026-10-28
status: active
module: packages/envoy/internal/pgmigrate
---

# Code that copies the Postgres lexer is tied to one major, so diff scan.l on an upgrade

- When the database moves to a new major, diff `src/backend/parser/scan.l` between the two
  majors' stable branches and port every changed character set into code that copies it. From
  Postgres 17 on, `space` and `non_newline_space` include `\v`.
- Send every whitespace test through one predicate; a second literal list drifts.
- Do not hand raw SQL whitespace to a Go regexp: RE2's `\s` is `[\t\n\f\r ]`, without `\v`. Write
  each whitespace token as one space, which keeps the offsets.
- Test the same statements with every space replaced by `\v`, and assert the same result as the
  spaced form.

## Evidence

- `packages/envoy/internal/pgmigrate/scan.go` modelled the 16 lexer: `isSQLSpace` (line 110-112)
  lacked `\v`, and the quote-continuation case (line 186) had its own literal list. Both now take
  `\v`, the second through `isSQLSpace`.
- `touched.go:394` copied raw whitespace into the text its `\s` regexps read, so `alter\vtable x`
  reported no touched table. The simplify pass of sjawhar/legion#2364 (0898b38c → 0fa111cd)
  found it; it now writes each whitespace token as one space.
- `census_test.go` `TestTouchedTablesReadsVerticalTabAsSpace` and the vertical-tab cases in
  `set_test.go` fail without either change.
