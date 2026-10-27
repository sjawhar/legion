---
title: "Sanitize a third-party renderer's SVG by allowlist, filter what CSS can fetch, and scope it in a shadow root"
category: dispatch-web
tags:
  - security
  - svg
  - sanitizer
  - shadow-dom
  - untrusted-input
date: 2026-10-27
status: active
module: packages/proof-editor
related_issues:
  - "INFRA-528"
  - "sjawhar/legion#2311"
---

# Sanitize a third-party renderer's SVG by allowlist, filter what CSS can fetch, and scope it in a shadow root

When a library turns author text into SVG markup that a page inserts:

- Parse it with the target document's own `DOMParser` as `image/svg+xml`; throw on a parse error
  or a root that is not the SVG namespace's `svg` by exact, case-sensitive name.
- Keep only the elements the library is measured to emit (render one sample of every type it
  draws and list them), matched by exact local name; remove everything else with its subtree.
- Drop every `on*` and `href` attribute, and every attribute except `data-*` whose value holds a
  CSS escape (`\`, since `\75rl(` is `url(`) or a fetching function (`url(…)` other than a local
  `url(#id)`, `image-set(`, `image(`, `cross-fade(`, `src(`, `element(`): author styling lands in
  presentation attributes. Strip `@import` from `<style>`.
- Mount it in an open shadow root, so the library's `<style>` and marker ids stay inside that one
  diagram.
- Test the sanitizer with hostile markup directly; a browser row whose payload the library already
  escapes passes with no sanitizer at all.

## Evidence

sjawhar/legion#2311, `packages/proof-editor/src/mermaid-svg.ts` and
`packages/dispatch/web/src/features/refs/mermaid-diagrams.ts` (`attachShadow`). beautiful-mermaid
copies a `classDef evil fill:url(https://evil.example/x.png)` and its escaped spelling straight
into `fill=` attributes and `@import`s Google Fonts in its `<style>`; upstream's private
`sanitizeSvg` blocks only five tags. Review round 1 also found the first version lowercased
element names, which let an `<SVG>` root through (fixed in aed1c65c).
