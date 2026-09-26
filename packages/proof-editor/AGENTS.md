# @legion/proof-editor

Dispatch's document editor: the Milkdown/ProseMirror entry point, the typed-block grammar and the
block-id feature. A placement red-team ruled all three CONSUMER (LEGION-287), so they live here
rather than in the `sjawhar/proof-sdk` fork, and a Dispatch editor change no longer costs a fork
release, an npm publish and a pin bump.

## Where the code came from

`src/` is proof-sdk's source at **24a5fc94915cda5704897c497af6312c140db40d** (the `library`
branch's v0.3.13 — what production ran before the move), copied byte for byte:

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

The only edits to that copy are the import specifiers of upstream modules, plus `bun test`
registration in the suites (`src/tests/harness.ts`). `jj --ignore-working-copy -R <proof-sdk> diff
--git -r 24a5fc94 --to 24a5fc94` is not how you check it; diff a file against
`jj --ignore-working-copy -R <proof-sdk> file show -r 24a5fc94 root:src/<file>` and the only lines
that differ should be those.

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
copied from the fork carries `// @ts-nocheck` so no consumer reports them either. Upstream types
therefore reach a consumer as `any`; `StoredMark`, which `src/lib.ts` re-exports, is the one that
crosses into Dispatch. Typing this boundary is open work, recorded on LEGION-287.

`patches/proof-sdk-upstream@24a5fc94.patch` keeps peer-cursor colours and mark decorations out of
inline `style` attributes: Dark Reader rewrites inline colours inside the contenteditable and
ProseMirror reads those writes as content mutations, an endless redraw that wedges the tab
(legion #1234). It is that fix moved from the published `dist` onto the fork's source, which does
not carry it.

## No build step

`exports` serves `src/*.ts` directly, the way `@legion/contracts` serves its own sources: Bun runs
TypeScript, Vite compiles it. There is no `dist` and nothing to build before using the package.

## Commands

```bash
bun test src/tests     # the moved suites, through src/tests/harness.ts
bun run typecheck      # tsc against tsconfig.check.json
bun run lint           # Biome, over what legion wrote (biome.json excludes the copied tree)
```

## Conventions

The copied tree keeps proof-sdk's style — single quotes, its own line width — and Biome is turned
off over it in the root `biome.json`, so a diff against the pinned commit stays readable. Anything
legion writes here follows the repo's conventions and is checked.
