# @legion/proof-editor

Dispatch's document editor: the Milkdown/ProseMirror entry point, the typed-block grammar and the
block-id feature. A placement red-team ruled all three CONSUMER (LEGION-287), so they live here
rather than in the `sjawhar/proof-sdk` fork, and a Dispatch editor change no longer costs a fork
release, an npm publish and a pin bump.

## Where the code came from

Two different commits of `sjawhar/proof-sdk` matter here, and they are not the same fact.

The files in the table below, but for `src/directive-factories.ts`, were copied byte for byte out
of **24a5fc94915cda5704897c497af6312c140db40d**, the fork's v0.3.13 — the last commit that still
held them. The cut that followed deleted them from the fork, so that commit stays the audit
source however far the fork line moves. `src/directive-factories.ts` comes from
micromark-extension-directive 4.0.0 instead.

The upstream modules this package imports at runtime come from the git dependency, pinned at
**dc6a852b2ab45463dc9d3d17762235450c8ff0d6** on the fork's cleaned `library` line. Moving that
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
| `src/directive-factories.ts` | `factoryName`, `factoryLabel` and `factoryAttributes`, copied from micromark-extension-directive 4.0.0 (`lib/factory-name.js`, `lib/factory-label.js` and `lib/factory-attributes.js`), which does not export them: `src/directive-container.ts` tokenizes a `:::` opener's name, label and attributes with them. They are kept whole, the `disallowEol` branches for `false` that neither call takes included (INFRA-581) |

Nineteen files under `src/` are legion's own rather than copies, so the file-by-file audit below does
not reach them:

