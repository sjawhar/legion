# @legion/proof-editor

Dispatch's document editor: the Milkdown/ProseMirror entry point, the typed-block grammar and the
block-id feature. A placement red-team ruled all three CONSUMER (LEGION-287), so they live here
rather than in the `sjawhar/proof-sdk` fork, and a Dispatch editor change no longer costs a fork
release, an npm publish and a pin bump.

## Where the code came from

Two different commits of `sjawhar/proof-sdk` matter here, and they are not the same fact.

The files in the table below were copied byte for byte out of
**24a5fc94915cda5704897c497af6312c140db40d**, the fork's v0.3.13 — the last commit that still
held them. The cut that followed deleted them from the fork, so that commit stays the audit
source however far the fork line moves.

The upstream modules this package imports at runtime come from the git dependency, pinned at
**06fc1977f1fee637d92c6393965555e88ab9adcb** on the fork's cleaned `library` line. Moving that
pin changes nothing in the table; it changes what `node_modules/proof-sdk-upstream` holds, and
`tests/upstream-pin.test.ts` is what checks it.

| File | What it is |
| --- | --- |
| `src/lib.ts` | The library entry: builds the Milkdown editor and its plugin stack, binds collab, exposes the handle |
| `src/lib-headless.ts` | The DOM-free engine: parse and serialize markdown, used by the SPA's reference bodies and by the pmdoc fixture generator |
| `src/dispatch-*.ts` | Dispatch's host contract: the selection action bar, mark events, `dispatch://` links, soft breaks, the popover hook |
| `src/lib-remark-*.ts` | The remark plugins the entry installs (container directives, frontmatter) |
| `src/block-schema.ts`, `src/typed-block-commands.ts` | The typed-block grammar and its commands |
| `src/editor/schema/block-ids.ts` | Block ids: minting, the DOM attribute, the duplicate-id repair rule |
| `src/lib.css` | The editor stylesheet |
| `src/tests/*.test.ts` | The suites that came with those files |

Seven files under `src/` are legion's own rather than copies, so the file-by-file audit below does
not reach them:

| File | What it is |
| --- | --- |
| `src/collab-cursor-plugin.ts` | The peer-cursor plugin: y-prosemirror's, except that a peer's caret is not drawn while it sits on the focused local caret, where Chromium and WebKit otherwise drop or misplace typing (LEGION-289); it is also biome-checked |
| `src/editor/schema/dom-attributes.ts` | `withDomAttributes`, the DOM-output-spec helper lifted out of `block-ids.ts` so the typed-block schema can use it too |
| `src/trailing-newline-input.ts` | Types over a selection that would leave its text block ending in a newline, where Firefox otherwise puts the text before a code block's newline or deletes a paragraph's hard break (LEGION-289); it is also biome-checked |
| `src/editor/schema/uuid.ts` | `uuidV4`, the block-id default generator: a v4 UUID from `crypto.getRandomValues`, which every browsing context defines, where `crypto.randomUUID` exists only in a secure context, so a document opened over plain HTTP by a LAN address or a host name minted no id (LEGION-461); it is also biome-checked |
| `src/record-mark-history.ts` | The plugin that keeps the composer's own record-mark writes out of undo history in both undo managers, so neither undo nor redo writes one back: a write of `proofComment`, `proofSuggestion` or `dispatchAsk` steps that takes nothing from another record (its removals are of the open composer's own mark, `setComposerMark`, or put straight back, as upstream's suggestion restamp does), and `removeRecordMark`'s removal; a write that cuts into another record's mark keeps its undo step (LEGION-363, LEGION-458); it is also biome-checked |
| `src/record-mark-retype.ts` | Retyping a provisional record mark for the margin composer's Comment / Suggest / Ask switch, and the precise span-by-span removal behind the handle's `removeMark`, over text and inline atoms such as an image alike (LEGION-363); it is also biome-checked |
| `src/tests/harness.ts` | The `bun test` registration the copied suites call instead of their own `test()` tally |

