# Dispatch SPA on the Proof editor (Lane B, PR 4) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The Dispatch document tab is Proof's collaborative editor bound to the server's `prosemirror` tree: humans type, see each other's cursors, comment/suggest/ask from a selection bar whose marks the server records, and the margin lists and focuses those marks; historical versions render through the same editor read-only; the Playwright suite proves acceptance lines 1–7, 10 and 12 on chromium and iphone against the real Go server.

**Architecture:** `@sjawhar/proof-editor` 0.2.0 (fork release, Task 1) is bound by a ~400-line host adapter under `packages/dispatch/web/src/features/doc/`: `connection.ts` owns the `HocuspocusProvider` to `/ws/doc/{artifact}`, `editor.ts` lazy-loads the library and applies Dispatch's a11y attributes, `ProofDocument.tsx` mounts the editor after first sync and routes every mark action to the margin's Composer (`POST … {artifact, mark_id}`), `VersionView.tsx` renders a version's markdown read-only with the anchored quote embedded as a mark. The margin (`features/margin/`) gains a promise-returning `composeForMark`, a mark ↔ card focus bridge, and mark-position ordering. Records and the `marks` projection stay server-written (PR 3); the browser only writes the tree.

**Tech Stack:** React 19 + TanStack Query (existing SPA), `@sjawhar/proof-editor` 0.2.0 (Milkdown 7 / ProseMirror / y-prosemirror externals), `yjs` 13, `@hocuspocus/provider` 2, Tailwind v4 composites from `web/src/theme/classes.ts`, Bun test + happy-dom, Playwright (chromium + iphone) against `packages/envoy/cmd/dispatch` + Postgres.

