# @legion/proof-editor

Dispatch's document editor: the Milkdown/ProseMirror entry point, the typed-block grammar and the
block-id feature. A placement red-team ruled all three CONSUMER (LEGION-287), so they live here
rather than in the `sjawhar/proof-sdk` fork, and a Dispatch editor change no longer costs a fork
release, an npm publish and a pin bump.

## Where the code came from

`src/` is proof-sdk's source at **24a5fc94915cda5704897c497af6312c140db40d** (the `library`
branch's v0.3.13; `main` pinned the npm package at 0.3.12, so the copy also brings 64dc42ed with
it — a duplicated block id now stays with the block that held it, where 0.3.12 kept the first
occurrence in document order, which is still what the server's `EnsureBlockIDs` does; legion
#1434 moves `main` to 0.3.13 and this paragraph loses its middle clause when it lands), copied
byte for byte:

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

`src/upstream-types.ts` sits beside them but is not one of them: it holds the upstream types
this package's public surface names, gathered from more than one pinned file, so it has no
counterpart path in the fork and the file-by-file audit below does not reach it.

Four kinds of edit are allowed in the copied files, and no others: the import specifiers of
upstream modules, including `src/lib.ts`'s `StoredMark` re-export and the `HeatMapMode` type
pulled out of its heatmap import, which now name `./upstream-types` (`export type` and `import
type` both erase, so neither reaches runtime); `bun test` registration in the suites
(`src/tests/harness.ts` replaces each file's own `test()` tally and its `process.exit` tail);
`src/tests/headless-no-dom.test.ts`, whose entry named the fork's built `dist/headless.js` and
now names `../lib-headless.js` — its banner, the comment beside that import and the case's own
name follow, since this package has no build and there is no distribution left to name; and a
`// @ts-nocheck` banner on every one of them (below).

To audit a copied file, diff it against `jj --ignore-working-copy -R <proof-sdk> file show -r
24a5fc94 root:src/<file>`; every file in the table above has that counterpart, and the only
lines that differ should be the four kinds. `src/upstream-types.ts` is checked the other way,
by `tests/upstream-pin.test.ts`, which reads each of its copied regions out of the pinned file
that region names.

`tests/` is legion's own, and is linted and type-checked like any other package's.
`tests/upstream-pin.test.ts` is the guard on everything above: it reads the installed dependency
and fails when the patch is not applied, or when a region of `src/upstream-types.ts` stops
matching the pinned file it was copied from.

## The upstream boundary

Everything `src/` imports as `proof-sdk-upstream/src/…` — the mark plugins, the mark popover,
`editor/schema/{proof-marks,code-block-ext,frontmatter}`, `formats/*` — still belongs to the fork
and comes from a git dependency pinned to that same commit. Three resolvers reach it, and each
needs telling, because proof-sdk's `package.json` `exports` publishes only its built `dist`:

| Consumer | Mechanism |
| --- | --- |
| Bun (`bun test`, the pmdoc generator) | `paths` in `tsconfig.json` |
| Vite (the Dispatch SPA) | `resolve.alias` in `packages/dispatch/web/vite.config.ts` |
| tsc | **nothing** — see below |

**No TypeScript program may open those sources.** proof-sdk emits its declarations with
`noCheck` (its `tsconfig.lib.json`), so neither its tree nor this copy of it type-checks; a
consumer that resolved them would inherit ~55 errors it cannot fix. `tsconfig.check.json` — what
`bun run typecheck` reads — drops the `paths` so the specifiers stay unresolved, and every file
copied from the fork carries `// @ts-nocheck` so no consumer reports them either.

An upstream type reached through that boundary is therefore `any`, so no type on the public
surface reaches a consumer that way. `StoredMark`, which `src/lib.ts` re-exports, and
`HeatMapMode`, which `CreateProofEditorOptions.heatMapMode` names, are both declared in
`src/upstream-types.ts` instead — copied from the pin, type-checked, and held to it by
`tests/upstream-pin.test.ts`. A type added to that surface belongs there too; reaching for
`proof-sdk-upstream/src/…` in an exported signature silently makes it `any`.

`patches/proof-sdk-upstream@24a5fc94.patch` keeps peer-cursor colours and mark decorations out of
inline `style` attributes: Dark Reader rewrites inline colours inside the contenteditable and
ProseMirror reads those writes as content mutations, an endless redraw that wedges the tab
(legion #1234). It is that fix moved from the published `dist` onto the fork's source, which does
not carry it. A `patchedDependencies` key that stops matching applies nothing, with no warning
and exit 0, so `tests/upstream-pin.test.ts` reads the installed files and fails instead. Moving
the pin means editing all three of: the key in the root `package.json`, the dependency in
`package.json`, and the patch file (whose hunks are line-anchored in the fork's source).

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