Eleven kinds of edit are allowed in the copied files, and no others: the import specifiers of
upstream modules; `bun test` registration in the suites (`src/tests/harness.ts` replaces each
file's own `test()` tally and its `process.exit` tail); `src/tests/headless-no-dom.test.ts`,
whose entry named the fork's built `dist/headless.js` and now names `../lib-headless.js` — its
banner, the comment beside that import and the case's own name follow, since this package has no
build and there is no distribution left to name; `src/editor/schema/block-ids.ts`'s two
consumer-side changes, which are what the move made necessary — it extends the upstream
`code_block` and `frontmatter` schemas itself (the fork used to do that in its own modules) and
reads `withDomAttributes` from `./dom-attributes`; `src/editor/schema/block-ids.ts`'s default
generator being `uuidV4` from `./uuid` in place of `crypto.randomUUID` (LEGION-461, the table
above); the copy-attributes fix legion #1452 lifted into `src/block-schema.ts` and
`src/tests/block-schema.test.ts`, which is the largest divergence
in the tree (136 lines in the module) — `typedBlockSpec` moved out of `blockSchemaPlugins`'s
`$nodeSchema` callback and exported so a test can build the spec without a Milkdown ctx,
`attributeText` lifted out of `markdownAttrs`, the new `domAttributeName`, `domAttrs` and
`parsedDomAttrs`, the parse rule's `getAttrs` and `contentElement`, the `withDomAttributes`
wrapper on `toDOM`, and the suite's cases for all of it; `src/lib.ts` installing its peer
cursors through `collabCursorPlugin` from `./collab-cursor-plugin` instead of calling
`yCursorPlugin` itself; `src/lib.ts` installing `trailingNewlineInputPlugin` from
`./trailing-newline-input`; `src/lib.ts` installing `recordMarkHistoryPlugin` from
`./record-mark-history`; `src/lib.ts` routing `removeMark` and the new `retypeMark` through
`./record-mark-retype`, and the new `setComposerMark` through `./record-mark-history`; and
annotations, casts and assertions
that make a file type-check, each of which erases before runtime (below).

To audit a copied file, diff it against `jj --ignore-working-copy -R <proof-sdk> file show -r
24a5fc94 root:src/<file>`; every file in the copied-files table has that counterpart, and the
only lines that differ should be the eleven kinds.

`scripts/`, `tests/` and `upstream/` are legion's own. `scripts/` and `tests/` are linted and
type-checked like any other package's; `upstream/` is generated, and Biome is off over it the
way it is over the copy. `tests/upstream-pin.test.ts` is the guard on the pin: it reads the
installed dependency and fails when a fix the pinned line carries has gone missing, or when
`upstream/` stops being what the pinned sources emit. `tests/upstream-boundary.ts` has no
runtime at all — `bun run typecheck` is what runs it, and it fails when a name crossing the
boundary is `any` again.

## The upstream boundary

Everything `src/` imports as `proof-sdk-upstream/src/…` — the mark plugins, the mark popover,
`editor/schema/{proof-marks,code-block-ext,frontmatter}`, `formats/*` — still belongs to the fork
and comes from a git dependency pinned to that same commit. Three resolvers reach it, and each
needs telling, because proof-sdk's `package.json` `exports` publishes only its built `dist`:

| Consumer | Mechanism |
| --- | --- |
| Bun (`bun test`, the pmdoc generator) | `paths` in `tsconfig.json` |
| Vite (the Dispatch SPA) | `resolve.alias` for the raw sources and `dedupe: ["prosemirror-model"]` — source-only editor modules and Dispatch both pass ProseMirror nodes into `DOMSerializer`; two module identities make historical renders fail with “multiple versions of prosemirror-model were loaded” |
| tsc | `paths` in `tsconfig.check.json` and in `packages/dispatch/tsconfig.json`, both naming `upstream/` |

**No TypeScript program may open those sources.** They do not type-check — the fork used to emit
its declarations with `noCheck`, and its own `tsconfig.json` sets `noEmit`, so nothing ever
checked them; a consumer that resolved them inherits 38 errors it cannot fix.

`upstream/` is how tsc sees them anyway. `scripts/upstream-declarations.ts` runs
`tsc --emitDeclarationOnly --noCheck` over the modules `src/` imports and their closure: `noCheck`
is what lets tsc read a tree it refuses to check, and what it writes is each value's and type's
real shape, inferred from the pinned sources rather than restated by hand. The output is
committed — it is what every tsc program and every editor reads, with no build step to run first
— and `tests/upstream-pin.test.ts` re-runs the generator in `--check` mode, so a moved pin that
changes the surface fails until `bun run upstream-declarations` rewrites it.

So an upstream type reaching this package, or reaching Dispatch through it, is the type the fork
declares. `StoredMark`, which `src/lib.ts` re-exports, and `HeatMapMode`, which
`CreateProofEditorOptions.heatMapMode` names, come straight from the upstream modules, and no
copied file carries `// @ts-nocheck` any more. `tests/upstream-boundary.ts` is the guard: it
names every binding this package imports across the boundary and fails `bun run typecheck` if
one of them is `any`.

The pinned source commit carries two fixes, each an open upstream pull request. The Dark Reader
fix (EveryInc/proof-sdk#81): peer-cursor colours and mark decorations avoid inline `style`
attributes, because Dark Reader rewrites those attributes inside the contenteditable and
ProseMirror reads the writes as content mutations. That can cause an endless redraw loop that
wedges the tab. The proof-mark rendering fix (EveryInc/proof-sdk#82): the five proof marks render
only their `data-*` attributes, where upstream renders `id`, `kind` and `by` as
`[object Object]`, and a replace suggestion's widget redraws when its replacement changes.
`tests/upstream-pin.test.ts` reads or runs the installed modules so a pin that loses either fix
fails. Moving the pin changes the package dependency, `bun.lock`, `upstream/` and this file's
source audit references; no Bun patch applies to this source.

## No build step

`exports` serves `src/*.ts` directly, the way `@legion/contracts` serves its own sources: Bun runs
TypeScript, Vite compiles it. There is no `dist` and nothing to build before using the package.
`upstream/` is generated rather than built: it is committed, and only a moved pin rewrites it.

## Commands

```bash
bun run test                   # the moved suites through src/tests/harness.ts, plus tests/
bun run typecheck              # tsc against tsconfig.check.json (src/, tests/ and scripts/)
bun run lint                   # Biome over the package; the root biome.json turns it off for the copy
bun run upstream-declarations  # rewrite upstream/ from the pinned sources (only a moved pin needs it)
```

## Conventions

The copied tree keeps proof-sdk's style — single quotes, its own line width — and Biome's
formatter, linter and `assist` are all turned off over it in the root `biome.json`, so a diff
against the pinned commit stays readable. `assist` matters as much as the other two: its
`organizeImports` is a safe fix, so one `biome check --write` or an editor with organize-on-save
would reorder the copy's imports. `upstream/` is excluded the same way, for the same reason:
it is tsc's output, not source. Anything legion writes here — `scripts/`, `tests/`,
`src/collab-cursor-plugin.ts`, `src/editor/schema/uuid.ts`, `src/trailing-newline-input.ts`,
`src/record-mark-history.ts`, `src/record-mark-retype.ts`, `src/tests/harness.ts` — follow the
repo's conventions and are checked.
