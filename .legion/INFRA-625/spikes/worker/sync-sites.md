# INFRA-625 spike — sites that rely on the Markdown parse being synchronous in the layout commit

Baseline: `renderWithEngine` calls `render(readyEngine)` **synchronously** once any body on the page
has loaded the engine (`markdown-engine.ts:166-170`); only the first body on a page, or a failed
schema load, takes the async path (`markdown-engine.ts:171-180`). The whole pass —
parse → paint → ask hosts → linkify → anchors → pictures → mermaid → `onRendered` — runs inside one
`useLayoutEffect` (`useRenderedMarkdown.tsx:154-192`). Paths are relative to
`packages/dispatch/web/src/features/` (`refs/`, `agent-view/`, `shell/`, `conversation/`, `activity/`).

| # | Site | What it does with the result in the same commit | What breaks if the parse lands ≥1 frame later (element holding literal source) | Is `setIsFallback(true)` + literal text an OK interim? |
|---|---|---|---|---|
| 1 | `refs/useRenderedMarkdown.tsx:161-173` | `engine.parse` then `paint(element, parsed, serializer)`; replaces children in the layout phase, before paint | Nothing in this hook itself; it is the pivot. All rows below are its downstream consumers | n/a — this is the fallback's own site |
| 2 | `refs/useRenderedMarkdown.tsx:169-173` + `:41-70` (`collectAskHosts`) | Queries `section[data-proof-block-type="ask"]` **on the just-painted element**, pairs shells 1:1 with the doc's `ask` nodes, calls `applyAskBlockShell`, and `setAskHosts` so `TextAskCard` portals mount in the same commit | No shells exist in literal text ⇒ `shells.length === 0` ⇒ `NO_ASK_HOSTS` ⇒ no decision card for ≥1 frame; the card pops in later. Pairing still works after the late paint because it re-queries | Acceptable *visually* (an ask reads as its source lines), but the card must be mounted on resolve and `askHosts` reset to `NO_ASK_HOSTS` on every source change, or a stale host (detached shell) is portalled into. `applyAskBlockShell` is called only where a card is mounted (`doc/typed-block-drawing.ts:265-269`) — keep that invariant |
| 3 | `refs/useRenderedMarkdown.tsx:174-181` | `linkifyDispatchRefs(element)` → `collectReferenceAnchors(element)` → `decorate(element, found)` → `setReferenceAnchors` / `setPictureAnchors`; `RefLink` / `DispatchPicture` portals land in the same commit | Literal source *does* get linkified (bare `dispatch://…` text is wrapped), so a fallback frame can mount `RefLink` portals into anchors inside raw source — the late paint then destroys those nodes. Portals keyed to removed DOM nodes are the real hazard, not the missing format | Only if anchor/picture state is cleared (or the pass skipped) while pending; otherwise portals point at detached nodes |
| 4 | `refs/MarkdownPreview.tsx:255-279` (`decorate`) | Runs `inertLinks`, `markPhrases` and the `lead` prepend on the painted element and returns the anchors to portal into; throws `"A reference anchor is not a link of the preview it was found in."` when an anchor is not in the replaced map (`:268-272`) | Search-hit `<mark>`s and `lead` get applied to raw source for a frame (`MarkdownPreview.test.tsx:288-289` shows that shape for the refuse case). Risk: running `decorate` twice (fallback + late paint) double-prepending `lead` | Yes — the fallback path already runs `decorate`, and the preview is CSS-clamped so height is stable. Needs per-paint idempotence (it has it: each paint calls `element.replaceChildren()` first, `MarkdownPreview.tsx:31`) |
| 5 | `refs/MarkdownBody.tsx:71-80` (`paintBlock`) → `refs/task-checkboxes.ts:18-30` | `drawTaskCheckboxes` runs **inside** the paint, right after `serializeFragment`, then tight `<li data-spread='false'>` paragraphs are unwrapped | Checkboxes simply do not exist until the late paint; no external consumer reads them in the commit | Yes, unconditionally. They are `disabled` and take no tab stop (`task-checkboxes.ts:12-15`), so no focus is lost |
| 6 | `refs/useRenderedMarkdown.tsx:182-184` → `refs/mermaid-diagrams.ts:326-357` | Queries `pre[data-language]` on the painted element; a **cached** source draws in this same layout effect (`:343-345`); uncached ones stay code and are replaced when the shared worker answers (`:349-357`) | Already async by design; a later parse only delays the first draw. **But** `endsInOpenMermaidFence` (`:335`, impl `:146-170`) calls `reparse(markdown + probe)` — a **second synchronous engine.parse per paint**. Async makes it a second round-trip, doubling worker traffic on every streaming frame of a turn ending in an open fence | Yes (blocks are code until drawn anyway). The cleanup contract matters: cancel and the next run happen in one commit, which is why the drop check is deferred to a microtask (`:357-367`). An async parse puts the re-ask **after** that microtask, so a streaming body would abandon and re-send its in-flight diagram every frame unless the pending source survives the gap |
| 7 | `refs/useRenderedMarkdown.tsx:185` (`onRenderedRef.current?.()`) | Fires after the render lands; prop plumbed through `refs/MarkdownBody.tsx:112,132` and `activity/EventBody.tsx:22,37,49,71,86,97` | Fires a frame late. No production caller passes it today (grep: only `EventBody`'s pass-through and tests) | Yes. Must fire on the *real* paint, not the fallback, or a future height-measuring caller reads the literal-text height |
| 8 | `shell/ViewportAnchor.tsx:186-197` (`getSnapshotBeforeUpdate`) + `:199-217` (`componentDidUpdate`) | Measures the anchor row's `getBoundingClientRect().top` immediately before React mutates the DOM, then **exactly once** after, and `window.scrollBy(0, delta)` before paint. Hosts: `conversation/ConversationTab.tsx:1048-1320`, `activity/ActivityTab.tsx:93-174` — both render `MarkdownBody` rows | **Main breakage.** One measurement per commit, no `ResizeObserver`, no second pass (`ViewportAnchor.tsx:83-96`: "componentDidUpdate then compensates exactly once, before paint"). A body above the anchor that resolves a frame later and changes height shifts the reader with nothing to correct it. `components/PinButton.tsx:29-31` already documents that a turn's height must not depend on an async body | No: literal text is not the rendered height (source is longer; a 10 k-char body as one text node is much taller). Needs (a) reserved height, (b) a `ResizeObserver`/second compensation in `ViewportAnchor`, or (c) excluding bodies inside a `ViewportAnchor` from the threshold |
| 9 | `conversation/use-follow-latest.ts:53-66` (`jumpToLatest`) | Layout effect reads `newest.getBoundingClientRect().top` plus the composer height, then `window.scrollTo` so the newest turn sits under the composer | A late body re-flows after placement; the reader lands wrong with no re-measure. Only an `enabled`/`ownSend`/arrival change re-runs it (`:86-103`) | No, not without re-placing once the parse lands |
| 10 | `agent-view/use-live-follow.ts:306-316` | `ResizeObserver` on the viewport **and** its content wrapper; `jumpToEnd()` whenever content grows while following | Already height-change driven, so a late body is tolerated at the live end. Scrolled-back readers are compensated by `settleRebuild` (`:227-231`), itself a commit-time measurement | Yes for a following reader; a scrolled-back reader may still drift, since that compensation runs on rebuild commits, not on a late parse |
| 11 | `agent-view/AgentThread.tsx:107-115` (streaming turns) | `MarkdownText` / `Reasoning` render `<MarkdownBody markdown={useFrameCoalesced(text)} softBreaks="line" />` | The layout effect re-runs **once per painted frame** while a turn streams — `agent-view/useFrameCoalesced.ts:13-26` caps re-parses at one per frame (the first value passes through at once, so the mounting commit parses). Async ⇒ one worker round-trip per frame per streaming turn, replies arriving out of order (must be sequence-gated), and a body that is always ≥1 frame stale. Each frame also re-runs the mermaid probe (row 6) | **No** for streaming: flipping to literal text every frame while a parse is in flight makes the turn flicker between source and rendered. A streaming turn must keep its last rendered DOM until the new parse lands; the interim state must be "previous paint", not "literal text" |
| 12 | `refs/markdown-engine.ts:245-262` (`useMarkdownHeadline`) | Already **state-based**: `useLayoutEffect` → `renderWithEngine` → `setHeadline`, `undefined` until the engine answers (one pre-paint commit when loaded). Consumers: `refs/MarkdownPreview.tsx:285` (`title=`), `refs/reference-target.ts:452` (link text) | Nothing structural breaks — the one site already tolerant of an async parse; it just stays `undefined` longer. A link's text falls back to `shortForm` (`refs/routes.ts:722-725`) | Yes. No change needed beyond the longer `undefined` window |
| 13 | `refs/MarkdownBody.tsx:138,146` / `refs/MarkdownPreview.tsx:290` (`data-markdown-fallback`) | The only `isFallback` consumers: they set `data-markdown-fallback="true"` on the root. No other app code reads it; tests assert it (`MarkdownBody.test.tsx:601,644,657,680,698`, `MarkdownPreview.test.tsx:152,222,288,297`, `MarkdownPreview.edges.test.tsx:116`) | Semantics collide: today it means "engine/parse failed, final". Reusing it for "parse pending" makes `[data-markdown-fallback]` transient and every `toBeNull()` assertion order-dependent | Needs a **separate** flag (e.g. `data-markdown-pending`); the terminal-failure meaning is what the tests and the hook doc (`useRenderedMarkdown.tsx:85-86`) describe |
| 14 | `app.tsx:755-757` + `refs/markdown-cards.ts:15-17` | The app provides `TextAskBlockCard` as `TextAskCard`; `Card` is a hook dependency (`useRenderedMarkdown.tsx:148,192`), so a provider change re-runs the whole effect | With an async parse, every provider identity change schedules another worker round-trip for every body on the page | Yes, but the `Card` dependency must not re-parse: cache by `(markdown, softBreaks)` so only the DOM passes re-run |

## Tests that assert on a body right after render/mount

Almost all already await (`findBy…` / `waitFor`), because the engine's *first* load is async in tests —
e.g. `MarkdownBody.test.tsx:596-601, 652-657, 664-669, 675-680, 691-698, 975`;
`markdown-surfaces.test.tsx:282, 314, 354, 382, 461, 498, 534`;
`doc/AskBlockCard.test.tsx:439-443` ("Option labels render through MarkdownBody, asynchronously").
What needs new awaits/flushes are the assertions **chained after** the first await, which rely on the
rest of the body being complete in that same commit:

| Test | Line | Why it needs a second `findBy…`/flush |
|---|---|---|
| "a container directive the schema refuses shows its fence lines and the body still formats" | `MarkdownBody.test.tsx:592-601` | `querySelectorAll("p")` and the `[data-markdown-fallback]` null check run synchronously after one `findByText` |
| "a decision the read-only parse refuses leaves the valid decision beside it its own card" | `MarkdownBody.test.tsx:607-645` | counts `ask` shells and asserts fallback null in the same tick |
| "a Pandoc fenced div reads as text and the body still formats" | `MarkdownBody.test.tsx:651-657` | same shape |
| "a clock time such as 16:25Z renders as Markdown, not as the literal-text fallback" | `MarkdownBody.test.tsx:686-698` | asserts the fallback attribute is `null`; transiently set under a pending flag |
| "raw unsupported HTML renders as literal text with no element created" | `MarkdownBody.test.tsx:674-680` | asserts fallback `=== "true"`; must distinguish terminal failure from pending |
| "a body re-renders when its text grows, formatting what has closed since" | `MarkdownBody.test.tsx:972-990` | the streaming contract: each `rerender` needs its own await |
| "a task item draws one disabled checkbox, ticked as its source is, in every list shape" | `MarkdownBody.test.tsx:1528-1600` | all eight items read out of one `waitFor` snapshot |
| "a source drawn before draws in the layout effect that paints its body" | `MarkdownBody.test.tsx:1298-1299` | explicitly asserts the cached-mermaid draw happens *in* the paint effect; `await warmMarkdownRenderer()` exists to make the parse synchronous (`:1273-1274`) |
| "a body whose source changes or that unmounts before the worker answers draws only the newest source" | `MarkdownBody.test.tsx:1272-1276` | same `warmMarkdownRenderer` premise |
| mermaid request/cancel tests (4) | `MarkdownBody.test.tsx:1374-1375, 1408-1409, 1452-1453, 1492-1493` | each opens with `await warmMarkdownRenderer()` so only the worker is late; an async parse adds a second late stage |
| "an ask block in a body draws the decision card…" / "…with no card provided keeps its question and options as the source list" | `MarkdownBody.test.tsx:457-480, 504-520` | shell attributes (`data-dispatch-ask-options`, `data-dispatch-ask-state`) read synchronously after the shell is found |
| "a body the engine cannot parse shows its literal text, led and highlighted…" | `MarkdownPreview.test.tsx:276-298` | waits for `data-markdown-fallback === "true"` then for `<strong>`; a pending flag on the same attribute makes this ambiguous |
| headline tests | `MarkdownPreview.edges.test.tsx:213-217, 220-248` | already state-driven; unaffected |

## `engine.parse` outside a React effect

- **No production call site.** Every parse goes through `renderWithEngine`, and all three call sites sit
  in hooks: `refs/useRenderedMarkdown.tsx:161-162`, `refs/markdown-engine.ts:255-256`
  (`useMarkdownHeadline`), and the mermaid probe `refs/mermaid-diagrams.ts:165` reached from that same
  layout effect via `refs/useRenderedMarkdown.tsx:182-184`.
- Test-only: `refs/markdown-engine.test.ts:14-17` (`parseThroughSharedEngine`).
- Selectors/sorts/search/export use **string** transforms only, never the engine:
  `refs/markdown-fragment.ts:21` (`searchSnippetMarkdown`), `:35` (`markdownHead`),
  `refs/markdown-engine.ts:200-214` (`shortenReferences`, used by `plainWords`), called at
  `search/SearchPalette.tsx:204,286`. These stay synchronous and need no change.
- The only synchronous-parse *requirement* outside a paint is the mermaid open-fence probe
  (`refs/mermaid-diagrams.ts:146-170`) — a parse-of-a-parse inside the paint.

## What an async-above-threshold parse must preserve

1. **One-measurement scroll compensation.** `ViewportAnchor` measures once per commit
   (`shell/ViewportAnchor.tsx:186-217`) with no `ResizeObserver`; `conversation/use-follow-latest.ts:53-66`
   places the reader from a single `getBoundingClientRect`. A body whose height changes a frame later must
   reserve its final height, be excluded from the threshold inside those lists, or gain a second
   compensation pass. `components/PinButton.tsx:29-31` already encodes this constraint.
2. **Streaming turns never flicker.** A re-parse while text streams (`agent-view/AgentThread.tsx:107-115`,
   one per frame via `agent-view/useFrameCoalesced.ts:13-26`) must keep the previous painted DOM rather
   than fall back to literal text, and replies must be sequence-gated so an older frame's parse cannot
   overwrite a newer one.
3. **Bounded worker traffic.** One parse per painted frame per body is today's ceiling; the mermaid
   open-fence probe doubles it (`refs/mermaid-diagrams.ts:335`). Cache by `(markdown, softBreaks)` so a
   `Card`/`decorate`/`paint` identity change (`refs/useRenderedMarkdown.tsx:192` deps) re-runs the DOM
   passes without a second round-trip.
4. **Mermaid request continuity across the cleanup/re-run gap.** The abandon check is a microtask precisely
   because cleanup and re-run land in one commit (`refs/mermaid-diagrams.ts:357-367`); with an async parse
   the re-ask arrives later, so in-flight sources must survive it.
5. **Portal/DOM identity.** Reference, picture and ask-host state (`refs/useRenderedMarkdown.tsx:174-181`)
   must never point at nodes from an interim paint: clear the three anchor states when a parse is
   dispatched, and mount portals only from the committed paint.
6. **`isFallback` keeps meaning "terminal failure".** `data-markdown-fallback`
   (`refs/MarkdownBody.tsx:138,146`, `refs/MarkdownPreview.tsx:290`) is asserted as a final state by nine
   tests; a pending state needs its own attribute.
7. **Ask cards still mount for every shell, 1:1.** `collectAskHosts` returns nothing unless shells and
   `ask` nodes match exactly (`refs/useRenderedMarkdown.tsx:50-54`); the late path must re-run it on the
   real paint, and `applyAskBlockShell` must stay confined to the card-mounting path.
8. **The synchronous path stays for small bodies.** `renderWithEngine`'s sync branch
   (`refs/markdown-engine.ts:166-170`) and `warmMarkdownRenderer` (`:151-153`, called from
   `shell/DeploymentResilience.tsx:259-261`) are what make previews, option labels
   (`inbox/AskOptionRow.tsx:25,29`) and hover cards paint in their mounting commit.
