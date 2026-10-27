---
title: "Ask the parser whether a streamed Markdown block is still open, with a probe line inside the last line's containers"
category: dispatch-web
tags:
  - markdown
  - commonmark
  - streaming
  - live-view
date: 2026-10-27
status: active
module: packages/dispatch/web
related_issues:
  - "INFRA-528"
  - "sjawhar/legion#2311"
---

# Ask the parser whether a streamed Markdown block is still open, with a probe line inside the last line's containers

Whether a body ends inside an open fence (a model still streaming it) is a question only the
parser answers: never read the source's last line for a closing fence.

- Only when the parse's last leaf block is the kind you care about, parse the body again with a
  probe line after it: a fixed string no author writes (wrap it in invisible separators such as
  U+2063).
- Put the probe in the last line's containers: keep its `>` quote markers, turn its list markers
  into spaces (the item's content indent), and add no blank line before it. A bare or blank line
  ends an open quote or list item, and the fence with it.
- The block is open exactly when the probe lands inside it (the re-parse's last leaf block ends in
  the probe). A re-parse the engine refuses counts as closed.
- Re-parse with the same engine and options that produced the body, passed in as a function.

## Evidence

sjawhar/legion#2311, `endsInOpenMermaidFence` in
`packages/dispatch/web/src/features/refs/mermaid-diagrams.ts` (fix 9607c1b9). The text-reading
version failed the tester three ways: a closed fence followed by a link reference definition
(which is no block, so the source's last line is the definition) never drew, and a last line that
cannot close the fence (three backticks under four, `~~~` under backticks) drew a still-open block.
The probe without container markers made a quoted or listed fence read as closed (the unit case
`a mermaid fence still open inside a quote or a list item keeps it as code` fails with it).
