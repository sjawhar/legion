---
title: "Goldmark's InsertAfter of a last child counts it twice; a parser harness counts panics apart and varies how inputs end"
category: envoy
tags:
  - goldmark
  - pmdoc
  - ast
  - panic
  - fuzz
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "INFRA-172"
  - "sjawhar/legion#2272"
---

# Goldmark's InsertAfter of a last child counts it twice; a parser harness counts panics apart and varies how inputs end

- Never add a node after a goldmark parent's last child with `InsertAfter`. Use
  `parent.AppendChild(parent, node)` when the sibling is the last child, and
  `parent.InsertBefore(parent, sibling.NextSibling(), node)` otherwise. `InsertAfter(self, v1, n)`
  is `InsertBefore(self, v1.NextSibling(), n)`, and `InsertBefore` adds one to `childCount` and,
  when its sibling is nil, calls `AppendChild`, which adds one again. `ChildCount()` then says one
  more child than the list holds, and code that walks `ChildCount()` steps dereferences nil.
- Where you can, count children by walking siblings, not by `ChildCount()`.
- A random-input harness for a parser counts `ErrPanic` (or any recovered panic) apart from its
  refusals, never as one, and generates how inputs start and end as well as their middles. A
  harness that folds panics into refusals reports a green run over a crash.

## Evidence

- `packages/envoy/internal/dispatch/pmdoc/table.go:421-428` (commit 21f5db10): the cell split
  after goldmark's table transform inserted the new cell with `InsertAfter`. A row such as
  `| a | b \\| \|` under two columns then made `keepEscapedClosingPipes`, which trusts
  `ChildCount()`, walk past the row, and `Parse` returned `ErrPanic` on untrusted markdown where
  the base read the row with no error. goldmark's own source: `ast/ast.go:376-386`.
- `table_test.go:308` (`TestParseReadsASplitRowEndingInAnEscapedPipe`) pins the wide-row refusal
  for that row and its neighbours.
- The tester's first generated differential missed the panic: every generated row closed with
  ` |`, and it counted `ErrPanic` as a refusal. With row endings varied and panics counted alone,
  the same harness found 5,877 panics at the head before the fix and none after it.
