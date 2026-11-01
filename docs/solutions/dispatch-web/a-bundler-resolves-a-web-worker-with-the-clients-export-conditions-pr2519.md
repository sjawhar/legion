---
title: "A bundler resolves a Web Worker with the client's export conditions"
category: dispatch-web
tags:
  - vite
  - web-worker
  - export-conditions
  - bundle
date: 2026-11-01
status: active
module: packages/dispatch/web
related_issues:
  - "sjawhar/legion#2519"
---

# A bundler resolves a Web Worker with the client's export conditions

Extends docs/solutions/dispatch-web/a-vite-web-worker-is-its-own-bundle-so-an-inventory-plugin-needs-a-worker-instance-pr2311.md.

That document covers the worker as its own bundle and what inventories it. This covers what that
bundle resolves to, and the one shape that makes it a bundle at all.

- Vite builds a worker with the client's export conditions, so a dependency's `browser` entry
  goes into the worker too. A `browser` module whose body touches `document` throws before the
  worker's first message, and nothing on the page sees why.
- Before moving a parser or an engine into a worker, read every transitive dependency's `browser`
  condition for DOM access at module scope.
- Pin the DOM-free entry with a `resolve.alias`, and resolve it from the package that actually
  consumes it rather than naming a subpath, because a package need not publish one.
- Prefer the entry the other runtimes already use, so one input reads the same above and below
  the worker's threshold.
- Write `new Worker(new URL("./x.worker.ts", import.meta.url), { type: "module" })` as one
  expression. Split in two, the worker's module is copied out as an asset instead of bundled as a
  chunk.

## Evidence

`packages/dispatch/web/vite.config.ts:14-34` (sjawhar/legion#2519), the comment on
`decode-named-character-reference`, which micromark decodes every named character reference
through: "The package's `browser` export condition is a module whose body is
`document.createElement('i')`, and Vite resolves a Web Worker's bundle with the client's
conditions, so the parse worker (features/refs/markdown-parse.worker.ts) would throw before its
first message. Its default condition answers from the `character-entities` table instead - the
table Bun, Node, the unit suites and the pmdoc fixture generator already run - so a body reads
the same above and below the parse worker's threshold, and the same as the generator that pins
Go's fixtures … for 11.6 KB gzipped on the page's bundle."

The resolution is by file, not by subpath, and the same comment says why: "The package publishes
no `./index.js` subpath, so the file is resolved rather than named: from the editor, which
`micromark-core-commonmark` is a dependency of, and from that package, which decodes references
with it. `require`'s own conditions hold no `browser`, so this is `index.js`." That is the two
nested `createRequire` calls at `:29-34`, and the alias
`"decode-named-character-reference": characterReferenceDecoder` at `:116`.

The one-expression rule is `newParseWorker`'s docblock in
`packages/dispatch/web/src/features/refs/markdown-engine.ts:265-274`: the `new Worker(new URL(…),
{ type: "module" })` "is written as one expression because that is the shape a bundler
recognises; split in two, the worker's module is copied out as an asset and never bundled". The
chunk it must produce is `markdown-parse.worker-<hash>.js`, which the browser rows that hold or
refuse the worker's answer address by that name
(`packages/dispatch/e2e/heavy-bodies.e2e.ts:52-60`).
