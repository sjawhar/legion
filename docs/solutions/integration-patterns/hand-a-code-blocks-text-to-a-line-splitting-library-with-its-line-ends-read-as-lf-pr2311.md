---
title: "Hand a code block's text to a line-splitting library with its line ends read as LF, and key the cache on that text"
category: integration-patterns
tags:
  - line-endings
  - markdown
  - mermaid
  - cache-key
date: 2026-10-27
status: active
module: packages/dispatch/web
---

# Hand a code block's text to a line-splitting library with its line ends read as LF, and key the cache on that text

Extends docs/solutions/integration-patterns/cut-source-text-by-mdast-offsets-from-the-text-micromark-counted-INFRA-529.md.

- A parsed code block keeps its author's line ends: a lone CR or a CRLF stays in the node's text.
  Where that text leaves for a library that splits lines at LF alone, replace `\r\n?` with `\n`
  first.
- Use the converted text as the cache key and the request too, so one diagram written with CRLF
  and with LF is one entry and one request.
- Any helper that finds "the last line" of a body ends a line at LF or CR (take the later of
  `lastIndexOf("\n")` and `lastIndexOf("\r")`; a CRLF's LF follows its CR).

## Evidence

sjawhar/legion#2311. beautiful-mermaid refused `graph TD\r  A --> B` and
`sequenceDiagram\r  Alice->>Bob: Hi` (`Invalid mermaid header`) while their LF and CRLF forms drew.
Dispatch stores comment and message bodies as sent, so a lone-CR comment's closed diagram showed the
refusal in Chromium. That had been true since the first drawing commit; it surfaced only once the
open-fence probe's own lone-CR fix (6b9f6b50) let the quoted case reach the renderer. Fixed in
59dacf3a, `packages/dispatch/web/src/features/refs/mermaid-diagrams.ts` (the `sources` map); the
unit case `a mermaid block of a body with lone-CR line ends draws its diagram` fails without it.
