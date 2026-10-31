---
title: "A line that starts with a block tag name opens an HTML block even inside a code span, so check guides with a parser"
category: best-practices
tags:
  - markdown
  - commonmark
  - html-block
  - escaping
date: 2026-10-31
status: active
module: scripts
related_issues:
  - "sjawhar/legion#2474"
---
# A line that starts with a block tag name opens an HTML block even inside a code span, so check guides with a parser

Extends docs/solutions/best-practices/a-rewriter-that-must-not-change-rendering-is-safe-by-what-it-refuses-and-by-the-renderer-readers-use-pr2422.md.
That file refuses to write such a line; this one is about a guide that already holds one.

- Never let a line of a guide start with `<` and a block-level tag name (`<dir>`, `<p>`, `<div>`, `<source>` …), even inside a code span cut by a line break.
  Block starts are decided before inline parsing, so the line opens a CommonMark kind-6 HTML block: GitHub shows the rest of the paragraph as raw HTML, and every line-based check skips it.
- Find such lines with a Markdown parser, never a regex: an `html` node whose parent is a block container (`root`, `listItem`, `blockquote`, `footnoteDefinition`).
- When text moves from a code fence or a table cell into Markdown prose, put every token holding `<`, `>`, `*`, `|` or `\`, or starting with `@`, in a code span, and prove with a renderer that each `<placeholder>` still renders.
- A checker that stops checking part of a file prints where, on stderr, without changing its exit code.

## Evidence

- Main's hard wrap of `docs/kubernetes.md` put `<dir>` at the start of two lines inside code spans, so two paragraphs rendered as raw HTML and were never checked; sjawhar/legion#2474 joins those line breaks (`html-block-k8s-workspace-init-log`, `html-block-k8s-volume-mount-refusal` in the issue's `restructure.ts` at 119c52c7).
- The parser test: `scripts/semantic-line-breaks.guides.test.ts`, "the root guide, the e2e README and the Kubernetes doc hold no raw HTML block, which the checker skips".
- Converting the root guide's `## Commands` fences code-spanned `<tailnet>`, `<0600 file>` and `@biomejs/biome`; the rendered-text check found all 120 `<…>` placeholders, and failed with the code span removed from one.
- Converting tables showed two README rows of main whose unescaped `|` inside a code span (`<on|off>`) made GitHub drop the rest of the row.
- The warning: `warnVerbatim` in `scripts/semantic-line-breaks.ts`, tested by "check warns at the verbatim tag's own line in a multi-line paragraph and keeps exit 0".