**Spec:** `docs/superpowers/specs/2026-09-09-dispatch-document-experience-design.md` — § Acceptance bar (lines 1–7, 10, 12 are this PR's Playwright scenarios), § Design > Browser: host adapter + Proof editor, § Fork `sjawhar/proof-sdk` (Q1a), § Events and delivery (unchanged payloads; `Anchor` carries `mark_id`), § Error handling (`ANCHOR_MISSING` row), § Testing > Playwright, § Delivery item 4, § Rejected. Plan 1 (`docs/superpowers/plans/2026-09-10-dispatch-tree-server.md`, branch `dispatch-doc/tree-server`) fixes the server contract this PR consumes. The margin's threaded discussion (`docs/superpowers/specs/2026-09-10-dispatch-threads-design.md`) is planned separately in `docs/superpowers/plans/2026-09-10-dispatch-threads.md` as two PRs stacked on PR 3 and on this PR; this plan's Task 1 ships the two library hooks that plan needs (`onMarkHover`, `markOffsets`) so the fork is released once.

---

## Decisions needed (Sami)

Tasks below implement the recommendation of each row; a different answer changes only the task named in its last column.

| # | Decision | Options and trade-offs | Recommendation | Changes if decided otherwise |
|---|---|---|---|---|
| D1 | Proof's *suggestion mode* (typing becomes tracked-change marks) in PR 4 | **A. Not in PR 4** — the selection-bar **Suggest** (replace one quote) is what acceptance lines 3/7 exercise; suggestion mode needs a fork toggle + `onSuggestion` callback (750 ms coalescing), a Dispatch "Suggesting" switch, and margin cards for `insert`/`delete` kinds (today's card is `del quote / ins replace_with`). **B. In PR 4** — one more fork option and ~2 Dispatch days; the server already accepts `insert`/`delete` kinds (plan 1 decision 6). | **A**. The spec's design paragraph mentions it; the acceptance bar (the definition of done) does not. Ship the bar, then decide B as its own spec line. | Task 1 gains `suggestionMode: boolean` + `onSuggestion(mark)` in the fork API; Task 5 adds the toggle and posts each coalesced suggestion with `suggestion.replace_with`; Task 4 adds insert/delete card variants. |
| D2 | How `@sjawhar/proof-editor` 0.2.0 reaches npm (Task 1's last step; 0.1.1's tag is already waiting on it) | **A. Finish npm trusted publishing**: npmjs.com → package `@sjawhar/proof-editor` → Settings → Trusted publishing → GitHub Actions, repository `sjawhar/proof-sdk`, workflow `publish.yml`, environment blank. Every future tag publishes itself. **B. One-off**: Sami runs `npm publish --access public` from the tagged checkout with his npm login; repeats for every release. | **A** (2 minutes, already the workflow's design). | None in this plan; B replaces Task 1 Step 9's `gh run watch` with the manual publish. |

Everything else in this document is decided (§ Decisions where the spec under-specifies). The thread-UI question (Proof popover vs Dispatch margin) is settled by Main's ruling of 2026-09-10 and the threads spec § Component Boundaries: Proof owns highlights (marks, decorations, click/hover, offsets, via `onMarkClick`); Dispatch owns the thread cards.

## Scope boundary (PR 4)

**In:**
- Fork release `@sjawhar/proof-editor` **0.2.0** (repo `sjawhar/proof-sdk`, branch `library`, via knives): host-contract completion — `onMarkClick` (margin mode) and `onMarkHover` + `markOffsets()` (consumed by the threads plan), `setMarkdown`, reply text in `MarkAction`, `unresolve`/`delete` routed through the hook, `keybindingsPlugin` dropped, `setCurrentActor` from `user.name`, scoped `lib.css` (`.proof-editor` root class instead of `body`/`:root`), type declarations, a self-checking browser smoke.
- Dispatch SPA: `features/doc/` rewritten on the editor (live document, presence, read-only, version chrome, version view), margin bridge (`composeForMark`, focus both ways, mark-position order, `mark_id` anchors, ask orphan link), theme variables for the editor, dependency swap.
- e2e rewrite: `doc.e2e.ts` and `margin.e2e.ts` replaced; `phone`/`writes`/`artifacts` adapted; new `e2e/editor.ts` helpers; carve-outs from plan 1 Task 2 restored (typing, cursors, artifacts no-doc-websocket, tab round-trip websocket/log scroll, orphaned card → View original text → history highlight).
- Leftovers plan 1 hands over: pmdoc fixture generator on the npm package (`prepare-dist.sh` and its CI step deleted, per `pmdoc/doc.go`'s own instruction); compose `acceptance` profile for running the suite against the deployed image; docs.

**Out:** server changes (PR 3 owns the whole API this PR calls: `POST …/comments|asks` with `{artifact, mark_id}`, accept/reject/resolve, `marks` projection, refresh/orphaning, sweep); the threads UI and its server deltas (`2026-09-10-dispatch-threads.md`, two PRs stacked on PR 3 and PR 4); TOON delivery (Lane A5; acceptance 8 is a live check after A5); `dispatch_doc_read` (acceptance 9, PR 3 Go tests); legacy migration (acceptance 11, PR 3 Go tests + `check-documents` + PR 5 boot); deploy and the live two-login check on the production instance (PR 5); suggestion mode (D1).

**Stacking and deploy rule:** branch `dispatch-doc/proof-editor` is cut from PR 3's head (`dispatch-doc/tree-server`) and rebased onto it as PR 3 advances (`jj rebase -b dispatch-doc/proof-editor -d dispatch-doc/tree-server`). PR 4 opens as a stacked PR (base = `dispatch-doc/tree-server`), is re-targeted to `main` the moment PR 3 merges, and the merge queue merges it immediately after. **The devbox (`envoy-dispatch-dispatch-1`) is not redeployed onto a `main` that contains PR 3 until PR 4 has merged** — the interim SPA state on PR 3 alone (read-only rendered document) never reaches users. PR 5 deploys `main` with both.

**Rebase surface (in-flight `main` PRs PR 3 rebases over before PR 4 is cut):**

| PR / branch | What changes under `packages/dispatch` | Effect on this plan |
|---|---|---|
| #881 `ff-artifacts-tab` (open) | `IssueTabs.tsx` fourth **Artifacts** tab; `IssuePage.tsx` renders artifact routes in the main column (`ArtifactHeader.tsx` new); `Margin.tsx`/`MarginSheet.tsx`/`useMarginItems.ts` lose the artifacts tab; `routes.ts` gains artifact routes; `styles.css` +3 lines; `e2e/artifacts.e2e.ts` reworked | Task 2's `IssuePage.tsx` edits are deltas (swap `DocEditor`→`ProofDocument`, `ArtifactVersionView`→`VersionView`); Task 4 does not touch the artifacts tab; Task 3 edits `artifacts.e2e.ts` after #881's version |
| #882 (merged on `main`) | replying to any comment — including an agent's — posts `reply_to` with `anchor: null` | Task 4 keeps `onReply → openComposer("comment", undefined, comment.id)` and asserts it (Task 4 Step 1); nothing else |
| `feat/spec-primary-layout` (stacked on #881, not yet open) | bare `/issues/KEY` lands on **Spec**; `BoardStrip.tsx` deleted; `Margin.tsx` model gains `needsYou`; `useMarginItems.ts` returns deduped open anchors + issue/inbox asks and keeps closed anchored asks in comments; `CommentsTab.tsx` renders **Needs you** before the list; `MarginSheet.tsx` toggle shows a count; `AskCard.tsx` edited; `useMarginItems.test.ts` moved; e2e `attention/inbox/layout/live/margin/nav/phone/spec-primary/threads/writes` touched | Task 4's margin edits are deltas on those files (context additions, comparator, `ComposerAnchor`); Task 6's `margin.e2e.ts` rewrite keeps spec-primary's `issue landing keeps Spec primary and puts open asks in Needs you` scenario; every "click the Spec tab" step in e2e stays harmless when Spec is already active |
| `ff-project-docs` (planned; `2026-09-10` project-documents plan) | `routes.ts` project routes `/projects/:key/documents/:slug[?version=&ask=|&comment=]`; `features/document/DocumentPage.tsx`; `ArtifactDocument`'s `issueKey` prop becomes `owner: { kind: "issue"; key } | { kind: "document"; artifactId; project; slug }`; the margin (`useMarginItems`, `Margin`, `MarginSheet`, `Composer`, `CommentsTab`, `useAnsweredAsks`) keyed by that `MarginOwner` (document owner → `/api/v1/artifacts/{id}/asks|comments`) | whichever lands second rebases: if `ff-project-docs` is first, `ProofDocument`/`VersionView` take `owner` beside `artifact` and `Composer`/`useMarginItems` keep the `MarginOwner` key (the editor needs only `artifact.id`; only the POST paths change); if PR 4 is first, that plan re-keys the files this plan creates |
| `main` #879 dark mode | `web/src/theme/*` rules (see Global Constraints) | every new component uses `classes.ts` composites; `styles.css` literals carry swatch comments |

## Global Constraints

- Library pin: `"@sjawhar/proof-editor": "0.2.0"` (exact) in `packages/dispatch/package.json`; the library's externals are installed as Dispatch dependencies with one caret range per family: `@milkdown/{core,kit,prose,preset-commonmark,preset-gfm,plugin-history,plugin-collab,plugin-listener,plugin-cursor,plugin-clipboard,theme-nord}` `^7.18.0`, `yjs ^13.6.32`, `y-prosemirror ^1.3.7`, `y-protocols ^1.0.6`, `@hocuspocus/provider ^2.15.2`, `remark-frontmatter ^5.0.0`, `prosemirror-model ^1.25.11`. After `bun install`, `bun pm ls --all` shows exactly one version each of `yjs`, `prosemirror-model`, `prosemirror-view` (two copies break `instanceof` inside y-prosemirror).
- Capability gaps in the library are closed **in the fork** (Task 1), never by host-side workarounds: no `window.proof` overrides, no CSS that hides Proof chrome, no ambient `declare module` shims. Everything Dispatch touches is on the library's typed surface (§ Library contract).
- One authority per record: browsers write the `prosemirror` tree (typing, and the mark the selection bar applies); every row and the `marks` map are written by the server over HTTP (spec § Design > Shape). The adapter never writes `Y.Map("marks")`.
- Theme rules (`packages/dispatch/AGENTS.md` § Dark mode, tests in `web/src/theme/`): components import composites from `web/src/theme/classes.ts`; no raw Tailwind colour utility in any `.ts`/`.tsx` outside `theme/` including `e2e/` (`no-raw-colors.test.ts`); every hex/`rgb()` literal in `web/src/styles.css` ends with `/* <swatch-name> */` matching `palette.ts` (`styles-css-pin.test.ts`); the editor follows `prefers-color-scheme` through Proof's CSS variables mapped to palette swatches (Task 2).
- Accessibility contract the e2e binds: `<article aria-label="Document">` wraps the live editor; the ProseMirror node carries `role="textbox" aria-label="Document editor" aria-multiline="true"`; the version view is `<section aria-label="Document version N">` with `data-testid="version-view"` on the picker-driven one; the connection pill is `role="status"`; a selected margin card carries `aria-current="true"`.
- Time bounds asserted, not assumed: peer visibility (text, highlight, accept) `{ timeout: 1000 }` measured from the moment the local action completed; server settle (versions, orphaning) polled with `expect.poll` ≤ 10 s (settle window is 2 s, `docs.Deps.Settle`).
- Every task's tests are written and run to failure before the implementation. Unit tests run under happy-dom with the `DocumentRuntime` doubles (Task 2); the real editor is exercised only by Playwright.
- Per-package checks while implementing: `cd packages/dispatch && bun run lint && bun run typecheck && bun run test`; partial e2e with `bun run e2e -- doc.e2e.ts` (the script's trailing `playwright test --config e2e/playwright.config.ts` accepts file filters). The full lane (`bun run e2e`, both projects) is green again at Task 6 and required at Task 7. Postgres for e2e: `./scripts/dev-postgres.sh` in `packages/envoy` (URL `postgres://postgres:dispatch@127.0.0.1:55432/dispatch_c?sslmode=disable` is the harness default).
- `bun install --frozen-lockfile` at the repo root must pass on every commit that touches `package.json`; Task 2 commits `bun.lock`. Task 2 therefore starts only after `npm view @sjawhar/proof-editor@0.2.0 version` prints `0.2.0`.
- jj, not git: commit = `jj describe -m "<msg>"` then `jj new`. One task, one commit (Task 1 in the fork repo has its own).
- Absolute paths; never touch `/home/ubuntu/legion` or another agent's workspace; the fork is entered only through `knives start library` (fork-work skill).

## Decisions where the spec under-specifies (decided here; not open questions)

1. **PR 4 depends on fork 0.2.0, not on 0.1.0 plus host workarounds.** 0.1.0 (on npm) ships no type declarations (`npm view` has no `types`; `vite.lib.config.ts` has no dts step), a `lib.css` with global `body`/`:root`/`*` rules that restyle the whole SPA (`body { padding-left: 86px; background: white }`), a popover hook that drops the reply text (`dispatch-popover-hook.ts` `markReply(markId, by, text)` → `{kind:'reply', markId}`), and three plugins that mutate marks locally outside `onMarkAction` (`keybindingsPlugin` Cmd+Shift+K comment, `arrowCommentPlugin` → Proof composer, popover **Delete**/**Reopen** fall back to local mutations). Each is a library defect; the fork exists to fix them (spec Q1: "Make the fork").
2. **Thread UI = margin; Proof supplies highlights.** `onMarkClick` set → the library registers `dispatchMarkEventsPlugin` instead of `markPopoverPlugin` + `arrowCommentPlugin`; Dispatch focuses the margin card. `applyRemoteMarks(projection, { hydrateAnchors: false })` is still fed from the `marks` map on every change so Proof's decoration layer reflects `resolved`/`accepted`/`rejected`. `onMarkHover` and `markOffsets()` ship in the same release for the threads plan (card hover ring, anchor-aligned cards) and are not called by this PR.
3. **Suggestion mode is not enabled (D1 recommendation).** The library keeps `suggestionsPlugins` registered with `enabled: false`; Dispatch exposes no toggle.
4. **Browser mark → record failure.** The Composer keeps its existing inline `QueryError` with the server message for `409 ANCHOR_MISSING` (replacing the dead `ANCHOR_STALE` code) and its Retry; the local mark stays while the composer is open (the Yjs update may simply have arrived late — Retry succeeds) and is removed when the composer is cancelled (the `composeForMark` promise rejects → the library removes it). Spec § Error handling's "adapter removes the local mark" is satisfied at cancel; no row and no projection ever exist for a mark the user abandons.
5. **Margin order = mark position in the live document.** `useMarginItems` sorts anchored items by `markPositions.get(anchor.mark_id)` (first ProseMirror position of the mark, published by `ProofDocument` on every document change); items whose mark is absent (orphaned, other artifact) sort after positioned ones, then unanchored items newest first. `documentText`/quote search (plan 1 decision 11) is deleted.
6. **Historical version view = the same editor, read-only, on a local `Y.Doc`.** `VersionView` calls `handle.setMarkdown(embedHighlight(markdown, {id, quote, by}) ?? markdown)`: the quote is wrapped as `<span data-proof="comment" data-id="<row id>" data-by="…">quote</span>` when it occurs exactly once in the version markdown (Proof's remark plugin turns that span into a `proofComment` mark), then `focusMark(id)` scrolls and pulses it; zero or several occurrences → the existing status "Text changed. The selected range no longer exists in this document." Route: `?version=N&comment=<id>` (plan 1) and new `?version=N&ask=<id>`; `AskCard` gains the same "Text changed. View original text" link `CommentCard` has.
7. **Diff base is the server's canonical text.** "Diff vs current" compares `GET /artifacts/{id}/versions/{n}` with `GET /artifacts/{id}/text` (refetched by the `artifact.version` SSE event, i.e. the last settled text), never `handle.getMarkdown()` (Milkdown's serializer formats differently from `pmdoc.Render` and would diff noise).
8. **Editor lifecycle.** The provider connects on mount; the editor is created on the first `synced` event (no flash of an empty document, no pre-sync typing); while a version is selected the live `<article>` gets `hidden` but stays mounted, so one artifact = one websocket per tab for the tab's lifetime (restores plan 1 Task 2's carved-out "tab round-trips keep one connection" assertion).
9. **`heatMapMode: "hidden"`.** Proof's authorship marks (`proofAuthored`) are still written by its tracker plugin with `by = human:<login>` (Task 1 sets `setCurrentActor`); they never render (`pmdoc` `visibleMarks`), are never swept (plan 1 decision 5), and carry no Dispatch record.
10. **Identity.** `ProofDocument` and `VersionView` read the signed-in user from the `["whoami"]` query `AuthGate` already populates; cursor colour = `colorForLogin(login)` (moved from the deleted CodeMirror `DocEditor` into `connection.ts`).
11. **Dependencies removed:** `react-markdown`, `rehype-sanitize`, `remark-gfm`, `beautiful-mermaid`, `dompurify` — their only consumer was `DocView.tsx`; the library bundles its own mermaid renderer (`dist/index.js`, 2.8 MB). Spec § Browser said the last two "remain"; with `DocView` gone they have no import.
12. **Dark mode for the editor** is a variable block in `styles.css` under `.dispatch-doc .proof-editor` (`--bg-color: transparent; --text-color: currentColor; --font-family: inherit` plus swatch-annotated `--code-bg/--pre-bg/--pre-color/--blockquote-*` for light and dark). Dispatch also adds `.dispatch-doc .milkdown table { display: block; max-width: 100%; overflow-x: auto }` (Proof's CSS scrolls `pre` but not tables — phone overflow), `.dispatch-doc .milkdown .editor { min-height: 24rem }`, `.dispatch-doc[data-read-only="true"] .dispatch-action-bar { display: none }`, and `.dispatch-mark-active` (margin hover/selection → highlighted span).
13. **Lazy library chunk.** `editor.ts` imports `@sjawhar/proof-editor` and its `style.css` with dynamic `import()` inside `createEditor`, so the Inbox and settings pages never download the editor; `web/vite.config.ts` sets `build.chunkSizeWarningLimit: 3000` with a comment naming the editor chunk (the warning would otherwise fire on every build for a known 2.8 MB chunk).
14. **Read-only** on a closed issue = `setReadOnly(true)` + `data-read-only="true"` on the article (hides the action bar); the server also refuses writes (`ConnectionConfig{ReadOnly: !open}` in `docs/websocket.go` and `requireOpenIssue` on the API).
15. **Test seam:** `features/doc/runtime.tsx` exports a React context `DocumentRuntime { connect: ConnectDocument; createEditor: CreateEditor }` defaulting to the real implementations; unit tests wrap in a provider with `fakeDocumentRuntime()` (`web/src/__tests__/document-runtime.ts`). No test imports the library.
16. **Selection in e2e** is a DOM `Range` set inside the focused ProseMirror node followed by a synthetic `selectionchange` (ProseMirror's `DOMObserver` reads it when the view has focus); typing is Playwright's keyboard. `e2e/preview.ts` (rendered-preview selection) is deleted in Task 6.
17. **pmdoc fixture generator** switches `gen/package.json` to `"@sjawhar/proof-editor": "0.2.0"`; `gen/prepare-dist.sh`, the `.gitignore` entry and the CI "Prepare pmdoc fixture generator dependencies" step are deleted; `pmdoc/doc.go`'s "Temporary" section is dropped — exactly what that section instructs once the package is published.
18. **Acceptance against the deployed image**, not the production instance: the production `dispatch` service authenticates with GitHub OAuth cookies, which Playwright cannot drive with real logins; `dispatch.compose.yml` gains a profile-gated `dispatch-acceptance` service (same image tag, `DISPATCH_IDENTITY=header:X-Dispatch-User`, `DISPATCH_ALLOWED_LOGINS=alice,bob`, `DISPATCH_PORT=8767`, database `dispatch_acceptance`, `DISPATCH_NATS_DISABLED=1` — with NATS disabled `main.go` opens no NATS client and reads no `envoy.json`) and `packages/dispatch` gains `e2e:deployed`. The two-real-login check on the production instance stays PR 5's manual live check (spec § Testing > OMP acceptance).
19. **Phone:** in margin mode Proof's `mark-mobile-strip` never exists; the bar's buttons inherit Dispatch's `min-width/min-height: 44px` rule for `button` under 1280 px (`styles.css`).
20. **Popover-kind actions cannot fire in margin mode**; `ProofDocument.onMarkAction` throws for them (`default:` branch) instead of ignoring — a silent branch would hide a library regression.

## Library contract — `@sjawhar/proof-editor` 0.2.0 exports PR 4 binds

| Export (entry `.`) | Bound at | Purpose |
|---|---|---|
| `createProofEditor(root, options): Promise<ProofEditorHandle>` | `features/doc/editor.ts` | construct; root receives class `proof-editor` |
| `CreateProofEditorOptions.ydoc` / `.awareness` | `ProofDocument` (provider's doc + awareness), `VersionView` (local `Y.Doc`, `awareness: null`) | host-owned transport |
| `.user: { name, color }` | both | cursor label + colour; the library calls `setCurrentActor(\`human:${name}\`)` |
| `.readOnly`, `handle.setReadOnly(bool)` | `ProofDocument` (issue closed), `VersionView` (always true) | |
| `.heatMapMode: "hidden"` | both | no authorship gutter |
| `.onMarkAction(action)` | `ProofDocument` | selection-bar `comment` / `suggest` / `ask` → margin composer; promise rejection removes the local mark |
| `.onMarkClick(markId)` **(new)** | `ProofDocument` | margin mode: no popover/arrow-comment plugin; click on `[data-id]` span reported |
| `.onMarkHover(markId | null)` **(new)** | — (threads plan) | hover enter/leave on `[data-id]` spans |
| `handle.markOffsets(): Map<string, number>` **(new)** | — (threads plan) | top offset of each mark's first span relative to `root` |
| `handle.view` (`EditorView`) | `editor.ts` (a11y attributes on `view.dom`), `ProofDocument` (`view.state.doc` for mark positions; `view.dom` for active-mark class) | |
| `handle.applyRemoteMarks(metadata, { hydrateAnchors: false })` | `ProofDocument` | `marks` map projection → Proof metadata |
| `handle.focusMark(markId)` | margin bridge | scroll + `dispatch-mark-pulse` |
| `handle.setMarkdown(markdown)` **(new)** | `VersionView` | replace the document with parsed markdown |
| `handle.destroy()` | both | |
| types `MarkAction`, `StoredMark`, `ProofEditorHandle`, `CreateProofEditorOptions` | `editor.ts`, `marks.ts` | from `dist/lib.d.ts` **(new)** |
| `@sjawhar/proof-editor/style.css` | `editor.ts` | scoped to `.proof-editor` **(changed)** |
| DOM contract | `e2e/editor.ts`, `styles.css` | `.dispatch-action-bar button` labelled **Comment**/**Suggest**/**Ask**; `span[data-proof="comment"][data-id]`, `span[data-proof="suggestion"][data-kind][data-id]`, `span[data-dispatch="ask"][data-id]`; `.proof-collab-cursor__label` (name text); `.dispatch-mark-pulse`; `.ProseMirror` node |

Unused on purpose: `handle.getMarkdown()` (decision 7), `handle.removeMark()` (the library removes rejected marks itself), `./headless` in the SPA (the pmdoc generator uses it).

## Host adapter — what Dispatch owns

| Concern | File | Mechanism |
|---|---|---|
| Transport + auth | `features/doc/connection.ts` | `HocuspocusProvider({ url: wsUrl(artifactId), name: artifactId, document })`; same-origin cookie/header identity (server `requestActor`); status → `connecting/connected/offline`; `synced` gate |
| Identity → presence | `connection.ts` `colorForLogin`, `ProofDocument` | `user: { name: login, color }` |
| Library loading + a11y | `features/doc/editor.ts` | dynamic import of library + CSS; `role/aria-*` on `view.dom` |
| Live document | `features/doc/ProofDocument.tsx` | mount after sync; read-only; connection pill; version picker / Name version / Diff (moved from `DocEditor.tsx`); mark actions → `composeForMark`; mark click → `focusItemForMark`; `marks` map → `applyRemoteMarks`; positions → `setMarkPositions`; `registerDocument({ focusMark, setActiveMarks })` |
| Version view | `features/doc/VersionView.tsx`, `highlight.ts` | local `Y.Doc`, `setMarkdown(embedHighlight(...))`, `focusMark` |
| Mark helpers | `features/doc/marks.ts` | `markPositions(doc)`, `composerKindFor(kind)`, `setActiveMarkClass(dom, ids)` |
| Margin contract | `features/margin/{Margin,Composer,CommentsTab,useMarginItems,useMarginListeners,MarginSheet}.tsx/.ts`, `features/inbox/AskCard.tsx` | `composeForMark` promise; `focusItemForMark`; `markPositions`; `DocumentBridge`; `ComposerAnchor { artifact, mark_id, quote }`; ask orphan link; `aria-current` on the selected card |
| Theme | `web/src/styles.css` | decision 12 |
| Test doubles | `features/doc/runtime.tsx`, `web/src/__tests__/document-runtime.ts` | decision 15 |
| e2e drivers | `e2e/editor.ts` | `documentEditor`, `selectEditorText`, `typeAtEnd`, `actionBar`, `markSpan`, `cursorLabel`, `countDocumentSockets`, `barAction`, `deleteEditorText`, `marginCard` |

---

## File Structure

```
sjawhar/proof-sdk (branch library → PR to fork main → tag v0.2.0)        [Task 1]
├── src/lib.ts                       // onMarkClick/onMarkHover, setMarkdown, markOffsets, setCurrentActor, no keybindingsPlugin, root.classList.add('proof-editor')
├── src/dispatch-marks.ts            // MarkAction: reply carries text; unresolve/delete kinds
├── src/dispatch-popover-hook.ts     // markReply passes text; markUnresolve/markDeleteThread routed through onMarkAction
├── src/dispatch-mark-events.ts      // NEW: handleClick + mouseover/mouseout on [data-proof][data-id] / [data-dispatch][data-id] → onMarkClick / onMarkHover
├── src/lib.css                      // body/:root → .proof-editor; * reset and app-shell #ids removed
├── vite.lib.config.ts               // + vite-plugin-dts (entryRoot src, tsconfig.lib.json)
├── tsconfig.lib.json                // NEW: emitDeclarationOnly
├── package.json                     // 0.2.0; exports types; devDeps vite-plugin-dts, @playwright/test; script smoke
├── smoke/{index.html,main.ts}       // + margin-mode editor C (click/hover/offsets), reply-text, setMarkdown, scoped-css checks; 12 s pause → 300 ms
├── smoke/run.mjs                    // NEW: builds + serves smoke, drives it with Playwright, exits 1 on any FAIL
└── README.lib.md                    // API section updated

packages/dispatch/
├── package.json, ../../bun.lock                                     [Task 2]
├── web/vite.config.ts                                               [Task 2] chunkSizeWarningLimit
├── web/src/styles.css                                               [Task 2 → Task 5 adds .dispatch-mark-active]
├── web/src/features/doc/
│   ├── connection.ts (NEW), editor.ts (NEW), runtime.tsx (NEW)      [Task 2]
│   ├── ProofDocument.tsx (NEW), ProofDocument.test.tsx (NEW)        [Task 2 → Task 5 mark wiring]
│   ├── VersionView.tsx (NEW), VersionView.test.tsx (NEW)            [Task 2]
│   ├── highlight.ts (NEW), highlight.test.ts (NEW)                  [Task 2]
│   ├── marks.ts (NEW), marks.test.ts (NEW)                          [Task 5]
│   ├── VersionDiff.tsx, VersionDiff.test.tsx                        unchanged
│   └── DocEditor.tsx, DocEditor.test.tsx, DocView.tsx, DocView.test.tsx   DELETED [Task 2]
├── web/src/__tests__/document-runtime.ts (NEW)                       [Task 2]
├── web/src/features/issue/IssuePage.tsx, IssuePage.test.tsx          [Task 2]
├── e2e/editor.ts (NEW)                                               [Task 3 → Task 6 extends]
├── e2e/doc.e2e.ts, e2e/artifacts.e2e.ts                              [Task 3]
├── web/src/features/margin/
│   ├── Composer.tsx, Composer.test.tsx                               [Task 4] ComposerAnchor{artifact, mark_id, quote}; onSaved; ANCHOR_MISSING
│   ├── Margin.tsx, Margin.test.tsx                                   [Task 4] composeForMark, focusItemForMark, markPositions, DocumentBridge
│   ├── MarginSheet.tsx, MarginSheet.test.tsx                         [Task 4] SelectionMenu removed
│   ├── CommentsTab.tsx                                               [Task 4] onSaved, artifactSlug to AskCard, aria-current
│   ├── useMarginItems.ts, useMarginItems.test.ts                     [Task 4] markPositions comparator
│   ├── useMarginListeners.ts                                         [Task 4] focus scroll, card click → bridge
│   └── SelectionMenu.tsx                                             DELETED [Task 4]
├── web/src/features/inbox/AskCard.tsx                                [Task 4] orphan link
├── e2e/margin.e2e.ts, e2e/phone.e2e.ts, e2e/writes.e2e.ts, e2e/preview.ts (DELETED)   [Task 6]
├── AGENTS.md, README.md                                              [Task 7]
packages/envoy/internal/dispatch/pmdoc/{doc.go, gen/package.json, gen/bun.lock, gen/.gitignore, gen/prepare-dist.sh (DELETED)}   [Task 7]
.github/workflows/envoy-and-contracts.yaml                            [Task 7]
packages/envoy/deploy/compose/dispatch.compose.yml                    [Task 7] acceptance profile
```

**Ownership and order.** Files are owned by exactly one task among tasks that can run concurrently; a later sequential task may modify a file its predecessor created (`→` above).

| Task | Runs after | Concurrent with | Needs from PR 3 |
|---|---|---|---|
| 1 fork 0.2.0 | — | PR 3 Tasks 4–8 | nothing |
| 2 editor foundation | 1 published; PR 3 Task 3 (`a0465a55` / `b62efcb2`) | PR 3 Tasks 4–6 | `prosemirror` fragment served; nothing else |
| 3 e2e part A (doc, artifacts) | 2 | PR 3 Tasks 4–6; Task 4 | fragment only |
| 4 margin contract | 2 (`DocEditor.tsx` gone); PR 3 Task 7 (`Anchor.mark_id` type) | Task 3 | Task 7 types only (server marks not required for unit tests) |
| 5 mark wiring | 3, 4; PR 3 Tasks 5 + 7 | — | `POST … {artifact, mark_id}`, `marks` map, refresh/orphaning |
| 6 e2e part B (margin, phone, writes) | 5 | — | as Task 5 |
| 7 docs, gen, compose, verification, PR | 6; PR 3 Task 8 | — | PR 3 complete |

**Today, against PR 3's branch (fragment served at `b62efcb2`):** Task 1 now (fork, via knives). Tasks 2 and 3 the moment 0.2.0 is on npm — they need nothing from PR 3 Tasks 4–8. Tasks 4–7 after PR 3's marks and types.

---

### Task 1: fork `@sjawhar/proof-editor` 0.2.0 — host contract completion

Repo `sjawhar/proof-sdk`, branch `library` (knives-managed; `knives status` shows `library` at `8106912fe9b9` "release 0.1.1", no claim). Their files stay untouched; every edit is in a file the fork added.

**Files:**
- Modify: `src/lib.ts`, `src/dispatch-marks.ts`, `src/dispatch-popover-hook.ts`, `src/lib.css`, `vite.lib.config.ts`, `package.json`, `README.lib.md`, `smoke/index.html`, `smoke/main.ts`
- Create: `src/dispatch-mark-events.ts`, `tsconfig.lib.json`, `smoke/run.mjs`

**Interfaces (produces — the whole public surface after this task):**
```ts
export interface ProofEditorUser { name: string; color: string }
export type SelectionBarActionKind = 'comment' | 'ask' | 'suggest';
export type PopoverActionKind = 'reply' | 'resolve' | 'unresolve' | 'accept' | 'reject' | 'delete';
export type MarkAction =
  | { kind: SelectionBarActionKind; markId: string; quote: string; from: number; to: number }
  | { kind: 'reply'; markId: string; text: string }
  | { kind: Exclude<PopoverActionKind, 'reply'>; markId: string };
export interface CreateProofEditorOptions {
  ydoc: Y.Doc; awareness?: Awareness | null; user: ProofEditorUser; readOnly?: boolean;
  onMarkAction?: (action: MarkAction) => void | Promise<void>;
  /** Margin mode: report clicks and hover on mark spans and register neither the mark popover nor
   *  the arrow-comment plugin; the host renders threads itself. */
  onMarkClick?: (markId: string) => void;
  onMarkHover?: (markId: string | null) => void;
  heatMapMode?: HeatMapMode;
}
export interface ProofEditorHandle {
  view: EditorView; getMarkdown(): string; setMarkdown(markdown: string): void; setReadOnly(readOnly: boolean): void;
  /** Top offset (px) of each mark's first span relative to `root`, for hosts that align UI with highlights. */
  markOffsets(): Map<string, number>;
  applyRemoteMarks(metadata: Record<string, StoredMark>, options?: { hydrateAnchors?: boolean }): void;
  removeMark(markId: string): void; focusMark(markId: string): void; destroy(): void;
}
export function createProofEditor(root: HTMLElement, opts: CreateProofEditorOptions): Promise<ProofEditorHandle>;
```
Behaviour rules: `root.classList.add('proof-editor')`; `setCurrentActor(\`human:${opts.user.name}\`)` before plugins run; `keybindingsPlugin` is not registered (its Cmd+Shift+K comment and resolve shortcuts mutate marks outside `onMarkAction`); with `onMarkClick` or `onMarkHover`: `.use(dispatchMarkEventsPlugin({ onMarkClick, onMarkHover }))` replaces `.use(markPopoverPlugin)` and `.use(arrowCommentPlugin)`, and `registerPopoverHookInstance` is not called; without either (popover mode): unchanged plus the hook fixes below. `markOffsets()` = for each distinct `data-id` under `view.dom`, `span.getBoundingClientRect().top - root.getBoundingClientRect().top` of its first span.

- [ ] **Step 1: Take the branch** — `cd /home/ubuntu/proof-sdk/default && knives status && knives notch library && knives start library --why "proof-editor 0.2.0: host contract (onMarkClick/onMarkHover, markOffsets, setMarkdown, reply text, scoped css, types)"`. Work in the workspace path knives prints (`@` is an empty child of `8106912fe9b9`). `npm install` there.

- [ ] **Step 2: Failing smoke checks first** — in `smoke/index.html` add `<div id="editor-c"></div>`; in `smoke/main.ts`: replace `await sleep(12000)` with `await sleep(300)`; add after the existing checks (all must read `FAIL` until Steps 3–6 land):

```ts
  // --- 7. margin mode: onMarkClick / onMarkHover / markOffsets, no popover chrome ---
  const clicks: string[] = [];
  const hovers: Array<string | null> = [];
  const handleC = await createProofEditor(document.getElementById('editor-c')!, {
    ydoc: docB, awareness: null, user: { name: 'Cleo', color: '#10b981' },
    onMarkAction: async (action) => { markActionsA.push(action); },
    onMarkClick: (markId) => clicks.push(markId),
    onMarkHover: (markId) => hovers.push(markId),
  });
  const cRange = findTextRange(handleC.view, 'Hello world');
  if (!cRange) throw new Error('editor C has no text');
  handleC.view.dispatch(handleC.view.state.tr.setSelection(TextSelection.create(handleC.view.state.doc, cRange.from, cRange.to)));
  handleC.view.focus();
  await sleep(100);
  Array.from(document.querySelectorAll<HTMLButtonElement>('#editor-c .dispatch-action-bar button')).find((b) => b.textContent === 'Comment')?.click();
  await sleep(300);
  const cAction = markActionsA[markActionsA.length - 1];
  const cMarkId = cAction && 'markId' in cAction ? cAction.markId : null;
  const cSpan = cMarkId ? document.querySelector<HTMLElement>(`#editor-c [data-id="${cMarkId}"]`) : null;
  cSpan?.dispatchEvent(new MouseEvent('mouseover', { bubbles: true }));
  for (const type of ['pointerdown', 'mousedown', 'mouseup', 'click']) cSpan?.dispatchEvent(new MouseEvent(type, { bubbles: true }));
  cSpan?.dispatchEvent(new MouseEvent('mouseout', { bubbles: true, relatedTarget: document.body }));
  await sleep(100);
  record('onMarkClick reports the clicked mark id', !!cMarkId && clicks.includes(cMarkId), JSON.stringify(clicks));
  record('onMarkHover reports enter and leave', !!cMarkId && hovers[0] === cMarkId && hovers[hovers.length - 1] === null, JSON.stringify(hovers));
  const offsets = handleC.markOffsets();
  record('markOffsets reports the mark top relative to the root', !!cMarkId && offsets.has(cMarkId) && Math.abs(offsets.get(cMarkId)! - (cSpan!.getBoundingClientRect().top - document.getElementById('editor-c')!.getBoundingClientRect().top)) < 1);
  record('margin mode renders no popover chrome', document.querySelectorAll('.mark-popover, .mark-popover-backdrop, .mark-mobile-strip').length === 0);
  record('root carries the proof-editor scope class', document.getElementById('editor-c')!.classList.contains('proof-editor'));

  // --- 8. popover mode (a second editor on docB): Reply text reaches onMarkAction ---
  const replies: MarkAction[] = [];
  const handleB2 = await createProofEditor(document.getElementById('editor-b')!.appendChild(document.createElement('div')), {
    ydoc: docB, awareness: null, user: { name: 'Bob', color: '#3b82f6' }, onMarkAction: async (a) => { replies.push(a); },
  });
  if (cMarkId) {
    // Thread metadata normally arrives through the host's marks projection; feed it the way a host would.
    handleB2.applyRemoteMarks({ [cMarkId]: { kind: 'comment', by: 'human:Cleo', text: '', thread: cMarkId, resolved: false, replies: [] } }, { hydrateAnchors: false });
  }
  const bSpan = cMarkId ? handleB2.view.dom.querySelector<HTMLElement>(`[data-id="${cMarkId}"]`) : null;
  for (const type of ['pointerdown', 'mousedown', 'mouseup', 'click']) bSpan?.dispatchEvent(new MouseEvent(type, { bubbles: true }));
  await sleep(200);
  const replyBox = document.querySelector<HTMLTextAreaElement>('.mark-popover textarea');
  if (replyBox) { replyBox.value = 'reply text'; replyBox.dispatchEvent(new Event('input', { bubbles: true })); }
  Array.from(document.querySelectorAll<HTMLButtonElement>('.mark-popover button')).find((b) => b.textContent === 'Reply')?.click();
  await sleep(200);
  record('popover Reply carries its text through onMarkAction', replies.some((a) => a.kind === 'reply' && 'text' in a && a.text === 'reply text'), JSON.stringify(replies));
  handleB2.destroy();

  // --- 9. setMarkdown replaces the document ---
  handleC.setMarkdown('# Title\n\nBody paragraph.');
  await sleep(100);
  record('setMarkdown renders parsed markdown', !!handleC.view.dom.querySelector('h1') && handleC.view.dom.textContent!.includes('Body paragraph.'));

  // --- 10. scoped CSS: the page body is untouched; the actor is the user ---
  record('lib.css leaves body alone', getComputedStyle(document.body).paddingLeft === '0px');
  handleA.view.dispatch(handleA.view.state.tr.insertText(' authored'));
  await sleep(200);
  record('typing is authored by human:<user>', JSON.stringify(docA.getXmlFragment('prosemirror').toJSON()).includes('human:Alice'));
```
  (`type MarkAction` is already imported in `smoke/main.ts`.) Create `smoke/run.mjs`:

```js
// Builds the smoke harness, serves it, drives it with Playwright, exits 1 on any FAIL.
import { chromium } from '@playwright/test';
import { build, preview } from 'vite';

await build({ configFile: 'vite.smoke.config.ts' });
const server = await preview({ configFile: 'vite.smoke.config.ts', preview: { port: 4173, strictPort: true } });
const browser = await chromium.launch();
const page = await browser.newPage();
page.on('console', (message) => console.log(message.text()));
await page.goto('http://127.0.0.1:4173/');
await page.waitForFunction(() => /DONE|FATAL/.test(document.getElementById('status')?.textContent ?? ''), null, { timeout: 90_000 });
const results = await page.evaluate(() => window.__smokeResults ?? {});
const fatal = await page.evaluate(() => window.__smokeFatal);
await browser.close();
await server.close();
const failed = Object.entries(results).filter(([, pass]) => !pass).map(([name]) => name);
if (fatal) console.error(`FATAL: ${fatal}`);
console.log(`${Object.keys(results).length - failed.length}/${Object.keys(results).length} smoke checks passed`);
process.exit(fatal || failed.length > 0 ? 1 : 0);
```
  `package.json`: `"version": "0.2.0"`, script `"smoke": "node smoke/run.mjs"`, devDependencies `"@playwright/test": "^1.63.0"`, `"vite-plugin-dts": "^4.5.4"`. Run `npx playwright install chromium && npm run smoke`. Expected: builds, then `FAIL` for checks 7–10 (a `.mark-popover` exists; body `padding-left` is `86px`; the actor is `human:user`) and a TypeScript build error on `onMarkClick`/`onMarkHover`/`markOffsets`/`setMarkdown` — exit 1.

- [ ] **Step 3: `src/dispatch-mark-events.ts`**

```ts
/**
 * Margin-mode mark events: when a host renders comment threads itself it passes `onMarkClick`
 * and/or `onMarkHover`; this plugin replaces the mark popover. A click inside a proof or
 * dispatch mark span reports the span's id (the click is not consumed, so the caret still
 * moves); hover reports the id on enter and null on leave.
 */
import { $prose } from '@milkdown/kit/utils';
import { Plugin, PluginKey } from '@milkdown/kit/prose/state';

const markEventsKey = new PluginKey('dispatch-mark-events');
const MARK_SELECTOR = 'span[data-proof][data-id], span[data-dispatch][data-id]';

export interface MarkEventsOptions {
  onMarkClick?: (markId: string) => void;
  onMarkHover?: (markId: string | null) => void;
}

function markIdAt(view: { dom: HTMLElement }, target: EventTarget | null): string | null {
  const element = target instanceof Element ? target : null;
  const span = element?.closest<HTMLElement>(MARK_SELECTOR) ?? null;
  if (!span || !view.dom.contains(span) || !span.dataset.id) return null;
  return span.dataset.id;
}

export const dispatchMarkEventsPlugin = (options: MarkEventsOptions) =>
  $prose(
    () =>
      new Plugin({
        key: markEventsKey,
        props: {
          handleClick(view, _pos, event) {
            const id = markIdAt(view, event.target);
            if (id && options.onMarkClick) options.onMarkClick(id);
            return false;
          },
          handleDOMEvents: {
            mouseover(view, event) {
              const id = markIdAt(view, event.target);
              if (id && options.onMarkHover) options.onMarkHover(id);
              return false;
            },
            mouseout(view, event) {
              const from = markIdAt(view, event.target);
              const to = markIdAt(view, (event as MouseEvent).relatedTarget);
              if (from && from !== to && options.onMarkHover) options.onMarkHover(null);
              return false;
            },
          },
        },
      }),
  );
```

- [ ] **Step 4: `src/dispatch-marks.ts` and `src/dispatch-popover-hook.ts`** — `PopoverActionKind` gains `'unresolve' | 'delete'`; `MarkAction` per Interfaces. In the hook: `markReply(markId, by, text)` fires `{ kind: 'reply', markId, text }` (fallback unchanged); add `markUnresolve(markId) { return fireHookOrFallBack('unresolve', markId, (view, id) => unresolveMutation(view, id)); }` and `markDeleteThread(markId) { return fireHookOrFallBack('delete', markId, (view, id) => deleteMark(view, id)); }` importing `unresolve as unresolveMutation` and `deleteMark` from `./editor/plugins/marks`; `PopoverHookKind` = `PopoverActionKind`. Update the module doc (the "intentionally left unset" paragraph is now false — delete it).

- [ ] **Step 5: `src/lib.ts`** — imports `setCurrentActor` from `./editor/actor`, `dispatchMarkEventsPlugin`, `parserCtx` (already imported); drop the `keybindingsPlugin` import and `.use(keybindingsPlugin)`; add `onMarkClick`/`onMarkHover` to the options interface and `setMarkdown`/`markOffsets` to the handle (Interfaces); at the top of `createProofEditor`: `root.classList.add('proof-editor'); setCurrentActor(\`human:${opts.user.name}\`);` build the chain in a variable so the two modes differ in one place:

```ts
  const marginMode = Boolean(opts.onMarkClick || opts.onMarkHover);
  const builder = Editor.make()
    /* …unchanged .config/.use calls through .use(suggestionsPlugins) … */;
  if (marginMode) {
    builder.use(dispatchMarkEventsPlugin({ onMarkClick: opts.onMarkClick, onMarkHover: opts.onMarkHover }));
  } else {
    builder.use(markPopoverPlugin).use(arrowCommentPlugin);
  }
  const editor = await builder
    .use(dispatchActionBarPlugin({ by: opts.user.name, onMarkAction: opts.onMarkAction }))
    /* …unchanged remaining .use/.config calls… */
    .create();
  // later:
  const unregisterPopoverHook = marginMode ? () => {} : registerPopoverHookInstance(view, opts.onMarkAction);
  // handle:
    setMarkdown(markdown: string): void {
      const parsed = editor.ctx.get(parserCtx)(markdown);
      const { state } = view;
      view.dispatch(state.tr.replaceWith(0, state.doc.content.size, parsed.content));
    },
    markOffsets(): Map<string, number> {
      const offsets = new Map<string, number>();
      const rootTop = root.getBoundingClientRect().top;
      for (const span of view.dom.querySelectorAll<HTMLElement>('[data-id]')) {
        const id = span.dataset.id;
        if (id && !offsets.has(id)) offsets.set(id, span.getBoundingClientRect().top - rootTop);
      }
      return offsets;
    },
```

- [ ] **Step 6: `src/lib.css` scoping** — replace the `body { … }` rule with `.proof-editor { font-family: var(--font-family); font-size: var(--font-size); background: var(--bg-color); color: var(--text-color); }` (no `min-height`, `margin`, `padding-left`); change both `:root { --… }` variable blocks' selector to `.proof-editor`; delete the `* { margin: 0; padding: 0; box-sizing: border-box }` reset, the `:root[data-theme="whitey"]` rules, and the app-shell rules `#app`, `#editor`, `#editor-container`, `#provenance-gutter`, `.gutter-segment`, `.loading`, `body[data-share-mode="true"] …`; keep `.milkdown …`, `.mark-popover*`, `.mark-mobile-*`, `.dispatch-*`, `span[data-…]`, `.markdown-link-action-card*` rules as they are (popover chrome attaches to `document.body` in popover mode). Update the header comment to say what the file is now (scoped library styles), not where it came from.

- [ ] **Step 7: Types** — `tsconfig.lib.json`: `{ "extends": "./tsconfig.json", "compilerOptions": { "noEmit": false, "declaration": true, "emitDeclarationOnly": true, "outDir": "./dist" }, "include": ["src/**/*.ts"], "exclude": ["src/tests/**"] }`. `vite.lib.config.ts`: `import dts from 'vite-plugin-dts'` and `plugins: [dts({ tsconfigPath: 'tsconfig.lib.json', entryRoot: 'src', include: ['src/**/*.ts'], exclude: ['src/tests/**'] })]` (emits `dist/lib.d.ts`, `dist/lib-headless.d.ts` and their dependency declarations). `package.json` exports: `".": { "types": "./dist/lib.d.ts", "import": "./dist/index.js", "default": "./dist/index.js" }`, `"./headless": { "types": "./dist/lib-headless.d.ts", "import": "./dist/headless.js", "default": "./dist/headless.js" }`. `README.lib.md` API block updated to the Interfaces above plus one paragraph on margin mode.

- [ ] **Step 8: Run** — `npm run build:lib && ls dist/lib.d.ts dist/lib-headless.d.ts && npm run smoke`. Expected: `dist/lib.d.ts` exists; smoke prints `N/N smoke checks passed`, exit 0. Consumer type check: in a scratch dir, `bun init -y && bun add /path/to/fork-workspace && printf 'import { createProofEditor, type MarkAction } from "@sjawhar/proof-editor"; const a: MarkAction = { kind: "reply", markId: "m", text: "t" }; void createProofEditor; void a;\n' > check.ts && bunx tsc --noEmit --strict --moduleResolution bundler --module esnext --target es2022 --skipLibCheck check.ts` → no errors.

- [ ] **Step 9: Commit, PR, tag, publish** — `jj describe -m "feat(lib): margin mode (onMarkClick/onMarkHover, markOffsets), setMarkdown, reply text and delete/unresolve through onMarkAction, scoped css, type declarations — 0.2.0" && jj new`; `jj git push -b library` (origin = sjawhar/proof-sdk); `gh pr create -R sjawhar/proof-sdk -B main -H library --title "release: @sjawhar/proof-editor 0.2.0 — host contract for Dispatch" --body-file <notes>`; after merge: `SHA=$(gh api repos/sjawhar/proof-sdk/commits/main -q .sha) && gh api repos/sjawhar/proof-sdk/git/refs -f ref=refs/tags/v0.2.0 -f sha=$SHA`; `gh run watch -R sjawhar/proof-sdk` (workflow `Publish @sjawhar/proof-editor`; needs D2-A done — on failure the run log says "trusted publisher not configured"); `npm view @sjawhar/proof-editor@0.2.0 version types` → `0.2.0`, `./dist/lib.d.ts`. `knives notch library -m "0.2.0 released: margin mode, markOffsets, setMarkdown, reply text, scoped css, types; consumed by legion PR 4 and the threads plan" --evidence $SHA && knives finish library`.

---

### Task 2: editor foundation — live document, presence, versions, version view, dependencies, theme

Needs only the `prosemirror` fragment (served since PR 3 Task 3) and the published 0.2.0. The margin's `selection`/`documentText` context members still exist after this task (their only producer, `DocEditor.tsx`, is deleted here; Task 4 removes them).

**Files:**
- Modify: `packages/dispatch/package.json`, `bun.lock` (root), `web/vite.config.ts`, `web/src/styles.css`, `web/src/features/issue/IssuePage.tsx`, `IssuePage.test.tsx`
- Create: `web/src/features/doc/connection.ts`, `editor.ts`, `runtime.tsx`, `ProofDocument.tsx`, `ProofDocument.test.tsx`, `VersionView.tsx`, `VersionView.test.tsx`, `highlight.ts`, `highlight.test.ts`, `web/src/__tests__/document-runtime.ts`
- Delete: `web/src/features/doc/DocEditor.tsx`, `DocEditor.test.tsx`, `DocView.tsx`, `DocView.test.tsx`

**Interfaces (produces):**
```ts
// connection.ts
export type ConnectionState = "connecting" | "connected" | "offline";
export interface DocumentConnection { readonly doc: Y.Doc; readonly awareness: Awareness; destroy(): void }
export interface ConnectionCallbacks { onStatus(state: ConnectionState): void; onSynced(): void }
export type ConnectDocument = (artifactId: string, callbacks: ConnectionCallbacks) => DocumentConnection;
export function wsUrl(artifactId: string): string;          // `${ws|wss}://${host}/ws/doc/${encodeURIComponent(id)}`
export function colorForLogin(login: string): string;       // stable pick from the six presence colours
export const connectDocument: ConnectDocument;              // HocuspocusProvider; throws "Dispatch document provider did not create awareness." when awareness is null

// editor.ts
export type { MarkAction, StoredMark } from "@sjawhar/proof-editor";
export type EditorHandle = ProofEditorHandle; export type EditorOptions = CreateProofEditorOptions;
export type CreateEditor = (root: HTMLElement, options: EditorOptions) => Promise<EditorHandle>;
export const editorAttributes = { "aria-label": "Document editor", "aria-multiline": "true", role: "textbox" } as const;
export const createEditor: CreateEditor;                    // dynamic import of the library + style.css; sets editorAttributes on handle.view.dom

// runtime.tsx
export interface DocumentRuntimeValue { connect: ConnectDocument; createEditor: CreateEditor }
export const DocumentRuntime: React.Context<DocumentRuntimeValue>;   // default { connect: connectDocument, createEditor }

// highlight.ts
export interface Highlight { by: string; id: string; quote: string }
export function embedHighlight(markdown: string, highlight: Highlight): string | undefined;  // decision 6

// ProofDocument.tsx
export function ProofDocument(props: { artifact: Artifact; isClosed: boolean }): ReactNode;
// VersionView.tsx
export function VersionView(props: { artifactId: string; createdAt: string | undefined; highlight: Highlight | undefined; version: number }): ReactNode;

// __tests__/document-runtime.ts
export interface FakeEditor { destroyed: boolean; focused: string[]; markdown: string | undefined; offsets: Map<string, number>; options: EditorOptions; readOnly: boolean; remoteMarks: Record<string, StoredMark>[]; root: HTMLElement }
export function fakeDocumentRuntime(seed?: { text?: string }): { connections: DocumentConnection[]; editors: FakeEditor[]; runtime: DocumentRuntimeValue; sync(): void; status(state: ConnectionState): void }
// the fake editor renders `fragment.toString()` (or setMarkdown's argument) as the root's textContent so tests assert visible text; its view is { dom: root, state: { doc: { descendants() {} } } }; markOffsets() returns `offsets`
```

- [ ] **Step 1: Dependencies** — `packages/dispatch/package.json`: add the Global Constraints list (`@sjawhar/proof-editor` `0.2.0` exact, the eleven `@milkdown/*` at `^7.18.0`, `yjs`, `y-prosemirror`, `y-protocols`, `@hocuspocus/provider`, `remark-frontmatter`, `prosemirror-model`); remove `react-markdown`, `rehype-sanitize`, `remark-gfm`, `beautiful-mermaid`, `dompurify`. Repo root: `bun install`, then `bun pm ls --all | rg 'yjs@|prosemirror-model@|prosemirror-view@' | sort -u` → one line per package. `web/vite.config.ts`: `build.chunkSizeWarningLimit: 3000` with the comment `// @sjawhar/proof-editor (Milkdown + mermaid) is one ~2.8 MB lazy chunk loaded only on the document tab.`

- [ ] **Step 2: Failing tests.**

```ts
// highlight.test.ts
test("embedHighlight wraps a unique quote in a proof comment span and refuses ambiguity", () => {
  expect(embedHighlight("The quick brown fox", { by: "user:alice", id: "c-1", quote: "brown" })).toBe(
    'The quick <span data-proof="comment" data-id="c-1" data-by="user:alice">brown</span> fox'
  );
  expect(embedHighlight("o o", { by: "u", id: "c", quote: "o" })).toBeUndefined();
  expect(embedHighlight("text", { by: "u", id: "c", quote: "gone" })).toBeUndefined();
  expect(embedHighlight("x", { by: 'a"b', id: "c", quote: "x" })).toBe('<span data-proof="comment" data-id="c" data-by="a&quot;b">x</span>');
});
```
```tsx
// ProofDocument.test.tsx (render inside QueryClientProvider + MarginProvider + DocumentRuntime.Provider with fakeDocumentRuntime; seed ["whoami"] = { kind: "user", login: "alice" }, ["artifact", id], ["artifact", id, "text"])
test("ProofDocument creates the editor on the synced document as the signed-in user", ...)   // before sync: "Connecting to the document…"; after fake.sync(): one editor with options.user {name:"alice", color: colorForLogin("alice")}, readOnly false, heatMapMode "hidden", ydoc === connections[0].doc, awareness === connections[0].awareness; root textContent contains the seeded text; view.dom has role textbox / aria-label "Document editor"
test("ProofDocument reflects connection status and read-only state", ...)                    // status pill "connecting" → status("connected") → "connected" → status("offline") → "offline"; rerender isClosed → editors[0].readOnly true, article data-read-only "true", notice "This issue is closed. Its document is read-only."
test("ProofDocument keeps the live editor mounted while a version is shown and destroys it once", ...)  // select version "1" → article has hidden attribute, data-testid version-view present, editors.length still 1; unmount → editors[0].destroyed true, connections[0] destroyed (spy)
test("ProofDocument compares a selected version with the current settled text", ...)         // ported from DocEditor.test.tsx: setQueryData version 1 markdown + text; Diff vs current → del/ins
test("ProofDocument shows the current document when Version changes back to Current", ...)    // ported
// VersionView.test.tsx
test("VersionView renders the version read-only with the highlighted quote focused", ...)     // fake editor: options.readOnly true, awareness null; markdown === embedHighlight(...); focused === ["c-1"]; no status text
test("VersionView reports an ambiguous or missing quote as changed text", ...)                // markdown === plain version text; status "Text changed. The selected range no longer exists in this document."
// IssuePage.test.tsx
// "IssuePage keeps the Spec mounted across tabs": wrap in DocumentRuntime.Provider with fakeDocumentRuntime({ text: "Keep this document" }); assert the textbox "Document editor" keeps its text across Log/Spec switches and exactly one connection was opened.
// "IssuePage highlights a historical quote from its comment deep link": `/issues/CORE-1/artifact/spec?version=1&comment=comment-1` → the fake editor's markdown contains `data-id="comment-1"` and focused includes "comment-1"; add the `&ask=ask-1` variant using api.getAsk.
// "IssuePage reports an ambiguous historical quote as changed text": unchanged assertion on the status text.
```
  Run: `bun run typecheck; bun run test web/src/features/doc web/src/features/issue` — expected: module-not-found for the new files, then failures.

- [ ] **Step 3: Implement.**
  - `connection.ts` per Interfaces (provider `onStatus: ({ status }) => onStatus(status === "disconnected" ? "offline" : status)`, `onSynced: ({ state }) => { if (state) onSynced(); }`; `destroy()` destroys provider then doc).
  - `editor.ts`: `createEditor = async (root, options) => { const [{ createProofEditor }] = await Promise.all([import("@sjawhar/proof-editor"), import("@sjawhar/proof-editor/style.css")]); const handle = await createProofEditor(root, options); for (const [name, value] of Object.entries(editorAttributes)) handle.view.dom.setAttribute(name, value); return handle; }`.
  - `runtime.tsx`: the context with real defaults.
  - `ProofDocument.tsx`: `useQuery(["whoami"])` for the user; `useContext(DocumentRuntime)`; mount effect keyed on `artifact.id`: `connect(artifact.id, { onStatus: setConnection, onSynced })` where `onSynced` (first time only) awaits `createEditor(root, { ydoc, awareness, user: { name: login, color: colorForLogin(login) }, readOnly: isClosedRef.current, heatMapMode: "hidden", onMarkAction: () => Promise.reject(new Error("mark actions are wired in Task 5")), onMarkClick: () => {} })` and stores the handle; cleanup destroys handle then connection (`mounted` flag guards the async create). Effect on `isClosed` → `handle?.setReadOnly(isClosed)`. Version chrome moved from `DocEditor.tsx` unchanged in behaviour (Name version prompt, `<select aria-label="Version">`, Diff vs current, `nameVersion` mutation) with composites (`surfaceBg`, `borderDefault`, `textSecondaryOnSurface`, `calloutWarning*`, `dangerText`, `controlHoverBorder`, `textDisabled`, `surfaceMutedBg`, `calloutSuccess*` for the pill). Markup per Global Constraints: `<section aria-label="Document editor">` → toolbar → notices → `<article aria-label="Document" className={\`dispatch-doc rounded-lg border p-4 ${borderDefault} ${surfaceBg}\`} data-read-only={String(isClosed)} hidden={selectedVersion !== null}>` containing `<div ref={root} />` and, until synced, `<p className={textMutedOnSurface}>Connecting to the document…</p>` → when a version is selected: `showDiff ? <VersionDiff after={liveMarkdown} before={selectedMarkdown} /> : <div data-testid="version-view"><VersionView … highlight={undefined} /></div>`. The `["artifact", id, "text"]` query stays for the diff base (decision 7). No call to the margin's `setDocumentText`/`setSelection` anywhere.
  - `VersionView.tsx`: `useQuery(["artifact", artifactId, "version", version])`; `useQuery(["whoami"])`; effect: `const ydoc = new Y.Doc(); createEditor(root, { ydoc, awareness: null, user, readOnly: true, heatMapMode: "hidden" })` → `const embedded = highlight ? embedHighlight(markdown, highlight) : undefined; handle.setMarkdown(embedded ?? markdown); if (embedded) handle.focusMark(highlight.id);` `setHighlightMissing(highlight !== undefined && embedded === undefined)`; cleanup destroys the handle and `ydoc`. Renders `<section aria-label={\`Document version ${version}\`}>` with the heading/Timestamp/status from the old `ArtifactVersionView` and `<article aria-label="Document" className="dispatch-doc …" data-read-only="true"><div ref={root} /></article>`.
  - `highlight.ts`: `escapeAttribute` (`&`, `"`, `<`, `>`) + `embedHighlight` per decision 6.
  - `IssuePage.tsx`: import `ProofDocument`, `VersionView`, `Highlight`; delete `historicalHighlight` and `ArtifactVersionView`; `commentId`/`askId` from the search params; `useQuery({ enabled: askId !== undefined, queryKey: ["ask", askId], queryFn: () => api.getAsk(askId) })` and the existing comments query build `highlight: Highlight | undefined = anchor && { id, quote: anchor.quote, by: \`${author.kind}:${author.id}\` }`; render `<ProofDocument artifact={selectedArtifact} isClosed={isClosed} key={selectedArtifact.id} />` and `<VersionView artifactId createdAt highlight version />`.
  - `styles.css`: replace the deleted `.cm-content` block with decision 12's rules. Literals: `#f1f5f9 /* slate-100 */`, `#0f172b /* slate-900 */`, `#020618 /* slate-950 */`, `#0084d1 /* sky-600 */`, `#00bcff /* sky-400 */` are already pinned in the file's history; compute the rest with `cd packages/dispatch && bun -e 'import { oklchToSrgb8 } from "./web/src/theme/contrast"; import * as P from "./web/src/theme/palette"; for (const s of [P.SLATE_200, P.SLATE_300, P.SLATE_400, P.SLATE_600, P.SLATE_700, P.SLATE_800]) console.log(s.name, oklchToSrgb8(s.oklch));'` and write each as `#rrggbb; /* name */`.
  - Delete `DocEditor.tsx`, `DocEditor.test.tsx`, `DocView.tsx`, `DocView.test.tsx`; `__tests__/document-runtime.ts` per Interfaces (fake connection: `new Y.Doc()` + `new Awareness(doc)` from `y-protocols/awareness`; `seed.text` inserted as `Y.XmlElement("paragraph")` containing `Y.XmlText(text)` into `doc.getXmlFragment("prosemirror")`; `sync()` fires every connection's `onSynced`).

- [ ] **Step 4: Run** — `cd packages/dispatch && bun run lint && bun run typecheck && bun run test && bun run build:web`; repo root `bun install --frozen-lockfile`. Expected: green; the build output lists one chunk > 2 MB containing `proof-editor` and no chunk-size warning. Smoke the real surface with a throwaway script (`/tmp/pr4-smoke.ts`, deleted afterwards): start `cd packages/envoy && ./scripts/dev-postgres.sh` then `DATABASE_URL=<url> bash ../dispatch/e2e/run-server.sh` (port 8777); the script uses `@playwright/test`'s `chromium.launch({ headless: false })`, `POST /api/v1/projects` `{key:"LOCAL",name:"Local"}` and `POST /api/v1/issues` `{project:"LOCAL", title:"Editor smoke", spec:"# Title\n\nBody with **bold**.\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n- [ ] task\n"}` with header `X-Dispatch-User: alice`, then two contexts (`extraHTTPHeaders: { "X-Dispatch-User": "alice" | "bob" }`, the second with `colorScheme: "dark"`) on `/issues/<KEY>/spec`; type in alice's editor, wait until bob's shows it, screenshot both (`/tmp/pr4-light.png`, `/tmp/pr4-dark.png`) for the PR. Then `bun run e2e -- shell.e2e.ts layout.e2e.ts` — expected PASS (these touch the document only through API anchors and the tab shell).

- [ ] **Step 5: Commit** — `jj describe -m "feat(dispatch-web): documents render and edit through the Proof editor — live document, presence, versions, read-only version view" && jj new`

---

### Task 3: e2e part A — editing, presence, versions, reload, tab round-trip, artifacts websocket

**Files:**
- Create: `packages/dispatch/e2e/editor.ts`
- Rewrite: `packages/dispatch/e2e/doc.e2e.ts`
- Modify: `packages/dispatch/e2e/artifacts.e2e.ts`

**Interfaces (produces — `e2e/editor.ts`):**
```ts
export function documentEditor(page: Page): Locator;            // page.getByRole("textbox", { name: "Document editor" })
export function actionBar(page: Page): Locator;                 // page.locator(".dispatch-action-bar")
export function markSpan(page: Page, markId: string): Locator;  // documentEditor(page).locator(`[data-id="${markId}"]`)
export function cursorLabel(page: Page, name: string): Locator; // page.locator(".proof-collab-cursor__label", { hasText: name })
export async function selectEditorText(page: Page, quote: string): Promise<void>;  // focus + DOM Range over the first text node containing quote + synthetic selectionchange; waits for actionBar visible
export async function typeAtEnd(page: Page, text: string): Promise<void>;          // click editor, Control+End, Enter, keyboard.type(text)
export function countDocumentSockets(page: Page): () => number;                    // page.on("websocket") counting URLs whose pathname starts with /ws/doc/
```

- [ ] **Step 1: Write `e2e/editor.ts`.**

```ts
import type { Locator, Page } from "@playwright/test";

export function documentEditor(page: Page): Locator {
  return page.getByRole("textbox", { name: "Document editor" });
}

export function actionBar(page: Page): Locator {
  return page.locator(".dispatch-action-bar");
}

export function markSpan(page: Page, markId: string): Locator {
  return documentEditor(page).locator(`[data-id="${markId}"]`);
}

export function cursorLabel(page: Page, name: string): Locator {
  return page.locator(".proof-collab-cursor__label", { hasText: name });
}

/** Selects the first occurrence of `quote` inside the focused ProseMirror node the way a drag
 *  does: a DOM Range plus the selectionchange ProseMirror's DOMObserver listens to. */
export async function selectEditorText(page: Page, quote: string): Promise<void> {
  const editor = documentEditor(page);
  await editor.waitFor();
  await editor.focus();
  await editor.evaluate((root, quote) => {
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const index = node.textContent?.indexOf(quote) ?? -1;
      if (index < 0) continue;
      const range = document.createRange();
      range.setStart(node, index);
      range.setEnd(node, index + quote.length);
      const selection = window.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
      document.dispatchEvent(new Event("selectionchange"));
      return;
    }
    throw new Error(`quote is not in the editor: ${quote}`);
  }, quote);
  await actionBar(page).waitFor({ state: "visible" });
}

export async function typeAtEnd(page: Page, text: string): Promise<void> {
  const editor = documentEditor(page);
  await editor.click();
  await page.keyboard.press("Control+End");
  await page.keyboard.press("Enter");
  await page.keyboard.type(text);
}

export function countDocumentSockets(page: Page): () => number {
  let count = 0;
  page.on("websocket", (socket) => {
    if (new URL(socket.url()).pathname.startsWith("/ws/doc/")) count += 1;
  });
  return () => count;
}
```

- [ ] **Step 2: Rewrite `doc.e2e.ts`** (seed spec `"## Database\n\nUse SQLite\n\n| col | value |\n|---|---|\n| a | 1 |\n\n- [ ] task\n\n```ts\nconst x = 1;\n```\n"`; `session` actor as today; `resetDatabase` in `beforeEach`):

  - `"the spec opens as a formatted, editable document with no source pane"` — `/issues/KEY/spec`; `documentEditor` visible with `contenteditable="true"`; `getByRole("heading", { level: 2, name: "Database" })` font-size > paragraph font-size; `table`, `input[type=checkbox]` (Proof task item) and `pre code` present inside the editor; `page.getByRole("button", { name: /^(Edit|Preview)$/ })` count 0; no `textarea` inside the Spec tabpanel; iphone project: `document.documentElement.scrollWidth <= window.innerWidth`. Screenshot `rendered-document.png`.
  - `"two users edit the same spec, see each other's text and cursor within a second, and settle one version attributed to both"` — alice and bob (`asUser`) open the spec, both editors contain "Use SQLite"; `typeAtEnd(alicePage, "hello from alice")` → `expect(bobEditor).toContainText("hello from alice", { timeout: 1000 })`; `typeAtEnd(bobPage, "hello from bob")` → alice sees it `{ timeout: 1000 }`; chromium only: `bobEditor.click()` → `expect(cursorLabel(alicePage, "bob")).toBeVisible({ timeout: 1000 })` and `cursorLabel(bobPage, "alice")`; `expect.poll(() => getArtifact(id).then(a => a.versions.find(v => v.number === 2)?.authors.map(x => x.id).sort()), { timeout: 10_000 }).toEqual(["alice", "bob"])`; version 2 `named: false`; `getArtifactVersion(id, 2).markdown` contains both strings. Screenshot `live-document-edit.png`.
  - `"named versions, the version picker, and the diff stay current across users"` — from the previous seed: `editArtifact` replace SQLite→Postgres (session); Name version "Decided Postgres" via the prompt dialog → poll version named; `getByLabel("Version")` contains "Decided Postgres"; select version 1 → `getByTestId("version-view")` contains "Use SQLite" and its `documentEditor` has `contenteditable="false"`; "Diff vs current" → `version-diff` `del` contains "SQLite", `ins` contains "Postgres"; select "Current" → live editor visible again; `countDocumentSockets` reads 1 throughout.
  - `"an agent edit lands live in every open editor"` — both users open; `await editArtifact(id, { ops: [{ find: "SQLite", op: "replace", with: "Postgres" }] }, session)`; both editors `toContainText("Postgres", { timeout: 1000 })`.
  - `"reloading mid-edit reopens the same content"` — `typeAtEnd(alicePage, "before reload")`; wait until bob's editor shows it (proves the tree carried it); `alicePage.reload()`; alice's editor contains both the seed and "before reload"; `getByLabel("Version")` still lists version 1.
  - `"tab round-trips keep one document connection, the typed text, and the log scroll position"` — restore the deleted scenario verbatim except `enterEditMode`/textbox lines → `typeAtEnd(page, "stay mounted")` and `documentEditor(page)`; `countDocumentSockets` is 1 before and after the round-trip; `window.scrollY` restored.
  - `"a version deep link renders the comment's original quote highlighted in a read-only editor"` — comment via API `{ anchor: { artifact: "spec", quote: "SQLit" } }`; goto `/issues/KEY/artifact/spec?version=1&comment=<id>`; `page.getByRole("region", { name: "Document version 1" })`'s `[data-id="<comment id>"]` has text "SQLit" and `.dispatch-mark-pulse` class appears once; the region's editor `contenteditable="false"`. Screenshot `spec-deep-link-highlight.png`.

- [ ] **Step 3: `artifacts.e2e.ts`** — at the start of the scenario `const sockets = countDocumentSockets(page)`; after `page.goto(\`/issues/${issue.key}\`)` + Spec visible: `expect(sockets()).toBe(1)`; after navigating to `/issues/KEY/artifacts/diagram-png` (image route) and its assertions: `expect(sockets()).toBe(1)` (no new document socket for a non-document artifact); the `notes.md` primary switch asserts `documentEditor(page)` contains "These notes replace the initial spec." (replacing the `article` locator).

- [ ] **Step 4: Run** — `cd packages/dispatch && bun run lint && bun run typecheck && bun run e2e -- doc.e2e.ts artifacts.e2e.ts`. Expected: PASS on `chromium` and `iphone` (cursor assertions skipped on iphone). Keep `test-results/*.png` for the PR.

- [ ] **Step 5: Commit** — `jj describe -m "test(dispatch-e2e): editor helpers; typing, presence, versions, reload, tab round-trip and artifact websocket scenarios on the Proof editor" && jj new`

---

### Task 4: margin contract for browser-written marks

Pure SPA + unit tests; no editor dependency. Every change is a delta on the file's current shape (see Rebase surface for spec-primary's concurrent edits). `DocEditor.tsx` — the last producer of `setDocumentText`/`setSelection` — is already gone (Task 2). The only thread-related acceptance in this task: **replying to any comment, including an agent's, posts `reply_to` with `anchor: null`** (#882 on `main`; PR 3 inherits it on rebase) — `onReply` stays `openComposer("comment", undefined, comment.id)`. Thread cards, inline composer, resolve/reopen and edit are the threads plan (`2026-09-10-dispatch-threads.md`), stacked on this PR; it names the files it takes over from this task.

**Files:**
- Modify: `web/src/features/margin/Composer.tsx`, `Composer.test.tsx`, `Margin.tsx`, `Margin.test.tsx`, `MarginSheet.tsx`, `MarginSheet.test.tsx`, `CommentsTab.tsx`, `useMarginItems.ts`, `useMarginItems.test.ts`, `useMarginListeners.ts`, `web/src/features/inbox/AskCard.tsx`
- Delete: `web/src/features/margin/SelectionMenu.tsx`

**Interfaces (produces):**
```ts
// Composer.tsx
export interface ComposerAnchor { artifact: string; mark_id: string; quote: string }
// Composer props gain: onSaved?: () => void   — called in save.onSuccess before onClose()
// payload: anchor: { artifact: anchor.artifact, mark_id: anchor.mark_id }   (never quote/occurrence); replies: { reply_to, body }, no anchor
// error copy: 409 ANCHOR_MISSING → the server message (replaces ANCHOR_STALE)

// Margin.tsx
export interface DocumentBridge { focusMark(markId: string): void; setActiveMarks(markIds: readonly string[]): void }
export interface MarkComposeRequest { anchor: ComposerAnchor; kind: "ask" | "comment" | "suggestion" }
interface MarginContextValue {
  composeForMark(request: MarkComposeRequest): Promise<void>;   // resolves when the Composer saved, rejects when it closed unsaved
  documentBridge: DocumentBridge | undefined;
  focusItemForMark(markId: string): void;                       // select + scroll the card whose anchor.mark_id matches; opens the sheet on compact viewports
  focusRequest: { markId: string; seq: number } | undefined;
  hoveredItemId: string | undefined;
  markPositions: ReadonlyMap<string, number>;
  pendingCompose: (MarkComposeRequest & { seq: number }) | undefined;
  registerDocument(bridge: DocumentBridge | undefined): void;
  selectItem(id: string): void;
  selectedItemId: string | undefined;
  setHoveredItemId(id: string | undefined): void;
  setMarkPositions(positions: ReadonlyMap<string, number>): void;
  settleCompose(outcome: "saved" | "cancelled"): void;
}
// removed: documentText, setDocumentText, selection, setSelection, MarginSelection; MarginSheetModel.selection.value; actions.onSelectionAction
// useMarginItems(issueKey, tab, visibleArtifact, markPositions: ReadonlyMap<string, number>)
// export function marginItemMarkId(item: MarginItem): string | undefined   // ask.anchor?.mark_id | comment.anchor?.mark_id
// MarginSheetModel.actions gains onComposerSaved(): void; items gains onSelectCard(id: string): void (used by useMarginListeners)
// CommentCard and the ask wrapper render aria-current="true" when selected (the e2e's selection probe; classes stay composites)
// AskCard props gain artifactSlug?: string — renders "Text changed. View original text" → `${buildIssuePath({key, kind:"artifact", slug, version: anchor.version})}&ask=${ask.id}` when anchor.orphaned
```

- [ ] **Step 1: Failing tests.**

```tsx
// Composer.test.tsx — replace the anchor fixtures and payload assertions:
const anchor = { artifact: "document-1", mark_id: "mark-1", quote: "selected" };
// "Composer sends the browser mark id for asks, comments, and suggestions": every createAsk/createComment
// payload has anchor: { artifact: "document-1", mark_id: "mark-1" } and no "quote"/"occurrence" key.
// "Composer shows the server's missing-anchor error and keeps the draft": createComment rejects with
// new ApiError(409, { code: "ANCHOR_MISSING", error: "anchor mark is not in the document" }) → the alert
// shows that text and the Comment textarea still holds the draft; "Retry" calls createComment again.
// "Composer reports onSaved before onClose": onSaved spy called once, then onClose.

// useMarginItems.test.ts — "useMarginItems orders items by their mark position, then unplaced anchors, then unanchored":
// comments A (mark m-a), B (mark m-b), C (orphaned, mark m-c absent), D (no anchor); markPositions = new Map([["m-b", 4], ["m-a", 12]])
// → order [B, A, C, D].

// Margin.test.tsx
test("composeForMark opens the composer with the mark anchor and resolves when it saves", async () => {
  // render <MarginProvider><Margin/><Probe/></MarginProvider> where Probe calls composeForMark({kind:"comment", anchor}) on a button click and records settle order;
  // expect the "Comment composer" form visible with blockquote "selected"; fill "why?" and submit (createComment spy resolves);
  // expect the promise resolved and the composer gone.
});
test("composeForMark rejects when the composer is dismissed unsaved", async () => {
  // Escape on an empty composer → promise rejected with Error("composer closed"); a second composeForMark rejects the first with "replaced by a newer composer".
});
test("focusItemForMark selects and scrolls the matching card and opens the sheet on compact viewports", async () => {
  // comments list has a card with anchor.mark_id "m-1"; call focusItemForMark("m-1") → the card has aria-current="true", its
  // scrollIntoView spy was called with { block: "center" }; with matchMedia("(max-width: 1279px)") stubbed true the sheet's data-expanded is "true".
});
test("selecting or hovering a card drives the document bridge", async () => {
  // registerDocument({ focusMark, setActiveMarks }) from a Probe; click a card → focusMark("m-1"); hover → setActiveMarks(["m-1"]).
});
test("replying to an agent's anchored comment posts reply_to with no anchor", async () => {
  // a comment by { kind: "session", id: "s1" } anchored on m-1; click its Reply → the composer has no blockquote; submit "ok" →
  // createComment payload is { body: "ok", reply_to: comment.id } with anchor undefined (#882 contract).
});
// Delete: every test that renders SelectionMenu / asserts "Selection actions" / uses setSelection.
```
  Run `cd packages/dispatch && bun run typecheck; bun run test web/src/features/margin` — expected: type errors (`ComposerAnchor.mark_id`, missing context members) and failing new tests.

- [ ] **Step 2: Implement.**
  - `Composer.tsx`: `ComposerAnchor` per Interfaces; `save.mutationFn` builds `anchor === undefined ? undefined : { artifact: anchor.artifact, mark_id: anchor.mark_id }`; `onSuccess` calls `onSaved?.()` before the invalidations and `onClose()`; the `QueryError` message branch tests `save.error.code === "ANCHOR_MISSING"`.
  - `Margin.tsx` (`MarginProvider`): `useRef<{ resolve(): void; reject(reason: Error): void }>()` for the pending promise plus `useState` for `pendingCompose`/`focusRequest`/`markPositions`/`documentBridge`; `composeForMark` rejects an earlier pending promise with `new Error("replaced by a newer composer")` and stores the new one; `settleCompose` resolves/rejects (`new Error("composer closed")`) and clears both; `focusItemForMark` increments `seq`. `useMarginSheet`: effect on `pendingCompose` → `setTab("comments")`, `setComposer({ anchor, kind })`, compact (`window.matchMedia("(max-width: 1279px)").matches`) → `setExpandedIssueKey(issueKey)`; `closeComposer` → `setComposer(undefined); settleCompose("cancelled")` and the two auto-close effects (issue closed, other artifact) call it; `onComposerSaved = () => settleCompose("saved")`; effect on `focusRequest` → find `items.find((item) => marginItemMarkId(item) === markId)` → `selectItem(id)`, `setTab("comments")`, compact → expand; handled `seq` kept in a ref so a later `items` change does not re-fire; effect on `[selectedItemId, hoveredItemId, items, documentBridge]` → `documentBridge?.setActiveMarks(ids of the selected + hovered items' marks)`; `onSelectCard(id)` → `selectItem(id)` + `documentBridge?.focusMark(marginItemMarkId(item))`; `onReply` unchanged (`openComposer("comment", undefined, comment.id)`).
  - `useMarginListeners.ts`: takes `onSelectCard` and `focus: { itemId, seq } | undefined`; the `routeItemId` scroll block is generalised into `scrollCardIntoView(id)` used by both the route and the focus request (`card.scrollIntoView({ block: "center" })` inside the list container).
  - `useMarginItems.ts`: comparator per decision 5; export `marginItemMarkId`.
  - `MarginSheet.tsx`: remove the `SelectionMenu` import/render and `selection.value`; pass `onComposerSaved` to `CommentsTab` → `Composer onSaved`.
  - `CommentsTab.tsx`: `<Composer … onSaved={onComposerSaved} />`; `<AskCard ask={item.ask} artifactSlug={artifactSlug} />`; `aria-current={selected ? "true" : undefined}` on `CommentCard`'s `<article>` and the ask wrapper `<div>`.
  - `AskCard.tsx`: orphan link per Interfaces (same classes/copy as `CommentCard`'s, composites from `classes.ts`).
  - Delete `SelectionMenu.tsx`.

- [ ] **Step 3: Run** — `cd packages/dispatch && bun run lint && bun run typecheck && bun run test`. Expected: green.

- [ ] **Step 4: Commit** — `jj describe -m "feat(dispatch-web): margin contract for browser marks — composeForMark, mark focus bridge, mark-position order, mark_id anchors" && jj new`

---

### Task 5: mark wiring — selection bar → composer, highlights ↔ margin, projection, ordering

Requires PR 3 Tasks 5 and 7 on the branch (mark anchors and the `Anchor.mark_id` type) plus Tasks 2, 3 and 4.

**Files:**
- Modify: `web/src/features/doc/ProofDocument.tsx`, `ProofDocument.test.tsx`, `web/src/styles.css`
- Create: `web/src/features/doc/marks.ts`, `marks.test.ts`

**Interfaces (produces — `marks.ts`):**
```ts
import type { Node as ProseMirrorNode } from "prosemirror-model";
export const recordMarkTypes = ["proofComment", "proofSuggestion", "dispatchAsk"] as const;
export function markPositions(doc: ProseMirrorNode): Map<string, number>;   // first position of each record-bearing mark id, document order
export function composerKindFor(kind: "comment" | "suggest" | "ask"): "comment" | "suggestion" | "ask";
export function setActiveMarkClass(root: HTMLElement, markIds: readonly string[]): void;  // toggles "dispatch-mark-active" on [data-id] spans under root
```

- [ ] **Step 1: Failing tests.**

```ts
// marks.test.ts — build a doc with a two-mark mini schema (prosemirror-model: nodes doc/paragraph/text, marks proofComment{id,by}/dispatchAsk{id,by}):
test("markPositions reports the first position of every record-bearing mark", () => {
  // paragraph: "The " + "quick"(proofComment c1) + " brown " + "fox"(dispatchAsk a1) + " and " + "quick"(proofComment c1 again)
  expect([...markPositions(doc)]).toEqual([["c1", 5], ["a1", 17]]);   // paragraph opens at 0, first char at 1
});
test("setActiveMarkClass toggles the active class on matching spans only", () => { /* three spans; ids ["a"] → only span a has the class; [] → none */ });
```
```tsx
// ProofDocument.test.tsx additions (fake runtime; MarginProvider with a Probe exposing the context):
test("a selection-bar action opens the margin composer for the mark and settles the library promise", async () => {
  // editors[0].options.onMarkAction({ kind: "comment", markId: "m-9", quote: "brown", from: 11, to: 16 }) → pendingCompose { kind: "comment", anchor: { artifact: id, mark_id: "m-9", quote: "brown" } };
  // settleCompose("saved") resolves it; a second action + settleCompose("cancelled") rejects with "composer closed".
  // { kind: "suggest" } → kind "suggestion"; { kind: "ask" } → "ask"; { kind: "resolve", markId } → throws.
});
test("a highlight click focuses its margin item and the margin drives the editor", async () => {
  // options.onMarkClick("m-9") → context.focusRequest.markId === "m-9";
  // context.documentBridge.focusMark("m-9") → editors[0].focused includes "m-9"; setActiveMarks(["m-9"]) → span[data-id="m-9"] under view.dom has dispatch-mark-active.
});
test("the marks projection reaches the editor on sync and on every change", async () => {
  // connections[0].doc.getMap("marks").set("c-1", { kind: "comment", by: "user:alice", text: "why?", resolved: false, replies: [] }) after sync →
  // editors[0].remoteMarks[last] deep-equals that record and the call used { hydrateAnchors: false }.
});
test("mark positions are published to the margin after document changes", async () => {
  // fake view.state.doc is a mini-schema doc as in marks.test.ts; after sync → context.markPositions has c1 → 5; mutate the fragment → republished.
});
```
  Run: `bun run test web/src/features/doc` — expected: module-not-found (`marks.ts`), then failures.

- [ ] **Step 2: Implement.**
  - `marks.ts` per Interfaces (`doc.descendants((node, pos) => { if (!node.isText) return true; for (const mark of node.marks) if (recordMarkTypes.includes(mark.type.name) && typeof mark.attrs.id === "string" && !positions.has(mark.attrs.id)) positions.set(mark.attrs.id, pos); return true; })`).
  - `ProofDocument.tsx`: `const { composeForMark, focusItemForMark, registerDocument, setMarkPositions } = useMargin();` kept current in refs for the long-lived editor callbacks; `onMarkAction`:
    ```ts
    onMarkAction: (action) => {
      switch (action.kind) {
        case "comment":
        case "suggest":
        case "ask":
          return composeForMarkRef.current({
            anchor: { artifact: artifact.id, mark_id: action.markId, quote: action.quote },
            kind: composerKindFor(action.kind),
          });
        default:
          throw new Error(`Dispatch renders mark threads in the margin; popover action ${action.kind} cannot fire`);
      }
    },
    onMarkClick: (markId) => focusItemForMarkRef.current(markId),
    ```
    after the handle exists: `registerDocument({ focusMark: (id) => handle.focusMark(id), setActiveMarks: (ids) => setActiveMarkClass(handle.view.dom, ids) })`; `const marks = connection.doc.getMap("marks"); const project = () => handle.applyRemoteMarks(marks.toJSON() as Record<string, StoredMark>, { hydrateAnchors: false }); project(); marks.observe(project);` `const fragment = connection.doc.getXmlFragment("prosemirror"); const publish = () => { cancelAnimationFrame(frame); frame = requestAnimationFrame(() => setMarkPositionsRef.current(markPositions(handle.view.state.doc))); }; publish(); fragment.observeDeep(publish);` cleanup: `marks.unobserve(project); fragment.unobserveDeep(publish); registerDocument(undefined)`.
  - `styles.css`: `.dispatch-doc .dispatch-mark-active { box-shadow: inset 0 -2px 0 #0084d1; /* sky-600 */ }` and the dark variant `#00bcff; /* sky-400 */`.

- [ ] **Step 3: Run** — `cd packages/dispatch && bun run lint && bun run typecheck && bun run test`; then against the branch's server (PR 3 through Task 7): `bun run e2e -- doc.e2e.ts artifacts.e2e.ts` still PASS, and a manual pass in Chromium (the Task 2 Step 4 throwaway script, adapted): select text → **Comment** → composer shows the quote → save → the highlight stays and the margin lists it; select → **Ask** → Escape → the highlight disappears; click a highlight → its card is selected and scrolled; hover a card → the span gets the active underline.

- [ ] **Step 4: Commit** — `jj describe -m "feat(dispatch-web): selection-bar marks record through the margin composer; highlights and margin cards focus each other; marks projection and position order" && jj new`

---

### Task 6: e2e part B — anchors, highlights, orphaning, accept/reject, Log comment, phone

**Files:**
- Rewrite: `packages/dispatch/e2e/margin.e2e.ts`
- Modify: `packages/dispatch/e2e/phone.e2e.ts`, `packages/dispatch/e2e/writes.e2e.ts`, `packages/dispatch/e2e/editor.ts`
- Delete: `packages/dispatch/e2e/preview.ts`

**Interfaces (produces — additions to `e2e/editor.ts`):**
```ts
export async function barAction(page: Page, label: "Comment" | "Suggest" | "Ask"): Promise<void>;  // actionBar(page).getByRole("button", { exact: true, name: label }).click()
export async function deleteEditorText(page: Page, quote: string): Promise<void>;                  // selectEditorText then keyboard.press("Backspace")
export function marginCard(page: Page, id: string): Locator;                                       // page.locator(`[data-margin-item="${id}"]`)
```

- [ ] **Step 1: Rewrite `margin.e2e.ts`** (seed `"The quick brown fox"`; `setSheet` helper kept for iphone):

  - `"the selection bar comments, suggests, and asks on marks that both users see within a second"` (acceptance 3, 10-anchors, cancel) — alice + bob open the spec; alice `selectEditorText("brown")` → bar shows exactly **Comment**, **Suggest**, **Ask**; `barAction("Comment")` → `Comment composer` form with blockquote "brown"; Ctrl+K picker still opens/closes (kept from today); fill "why?" → submit → `expect.poll(listComments)` finds it; `typeof comment.anchor.mark_id === "string"` and `comment.anchor.quote === "brown"`; `markSpan(alicePage, mark_id)` and `markSpan(bobPage, mark_id)` visible `{ timeout: 1000 }` with text "brown"; both margins show `marginCard(*, comment.id)` containing "brown"; `selectEditorText("quick")` → **Suggest** → `Suggest composer` Replacement "fast" → submit → suggestion span `span[data-proof="suggestion"][data-kind="replace"]` in both; `selectEditorText("fox")` → **Ask** → `Ask composer` question → submit → `span[data-dispatch="ask"]` in both and the AskCard in both margins; cancel path: `selectEditorText("The")` → **Comment** → `Escape` → `expect(documentEditor(alicePage).locator('span[data-proof="comment"]')).toHaveCount(1)` (only the recorded one); reload alice → the three spans are still there (`[data-id]` count 3) and the margin lists three items. Screenshot `anchored-margin-items.png`.
  - `"an agent's quote-anchored comment and ask render as highlights in open editors"` (acceptance 5) — `createComment` and `createAsk` via API with `{ artifact: "spec", quote }` while alice's page is open → `markSpan(alicePage, comment.id)` and `markSpan(alicePage, ask.id)` visible `{ timeout: 1000 }` (server marks use the row id).
  - `"highlights follow edits in the other browser and orphan to their original version when the text is deleted"` (acceptance 6) — API comment on "brown" (id `c`) and on "fox" (id `f`); alice types `"Note: "` at the document start (click editor, `Control+Home`, type) → `markSpan(bobPage, c)` still has text "brown" `{ timeout: 1000 }` and `expect.poll(listComments → c.anchor)` stays `{ orphaned: false, quote: "brown" }` after version 2 settles; `deleteEditorText(alicePage, "fox")` → `expect.poll(() => listComments → f.anchor, { timeout: 10_000 }).toMatchObject({ orphaned: true, version: 1 })`; `setSheet(bobPage, project, true)`; `marginCard(bobPage, f)` contains "Text changed" → click "View original text" → `region "Document version 1"` `[data-id="f"]` has text "fox", editor `contenteditable="false"`. Screenshot `orphaned-margin-card.png`.
  - `"accepting a suggestion changes the text in both browsers and names a version; rejecting leaves the text"` (acceptance 7) — bob: `selectEditorText("brown")` → **Suggest** → Replacement "red" → submit; alice clicks **Accept** on `marginCard(alicePage, id)` → both editors `toContainText("The quick red fox", { timeout: 1000 })`, `markSpan(*, mark_id)` count 0 in both; `expect.poll(getArtifact → versions.some(v => v.named))` true; second suggestion (quote "quick" → "slow") → alice **Reject** → both editors still contain "quick", span gone, `listComments` shows `suggestion.accepted === false`.
  - `"a comment from the Log with nothing selected reaches the Log and margin of both users"` (acceptance 4) — alice opens the Log tab; the Log composer (`role="tablist" aria-label="Compose target"` when a route exists, otherwise the plain `Comment composer`) → fill "General remark" → submit; bob's Log shows "General remark" `{ timeout: 1000 }` (SSE) and bob's margin (Comments tab, after `setSheet` on iphone) lists `marginCard(bobPage, id)` without a blockquote, after every anchored item.
  - `"margin cards and document highlights focus each other"` — API comment on "brown" (`c`); alice clicks `markSpan(alicePage, c)` → `marginCard(alicePage, c)` has `aria-current="true"`; on iphone the sheet is expanded (`margin-sheet` `data-expanded="true"`); clicking the card → `markSpan(alicePage, c)` gets `.dispatch-mark-pulse` within 300 ms.
  - `"a reply to an agent's anchored comment carries no anchor and lands in the same thread"` (#882) — API comment by `session` anchored on "brown"; alice clicks **Reply** on its card → composer without blockquote → "ok" → `listComments` shows `{ reply_to: comment.id, anchor: null }`; the reply renders under the root in both margins `{ timeout: 1000 }`.
  - keep `"margin ask composer sends option choices that the inbox records as a selected answer"` (selection via `selectEditorText` + `barAction("Ask")`) and `"a viewer who opens the issue after an anchored ask is answered sees it in the margin"` unchanged in intent; delete `"margin anchors a whole-paragraph selection made in the rendered preview"` (its subject was the deleted DOM mapper) and every `from`/`to` assertion.

- [ ] **Step 2: `phone.e2e.ts`** — `selectPreviewText(page, "brown")` → `selectEditorText(page, "brown")` and `page.getByRole("button", { exact: true, name: "Comment" }).click()` → `barAction(page, "Comment")`; add before the composer steps, iphone only: every `actionBar(page).getByRole("button")` bounding box ≥ 44×44 and `document.documentElement.scrollWidth <= window.innerWidth` with the seed spec extended by a 6-column table. `writes.e2e.ts`: the same two substitutions. Delete `e2e/preview.ts`; `rg -n "selectPreviewText|from \"./preview\"" e2e` returns nothing.

- [ ] **Step 3: Run** — `cd packages/dispatch && bun run lint && bun run typecheck && bun run e2e`. Expected: full lane PASS on `chromium` and `iphone`. Save `test-results/*.png`.

- [ ] **Step 4: Commit** — `jj describe -m "test(dispatch-e2e): anchored marks, following highlights, orphan links, accept/reject, Log comments, agent-comment replies and phone scenarios on the Proof editor" && jj new`

---

### Task 7: documentation, generator on npm, acceptance profile, full verification, PR

**Files:**
- Modify: `packages/dispatch/AGENTS.md`, `packages/dispatch/README.md`, `packages/dispatch/package.json` (script `e2e:deployed`), `packages/envoy/internal/dispatch/pmdoc/doc.go`, `packages/envoy/internal/dispatch/pmdoc/gen/package.json`, `gen/bun.lock`, `gen/.gitignore`, `.github/workflows/envoy-and-contracts.yaml`, `packages/envoy/deploy/compose/dispatch.compose.yml`
- Delete: `packages/envoy/internal/dispatch/pmdoc/gen/prepare-dist.sh`

- [ ] **Step 1: Docs.** `packages/dispatch/AGENTS.md`: § Dark mode's last two sentences (CodeMirror `Compartment`, `.cm-content` overlays in the pin test's description) → "The document editor (`@sjawhar/proof-editor`) themes itself through CSS variables scoped to `.proof-editor`; `styles.css` maps them to palette swatches under `.dispatch-doc .proof-editor` for both schemes, and those literals are what `styles-css-pin.test.ts` pins." Add a § Document editor paragraph: what the adapter owns (this plan's § Host adapter, prose form), the `DocumentRuntime` test seam, the e2e helpers, and the rule that library gaps go to the fork. `README.md`: "Application shape" mentions the editor; § End-to-end tests gains "Acceptance run against the deployed image" with the commands from Step 3. Read each paragraph back against the running app, not the diff.

- [ ] **Step 2: Generator on npm + compose profile.** `gen/package.json`: `"@sjawhar/proof-editor": "0.2.0"`; `cd packages/envoy/internal/dispatch/pmdoc/gen && rm -rf node_modules proof-editor-dist && bun install && bun run check` → `fixtures up to date`; delete `prepare-dist.sh`; `.gitignore` drops `proof-editor-dist/`; `doc.go` drops the "Temporary" section; `.github/workflows/envoy-and-contracts.yaml` step "Prepare pmdoc fixture generator dependencies" → `run: bun install --frozen-lockfile` with the temporary comment removed. `dispatch.compose.yml`: add

```yaml
  # Acceptance run of the Playwright suite against the deployed image (never the production
  # database): `docker compose --profile acceptance up -d dispatch-acceptance`, then
  # `bun run e2e:deployed` in packages/dispatch with the environment README.md lists.
  dispatch-acceptance:
    profiles: ["acceptance"]
    image: ghcr.io/sjawhar/legion/envoy:${ENVOY_IMAGE_TAG:?set ENVOY_IMAGE_TAG (see deploy/README.md)}
    network_mode: host
    entrypoint: ["/usr/local/bin/envoy-dispatch"]
    depends_on:
      postgres:
        condition: service_healthy
    environment:
      HOME: /home/envoy
      DATABASE_URL: postgres://postgres:${DISPATCH_PG_PASSWORD:?set DISPATCH_PG_PASSWORD}@127.0.0.1:${DISPATCH_PG_PORT:-55432}/dispatch_acceptance?sslmode=disable
      DISPATCH_LISTEN_HOST: 127.0.0.1
      DISPATCH_PORT: "8767"
      DISPATCH_AGENT_TOKEN: ${DISPATCH_ACCEPTANCE_TOKEN:-acceptance-token}
      DISPATCH_ALLOWED_LOGINS: alice,bob
      DISPATCH_IDENTITY: header:X-Dispatch-User
      DISPATCH_NATS_DISABLED: "1"
      DISPATCH_TEST_HOOKS: "1"
```
  (no `envoy.json` or data volume: with NATS disabled `main.go` opens no NATS client, and the image's `/home/envoy/.local/share/dispatch` holds the ephemeral signing key.) `packages/dispatch/package.json`: `"e2e:deployed": "playwright test --config e2e/playwright.config.ts"` (no web build; `PLAYWRIGHT_BASE_URL` required — `playwright.config.ts` already skips `webServer` when it is set).

- [ ] **Step 3: Full verification** (the PR gate; paste outputs in the PR):
  - `bun install --frozen-lockfile` (repo root)
  - `cd packages/dispatch && bun run lint && bun run typecheck && bun run test && bun run build:web && bun run e2e` (both projects)
  - `cd packages/envoy/internal/dispatch/pmdoc/gen && bun install --frozen-lockfile && bun run check`
  - `cd packages/envoy && gofmt -l ./internal/dispatch && go vet ./internal/dispatch/... && go build ./cmd/dispatch` (doc.go comment edit only)
  - Deployed-image substitute from this branch: `cd packages/envoy/deploy/compose && ENVOY_IMAGE_TAG=pr4-local docker compose build dispatch` (the Dockerfile builds `web/dist` from the lockfile — proves the npm pin resolves in a clean image), `docker compose exec postgres createdb -U postgres dispatch_acceptance || true`, `ENVOY_IMAGE_TAG=pr4-local docker compose --profile acceptance up -d dispatch-acceptance`, then `cd packages/dispatch && PLAYWRIGHT_BASE_URL=http://127.0.0.1:8767 PLAYWRIGHT_DATABASE_URL='postgres://postgres:<pw>@127.0.0.1:55432/dispatch_acceptance?sslmode=disable' E2E_AGENT_TOKEN=acceptance-token bun run e2e:deployed` → PASS both projects; `docker compose --profile acceptance down dispatch-acceptance` (production `dispatch` untouched).
  - Leftovers: `rg -n "react-markdown|rehype-sanitize|remark-gfm|beautiful-mermaid|dompurify|DocView|DocEditor|SelectionMenu|selectPreviewText|documentText|ANCHOR_STALE|prepare-dist|proof-editor-dist" packages/dispatch packages/envoy/internal/dispatch/pmdoc .github` → nothing.
  - Bundle: the `build:web` output line for the editor chunk (size) goes in the PR body.

- [ ] **Step 4: Commit and PR** — `jj describe -m "docs(dispatch): Proof editor adapter, acceptance profile; pmdoc generator on the published @sjawhar/proof-editor" && jj new`; bookmark `dispatch-doc/proof-editor`; `jj git push`; `gh pr create --base dispatch-doc/tree-server --title "feat(dispatch): SPA on the Proof editor — live multiplayer documents, mark anchors from the selection bar, margin bridge, e2e rewrite"` with the body below; register with the merge queue as stacked on PR 3; re-target to `main` when PR 3 merges (`gh pr edit --base main`). Post-merge → PR 5 (deploy, then this README's acceptance run against `ghcr.io/sjawhar/legion/envoy:<main sha>` and the two-login live check).

---

## Acceptance pass (surfaces, drivers, tooling)

| Acceptance line | Surface a human touches | Driver (named scenario / check) | Task |
|---|---|---|---|
| 1 document, no source pane | `/issues/KEY/spec` in Chromium and an iPhone viewport | `doc.e2e.ts` "the spec opens as a formatted, editable document with no source pane" (both projects) | 3 |
| 2 two users, cursors ≤1 s, no loss, one unnamed version for both after idle | two signed-in browsers typing | `doc.e2e.ts` "two users edit the same spec, see each other's text and cursor within a second, and settle one version attributed to both"; "named versions, the version picker, and the diff stay current across users" | 3 |
| 3 selection bar Comment/Suggest/Ask → composer with quote → highlight + margin on both screens ≤1 s | select text in the editor | `margin.e2e.ts` "the selection bar comments, suggests, and asks on marks that both users see within a second" | 6 |
| 4 comment with nothing selected from the Log, both users, every agent | Log composer | `margin.e2e.ts` "a comment from the Log with nothing selected reaches the Log and margin of both users" (browser half); agent half = live OMP check after Lane A5 (`e2e/acceptance/omp-roundtrip.sh`, PR 5) | 6 / PR 5 |
| 5 `dispatch_doc_edit` lands live; agent quote anchors highlight | agent API against an open editor | `doc.e2e.ts` "an agent edit lands live in every open editor"; `margin.e2e.ts` "an agent's quote-anchored comment and ask render as highlights in open editors"; live OMP session in PR 5 | 3, 6 / PR 5 |
| 6 highlight follows edits in the other browser; deletion → orphaned + link to the version | typing around/through a highlight | `margin.e2e.ts` "highlights follow edits in the other browser and orphan to their original version when the text is deleted"; `doc.e2e.ts` "a version deep link renders the comment's original quote highlighted in a read-only editor" | 6, 3 |
| 7 Accept → text changes in both, mark gone, named version; Reject → mark gone, text stays | margin buttons | `margin.e2e.ts` "accepting a suggestion changes the text in both browsers and names a version; rejecting leaves the text" | 6 |
| 8 TOON envelope to the asking session | OMP session | Lane A5 + live check (PR 5); not this PR | — |
| 9 `dispatch_doc_read` clean markdown + open items | OMP tool | PR 3 Go tests (`TestMarkQuoteWritesMarkAndReturnsCoveredText`), envoy-client tests | — |
| 10 reload mid-edit: same content, every anchor intact | browser reload | `doc.e2e.ts` "reloading mid-edit reopens the same content"; anchors in the reload step of `margin.e2e.ts` scenario for line 3 | 3, 6 |
| 11 legacy documents open converted | deployed boot | PR 3 `TestMigrateLegacyDocumentsConvertsContentAndAnchors` + `dispatch check-documents`; PR 5 boot log | — |
| 12 phone: every scenario except cursor names; no horizontal overflow; controls ≥44 px | iPhone viewport | every scenario runs in the `iphone` project; `phone.e2e.ts` adds the bar-button size and overflow assertions | 3, 6 |
| reply anywhere incl. agent comments, `anchor: null` (#882) | margin Reply | `margin.e2e.ts` "a reply to an agent's anchored comment carries no anchor and lands in the same thread"; `Margin.test.tsx` reply test | 6, 4 |
| carve-outs (plan 1 Task 2) | — | typing (line 2 scenario), artifacts no-doc-websocket (`artifacts.e2e.ts`), tab round-trip websocket + log scroll (`doc.e2e.ts`), orphaned card / View original text / history highlight (line 6 scenarios) | 3, 6 |
| margin ↔ highlight focus | click a card / a highlight | `margin.e2e.ts` "margin cards and document highlights focus each other" | 6 |
| library contract | `npm run smoke` in the fork | `smoke/run.mjs` (Task 1) — reusable for every future release | 1 |
| deployed artifact | the image the devbox runs | compose `acceptance` profile + `bun run e2e:deployed` (Task 7); run from the branch image at Task 7 and from `main`'s image in PR 5 | 7 |

What already drives these surfaces today: `e2e/run-server.sh` + `seed.ts` + `users.ts` (real Go server, Postgres, header identity for alice/bob); the `iphone` project. Gaps closed here: editor drivers (`e2e/editor.ts`), the library smoke (fork), the deployed-image profile. The restricted real path (production instance, GitHub OAuth, two humans) has no automated substitute by design; PR 5 performs it by hand and records it.

## PR body checklist (browser side of the acceptance bar)

- [ ] **1** — formatted, editable document, no source pane: `doc.e2e.ts` scenario 1 (chromium + iphone), screenshot `rendered-document.png` light and dark.
- [ ] **2** — two users, cursors, ≤1 s, version attributed to both: `doc.e2e.ts` scenarios 2–3, screenshot `live-document-edit.png`.
- [ ] **3** — bar → composer → highlight + margin on both screens ≤1 s: `margin.e2e.ts` scenario 1, screenshot `anchored-margin-items.png`.
- [ ] **4** — Log comment with nothing selected: `margin.e2e.ts` scenario 5 (agent delivery = Lane A5 live check, PR 5).
- [ ] **5** — agent edit and agent anchors live: `doc.e2e.ts` scenario 4, `margin.e2e.ts` scenario 2.
- [ ] **6** — highlights follow, orphan, link to version: `margin.e2e.ts` scenario 3, `doc.e2e.ts` scenario 7, screenshot `orphaned-margin-card.png`.
- [ ] **7** — accept/reject: `margin.e2e.ts` scenario 4.
- [ ] **10** — reload: `doc.e2e.ts` scenario 5 + reload step of `margin.e2e.ts` scenario 1.
- [ ] **12** — iphone project green; `phone.e2e.ts` 44 px + overflow assertions.
- [ ] Reply anywhere incl. agent comments with `anchor: null` (#882): `margin.e2e.ts` reply scenario.
- [ ] Library: `@sjawhar/proof-editor` 0.2.0 on npm with types; `npm run smoke` output.
- [ ] Deployed-image substitute: `bun run e2e:deployed` against the branch image via the `acceptance` profile — output pasted.
- [ ] Deleted: `DocEditor.tsx`, `DocView.tsx`, `SelectionMenu.tsx`, `e2e/preview.ts`, `gen/prepare-dist.sh`, five rendering dependencies; unit + e2e green; bundle chunk size stated.
- [ ] Stacked on PR 3; deploy rule stated (no devbox redeploy until this merges).

## Self-review

- **Spec coverage:** § Browser: `ProofDocument` replaces `DocEditor`/`DocView` (T2); Y.Doc + provider + awareness from identity (T2); `createProofEditor(root, { ydoc, awareness, user, readOnly, onMarkAction })` (T2/T5); `onMarkAction` seam → Composer → mark id → `POST {artifact, mark_id}`; rejection removes the mark (T4/T5, decision 4); popover Reply/Resolve/Accept/Reject → margin buttons on the same routes (decision 2); `marks` map → `applyRemoteMarks` (T5); read-only when closed (T2); historical version through the same editor read-only with the highlight (T2, decision 6); versions picker/naming/diff moved (T2, decision 7); margin list role + click-to-focus both ways (T4/T5); Log composer anchor-less path (main, scenario 4); dependencies removed/added (T2, decision 11) ✓. § Fork (Q1a): additional files only, release = tag → publish → bump (T1) ✓. § Events and delivery: no browser change beyond `Anchor.mark_id` (T4) ✓. § Error handling `ANCHOR_MISSING` (T4, decision 4) ✓. § Testing > Playwright: doc/margin replaced, two contexts, chromium + iphone, lines 1–7, 10, 12 (T3/T6) ✓. § Delivery item 4 = this PR; stacking + deploy rule (Scope boundary) ✓. Suggestion mode (design paragraph) → D1. Threads spec → separate plan, with the two library hooks it needs shipped in T1.
- **Placeholders:** none — every task names files, tests, commands, expected output; every scenario has a name and its assertions.
- **Type consistency:** `ComposerAnchor { artifact, mark_id, quote }` (T4) is what `ProofDocument.onMarkAction` builds (T5) and `Composer` posts as `{ artifact, mark_id }`; `MarkComposeRequest.kind` values match `composerKindFor` (T5); `DocumentBridge { focusMark, setActiveMarks }` (T4) is what T5 registers; `markPositions: ReadonlyMap<string, number>` (T4 context) is produced by `markPositions(doc)` (T5); `Highlight { by, id, quote }` (T2) is built in `IssuePage` and consumed by `VersionView`/`embedHighlight`; `DocumentRuntime { connect, createEditor }` (T2) is what `fakeDocumentRuntime` (T2) and T5's tests provide; `MarkAction` kinds in T1's Interfaces are the ones T5 switches on; e2e helpers named in T3 are the ones T6 calls; `aria-current` (T4) is what T6's focus scenario asserts.
- **Judgment calls the executor must not "fix":** decisions 1–20; `heatMapMode: "hidden"`; the editor stays mounted while a version is shown; `Anchor.mark_id`, never row ids, keys the editor ↔ margin mapping; `{ timeout: 1000 }` on peer-visibility assertions; `bun run e2e -- <file>` partial runs are for Tasks 2–5 only.

## Hardening ledger

(empty — filled by the implementer/reviewer as hardening items are found and closed)
