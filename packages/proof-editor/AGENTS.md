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
**c2697996af3ca82817e8ce8748a56b491717bd21** on the fork's cleaned `library` line. Moving that
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

Three files under `src/` are legion's own rather than copies, so the file-by-file audit below
does not reach them:

| File | What it is |
| --- | --- |
| `src/upstream-types.ts` | The upstream types this package's public surface names, gathered from more than one pinned file, so it has no counterpart path in the fork |
| `src/editor/schema/dom-attributes.ts` | `withDomAttributes`, the DOM-output-spec helper lifted out of `block-ids.ts` so the typed-block schema can use it too |
| `src/editor/schema/upstream-schemas.js` | Re-exports the two upstream block schemas `block-ids.ts` extends; it is JavaScript so no TypeScript program follows the specifier into the fork's untyped sources (see below) |

Five kinds of edit are allowed in the copied files, and no others: the import specifiers of
upstream modules, including `src/lib.ts`'s `StoredMark` re-export and the `HeatMapMode` type
pulled out of its heatmap import, which now name `./upstream-types` (`export type` and `import
type` both erase, so neither reaches runtime); `bun test` registration in the suites
(`src/tests/harness.ts` replaces each file's own `test()` tally and its `process.exit` tail);
`src/tests/headless-no-dom.test.ts`, whose entry named the fork's built `dist/headless.js` and
now names `../lib-headless.js` — its banner, the comment beside that import and the case's own
name follow, since this package has no build and there is no distribution left to name;
`src/editor/schema/block-ids.ts`'s two consumer-side changes, which are what the move made
necessary — it extends the upstream `code_block` and `frontmatter` schemas itself through
`./upstream-schemas.js` (the fork used to do that in its own modules) and reads
`withDomAttributes` from `./dom-attributes`; and a `// @ts-nocheck` banner on every one of them
(below).

To audit a copied file, diff it against `jj --ignore-working-copy -R <proof-sdk> file show -r
24a5fc94 root:src/<file>`; every file in the copied-files table has that counterpart, and the only
lines that differ should be the five kinds. `src/upstream-types.ts` is checked the other way,
by `tests/upstream-pin.test.ts`, which reads each of its copied regions out of the pinned file
that region names. That test is a check on the pin, not on the copy.

`tests/` is legion's own, and is linted and type-checked like any other package's.
`tests/upstream-pin.test.ts` is the guard on the pin: it reads the installed dependency and
fails when a fix the pinned line carries has gone missing, or when a region of
`src/upstream-types.ts` stops matching the pinned file it was copied from.

## The upstream boundary

Everything `src/` imports as `proof-sdk-upstream/src/…` — the mark plugins, the mark popover,
`editor/schema/{proof-marks,code-block-ext,frontmatter}`, `formats/*` — still belongs to the fork
and comes from a git dependency pinned to that same commit. Three resolvers reach it, and each
needs telling, because proof-sdk's `package.json` `exports` publishes only its built `dist`:

| Consumer | Mechanism |
| --- | --- |
| Bun (`bun test`, the pmdoc generator) | `paths` in `tsconfig.json` |
| Vite (the Dispatch SPA) | `resolve.alias` for the raw sources and `dedupe: ["prosemirror-model"]` — source-only editor modules and Dispatch both pass ProseMirror nodes into `DOMSerializer`; two module identities make historical renders fail with “multiple versions of prosemirror-model were loaded” |
| tsc | An opaque `paths` declaration in `tsconfig.check.json` |

**No TypeScript program may open those sources.** proof-sdk emits its declarations with
`noCheck` (its `tsconfig.lib.json`), so neither its tree nor this copy of it type-checks; a
consumer that resolved them would inherit ~55 errors it cannot fix. `tsconfig.check.json` — what
`bun run typecheck` reads — maps the runtime source specifiers to `src/upstream-runtime.d.ts`, and
every copied file carries `// @ts-nocheck` so no consumer reports the upstream implementation.

An upstream type reached through that boundary is therefore `any`, so no type on the public
surface reaches a consumer that way. `StoredMark`, which `src/lib.ts` re-exports, and
`HeatMapMode`, which `CreateProofEditorOptions.heatMapMode` names, are both declared in
`src/upstream-types.ts` instead — copied from the pin, type-checked, and held to it by
`tests/upstream-pin.test.ts`. A type added to that surface belongs there too; reaching for
`proof-sdk-upstream/src/…` in an exported signature silently makes it `any`.

The pinned source commit carries the Dark Reader fix: peer-cursor colours and mark decorations
avoid inline `style` attributes, because Dark Reader rewrites those attributes inside the
contenteditable and ProseMirror reads the writes as content mutations. That can cause an endless
redraw loop that wedges the tab. `tests/upstream-pin.test.ts` reads the installed modules so a
pin that loses the fix fails. Moving the pin changes the package dependency, `bun.lock`, and this
file's source audit references; no Bun patch applies to this source.

## No build step

`exports` serves `src/*.ts` directly, the way `@legion/contracts` serves its own sources: Bun runs
TypeScript, Vite compiles it. There is no `dist` and nothing to build before using the package.

## Commands

```bash
bun run test           # the moved suites through src/tests/harness.ts, plus tests/
bun run typecheck      # tsc against tsconfig.check.json (src/ and tests/)
bun run lint           # Biome over the package; the root biome.json turns it off for the copy
```

## Conventions

The copied tree keeps proof-sdk's style — single quotes, its own line width — and Biome's
formatter, linter and `assist` are all turned off over it in the root `biome.json`, so a diff
against the pinned commit stays readable. `assist` matters as much as the other two: its
`organizeImports` is a safe fix, so one `biome check --write` or an editor with organize-on-save
would reorder the copy's imports. Anything legion writes here — `tests/`, `src/tests/harness.ts`
— follows the repo's conventions and is checked.