| File | What it is |
| --- | --- |
| `src/collab-cursor-plugin.ts` | The peer-cursor plugin: y-prosemirror's, except that a peer's caret is not drawn while it sits on the focused local caret, where Chromium and WebKit otherwise drop or misplace typing (LEGION-289); it is also biome-checked |
| `src/editor/schema/dom-attributes.ts` | `withDomAttributes`, the DOM-output-spec helper lifted out of `block-ids.ts` so the typed-block schema can use it too |
| `src/editor-highlights.ts` | Presentation-only active and pulse highlights for marks and blocks, drawn as a cached ProseMirror `DecorationSet`, because a host class write on editor DOM is read back as an edit (LEGION-458). The set is mapped through edits inside one node and rebuilt when the active or pulsed set changes, or an edit can change which nodes carry a highlighted id or where a highlighted block ends: a mark or attribute step, inserted content that holds a highlighted target (a collaborator's update replaces the whole document), and, while a block is highlighted, a replace across a node boundary; it is also biome-checked |
| `src/editor-change-decorations.ts` | Presentation-only change marks a host draws over a read-only document, as a ProseMirror `DecorationSet`, for the same reason `src/editor-highlights.ts` is one (INFRA-575): one `setChangeDecorations({ text, widgets })` carries the whole set and replaces the last one, since its one caller computes both lists from one `ChangeMarks`. A text entry draws a class over a run of a block's own text, by a `range` of character offsets into that text (the block's inline content is walked, counting each leaf node the characters its type's `leafText` draws, a `hardbreak`'s newline among them, exactly as `textContent` counts them, so an offset after an inline atom or a hard break still lands on its character) or over the whole block where it names no range - the two ends are one optional field, so an entry with only one end cannot be written - and a widget entry draws a piece of text beside the document, before the top-level block it names or at the end of it. An entry names its block by index among the document's top-level blocks, and one naming a block the document does not hold, or text it does not reach, is dropped. The editor this serves is read-only and `setMarkdown` replaces its whole document, so a document change rebuilds the set from the entries rather than mapping stale positions; it is also biome-checked |
| `src/trailing-newline-input.ts` | Types over a selection that would leave its text block ending in a newline, where Firefox otherwise puts the text before a code block's newline or deletes a paragraph's hard break (LEGION-289); it is also biome-checked |
| `src/editor/schema/uuid.ts` | `uuidV4`, the block-id default generator: a v4 UUID from `crypto.getRandomValues`, which every browsing context defines, where `crypto.randomUUID` exists only in a secure context, so a document opened over plain HTTP by a LAN address or a host name minted no id (LEGION-461); it is also biome-checked |
| `src/record-mark-history.ts` | The plugin that keeps the composer's own record-mark writes out of undo history in both undo managers, so neither undo nor redo writes one back: a write of `proofComment`, `proofSuggestion` or `dispatchAsk` steps that takes nothing from another record (it removes no span, as a creation over another record's mark does not since the three declare `excludes: ''`, or puts back what it removes, as upstream's suggestion restamp does), and `removeRecordMark`'s removal; any other removal of a record mark keeps its undo step (LEGION-363, LEGION-458); it is also biome-checked |
| `src/record-mark-retype.ts` | Retyping a provisional record mark for the margin composer's Comment / Suggest / Ask switch, and the precise span-by-span removal behind the handle's `removeMark`, over text and inline atoms such as an image alike (LEGION-363); it is also biome-checked |
| `src/record-mark-target.ts` | Which mark a click or a hover in the document names: of the mark spans around the target, the record mark whose range is the smallest, the innermost span's among marks the same size, since which of two overlapping record marks nests inside the other follows the order they were made in (LEGION-458); it is also biome-checked |
| `src/typed-block-drawing.ts` | The typed-block drawing both parse modes share: `markdownSource`, a file's text without one leading U+FEFF, which micromark drops before it counts the offsets the tree carries; the content-rule fit check, which maps each child to the node its `parseMarkdown.match` accepts (a mark node's children in its place) and calls `NodeType.createAndFill`, since Milkdown drops a node whose content breaks its rule, with all its text, without throwing; `drawDirective`, a directive drawn in place as a paragraph of its opening line, its content formatted (the label paragraph dropped) and a paragraph of its closing fence when the source closes it; and `remarkDecideTypedBlocks`, the transformer whose innermost-first walk keeps a container directive typed only when the caller accepts it and its processed content fits. `remarkUnfitTypedBlocks` (the headless engine's default mode) accepts every directive `remarkTypedBlocks` let through, so an ask holding a heading is drawn as its lines instead of vanishing (INFRA-565); it is also biome-checked |
| `src/read-only-directives.ts` | `remarkReadOnlyDirectives`, the headless engine's read-only stand-in for `remarkTypedBlocks`, built on `remarkDecideTypedBlocks`: a container directive the block schema refuses (or any, with no schema) is drawn in place, and the paragraph-line directive refusals are not applied, so a body only shown to a reader formats around a block Dispatch does not know instead of falling back whole to its raw source (INFRA-529). An accepted typed block whose content would break its content rule is drawn the same way. A typed block kept typed loses every attribute the block schema marks `server`, after the schema accepts it, so each reads at its schema default and a body's forged `:::ask{state="answered" …}` reads as an open decision (INFRA-612). The live editor makes the same drawing decision, through the shared `acceptedTypedBlock`, as `liveTypedBlocksRemarkPlugin` (reading the editor's schema the first time it parses, registered after the mark plugins): a paste or `setMarkdown` (a past version's view) draws a directive the block schema refuses as its literal lines rather than throwing, where a throw in a paste let the browser insert the plain text with its lines joined; it keeps the `server` attributes, which a past version must show and settlement writes from the ask's row (sjawhar/legion#2433); it is also biome-checked |
| `src/ask-content.ts` | `relaxAskContentCheck(schema)`, which replaces the `ask` node's `validContent` so a built ask may hold any non-empty run of the children of the schema's `block` group, the blocks the document service accepts in it. The non-typed ones are listed in `packages/envoy/internal/dispatch/pmdoc/schema/block-node-types.json`, which pmdoc's `isBlockNodeType` embeds and this package's drift test reads, so the two sides cannot drift. y-prosemirror builds each node through `createChecked` and deletes from the shared document one it refuses, so an ask two collaborators' concurrent edits left off its `paragraph+ bullet_list?` rule (two lists, a paragraph after its list) lost its text in every editor and on the server (sjawhar/legion#2433). Content matching keeps the rule, so local edits, input rules, pastes and markdown parses still hold to it. `brokenAskInputGuards`, view props `src/lib.ts` installs with `view.setProps(brokenAskInputGuards)`: inside an ask that breaks its rule, an input whose edit throws - a key command (Enter twice to leave a list, Backspace joining a list item into the question or over a selection across blocks), text input over a selection, a drop, a cut, or a `beforeinput` insertion or deletion over a selection across blocks - is declined, with no edit and the default prevented, instead of escaping as a page error while the browser applies its own edit (a `beforeinput` whose own edit does not throw is applied as ProseMirror's edit in place of the browser's); an input method that starts composing over such a selection has the selection collapsed to its start first, so the composed text lands there and nothing is deleted. `keepUnselectedAskBlocks`, a plugin `src/lib.ts` registers with `.use(keepUnselectedAskBlocks)`, refuses a local edit inside such an ask that deletes any character or other leaf (an image, a hard break, a divider that is itself the block) of a child block the selection does not touch - never a pure insertion, and never an undo or redo of the person's own edit (prosemirror-history's transactions pass, as y-prosemirror's do) - which an edit the browser makes itself (`document.execCommand`, as writing extensions use) and ProseMirror reads back from the DOM would otherwise do; the DOM observer then redraws the page from the kept state; it is also biome-checked |
| `src/mermaid-svg.ts` | `@legion/proof-editor/mermaid`, the DOM half of a Mermaid diagram, for Dispatch's Markdown bodies (INFRA-528) and the editor's mermaid code blocks (INFRA-532): `mermaidSvgElement` turns the markup `src/mermaid-render.ts` returns into an `<svg>` of the page's document with the editor's accessible name; `showMermaidSvg` mounts it with a sheet in the host element's open shadow root, created once and reused, so the diagram's own `<style>` and marker ids reach nothing outside it; `MermaidDrawing` and `MermaidDrawer` are the contract by which the editor asks its host to draw a source. It imports nothing from upstream, so a page imports it without beautiful-mermaid or ELK. `sanitizeMermaidSvg` is stricter than upstream's private `sanitizeSvg`, because a comment or a document is written by any person or agent: markup that is not an SVG document throws `Invalid SVG output`; element names compare exactly, since XML names are case-sensitive, so an `<SVG>` root is refused and a `<RECT>` removed; only the elements beautiful-mermaid emits survive, every `on*` and `href` attribute goes (matched case-insensitively), every attribute but a `data-*` one that holds a CSS escape or a fetching function (`url(…)` other than a local `url(#id)`, `image-set(…)` and the like, which an author's `classDef` or `style` paint reaches) goes, and each `<style>` loses its Google Fonts `@import`; it is also biome-checked |
| `src/mermaid-render.ts` | `@legion/proof-editor/mermaid-render`, the DOM-free half: `renderMermaidMarkup` calls upstream's `renderProofMermaidSvg`, so the validation, renderer, colours and spacing are upstream's, and returns the unsanitized markup, or throws upstream's reason for a source it refuses. It touches no DOM, so its only caller is Dispatch's Web Worker, for comments and documents alike, off the reader's main thread, where beautiful-mermaid's and ELK's cost grows without bound with the diagram; in a browser worker the caller hides `self` during the call, or ELK's bundled engine takes the worker for its own and refuses every flowchart (Dispatch's `features/refs/mermaid-render.worker.ts`); it is also biome-checked |
| `src/mermaid-code-block-view.ts` | The editor's view of every code block, in place of upstream's `mermaidDiagramsPlugin` (INFRA-532), whose view drew on the page's thread and put the diagram's `<style>`, with its Google Fonts `@import`, into the page. A plain block keeps upstream's DOM. A mermaid block is drawn by the host's `drawMermaid` (Dispatch's Web Worker) into the preview's shadow root with `showMermaidSvg`; its layout (preview, status line, the open source disclosure holding the `pre`) is set only in ProseMirror's own `update`, never when an answer arrives, so a late drawing cannot move the contentDOM under a caret; an edit asks again `EDIT_REDRAW_DELAY_MS` (300 ms) after the last change, cancelling the earlier request (a drawer never draws after its cancel), the shown drawing staying until the answer; `destroy` cancels. With no drawer, or one that never answers, the block shows its source, and an `unavailable` answer (no renderer can run) clears any earlier drawing and the status, so the block shows its source again. It imports nothing from upstream, so beautiful-mermaid and ELK stay out of the editor's chunk; it is also biome-checked |
| `src/directive-container.ts` | `directiveContainerSyntax`, the micromark extension `remarkContainerDirectives` registers for the `:::name[label]{attributes}` container: a micromark document construct, held on the document tokenizer's container stack as a block quote or a list item is, in place of micromark-extension-directive's flow construct, which tokenized a block's content once for each block around it, so a body nesting `:::` a few thousand deep froze a reader's tab for about a minute (INFRA-581). It emits that package's token types, so `mdast-util-directive` builds the same `containerDirective` nodes from them; its `resolveAll` (`resolveDirectiveContainers`) and the `lazyContentEnd` construct keep the flow construct's reading of blank lines, lazy lines and the line endings after fences; and an opener standing inside `maxDirectiveNesting` (32) open directive containers does not open, so its line reads as the text it is. It reads an opener's name, label and attributes with the copied factories in `src/directive-factories.ts`; it is also biome-checked |
| `src/block-nesting.ts` | The block-nesting bound: micromark's own `list` and `blockQuote` held to `maxBlockNesting` (100), the number of blocks Dispatch's document service lets a document open inside one another. "The block-nesting bound" below says why it rewrites the two constructs in place and what that rewrite reaches; it is also biome-checked |
| `src/editor/schema/leaf-marks.ts` | `withoutLeafMarks`: a node that is inline, is a leaf and is not text - `html`, `image`, `hardbreak`, `footnote_reference` - may hold no mark, so every parse seam (`src/lib-headless.ts`'s `parseMarkdown`, `src/lib.ts`'s shared `parserCtx` and its `setMarkdown`) strips the emphasis, strong, strikethrough or link mark the markdown wrapped around one. Go's reader of the same document holds marks on text alone (`packages/envoy/internal/dispatch/pmdoc/node.go` refuses a mark on any node that is not text) and the editor's own collaborative store drops such a mark on the next round trip, so a document drew one way in the editor and another way in a comment or a past version. The rule is structural rather than a list of those four names, because that is the shape both readers have. The bottom-up walk returns the same node wherever nothing changed, so a document holding no marked leaf comes back as it went in, and it builds with `node.mark` and `node.copy` rather than `Node.fromJSON` or `doc.check()`, which validate attributes the commonmark preset's own image parser leaves `null`. It is composed beside `withBlockIds`, never folded into it, since that walk returns its input unchanged when no block was missing an id |
| `src/nested-links.ts` | `remarkHoistNestedLinks` and the `$remark` plugin that registers it: a `link` holding a `link` is rewritten into siblings, the inner link beside the runs around it and each run wrapped in a copy of the outer link's `url` and `title`, so `[a <https://u.test> b](u)` reads as `a ` under the label's link, the autolink's text under its own, and ` b` under the label's again. The only markdown that nests the two is an autolink inside a link label, since CommonMark forbids an inline link there, and `@milkdown/transformer` cannot read that label as written: it holds one flat mark set, so the link mark runner's `closeMark` on the inner autolink removes the `link` type outright (`removeFromSet`) and every later run of the label was left with no link at all, where Go's reader gives it the label's link again (`packages/envoy/internal/dispatch/pmdoc/parse.go`). The hoisted spelling is the tree Go's renderer writes for itself (`[a ](u)<https://u.test>[ b](u)`), so both readers agree rather than meeting at a third reading. It is registered at every parse seam - the explicit chain in `src/lib-headless.ts` and, through `remarkCtx`, `src/lib.ts`'s shared `parserCtx` and its `setMarkdown` - and changes no output, since a unified transformer runs on `runSync` at parse and never on `stringify` |
| `src/tests/harness.ts` | The `bun test` registration the copied suites call instead of their own `test()` tally |

Twenty-three kinds of edit are allowed in the copied files, and no others: the import specifiers of
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
`./record-mark-retype`; `src/lib.ts` installing `editorHighlightsPlugin` and routing
`setActiveMarks`, `setActiveBlocks`, `focusMark`'s pulse and `focusBlock`'s pulse through
`./editor-highlights`; `src/lib.ts` installing `editorChangeDecorationsPlugin` and routing the
handle's new `setChangeDecorations` through `./editor-change-decorations`, whose
`ChangeDecorations`, `TextDecoration` and `BlockWidget` it re-exports (INFRA-575);
`src/dispatch-marks.ts`'s `dispatchAsk` schema declaring `excludes: ''`,
so two readers' asks may cover the same text, with `removeAskMark` removing that mark's instance
by id rather than every `dispatchAsk` over its range and the parse runner closing its own span of
nested asks rather than every open ask (LEGION-458); `src/dispatch-mark-events.ts` reading the mark
a click or a hover names from `./record-mark-target` rather than taking the innermost mark span,
so a click on text two record marks cover opens the narrower one (LEGION-458);
`src/dispatch-action-bar.ts`'s Comment / Suggest / Ask buttons and its exported
`commentOnSelection` taking one path, `ActionBarController.act`, which marks nothing for an empty
or stale range or a read-only view, with `src/lib.ts` exposing `commentOnSelection` on the handle
for Dispatch's `Ctrl+Alt+M` (LEGION-680); `src/block-schema.ts` exporting `typedBlockRefusal`,
the refusal `visit` throws, so the read-only transform decides with the same rule, and
`src/lib-headless.ts`'s `readOnly` option installing `remarkReadOnlyDirectives` from
`./read-only-directives` in place of `remarkTypedBlocks` (INFRA-529); `src/lib-headless.ts`
taking `renderBlock` and passing it to `blockSchemaPlugins`, as `src/lib.ts` does, and
re-exporting `HostBlockRenderer`, with the case in `src/tests/block-schema.test.ts` that a host
drawing keeps the block's copy attributes (INFRA-526); `src/block-schema.ts`'s
`remarkTypedBlocks` reading its source through `markdownSource` from `./typed-block-drawing`, and
`src/lib-headless.ts`'s default branch installing `remarkUnfitTypedBlocks` after
`remarkTypedBlocks` (INFRA-565); `src/lib.ts` installing `liveTypedBlocksRemarkPlugin` from
`./read-only-directives` after the mark plugins in place of `typedBlockRemarkPlugin`, and calling
`relaxAskContentCheck(schema)` from `./ask-content` before it binds the shared document and
installing `brokenAskInputGuards` from it with `view.setProps(brokenAskInputGuards)` and
`keepUnselectedAskBlocks` from it with `.use(keepUnselectedAskBlocks)` among its plugins,
`src/lib-headless.ts` calling `relaxAskContentCheck` after building its schema, with its
`parseMarkdown` and `readOnly` comments naming the live editor's step,
`src/lib-remark-directive-plugin.ts` dropping `typedBlockRemarkPlugin`, and
`src/typed-block-commands.ts`'s `retype` and `canRetype` requiring the typed node's content match
of the body, so a block is never retyped into an ask it cannot be (sjawhar/legion#2433);
`src/lib-remark-directive-plugin.ts` registering `directiveContainerSyntax` from
`./directive-container` in place of micromark-extension-directive's flow container, with
`containerDirectiveSyntax`, the package's import and the `micromark-util-types` import it needed
gone and the plugin's doc comment rewritten to say which construct it registers (INFRA-581);
`src/block-schema.ts`'s default-mode refusal of a `:::name{...}` paragraph line standing inside
`maxDirectiveNesting` directives (imported from `./directive-container`), an opener the tokenizer
left as text — `rejectUnsupportedDirectiveSyntax` counts the `containerDirective` nodes around
each paragraph and `unsupportedDirectiveReason` takes that count (INFRA-581);
`src/directive-factories.ts` joining micromark-extension-directive 4.0.0's `lib/factory-name.js`,
`lib/factory-label.js` and `lib/factory-attributes.js` into one module, in that order, each after
a comment naming its file, under the package's MIT notice, with the three files' imports one block
at the top and each JSDoc `@import` an `import type` (INFRA-581); `src/lib.ts` installing
`mermaidCodeBlockPlugin` from `./mermaid-code-block-view` in place of upstream's
`mermaidDiagramsPlugin` and taking the host's `drawMermaid` option (typed `MermaidDrawer`,
imported from `./mermaid-svg`) for it, and `src/lib.css` dropping its `.proof-mermaid-svg` rules,
which that view's shadow-root sheet carries (INFRA-532); and annotations, casts and assertions
that make a file type-check, each of which erases before runtime (below).

To audit a copied file, diff it against `jj --ignore-working-copy -R <proof-sdk> file show -r
24a5fc94 root:src/<file>`; every file in the copied-files table but `src/directive-factories.ts`
has that counterpart, and the only lines that differ should be the twenty-three kinds.
`src/directive-factories.ts`'s counterparts are those three files of micromark-extension-directive
4.0.0 (`node_modules/.bun/micromark-extension-directive@4.0.0` at the repository root, or the
package's npm tarball); each function there differs from its source by the join and the type
annotations and casts alone.

`scripts/`, `tests/` and `upstream/` are legion's own. `scripts/` and `tests/` are linted and
type-checked like any other package's; `upstream/` is generated, and Biome is off over it the
way it is over the copy. `tests/upstream-pin.test.ts` is the guard on the pin: it reads the
installed dependency and fails when a fix the pinned line carries has gone missing, or when
`upstream/` stops being what the pinned sources emit. `tests/upstream-boundary.ts` has no
runtime at all — `bun run typecheck` is what runs it, and it fails when a name crossing the
boundary is `any` again.

The copied files are proof-sdk's and stay under its MIT license, as does `upstream/`, which tsc
derives from proof-sdk's pinned sources; `LICENSE` beside this file is that license, taken from the
fork at 24a5fc94. `src/directive-factories.ts` is micromark-extension-directive's instead and
stays under that package's MIT license, whose notice the file carries at its top. Legion's own
files listed above, `scripts/` and `tests/` are under the repository's Apache-2.0 license.

`src/directive-container.ts` relies on two parts of micromark's document tokenizer: a container
state's `_closeFlow`, which a closing fence sets to end the flow inside the block, and a
construct's `resolveAll`, which runs over the document's events once it is tokenized. micromark,
which reaches this package through `mdast-util-from-markdown`, stays at 4.0.2 in `bun.lock`: a
move must keep both working, and 4.0.3 is quadratic for quotes and lists (micromark/micromark#246).

## The block-nesting bound

`src/block-nesting.ts` holds micromark's `list` and `blockQuote` to `maxBlockNesting`, 100, which
is `maxNesting` in `packages/envoy/internal/dispatch/pmdoc/nesting.go`, whose comment names this
one back. Both count a quote, a list, a list item and a typed block as one block each, so a list
opener takes two levels (the list and its first item) while a later item of an open list takes
none, and `src/directive-container.ts` records a `:::` container's depth into the same count. An
opener that would stand past the bound returns `nok` and its line reads as the text it is, the way
a `:::` opener past `maxDirectiveNesting` does. Only a document the document service refuses can
reach the bound, so every document it stores reads here exactly as it read before; past the bound,
where micromark ran every open container's continuation on every line and a few hundred `- `
markers on one line overflowed the stack of Milkdown's parser and of prosemirror-model's
`DOMSerializer`, a reader now sees the text instead of losing the page (sjawhar/legion#2519).
Footnote definitions are the one difference between the two counts: the service counts them, and
this module does not, because the browser reads them through micromark-extension-gfm-footnote,
whose construct it does not rewrite. That only lets the browser draw deeper than the service would
accept, never shallower, so everything the service stores still reads the same.

The module rewrites the two construct objects `micromark-core-commonmark` exports, in place and
once, when `src/lib-remark-directive-plugin.ts` is imported. micromark's `disable` cannot express
this bound: `create-tokenizer.js` refuses a construct whose name is disabled whoever passes it, so
turning `list` and `blockQuote` off by name would also turn off the `effects.attempt(list, …)` and
`effects.attempt(blockQuote, …)` each of them runs on itself to read a later item or a deeper `>`.
Rewriting the objects instead makes the bound realm-wide, because every parser in the tree shares
them: the page bundle, Dispatch's parse worker, the live editor's Milkdown parser (built by
`@milkdown/preset-commonmark`, which passes through none of this package's unified processors) and
the pmdoc fixture generator, which imports `@legion/proof-editor/headless`. A second copy of
`micromark-core-commonmark` installed beside this one would carry constructs of its own and escape
the rewrite, so `tests/heavy-bodies.test.ts` parses through a bare `unified().use(remarkParse)`
processor as well: a duplicate copy fails that test.

## The four dependency patches

Four of the root `package.json`'s `patchedDependencies` entries are the parsers this package reads
markdown with, patched so that a body costs time in proportion to its length rather than to its
square (sjawhar/legion#2519). Each patch leaves the package's output as it was, and each patches
both `lib/` and `dev/lib/`, because Vite resolves the `development` export condition.

| Patch | What it makes linear |
| --- | --- |
| `micromark@4.0.2` | `lib/initialize/document.js`'s lazy-line test. `writeToChild` scanned every event of the child flow backwards on each lazy line; it now scans forward from a watermark of events already ended at or before the line's start, which can never be open across this line or a later one, since that offset only grows while one child flow lives and an end is never unset. The watermark is dropped when the event below it is no longer the one it was (a resolver rewrites events), for a new child flow, and at the end of input |
| `micromark-core-commonmark@2.0.3` | `lib/label-end.js`. The scan for an opening `[` resumes where the scan of the previous `]` stopped and then goes on below the label start that `]` balanced, rather than walking every event again on each `]`; and the definition lookup is skipped outright where the document defines nothing, and otherwise bails, without serializing the label, once the label holds more characters that are not markdown whitespace than the longest defined identifier. `lib/list.js` takes a list item's blank-line continuation without asking `blankLine` whether the line is blank: that continuation opens with `effects.check(blankLine, …)`, and where the line is already over - the code is a line ending or the end of the document, which is what a line of only spaces and tabs is once the continuations above have taken their indentation - the answer is yes and the check's store and restore are work for nothing. Every open list item runs it once per blank line, so a body of D nested items and N blank lines runs D*N of them: on 50 nested items and 32 KB of blank lines, about two fifths of what micromark spent |
| `micromark-extension-gfm-footnote@2.1.0` | `lib/syntax.js`'s call scan, the same backwards walk. Footnote definitions are flow constructs, so they are all known before text is tokenized: with none defined the walk is skipped, and otherwise it resumes where the previous `]`'s walk stopped |
| `mdast-util-from-markdown@2.0.3` | `lib/index.js`'s `prepareList`, which spliced each list item's enter and its exit into the whole event array and so moved everything after them once per item. It now records where each one goes and `compile` rebuilds the events once, the sort being stable so an inner item's exit stays before an outer item's |

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

The pinned source commit carries five fixes; the first four are each an open upstream pull
request. The Dark Reader
fix (EveryInc/proof-sdk#81): peer-cursor colours and mark decorations avoid inline `style`
attributes, because Dark Reader rewrites those attributes inside the contenteditable and
ProseMirror reads the writes as content mutations. That can cause an endless redraw loop that
wedges the tab. The proof-mark rendering fix (EveryInc/proof-sdk#82): the five proof marks render
only their `data-*` attributes, where upstream renders `id`, `kind` and `by` as
`[object Object]`, and a replace suggestion's widget redraws when its replacement changes. The
split-mark fix (EveryInc/proof-sdk#83): accept, reject and deleteMark act on each run of a mark
that other text splits, rather than on one range from its first run to its last, which deleted or
re-attributed the text between. The overlapping record marks fix (EveryInc/proof-sdk#84):
`proofComment` and `proofSuggestion` declare `excludes: ''`, so a comment or suggestion over text
another one covers no longer cuts it out of the overlap; the plugin's removals take the mark with
the acted-on id rather than every mark of its type over its text, and the markdown parser closes
the nested span it is closing rather than every open span of the type. The Mermaid diagram types
fix: the Mermaid preview's validator runs its flowchart checks only on the diagrams
beautiful-mermaid renders as flowcharts or state diagrams, so a sequence, class, ER or xychart
diagram reaches its own renderer instead of being refused with `Invalid mermaid header`.
`tests/upstream-pin.test.ts` reads or runs the installed modules so a pin that loses any of them
fails: it reads the Dark Reader fix where it lives, renders each proof mark, redraws a revised
replacement in a happy-dom view, rejects an insert split by another, comments twice over one word,
round-trips that through markdown and deletes one of the two, and renders one diagram of every
Mermaid type while still refusing a flowchart that ends in an arrow. Moving the pin changes the
package dependency, `bun.lock`, `upstream/` and this file's source audit references; no Bun patch
applies to this source.

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

The copied tree keeps its sources' style — proof-sdk's single quotes and line width, and in
`src/directive-factories.ts` micromark-extension-directive 4.0.0's published layout — and Biome's
formatter, linter and `assist` are all turned off over it in the root `biome.json`, so a diff
against the source stays readable. `assist` matters as much as the other two: its
`organizeImports` is a safe fix, so one `biome check --write` or an editor with organize-on-save
would reorder the copy's imports. `upstream/` is excluded the same way, for the same reason:
it is tsc's output, not source. Anything legion writes here — `scripts/`, `tests/`,
`src/collab-cursor-plugin.ts`, `src/editor-highlights.ts`, `src/editor-change-decorations.ts`, `src/editor/schema/uuid.ts`, `src/trailing-newline-input.ts`,
`src/record-mark-history.ts`, `src/record-mark-retype.ts`, `src/record-mark-target.ts`, `src/read-only-directives.ts`, `src/typed-block-drawing.ts`, `src/ask-content.ts`, `src/mermaid-svg.ts`, `src/mermaid-render.ts`, `src/mermaid-code-block-view.ts`,
`src/directive-container.ts`, `src/block-nesting.ts`,
`src/tests/harness.ts` — follow the
repo's conventions and are checked.
