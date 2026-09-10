# Dispatch server on tree documents (Lane B, PR 3) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The Go Dispatch server stores every document as Proof's ProseMirror tree (`Y.XmlFragment "prosemirror"`) with a server-maintained `marks` map; anchors are marks in the tree; suggestions accept/reject edit the tree; settle and versions render the tree; every existing `Y.Text("content")` document and anchor is converted once at boot; the SPA keeps working (read-only rendered view) until PR 4 replaces it with the Proof editor.

**Architecture:** `internal/dispatch/docs` composes the pure `pmdoc` package (merged as #839 + #853) over ygo rooms: `Read → FindQuote/FindMark → Splice/MarkRange/Unmark → Update` inside one `Server.Apply`, one Yjs update, one version write. `model.Anchor` becomes `{artifact_id, mark_id, quote, version, orphaned}`; the API accepts `{artifact, quote, occurrence?}` (server writes the mark) and `{artifact, mark_id}` (server verifies the mark, waiting ≤1 s for the Yjs update). A one-shot boot migration behind `schema_migrations` converts legacy rooms. The SPA moves to the quote path and a read-only rendered preview *before* the server flips, so every task boundary is green in CI.

**Tech Stack:** Go 1.26, `github.com/reearth/ygo v1.49.5` (`crdt`, `provider/websocket`), `internal/dispatch/pmdoc`, Postgres 16 (pgx), Bun + TypeScript (contracts, envoy-client, SPA, Playwright e2e).

**Spec:** `docs/superpowers/specs/2026-09-09-dispatch-document-experience-design.md` — § Document model, § Go package `internal/dispatch/pmdoc` (server operations table), § Anchors, § Events and delivery, § Migration, § Error handling (normative), § Testing > Server, § Delivery item 3. Plan 1 (`docs/superpowers/plans/2026-09-09-pmdoc-tree-package.md`) is the executed predecessor; this plan is its "plan 2".

---

## Scope boundary (PR 3)

**In:**
- Go server: `internal/dispatch/docs` on the tree, `internal/dispatch/pmdoc` additions the server needs (empty-range `Splice`, `FindHeading`, `Size`, `ListMarks`, `MarkAttrs`), `model.Anchor`/`AnchorInput`, `api` anchors/comments/asks/artifacts/issues/error mapping, one-shot boot migration + `check-documents` preflight subcommand, deletions of the offset machinery.
- Contracts type change for `Anchor`/`AnchorInput` (drop `from`/`to`; add `mark_id`) and the compile fixes it forces in envoy-client tests.
- SPA: the minimum that keeps the dashboard working against a tree-backed server and green in CI — the CodeMirror/`y-codemirror.next` edit mode is **removed** (there is no `Y.Text` to bind to), the document tab becomes the existing rendered `DocView` of `GET /artifacts/{id}/text` (already refetched on `artifact.version` SSE events, `web/src/api/sse.ts`), selections anchor through `{quote, occurrence}`, the "View original text" link keys on the comment id. Playwright scenarios that typed into CodeMirror or asserted `.cm-*` carets are deleted here; PR 4 replaces `doc.e2e.ts`/`margin.e2e.ts` on the Proof editor (spec § Testing > Playwright).
- Server API accepts **both** anchor inputs so PR 4 needs no server change.

**Out:** Proof editor in the SPA, host adapter, `marks`-map consumer, live anchor highlights in the browser, browser-written marks, e2e rewrite on the editor (PR 4); deploy and live acceptance (PR 5); TOON delivery (Lane A5, separate).

**Interim state on `main` between this PR and PR 4 (not deployed until PR 5):** browsers read documents and create anchored items from a selection in the rendered view; nobody can type into a document from the browser; agents edit through `dispatch_doc_edit` as before.

## Global Constraints

- Dispatch stays Go; the only Node at build time is the pmdoc fixture generator (`PROOF_EDITOR_DIST=/home/ubuntu/tmp/proof-editor-dist bash prepare-dist.sh` then `bun install && bun run gen` inside `packages/envoy/internal/dispatch/pmdoc/gen`).
- Room = artifact id. Two shared types per room: `prosemirror` (`Y.XmlFragment`, authoritative) and `marks` (`Y.Map<string, StoredMark>`, written only by the server). `Y.Text("content")` is never read or written after the migration.
- Rendered markdown = `pmdoc.Render(pmdoc.Read(fragment))` everywhere text is needed (`GET .../text`, versions, settle comparison, `dispatch_doc_read`). Anchor marks never render (`pmdoc` `visibleMarks`).
- All agent- and upload-supplied markdown enters the tree as `pmdoc.StripAnchorMarks(pmdoc.Parse(md))`; a `pmdoc.ErrSchema` from `Parse` is the caller's fault → `400 INVALID_MARKDOWN` with the reason. A `pmdoc.ErrSchema` from `Read`/`Render` of a live tree is the server's fault → `500 DOC_SCHEMA`; settle logs and skips the version.
- Error table (spec § Error handling) is normative: `404 TARGET_NOT_FOUND`, `409 TARGET_AMBIGUOUS` with candidates, `409 ANCHOR_MISSING`, `409 ANCHOR_ORPHANED`, `500 DOC_SCHEMA`, migration parse failure fails boot naming the artifact. No silent fallbacks anywhere.
- Every mutation stays one `Server.Apply`, one Yjs transaction, one persisted update (`applyLive`'s joined-transaction path is unchanged: suppress ygo's own persist, `AppendUpdateTx` inside the API transaction). `pmdoc.Read` runs *outside* the Yjs transaction (before `transact`), the mutators (`Update`, `MarkRange`, `Unmark`) inside it, and a fresh `Read` afterwards when the result is needed — the pmdoc tests use exactly this shape.
- Lock order preserved: issue row (`requireOpenIssue` / settle's `for update of i`) before artifact/comment rows.
- Red-first: every task's tests are written and run to failure before the implementation.
- Comments describe current behaviour, never history.
- Go checks while implementing are package-scoped: `cd packages/envoy && gofmt -l ./internal/dispatch && go vet ./internal/dispatch/... && DISPATCH_TEST_DATABASE_URL=<url> go test ./internal/dispatch/<pkg>/ -run <Test> -v`. Postgres: `./scripts/dev-postgres.sh` prints the URL (`postgres://postgres:dispatch@127.0.0.1:55432/dispatch?sslmode=disable`); the test harness (`docs/persistence_test.go` `openTestStore`) creates an isolated database per test from it.
- TS checks are per package with the package's own scripts (`bun run typecheck`, `bun run test`, `bun run lint`, `bun run e2e`); `bun install --frozen-lockfile` must pass, so a dependency change commits `bun.lock`.
- Migration numbers are claimed at merge time: this plan reserves `schema_migrations` version **8** for the Go-owned legacy-document migration; before pushing, confirm no `0008_*.up.sql` landed on `main` and renumber if one did.
- jj, not git: commit = `jj describe -m "<msg>"` then `jj new`. One task, one commit.

## Decisions where the spec under-specifies (decided here; not open questions)

1. **Replies carry no anchor of their own.** Proof's `proofComment` mark excludes overlap, so a reply cannot mark the same range as its parent. `POST /comments` with `anchor` together with `reply_to` or `ask_id` → `400 INVALID_COMMENT "replies inherit their parent's anchor; omit anchor"`. The SPA stops sending the parent's anchor on replies (Margin `onReply`). `skills/dispatch/SKILL.md` gains that one clause.
2. **`marks` projection = comments and suggestions only, in Proof's `StoredMark` field names** (`proof-sdk/src/formats/marks.ts`). Comment: `{kind:"comment", by, createdAt, text, resolved, replies:[{by,text,at}]}`. Suggestion: `{kind:"replace", by, createdAt, text, content, status:"pending"|"accepted"|"rejected"}`. Accept/reject **set `status`** (Proof's `applyRemoteMarks` removes anchors and local metadata for `accepted`/`rejected`; a deleted key would leave stale local metadata). No `quote` field (marks are always in the tree; PR 4 calls `applyRemoteMarks` with `hydrateAnchors: false`). Asks are not projected: Proof has no ask kind and the AskCard is driven by Dispatch's asks API.
3. **Mark attributes.** `{id, by}` where `by = actor.Kind + ":" + actor.ID` (`user:alice`, `session:…`); suggestions add `kind:"replace"`. Server-written marks use the **row id** as the mark id (rows are inserted with an explicit `gen_random_uuid()` allocated first); browser-written marks (PR 4) keep whatever id the browser chose. Consumers always read `anchor.mark_id`.
4. **Version markdown is canonical.** `SeedText`/`ReplaceText` return `Render(Parse(md))`; version 1 / upload versions store that string; the migration writes no version (the first snapshot or settle after boot records the canonicalised text under whoever touches it).
5. **Unrecorded-mark sweep** considers only record-bearing types (`dispatchAsk`, `proofComment`, `proofSuggestion`); Proof-native `proofAuthored`/`proofFlagged`/`proofApproved` are never swept. "Recorded" = any `asks`/`comments` row on the artifact whose `anchor->>'mark_id'` matches, any state. First-seen times live in `roomState`; settle re-arms itself for the earliest expiry so a dangling mark is removed at TTL without waiting for another edit. TTL = `Deps.UnrecordedMarkTTL` (default 1 min).
6. **Suggestion accept/reject is kind-aware** so PR 4's browser suggestions (`insert`/`delete`/`replace` marks) need no server change: accept `replace|delete` → `Splice(range, Parse(replace_with))`; accept `insert` → `Unmark` (text stays); reject `replace|delete` → `Unmark`; reject `insert` → `Splice(range, empty)`. Missing mark → `409 ANCHOR_ORPHANED` and the row is marked orphaned. Accept needs no separate `Unmark` for `replace`: the marked runs are replaced, and `Update` writes exactly the target marks.
7. **`TARGET_AMBIGUOUS` candidates carry ProseMirror positions** (`pmdoc.Candidate{from,to,context}`); agents disambiguate by `context`/`occurrence`. Contract type `TargetCandidate` is unchanged.
8. **Anchors' `TARGET_NOT_FOUND` is `404`** (spec table), no longer `422`.
9. **`pmdoc.Splice` gains empty-range insertion**, the piece the `insert` op needs; `after: "start"|"end"` and `heading:Title` are block boundaries (inserted markdown becomes new blocks), a quote anchor is an inline point (inline markdown splices into the run; block markdown splits the textblock exactly as ProseMirror's `replaceRange` does — the fixture generator is the arbiter).
10. **`ReplaceText` (re-upload) re-anchors** every open row: after `Update(frag, Parse(md))`, `FindQuote(target, quote, near: old position)` → `MarkRange`; the refresh that follows orphans the rest. Proof-native marks do not survive a wholesale replace.
11. **SPA interim** (see Scope boundary): read-only rendered view, quote anchors, margin ordered by the quote's position in the live text, `Suggest` enabled from a rendered selection (the offset-precision reason for disabling it is gone).
12. **Preflight tooling:** `dispatch check-documents` reports, without writing, every doc artifact's state and whether its legacy markdown parses and its anchors resolve — because block-level HTML (e.g. `<details>`) is outside Proof's schema (`pmdoc/parse.go` rejects `ast.HTMLBlock`) and a production spec containing it would fail boot. PR 5 runs it against the production database before deploying.
13. **Inline edits keep their edge whitespace.** CommonMark strips a paragraph's leading/trailing spaces, so `Parse(" more")` loses the space an agent meant. When an op's replacement parses to an inline document, `applyOperations` re-attaches the markdown's leading/trailing whitespace to the first/last text run before splicing; block replacements are untouched.

---

## File Structure

```
packages/envoy/internal/dispatch/
├── pmdoc/
│   ├── splice.go            // Splice accepts r.From == r.To (point insertion, block-boundary insertion)
│   ├── find.go              // + FindHeading, Size, ListMarks, MarkAttrs
│   ├── find_test.go         // + point/heading/list/attrs tests; oracle test handles `point` cases
│   ├── fixtures_test.go     // spliceFixture gains At, Point
│   └── gen/gen.ts           // + point insertion oracle cases
├── docs/
│   ├── api.go               // API interface: SeedText/ReplaceText return canonical; MarkQuote, VerifyMark, AcceptSuggestion, RejectSuggestion, ProjectMark; ApplyReplace removed (Task 5)
│   ├── tree.go              // NEW: fragment/marks names, treeOf, renderTree, parseInput, error vars
│   ├── marks.go             // NEW: MarkKind/MarkSpec/MarkRecord, mark operations, projection, refreshAnchors, sweep
│   ├── edits.go             // REWRITTEN: EditOp → tree (FindQuote/FindHeading/Splice); ErrInvalidOp gains Reason
│   ├── mutation.go          // SeedText/ReplaceText/Text/ApplyOps/captureLive/writeVersionTx on the tree
│   ├── service.go           // settle renders the tree, DOC_SCHEMA skip, sweep re-arm; Deps.MarkWait/UnrecordedMarkTTL
│   ├── websocket.go         // onLoadDocument refuses an unmigrated legacy room
│   ├── legacy.go            // NEW: MigrateLegacyDocuments, InspectLegacyDocuments (version 8)
│   └── *_test.go            // rewritten to the tree + mark model; legacy_test.go, marks_test.go new
├── model/model.go           // Anchor{ArtifactID, MarkID, Version, Quote, Orphaned}; AnchorInput{Artifact, Quote*, Occurrence*, MarkID*}
├── api/
│   ├── anchors.go           // resolveAnchor: quote path (MarkQuote) / mark path (VerifyMark); offset branches gone
│   ├── asks.go, comments.go // explicit row ids, evict-on-failure, projection, reply-anchor rejection, accept/reject via mark ops
│   ├── artifacts.go, issues.go // canonical version markdown from SeedText/ReplaceText
│   ├── server.go            // error mapping: pmdoc + docs errors; len16 for caps (Task 5)
│   └── *_test.go            // mark-model assertions
├── text/                    // anchor.go, anchor_test.go, utf16.go DELETED in Task 5; refs.go, diff.go stay
packages/envoy/cmd/dispatch/main.go           // boot: MigrateLegacyDocuments; subcommand check-documents
packages/envoy/cmd/dispatch/{README,AGENTS}.md
packages/contracts/src/dispatch-api.ts        // Anchor, AnchorInput
packages/envoy-client/src/__tests__/*.ts      // fixtures
packages/dispatch/web/src/…                   // read-only DocEditor, quote Composer, margin ordering, comment-id version link; anchors.ts deleted
packages/dispatch/e2e/…                       // preview.ts helper; CodeMirror scenarios removed; quote anchors
packages/dispatch/package.json, bun.lock      // CodeMirror family, y-codemirror.next, yjs, @hocuspocus/provider removed (PR 4 re-adds the last two)
skills/dispatch/SKILL.md                      // reply/anchor clause; INVALID_MARKDOWN in the error list
```

Task order (each boundary green in CI): 1 pmdoc → 2 SPA/e2e to the quote path against the *current* server → 3 document content on the tree → 4 mark operations → 5 anchors are marks → 6 migration + preflight → 7 contract type change → 8 docs, sweep of leftovers, PR.

---

### Task 1: pmdoc — point insertion, headings, mark listing

**Files:**
- Modify: `packages/envoy/internal/dispatch/pmdoc/splice.go`, `find.go`, `find_test.go`, `fixtures_test.go`, `gen/gen.ts`
- Regenerate: `packages/envoy/internal/dispatch/pmdoc/testdata/splices.json`

**Interfaces (produces):**
```go
// Splice: when r.From == r.To, with is inserted at that position. An inline point inside a
// textblock splices inline content into the run and splits the textblock around block content;
// a position on a block boundary inserts with's blocks there, fitted to the parent.
func FindHeading(doc *Node, title string, occurrence *int) (Range, error) // [pos,end) of the heading whose text == title; ErrTargetNotFound / *ErrTargetAmbiguous (Candidates hold each heading's range and text)
func Size(doc *Node) int                                                 // ProseMirror position after the last block
type MarkRef struct{ Type, ID string }
func ListMarks(doc *Node) []MarkRef        // distinct proof*/dispatchAsk marks with a string id, document order
func MarkAttrs(doc *Node, markType, id string) (Attrs, bool) // attrs of the first run carrying the mark
```

- [ ] **Step 1: Oracle cases.** In `gen/gen.ts` add to `replaceRangeCases` (the case type gains `at?: string; point?: "after" | "before" | "after-textblock" | "before-textblock" | "doc-start" | "doc-end"`; `from`/`to` become optional):

```ts
  { name: "insert-inline-after-quote", markdown: "Alpha first. omega.\n", at: "first.", point: "after", replacement: "X\n", inline: true },
  { name: "insert-inline-at-textblock-start", markdown: "Alpha omega.\n", at: "Alpha", point: "before", replacement: "X\n", inline: true },
  { name: "insert-inline-at-textblock-end", markdown: "Alpha omega.\n", at: "omega.", point: "after", replacement: "X\n", inline: true },
  { name: "insert-blocks-after-quote-splits-paragraph", markdown: "Alpha first. omega.\n", at: "first.", point: "after", replacement: "- X\n- Y\n" },
  { name: "insert-block-at-doc-start", markdown: "Body.\n", point: "doc-start", replacement: "# Title\n" },
  { name: "insert-block-at-doc-end", markdown: "Body.\n", point: "doc-end", replacement: "Tail.\n" },
  { name: "insert-block-after-heading-textblock", markdown: "# Title\n\nBody.\n", at: "Title", point: "after-textblock", replacement: "Intro.\n" },
  { name: "insert-block-before-heading-textblock", markdown: "# Title\n\nBody.\n", at: "Title", point: "before-textblock", replacement: "Lead.\n" },
  { name: "insert-paragraph-after-list-item-textblock", markdown: "- one\n- two\n", at: "one", point: "after-textblock", replacement: "extra\n" },
```

and in the `.map` compute the range:

```ts
  const pointPosition = (): number => {
    switch (point) {
      case "doc-start": return 0;
      case "doc-end": return doc.content.size;
      case "after": return quotePosition(doc, at!) + at!.length;
      case "before": return quotePosition(doc, at!);
      case "after-textblock": return doc.resolve(quotePosition(doc, at!)).after();
      case "before-textblock": return doc.resolve(quotePosition(doc, at!)).before();
    }
  };
  const fromPos = point ? pointPosition() : quotePosition(doc, from!);
  const toPos = point ? fromPos : quotePosition(doc, to!) + to!.length;
```

Emit `at` and `point` in the case record. Regenerate: `cd packages/envoy/internal/dispatch/pmdoc/gen && PROOF_EDITOR_DIST=/home/ubuntu/tmp/proof-editor-dist bash prepare-dist.sh && bun install && bun run gen && bun run check`. Expected: `wrote 31 documents and 36 splice cases` (27 existing + 9) then `fixtures up to date`.

- [ ] **Step 2: Failing tests** (`find_test.go`, `fixtures_test.go`):

```go
// fixtures_test.go: spliceFixture gains
//   At    string `json:"at"`
//   Point string `json:"point"`
// and TestSpliceFixturesHaveRequiredCoverage's wantNames gains the nine names above.

// find_test.go — TestSpliceMatchesEngineReplaceRange computes its range through this helper:
func spliceRange(t *testing.T, doc *Node, fx spliceFixture) Range {
	t.Helper()
	if fx.Point == "" {
		from, err := FindQuote(doc, fx.From, nil, nil)
		if err != nil { t.Fatal(err) }
		to, err := FindQuote(doc, fx.To, nil, nil)
		if err != nil { t.Fatal(err) }
		return Range{From: from.From, To: to.To}
	}
	var pos int
	switch fx.Point {
	case "doc-start":
		pos = 0
	case "doc-end":
		pos = Size(doc)
	default:
		at, err := FindQuote(doc, fx.At, nil, nil)
		if err != nil { t.Fatal(err) }
		block := textblockContaining(doc, at.From) // test helper over walk(): the textblock whose [pos+1,end-1] contains at.From
		switch fx.Point {
		case "after":
			pos = at.To
		case "before":
			pos = at.From
		case "after-textblock":
			pos = block.To
		case "before-textblock":
			pos = block.From
		}
	}
	return Range{From: pos, To: pos}
}

func TestFindHeadingMatchesTitleAcrossLevels(t *testing.T) {
	doc, err := Parse("# A\n\ntext\n\n## A\n\n# B\n")
	if err != nil { t.Fatal(err) }
	if _, err := FindHeading(doc, "A", nil); !errors.As(err, new(*ErrTargetAmbiguous)) {
		t.Fatalf("two headings titled A: err = %v, want ErrTargetAmbiguous", err)
	}
	one := 1
	r, err := FindHeading(doc, "A", &one)
	if err != nil { t.Fatal(err) }
	// doc = h1("A")[0,3) p("text")[3,9) h2("A")[9,12) h1("B")[12,15)
	if r != (Range{From: 9, To: 12}) { t.Fatalf("second A = %v, want [9,12)", r) }
	if r, err := FindHeading(doc, "B", nil); err != nil || r != (Range{From: 12, To: 15}) { t.Fatalf("B = %v %v", r, err) }
	if _, err := FindHeading(doc, "text", nil); !errors.Is(err, ErrTargetNotFound) { t.Fatalf("paragraph text is not a heading: %v", err) }
	if got := Size(doc); got != 15 { t.Fatalf("Size = %d, want 15", got) }
}

func TestListMarksAndMarkAttrs(t *testing.T) {
	doc, err := FromJSON(fixtureNamed(t, "marks").PMJSON)
	if err != nil { t.Fatal(err) }
	want := []MarkRef{{"proofComment", "c1"}, {"dispatchAsk", "a1"}, {"proofSuggestion", "s1"}}
	if got := ListMarks(doc); !reflect.DeepEqual(got, want) { t.Fatalf("ListMarks = %v, want %v", got, want) }
	attrs, ok := MarkAttrs(doc, "proofSuggestion", "s1")
	if !ok || attrs["kind"] != "replace" || attrs["by"] != "session:01a0" { t.Fatalf("MarkAttrs = %v %v", attrs, ok) }
	if _, ok := MarkAttrs(doc, "proofComment", "nope"); ok { t.Fatal("unknown id must not resolve") }
}
```

- [ ] **Step 3: Run** — `cd packages/envoy && go test ./internal/dispatch/pmdoc/ -run 'TestSplice|TestFindHeading|TestListMarks' -v`. Expected: compile failure (`Size`, `FindHeading`, `ListMarks`, `MarkAttrs` undefined); after stubbing, the nine point cases fail with `invalid splice` from `validateSplice`.

- [ ] **Step 4: Implement.**
  - `splice.go`: `validateSplice` allows `r.From == r.To`; `Splice` dispatches `insertAt(doc, pos, with)`: (a) an inline point — the textblock whose `pos+1 <= p <= end-1` (inclusive at both ends, unlike `selectRange`'s strict filter) — inline `with` (`inlineDocument`) → `spliceInline` with a zero-width range (make `spliceInline` insert at `position == r.From` when `From == To`); block `with` → `ascend` with a `spliceSelection` whose `first == last ==` that textblock so `sliceBoundaries` splits the run around the blocks (ProseMirror's fit); (b) otherwise a block boundary — the innermost container (`doc`, `blockquote`, `list_item`, `footnote_definition`, `table_cell`…) with a child boundary equal to `pos` → `fitReplacement(parent, with, enclosingListItem(...), false)` and insert at that child index; when it does not fit, retry at the parent's boundary (same loop shape as `ascend`). Iterate until all nine oracle cases pass — the engine JSON is the arbiter; do not special-case names.
  - `find.go`: `FindHeading` walks `doc.Children` (headings are top-level blocks in Proof's schema) collecting headings whose concatenated text-child content equals `title` exactly; occurrence/ambiguity rules mirror `FindQuote` (`Candidate.Context` = heading text). `Size` = `nodeSize(doc)` exported. `ListMarks` walks text nodes, appending `(Type, id)` for marks of a `proof*`/`dispatchAsk` type with a string `id`, de-duplicated in first-seen order. `MarkAttrs` walks until `nodeMarkID(node, markType) == id` and returns a clone of that mark's attrs.

- [ ] **Step 5: Run** the whole package: `go test ./internal/dispatch/pmdoc/... -count=1`. Expected: PASS (the cross-language `decode.ts` test needs `bun` on PATH — present).

- [ ] **Step 6: Commit** — `jj describe -m "feat(pmdoc): point insertion in Splice, heading lookup, mark listing for the document server" && jj new`

---

### Task 2: SPA and e2e move to quote anchors and a read-only rendered document (against the current server)

This task ships first so the server cutover (Tasks 3–5) never breaks the CI e2e lane. Everything here works against today's server (the quote anchor path and `GET /artifacts/{id}/text` already exist).

**Files:**
- Delete: `packages/dispatch/web/src/features/doc/anchors.ts`, `anchors.test.ts`, `packages/dispatch/e2e/editor.ts`
- Modify: `packages/dispatch/web/src/features/doc/DocEditor.tsx`, `DocEditor.test.tsx`, `features/margin/Composer.tsx`, `Composer.test.tsx`, `features/margin/Margin.tsx`, `features/margin/useMarginItems.ts`, `features/margin/CommentsTab.tsx`, `features/issue/IssuePage.tsx`, `IssuePage.test.tsx`, `web/src/styles.css`, `packages/dispatch/package.json`, `bun.lock`
- Create: `packages/dispatch/e2e/preview.ts`
- Modify: `packages/dispatch/e2e/doc.e2e.ts`, `margin.e2e.ts`, `phone.e2e.ts`, `writes.e2e.ts`, `artifacts.e2e.ts`, `layout.e2e.ts`, `shell.e2e.ts`

**Interfaces:**
- `ComposerAnchor` becomes `{ artifact: string; quote: string; occurrence?: number }`; `Composer` posts `anchor: { artifact, quote, occurrence }` (never `from`/`to`).
- `MarginContext` gains `documentText: string` and `setDocumentText(text: string): void`; `useMarginItems(issueKey, tab, visibleArtifact, documentText)`.
- Version-highlight route: `?version=N&comment=<comment id>` (replaces `&from=&to=`).
- e2e helper: `selectPreviewText(page: Page, quote: string): Promise<void>` selects the first rendered occurrence of `quote` inside the document `article` and dispatches `mouseup` so `DocView.onSelectionChange` fires.

- [ ] **Step 1: Failing unit tests.**
  - `Composer.test.tsx`: every anchor fixture becomes `{ artifact: "document-1", occurrence: 1, quote: "selected" }`; the assertion on `api.createComment`/`createAsk` payloads expects `anchor: { artifact: "document-1", quote: "selected", occurrence: 1 }` and asserts `"from" in payload.anchor === false`.
  - `DocEditor.test.tsx`: delete the tests that construct a `HocuspocusProvider`/`EditorView` (edit mode, `.cm-content` contenteditable, awareness cursor, `getText("content")` insertion); add `test("DocEditor renders the live text and reports a quote selection")`: seed `["artifact", id, "text"]` with `"The quick brown fox"`, render inside a margin provider, drive a DOM `Range` over `brown` inside the `article` + `mouseup`, and assert the margin context received `{ artifact: id, quote: "brown", occurrence: 0, canSuggest: true }`; keep the version picker / diff tests (they use `queryClient.setQueryData(["artifact", id, "text"], …)` already).
  - `IssuePage.test.tsx`: the historical highlight test navigates to `/issues/CORE-1/artifact/spec?version=1&comment=comment-1` with `api.listComments` returning that comment with `anchor.quote = "SQLit"` and asserts `mark.dispatch-anchor-history` has text `SQLit`; the "Spec stays mounted across tabs" test drops its `.cm-editor` wait and asserts the `article` text survives the tab switch instead.
  - `useMarginItems` (add `useMarginItems.test.ts` if absent): two anchored comments with quotes `"fox"` and `"quick"` and `documentText = "The quick brown fox"` order `quick` first; a quote not in the text sorts after anchored items and before unanchored ones.
  - Run `cd packages/dispatch && bun run typecheck; bun run test` — expected: type errors (`ComposerAnchor.from`), failing new tests.

- [ ] **Step 2: Implement the SPA changes.**
  - `DocEditor.tsx`: remove `@codemirror/*`, `y-codemirror.next`, `yjs`, `@hocuspocus/provider` imports, `wsUrl`, `colorForLogin`, `editorAccess`, `mode`, the `host` div, the provider/`EditorView` effect, the decoration effects, and the `Edit`/`Preview` button and connection pill. `liveMarkdown = liveTextQuery.data?.markdown ?? ""`; render `DocView` with `onSelectionChange` mapping to `{ ...next, artifact: artifact.id, artifactId: artifact.id, canSuggest: true }` (`DocView` already supplies `quote`, `occurrence`, `rect`, `from`, `to`; the margin reads `artifact`, `quote`, `occurrence`, `rect`, `canSuggest`). Add `useEffect(() => setDocumentText(liveMarkdown), [liveMarkdown, setDocumentText])`. Keep `Name version`, the version `<select>`, `Diff vs current`, the closed-issue notice, and the section's `aria-label` (unchanged so nothing else moves).
  - `Composer.tsx`: `ComposerAnchor` per Interfaces; mutation payload `anchor: { artifact, quote, ...(occurrence === undefined ? {} : { occurrence }) }`.
  - `Margin.tsx`: `openComposer(kind, anchor: ComposerAnchor | undefined, replyTo?)`; `onReply` calls `openComposer("comment", undefined, comment.id)`; `MarginSelection extends ComposerAnchor` keeps `artifactId`, `canSuggest`, `rect`; context gains `documentText`/`setDocumentText` (`useState("")`).
  - `useMarginItems.ts`: replace the `from`/`to` comparison with `position(anchor) = documentText.indexOf(anchor.quote)`; `-1` sorts after found anchors; ties by `created_at` ascending; unanchored items after all anchored ones, newest first (unchanged).
  - `CommentsTab.tsx`: link `${buildIssuePath({...version})}&comment=${comment.id}`.
  - `IssuePage.tsx`: `IssueDetail` reads `query.get("comment")`; `ArtifactVersionView` takes `commentId` and finds `comments.data.find(c => c.id === commentId)?.anchor`; `historicalHighlight(markdown, undefined, anchor)` already resolves a unique quote occurrence (keep its ambiguity rule → no highlight + "Text changed." status).
  - Delete `anchors.ts`/`anchors.test.ts`. `styles.css`: delete the `.cm-content .dispatch-anchor` and `.cm-content .dispatch-anchor-active` rules. `package.json`: remove `codemirror`, `@codemirror/lang-markdown`, `@codemirror/language`, `@codemirror/state`, `@codemirror/view`, `y-codemirror.next`, `yjs`, `@hocuspocus/provider`; run `bun install` at the repo root and commit `bun.lock`.

- [ ] **Step 3: Run** `cd packages/dispatch && bun run lint && bun run typecheck && bun run test`. Expected: all green; `bun install --frozen-lockfile` at the repo root exits 0.

- [ ] **Step 4: e2e.** Create `e2e/preview.ts`:

```ts
import type { Page } from "@playwright/test";

/** Selects the first rendered occurrence of `quote` inside the document article and fires the
 *  mouseup the app listens to, the way a drag selection arrives. */
export async function selectPreviewText(page: Page, quote: string): Promise<void> {
  const article = page.getByRole("article").first();
  await article.waitFor();
  await article.evaluate((root, quote) => {
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
      root.dispatchEvent(new MouseEvent("mouseup", { bubbles: true }));
      return;
    }
    throw new Error(`quote is not rendered: ${quote}`);
  }, quote);
}
```

  Then per file:
  - `doc.e2e.ts`: "document edits synchronize, version, and compare across users" → keep both users open on the Spec tab; delete the typing/caret block (`aliceEditor.type`, `.cm-ySelectionCaret`, `bobEditor`); keep `editArtifact(...)` and assert both pages' `article` contain `Postgres`; keep the version/diff assertions but compare `version.markdown` with `toContain("Postgres")` rather than exact strings (Task 3 canonicalises). The "stays mounted across tabs" test counts websocket connections (`documentConnections`) that no longer exist — delete it. The historical highlight test: navigate to `?version=1&comment=<id>`; replace the `Edit`-button expectation with `article` visible.
  - `margin.e2e.ts`: API-created anchors → `{ artifact: "spec", quote: "fox" }` / `{ artifact: "spec", quote: "brown" }`; `selectEditorRange` → `selectPreviewText(page, "brown")`; `toMatchObject` assertions drop `from`/`to`; delete the "follows edits" segment (`editor.press("Backspace")` and its `orphaned` poll — the server-side behaviour is Task 5's Go test; the browser scenario is PR 4); `enterEditMode` → nothing (already on the rendered view); the whole-paragraph preview test's `toMatchObject` becomes `{ quote: initialMarkdown, version: 1 }`; the accept flow asserts `article` contains the replacement after the SSE refetch.
  - `phone.e2e.ts`, `writes.e2e.ts`: `selectEditorRange(editor, 10, 5)` → `selectPreviewText(page, "brown")`; drop `enterEditMode`/`Edit` clicks and the `Document editor` textbox locators (assert on `article` text instead).
  - `artifacts.e2e.ts`: replace `enterEditMode` + textbox assertion with `expect(page.getByRole("article")).toContainText("These notes replace the initial spec.")`.
  - `layout.e2e.ts`: `anchor: { artifact: "spec", quote: "<six characters that occur once in the seed spec>" }`.
  - `shell.e2e.ts`: delete the conditional `Preview` toggle block (the button is gone).
  - Delete `e2e/editor.ts`.

- [ ] **Step 5: Run the e2e lane against the current server**: `cd packages/dispatch && bun run e2e` (needs `DATABASE_URL` for `run-server.sh`; use the dev Postgres). Expected: PASS on both projects (`chromium`, `iphone`). Save the `test-results/` screenshots of the rendered document with the margin for the PR.

- [ ] **Step 6: Commit** — `jj describe -m "refactor(dispatch-web): anchor by quote from the rendered document; retire the CodeMirror edit mode ahead of the tree server" && jj new`

---

### Task 3: docs — document content lives in the `prosemirror` tree

Anchors stay offset-based over the *rendered* markdown in this task (`text.Reresolve` on `Render` output); Task 5 replaces them with marks and deletes the offset code. `Y.Text("content")` is no longer written or read by the service.

**Files:**
- Create: `packages/envoy/internal/dispatch/docs/tree.go`
- Rewrite: `packages/envoy/internal/dispatch/docs/edits.go`, `edits_test.go`
- Modify: `docs/api.go`, `docs/mutation.go`, `docs/service.go`, `docs/mutation_test.go`, `docs/service_test.go`, `docs/websocket_test.go`
- Modify: `api/artifacts.go` (`storeArtifact`), `api/issues.go` (`createIssue`), `api/server.go` (error mapping), `api/anchors.go` (`anchorResolveError` also maps `pmdoc.ErrTargetNotFound`), `api/interactions_test.go`, `api/artifacts_test.go` (canonical text expectations)

**Interfaces:**
```go
// tree.go
const fragmentName = "prosemirror"
const marksMapName = "marks"
var ErrInvalidMarkdown = errors.New("markdown is not a Proof document") // wraps the pmdoc reason; api → 400 INVALID_MARKDOWN
var ErrDocSchema = errors.New("document is outside the Proof schema")    // api → 500 DOC_SCHEMA
func parseInput(markdown string) (*pmdoc.Node, error) // StripAnchorMarks(Parse(md)); ErrSchema → fmt.Errorf("%w: %v", ErrInvalidMarkdown, err)
func treeOf(doc *crdt.Doc) (*pmdoc.Node, error)       // Read(doc.GetXmlFragment(fragmentName)); ErrSchema → ErrDocSchema
func renderTree(tree *pmdoc.Node) (string, error)     // Render; ErrSchema → ErrDocSchema
// api.go changes
SeedText(ctx, tx, artifactID, markdown string) (string, error)                     // returns canonical markdown
ReplaceText(ctx, artifactID, markdown string, actor model.Actor) (string, error)   // returns canonical markdown
// edits.go
type ErrInvalidOp struct{ Field, Reason string } // Error(): `invalid document operation field "with": <reason>` (Reason may be empty)
func applyOperations(tree *pmdoc.Node, ops []model.EditOp) (*pmdoc.Node, error)
```
Rules `applyOperations` implements (ops resolve sequentially against the progressively edited tree, as today):
- `replace{find, with, occurrence}`: `FindQuote(tree, find, occurrence, nil)` → `Splice(tree, r, inlineAware(with))`.
- `delete{find, occurrence}`: `Splice(tree, r, parseInput(""))`.
- `insert{markdown, after|before, occurrence}`: position = `"start"` → 0; `"end"` → `pmdoc.Size(tree)`; `"heading:T"` → `FindHeading(tree, T, occurrence)` then `To` (after) / `From` (before); otherwise `FindQuote(tree, anchor, occurrence, nil)` then `To`/`From`; `Splice(tree, Range{p,p}, inlineAware(markdown))`.
- `inlineAware(md)`: `parseInput(md)`; when the result is a single paragraph of text/leaf nodes (an inline document), re-attach `md`'s leading and trailing whitespace to its first/last text run (decision 13).
- Field validation as today (`invalidOp("find")`, `"markdown"`, `"after or before"`, `"op"`, `"heading"`), plus `ErrInvalidMarkdown` from `parseInput` wraps into `&ErrInvalidOp{Field: "with"|"markdown", Reason: err.Error()}`.

- [ ] **Step 1: Failing tests** (rewrite; the listed names are the new/renamed tests — every remaining test in these files is re-pointed from `content.Insert` to the helpers below):

```go
// service_test.go helpers
func editLiveTree(t *testing.T, service *Service, artifactID string, edit func(*pmdoc.Node) *pmdoc.Node) {
	t.Helper()
	err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		frag := doc.GetXmlFragment(fragmentName)
		tree, err := pmdoc.Read(frag)
		if err != nil {
			t.Fatalf("read live tree: %v", err)
		}
		want := edit(tree)
		transact(func(txn *crdt.Transaction) {
			if err := pmdoc.Update(txn, frag, want); err != nil {
				t.Errorf("update live tree: %v", err)
			}
		})
	})
	if err != nil {
		t.Fatalf("apply browser edit: %v", err)
	}
}

// liveTree reads the resident tree under the room lock.
func liveTree(t *testing.T, service *Service, artifactID string) *pmdoc.Node {
	t.Helper()
	var tree *pmdoc.Node
	err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		var readErr error
		tree, readErr = pmdoc.Read(doc.GetXmlFragment(fragmentName))
		if readErr != nil {
			t.Errorf("read live tree: %v", readErr)
		}
	})
	if err != nil && !errors.Is(err, ygws.ErrNoChanges) {
		t.Fatalf("read live document: %v", err)
	}
	return tree
}

// replaceRun rewrites the text of the first run containing find (marks on that run are kept).
func replaceRun(find, with string) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		var visit func(*pmdoc.Node) bool
		visit = func(n *pmdoc.Node) bool {
			if n.Type == "text" && strings.Contains(n.Text, find) {
				n.Text = strings.Replace(n.Text, find, with, 1)
				return true
			}
			for _, c := range n.Children {
				if visit(c) {
					return true
				}
			}
			return false
		}
		visit(tree)
		return tree
	}
}

// deleteRun removes the first text run whose text equals text (ProseMirror has no empty text nodes).
func deleteRun(text string) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		var visit func(*pmdoc.Node) bool
		visit = func(n *pmdoc.Node) bool {
			for i, c := range n.Children {
				if c.Type == "text" && c.Text == text {
					n.Children = append(n.Children[:i], n.Children[i+1:]...)
					return true
				}
				if visit(c) {
					return true
				}
			}
			return false
		}
		visit(tree)
		return tree
	}
}

// mutation_test.go
func TestSeedTextStoresTreeAndReturnsCanonicalMarkdown(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	tx, _ := database.Pool.Begin(context.Background())
	canonical, err := service.SeedText(context.Background(), tx, artifactID, "## Database\nUse SQLite")
	if err != nil { t.Fatal(err) }
	if err := tx.Commit(context.Background()); err != nil { t.Fatal(err) }
	if canonical != "## Database\n\nUse SQLite\n" { t.Fatalf("canonical = %q", canonical) }
	if got, _ := service.Text(context.Background(), artifactID); got != canonical { t.Fatalf("Text = %q", got) }
	loaded, err := service.persistence.Load(context.Background(), artifactID)
	if err != nil { t.Fatal(err) }
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil { t.Fatal(err) }
	if doc.GetText("content").Len() != 0 { t.Fatal("legacy content text must stay empty") }
	tree, err := pmdoc.Read(doc.GetXmlFragment(fragmentName))
	if err != nil || len(tree.Children) != 2 || tree.Children[0].Type != "heading" { t.Fatalf("tree = %#v (%v)", tree, err) }
}

func TestApplyOpsEditsLiveDocumentAndSettlesVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "one two one")
	actor := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	first := 0
	applied, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{
		{Op: "replace", Find: "two", With: "TWO"},
		{Op: "insert", Markdown: "!", After: "end"},          // block boundary → a new paragraph
		{Op: "delete", Find: "one ", Occurrence: &first},
	}, actor)
	if err != nil || applied != 3 { t.Fatalf("applied = %d, %v", applied, err) }
	waitForDocumentText(t, service, artifactID, "TWO one\n\n!\n")
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	if len(version.Authors) != 1 || version.Authors[0] != actor { t.Fatalf("authors = %#v", version.Authors) }
}

func TestApplyOpsInsertsAtHeadingsQuotesAndEdges(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "# Title\n\nBody text.\n")
	_, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{
		{Op: "insert", Markdown: "Intro.", After: "heading:Title"},
		{Op: "insert", Markdown: " more", After: "text."},        // inline point; edge space kept (decision 13)
		{Op: "insert", Markdown: "- item", Before: "start"},
	}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
	if err != nil { t.Fatal(err) }
	waitForDocumentText(t, service, artifactID, "- item\n\n# Title\n\nIntro.\n\nBody text. more\n")
}

func TestApplyOpsRejectsMarkdownOutsideProofSchema(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "keep")
	_, err := service.ApplyOps(context.Background(), artifactID, []model.EditOp{{Op: "replace", Find: "keep", With: "<details>x</details>"}}, model.Actor{Kind: "session", ID: "session-0123456789abcdef"})
	var invalid *ErrInvalidOp
	if !errors.As(err, &invalid) || invalid.Field != "with" || !strings.Contains(invalid.Reason, "block HTML") { t.Fatalf("err = %v", err) }
	waitForDocumentText(t, service, artifactID, "keep\n")
}

// service_test.go
func TestSettleRendersTreeAndWritesVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "before")
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	version := waitForDocumentVersion(t, service.store, artifactID, 2)
	var markdown string
	_ = service.store.Pool.QueryRow(context.Background(), `select markdown from artifact_versions where artifact_id = $1 and number = 2`, artifactID).Scan(&markdown)
	if markdown != "after\n" || version.Named { t.Fatalf("version 2 = %q named=%v", markdown, version.Named) }
}

func TestSettleSkipsVersionWhenTreeLeavesTheSchema(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 20 * time.Millisecond
	seedServiceText(t, service, artifactID, "before")
	if err := service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		transact(func(txn *crdt.Transaction) { doc.GetXmlFragment(fragmentName).InsertElement(txn, 0, crdt.NewYXmlElement("callout")) })
	}); err != nil { t.Fatal(err) }
	time.Sleep(200 * time.Millisecond)
	var versions int
	_ = service.store.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions)
	if versions != 1 { t.Fatalf("settle wrote %d versions for a document outside the schema", versions) }
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocSchema) { t.Fatalf("Text err = %v, want ErrDocSchema", err) }
}
```

Existing tests to re-point (no semantic change): `TestApplyOpsRejectsAmbiguousTargetWithoutChangingDocument` (`*pmdoc.ErrTargetAmbiguous`), `TestApplyOpsResolvesAgainstDocumentInsideApply` (concurrent edit via `editLiveTree(... replaceRun("base", "base browser"))`; `"!"` after `end` is a new paragraph → expect `"base browser\n\n!\n"`), `TestReplaceText*` (expect canonical `"after\n"`), `TestApplyReplaceResolvesAgainstDocumentInsideApply` (still offset-based in this task), every `waitForDocumentText` expectation gains the canonical trailing `\n`, `TestReresolveAnchorsClosesRowsBeforeUpdating` and `TestTransactionalApplyReresolvesAnchoredComment` (offsets over the canonical text `"before target\n"` → `[7,13)`, unchanged). In `api/*_test.go`: `GET .../text` expectations gain the trailing newline (`"Postgres\n"`), `TestEditArtifact*` version assertions likewise; add `TestUploadRejectsMarkdownOutsideProofSchema` (`POST /artifacts` JSON content `"<details>x</details>"` → `400 INVALID_MARKDOWN`) and `TestCreateIssueRejectsSpecOutsideProofSchema`.

- [ ] **Step 2: Run** — `go test ./internal/dispatch/docs/ -run 'TestSeedText|TestApplyOps|TestSettle' -v`. Expected: compile failures (`fragmentName`, `ErrInvalidOp.Reason`), then failures on the old `content` text.

- [ ] **Step 3: Implement.**
  - `tree.go` as in Interfaces.
  - `mutation.go`: `SeedText` → `tree := parseInput(md)`; fresh `crdt.New()`; `Transact(Update(txn, frag, tree))`; `AppendUpdateTx`; return `renderTree(tree)`. `ReplaceText` → inside `applyLive`: `current := treeOf(doc)`, `target := parseInput(md)`; unchanged when `renderTree(current) == renderTree(target)` (return `false`, `ErrNoChanges` handling as today); else `recordActor`, `transact(Update(txn, frag, target))`; return canonical. `Text` → resident doc or decoded persisted update → `treeOf` → `renderTree`. `ApplyOps` → `tree := treeOf(doc)`; `next, err := applyOperations(tree, ops)`; `transact(Update(txn, frag, next))`. `ApplyReplace` (offset anchors until Task 5) → `tree := treeOf(doc)`; `md, pm, _ := pmdoc.Render(tree)`; `resolved := text.Reresolve(md, anchor)`; `r := pmdoc.Range{From: pm.ToPM(resolved.From), To: pm.ToPM(resolved.To)}`; `Splice(tree, r, inlineAware(with))` → `Update`. `applyLive` computes `markdown` via `treeOf`+`renderTree` after `mutate` (an `ErrDocSchema` here fails the mutation) and passes the tree to `writeVersionTx` (new parameter, used by Task 5's refresh). `captureLiveTextAndAuthors` returns the tree and its rendering. `latestVersion` comparison unchanged.
  - `service.go` `settleRoom`: `markdown, err := renderTree(treeOf(doc))`; on `ErrDocSchema` → `slog.Error("dispatch: settle document outside Proof schema", "room", room, "error", err)` and **return without `retrySettle`** (a schema break needs a human; retrying would spin).
  - `edits.go` rewritten per Interfaces; the `text` import goes.
  - `api/server.go`: `writeHandlerError` maps `*pmdoc.ErrTargetAmbiguous` → 409 `TARGET_AMBIGUOUS` + `candidates`, `pmdoc.ErrTargetNotFound` → 404 `TARGET_NOT_FOUND`, `docs.ErrInvalidMarkdown` → 400 `INVALID_MARKDOWN` (message `err.Error()`), `docs.ErrDocSchema` → 500 `DOC_SCHEMA` (message `err.Error()`, logged). `anchors.go` still uses `text.Resolve` in this task; `anchorResolveError` maps both `text.ErrTargetNotFound` and `pmdoc.ErrTargetNotFound`.
  - `api/artifacts.go` `storeArtifact`: for `kind == "doc"` call `SeedText`/`ReplaceText` **before** the `artifact_versions` insert and store the returned canonical string as `markdown`; `replaceRefs` with the canonical string; `ErrInvalidMarkdown` propagates to `writeHandlerError`. `api/issues.go` `createIssue`: same reordering for `spec`.

- [ ] **Step 4: Run** — `go vet ./internal/dispatch/... && go test ./internal/dispatch/docs/ ./internal/dispatch/api/ -count=1`. Expected: PASS. Then `cd packages/dispatch && bun run e2e` against `run-server.sh` on this tree: PASS (the rendered view shows canonical markdown; agent edits land via SSE refetch).

- [ ] **Step 5: Commit** — `jj describe -m "feat(dispatch): documents are Proof trees — seed, replace, read, edit, and settle through pmdoc" && jj new`

---

### Task 4: docs — mark operations and the `marks` projection

Additive: new methods on `Service` (and `API`), tested at the docs level on tree rooms. No API handler uses them yet.

**Files:**
- Create: `packages/envoy/internal/dispatch/docs/marks.go`, `marks_test.go`
- Modify: `docs/api.go`, `docs/service.go` (`Deps.MarkWait`, default 1 s)

**Interfaces (produces):**
```go
type MarkKind string
const (
	MarkAsk        MarkKind = "dispatchAsk"
	MarkComment    MarkKind = "proofComment"
	MarkSuggestion MarkKind = "proofSuggestion"
)
type MarkSpec struct { Kind MarkKind; ID string; By model.Actor }
func ActorRef(actor model.Actor) string           // "user:alice", "session:0123…"
func (m MarkSpec) pmMark() pmdoc.Mark             // Attrs{"id","by"}; MarkSuggestion adds "kind":"replace"
type MarkReply struct { By string `json:"by"`; Text string `json:"text"`; At string `json:"at"` }
type MarkRecord struct {
	Kind      string      `json:"kind"`               // "comment" | "replace"
	By        string      `json:"by"`
	CreatedAt string      `json:"createdAt"`          // RFC3339
	Text      string      `json:"text"`
	Resolved  bool        `json:"resolved"`
	Replies   []MarkReply `json:"replies"`            // encoded as [] when empty, never null
	Content   string      `json:"content,omitempty"`  // suggestion replace_with
	Status    string      `json:"status,omitempty"`   // pending | accepted | rejected
}
var ErrAnchorMissing  = errors.New("anchor mark is not in the document")  // api → 409 ANCHOR_MISSING
var ErrAnchorOrphaned = errors.New("anchor mark no longer exists")        // api → 409 ANCHOR_ORPHANED

// API additions (all honour docs.WithTx like ApplyOps):
MarkQuote(ctx, artifactID string, mark MarkSpec, quote string, occurrence *int) (string, error) // FindQuote → MarkRange; returns the text actually covered
VerifyMark(ctx, artifactID string, kind MarkKind, id string) (string, error)                    // FindMark now, or after the next Yjs update within MarkWait; ErrAnchorMissing
AcceptSuggestion(ctx, artifactID, id, replaceWith string, actor model.Actor) error               // decision 6; ErrAnchorOrphaned
RejectSuggestion(ctx, artifactID, id string, actor model.Actor) error                            // decision 6; ErrAnchorOrphaned
ProjectMark(ctx, artifactID, markID string, record MarkRecord) error                             // marks.Set(id, record as map[string]any)
```

Test helpers (in `marks_test.go`): `browserMark(t, service, artifactID, markType, id, quote)` = `browserMarkWithAttrs(..., pmdoc.Attrs{"id": id, "by": "user:bob"})`; `browserMarkWithAttrs` runs `srv.Apply`, `FindQuote` on the read tree, then `pmdoc.MarkRange` inside `transact` — it reports failures with `t.Errorf` so it is safe from a goroutine.

- [ ] **Step 1: Failing tests** (`marks_test.go`):

```go
func TestMarkQuoteWritesMarkAndReturnsCoveredText(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	spec := MarkSpec{Kind: MarkAsk, ID: "ask-1", By: model.Actor{Kind: "session", ID: "s1"}}
	quote, err := service.MarkQuote(context.Background(), artifactID, spec, "brown", nil)
	if err != nil || quote != "brown" { t.Fatalf("MarkQuote = %q, %v", quote, err) }
	tree := liveTree(t, service, artifactID)
	r, got, ok := pmdoc.FindMark(tree, "dispatchAsk", "ask-1")
	if !ok || got != "brown" || r != (pmdoc.Range{From: 11, To: 16}) { t.Fatalf("FindMark = %v %q %v", r, got, ok) } // paragraph opens at 0, "The quick " is 10 units
	attrs, _ := pmdoc.MarkAttrs(tree, "dispatchAsk", "ask-1")
	if attrs["by"] != "session:s1" { t.Fatalf("by = %v", attrs["by"]) }
	if text, _ := service.Text(context.Background(), artifactID); text != "The quick brown fox\n" { t.Fatalf("marks must not render: %q", text) }
	var ambiguous *pmdoc.ErrTargetAmbiguous
	if _, err := service.MarkQuote(context.Background(), artifactID, spec, "o", nil); !errors.As(err, &ambiguous) { t.Fatalf("ambiguous quote err = %v", err) }
	if _, err := service.MarkQuote(context.Background(), artifactID, spec, "purple", nil); !errors.Is(err, pmdoc.ErrTargetNotFound) { t.Fatalf("missing quote err = %v", err) }
}

func TestMarkQuoteJoinsTheCallerTransaction(t *testing.T) {
	// WithTx: the Yjs update is appended inside tx; rollback + eviction leaves no mark after reload.
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	tx, _ := service.store.Pool.Begin(context.Background())
	if _, err := service.MarkQuote(WithTx(context.Background(), tx), artifactID, MarkSpec{Kind: MarkComment, ID: "c1", By: model.Actor{Kind: "user", ID: "alice"}}, "quick", nil); err != nil { t.Fatal(err) }
	_ = tx.Rollback(context.Background())
	if err := service.Evict(context.Background(), artifactID); err != nil { t.Fatal(err) }
	if _, _, ok := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "c1"); ok { t.Fatal("rolled-back mark survived reload") }
}

func TestVerifyMarkWaitsForTheBrowserUpdate(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour, MarkWait: 300 * time.Millisecond})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "The quick brown fox")
	go func() {
		time.Sleep(100 * time.Millisecond)
		browserMark(t, service, artifactID, "proofComment", "b1", "brown")
	}()
	quote, err := service.VerifyMark(context.Background(), artifactID, MarkComment, "b1")
	if err != nil || quote != "brown" { t.Fatalf("VerifyMark = %q, %v", quote, err) }
	started := time.Now()
	if _, err := service.VerifyMark(context.Background(), artifactID, MarkComment, "never"); !errors.Is(err, ErrAnchorMissing) { t.Fatalf("missing mark err = %v", err) }
	if waited := time.Since(started); waited < 250*time.Millisecond || waited > time.Second { t.Fatalf("waited %v, want ≈MarkWait", waited) }
}

func TestAcceptSuggestionReplacesMarkedTextAndRemovesTheMark(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "The quick brown fox")
	spec := MarkSpec{Kind: MarkSuggestion, ID: "s1", By: model.Actor{Kind: "session", ID: "s1"}}
	if _, err := service.MarkQuote(context.Background(), artifactID, spec, "brown", nil); err != nil { t.Fatal(err) }
	if err := service.AcceptSuggestion(context.Background(), artifactID, "s1", "*red*", model.Actor{Kind: "user", ID: "alice"}); err != nil { t.Fatal(err) }
	waitForDocumentText(t, service, artifactID, "The quick *red* fox\n")
	if _, _, ok := pmdoc.FindMark(liveTree(t, service, artifactID), "proofSuggestion", "s1"); ok { t.Fatal("mark survived accept") }
	if err := service.AcceptSuggestion(context.Background(), artifactID, "s1", "x", model.Actor{Kind: "user", ID: "alice"}); !errors.Is(err, ErrAnchorOrphaned) { t.Fatalf("second accept err = %v", err) }
}

func TestRejectSuggestionIsKindAware(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "keep this and drop that")
	if _, err := service.MarkQuote(context.Background(), artifactID, MarkSpec{Kind: MarkSuggestion, ID: "rep", By: model.Actor{Kind: "session", ID: "s1"}}, "this", nil); err != nil { t.Fatal(err) }
	browserMarkWithAttrs(t, service, artifactID, "proofSuggestion", "that", pmdoc.Attrs{"id": "ins", "by": "user:bob", "kind": "insert"})
	if err := service.RejectSuggestion(context.Background(), artifactID, "rep", model.Actor{Kind: "user", ID: "alice"}); err != nil { t.Fatal(err) }
	if err := service.RejectSuggestion(context.Background(), artifactID, "ins", model.Actor{Kind: "user", ID: "alice"}); err != nil { t.Fatal(err) }
	waitForDocumentText(t, service, artifactID, "keep this and drop \n") // replace-kind text kept, insert-kind text removed (the renderer writes the run verbatim)
	if len(pmdoc.ListMarks(liveTree(t, service, artifactID))) != 0 { t.Fatal("marks survived reject") }
}

func TestProjectMarkWritesProofStoredMark(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "The quick brown fox")
	record := MarkRecord{Kind: "comment", By: "user:alice", CreatedAt: "2026-09-10T00:00:00Z", Text: "why?", Replies: []MarkReply{{By: "session:s1", Text: "because", At: "2026-09-10T00:01:00Z"}}}
	if err := service.ProjectMark(context.Background(), artifactID, "c1", record); err != nil { t.Fatal(err) }
	var got map[string]any
	_ = service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		value, _ := doc.GetMap(marksMapName).Get("c1")
		got, _ = value.(map[string]any)
	})
	if got["kind"] != "comment" || got["text"] != "why?" || got["resolved"] != false || len(got["replies"].([]any)) != 1 { t.Fatalf("projection = %#v", got) }
}
```

- [ ] **Step 2: Run** — `go test ./internal/dispatch/docs/ -run 'TestMarkQuote|TestVerifyMark|TestAcceptSuggestion|TestRejectSuggestion|TestProjectMark' -v`. Expected: compile failure.

- [ ] **Step 3: Implement `marks.go`.**
  - `MarkQuote`: `applyLive(ctx, id, func(doc, transact) bool { tree := treeOf(doc); r, err := pmdoc.FindQuote(tree, quote, occurrence, nil) (pmdoc errors propagate unwrapped so the api maps them); transact(func(txn){ err = pmdoc.MarkRange(txn, frag, r, mark.pmMark()) }); after transact: _, covered, _ = pmdoc.FindMark(treeOf(doc), string(mark.Kind), mark.ID); return true })`. Marks are invisible to `Render`, so the `writeVersionTx(nil)` that follows is a no-op on text; no `recordActor` (a mark is not authorship).
  - `VerifyMark`: warm the room (`srv.Apply` no-op, ignore `ErrNoChanges`); subscribe `doc.OnUpdate` on `srv.GetDoc(room)` into a buffered channel (unsubscribe on return); loop: `tree := liveTree(ctx, id)` (read under `srv.Apply`); `FindMark` → return quote; else `select` on the channel, `time.After(remaining)` → `ErrAnchorMissing`, `ctx.Done()` → `ctx.Err()`.
  - `AcceptSuggestion`/`RejectSuggestion`: `applyLive` → `tree := treeOf(doc)`; `r, _, ok := FindMark(tree, "proofSuggestion", id)`; `!ok → ErrAnchorOrphaned` (return false); `attrs, _ := MarkAttrs(...)`; `kind := attrs["kind"]` (default `"replace"`); per decision 6 either `next := Splice(tree, r, inlineAware(replaceWith|""))` + `Update`, or `pmdoc.Unmark(txn, frag, "proofSuggestion", id)`; `recordActor(actor)` when the text changes.
  - `ProjectMark`: `applyLive` → `transact(func(txn){ doc.GetMap(marksMapName).Set(txn, markID, toPlain(record)) })` where `toPlain` round-trips through `encoding/json` into `map[string]any` (ygo stores a `map[string]any` as `ContentAny`, which Yjs decodes as a plain object; `Replies` must encode as `[]`, so marshal `[]MarkReply{}` not `nil`).
  - `Deps.MarkWait` (default `time.Second`) stored on `Service`.

- [ ] **Step 4: Run** the docs package: `go test ./internal/dispatch/docs/ -count=1`. Expected: PASS.

- [ ] **Step 5: Commit** — `jj describe -m "feat(dispatch): mark operations on the live tree — quote marks, browser-mark verification, suggestion accept/reject, marks projection" && jj new`

---

### Task 5: anchors are marks — model, API, refresh, sweep, re-anchoring

**Files:**
- Modify: `model/model.go`, `api/anchors.go`, `api/asks.go`, `api/comments.go`, `api/messages.go`, `api/server.go`, `api/anchors_test.go`, `api/interactions_test.go`, `docs/api.go` (remove `ApplyReplace`), `docs/mutation.go` (`ReplaceText` re-anchoring, `reresolveAnchors` → `refreshAnchors`), `docs/marks.go` (`refreshAnchors`, `sweepUnrecordedMarks`), `docs/service.go` (`Deps.UnrecordedMarkTTL`, `roomState.unrecorded`, settle sweep + re-arm), `docs/mutation_test.go`, `docs/marks_test.go`
- Delete: `text/anchor.go`, `text/anchor_test.go`, `text/utf16.go` (`Len16`'s three cap-check callers switch to a private `len16` in `api/server.go`)

**Interfaces:**
```go
// model
type Anchor struct {
	ArtifactID string `json:"artifact_id"`
	MarkID     string `json:"mark_id"`
	Version    int    `json:"version"`
	Quote      string `json:"quote"`
	Orphaned   bool   `json:"orphaned"`
}
type AnchorInput struct {
	Artifact   string  `json:"artifact"`
	Quote      *string `json:"quote,omitempty"`
	Occurrence *int    `json:"occurrence,omitempty"`
	MarkID     *string `json:"mark_id,omitempty"`
}
// api
func (s *server) resolveAnchor(ctx, tx, issueKey string, input *model.AnchorInput, kind docs.MarkKind, rowID string, actor model.Actor) (*model.Anchor, string, *model.Version, error)
func len16(value string) int // beside capExceeded
// docs
func (s *Service) refreshAnchors(ctx, tx pgx.Tx, artifactID string, tree *pmdoc.Node) error // open asks → dispatchAsk, unresolved comments → proofSuggestion|proofComment; FindMark → quote / orphaned
func (s *Service) sweepUnrecordedMarks(room string, tree *pmdoc.Node)                      // called from settleRoom before the "unchanged" early return
```
Behaviour:
- `resolveAnchor` validation: `Quote`/`Occurrence` xor `MarkID` (`400 INVALID_ANCHOR` otherwise); `Quote == ""` → `INVALID_ANCHOR`. Quote path: `Docs.MarkQuote(docs.WithTx(ctx, tx), artifact.ID, docs.MarkSpec{Kind: kind, ID: rowID, By: actor}, quote, occurrence)`; mark path: `Docs.VerifyMark(ctx, artifact.ID, kind, *input.MarkID)`; then `SnapshotVersion` as today. `anchorResolveError` is deleted (404/409 come from `writeHandlerError`).
- `createAsk`/`createComment`: allocate `rowID` with `select gen_random_uuid()::text` inside the tx before `resolveAnchor`; insert with the explicit id; adopt `editArtifact`'s `evictOnFailure` pattern around the live mark write; `createComment` rejects `anchor` + (`reply_to` | `ask_id`) with `400 INVALID_COMMENT` (decision 1); after insert, an anchored comment calls `ProjectMark(docs.WithTx(ctx, tx), artifactID, rowID, record)` (`Kind` `"replace"` with `Content`/`Status:"pending"` for suggestions, `"comment"` otherwise); a reply whose thread root is an anchored comment re-projects the root with `Replies` rebuilt from `loadReplyChain(ctx, tx, "reply_to", root.ID)` (find the root by following `reply_to` upward; replies under an `ask_id` project nothing).
- `commentAction`: `resolve` → if anchored, `ProjectMark` with `Resolved: true`; `accept` → `Docs.AcceptSuggestion(docs.WithTx, artifactID, comment.Anchor.MarkID, replace_with, actor)` then the named version and `ProjectMark(Status: "accepted")`; `reject` → `Docs.RejectSuggestion(...)` and `ProjectMark(Status: "rejected")`; `docs.ErrAnchorOrphaned` → the row's anchor is updated to `orphaned: true`, the tx commits, then the response is `409 ANCHOR_ORPHANED`.
- `writeHandlerError`: `docs.ErrAnchorMissing` → 409 `ANCHOR_MISSING`; `docs.ErrAnchorOrphaned` → 409 `ANCHOR_ORPHANED`.
- `refreshAnchors` replaces `reresolveAnchors` inside `writeVersionTx` (which received the tree in Task 3). Keep its "close rows before `Exec`" shape.
- `ReplaceText` re-anchoring per decision 10: query open anchored rows before `srv.Apply`; inside the transaction after `Update`, `FindQuote(target, row.Quote, nil, &oldRange.From)` → `MarkRange` for each row whose mark was found in the old tree.
- Sweep per decision 5: `settleRoom` computes `unrecorded := ListMarks(tree) − recordedMarkIDs(room)` (types `dispatchAsk|proofComment|proofSuggestion` only); `state.unrecorded` keeps first-seen; expired ones are removed via `srv.Apply` + `pmdoc.Unmark` after the settle transaction commits (or after the early return when the text is unchanged); the earliest unexpired one re-arms the settle timer for `ttl - age`.

- [ ] **Step 1: Failing tests.**

```go
// api/anchors_test.go (rewrite)
func TestQuoteAnchorWritesServerMark(t *testing.T) {
	// POST /asks {anchor:{artifact:"spec", quote:"brown"}} → 201; body.anchor == {artifact_id, mark_id == ask.id, quote "brown", version 1, orphaned false};
	// GET /artifacts/{id}/text has no "<span"; the live tree carries dispatchAsk{id: ask.id, by: "user:alice"} (read via the docs.Service handed to newInteractionHandler).
}
func TestMarkAnchorVerifiesBrowserMark(t *testing.T) {
	// browserMark(...) writes proofComment{id:"m-1", by:"user:alice"} over "quick"; POST /comments {anchor:{artifact:"spec", mark_id:"m-1"}, body:"why"} → 201 with anchor.quote "quick", mark_id "m-1".
}
func TestMarkAnchorMissingAfterWaitIs409(t *testing.T) {
	// docs.Deps{MarkWait: 50ms}; POST /comments {anchor:{artifact:"spec", mark_id:"ghost"}} → 409 ANCHOR_MISSING; no comments row.
}
func TestAnchorInputRequiresQuoteXorMarkID(t *testing.T) {
	// {artifact} alone, {quote, mark_id} together, {quote:""} → 400 INVALID_ANCHOR; {artifact, from:0, to:3} → 400 (unknown field, decodeJSON).
}
func TestAnchorQuoteNotFoundIs404(t *testing.T) { /* quote "purple" → 404 TARGET_NOT_FOUND */ }
func TestReplyMayNotCarryAnAnchor(t *testing.T) { /* reply_to + anchor → 400 INVALID_COMMENT; ask_id + anchor → 400 */ }

// api/interactions_test.go (adjust): every `Anchor.From/To` assertion becomes `Anchor.MarkID != ""` + `Anchor.Quote`;
// the missing-anchor test expects 404 (was 422);
// accept asserts the named version markdown, that FindMark no longer finds the mark, and the marks projection has status "accepted";
// reject asserts the text is unchanged and status "rejected"; resolve asserts projection resolved: true;
// the `firstReplaceGate`/`postApplyFailureDocs` doubles wrap AcceptSuggestion instead of ApplyReplace (lock-order and evict-on-failure tests keep their names).
// Add TestAcceptOrphanedSuggestionIs409: mark's text deleted via editLiveTree(deleteRun("brown")) → POST accept → 409 ANCHOR_ORPHANED and the row's anchor.orphaned == true.

// docs/marks_test.go
func TestVersionWriteRefreshesAnchorsByMark(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = 20 * time.Millisecond
	seedServiceText(t, service, artifactID, "The quick brown fox")
	askID := insertAnchoredAsk(t, service, artifactID, "brown") // helper: MarkQuote with MarkAsk + insert asks row {state open, anchor{artifact_id, mark_id: askID, quote, version 1}}
	editLiveTree(t, service, artifactID, replaceRun("brown", "browner"))      // edit inside the marked run (the run keeps its mark)
	waitForDocumentVersion(t, service.store, artifactID, 2)
	if a := loadAskAnchor(t, service, askID); a.Quote != "browner" || a.Orphaned { t.Fatalf("anchor = %#v", a) }
	editLiveTree(t, service, artifactID, deleteRun("browner"))               // delete the marked run
	waitForDocumentVersion(t, service.store, artifactID, 3)
	if a := loadAskAnchor(t, service, askID); !a.Orphaned || a.Version != 1 { t.Fatalf("anchor = %#v, want orphaned against version 1", a) }
}

func TestSettleSweepsUnrecordedMarksAfterTTL(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: 20 * time.Millisecond, UnrecordedMarkTTL: 200 * time.Millisecond})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	seedServiceText(t, service, artifactID, "The quick brown fox")
	browserMark(t, service, artifactID, "proofComment", "dangling", "quick")
	browserMarkWithAttrs(t, service, artifactID, "proofAuthored", "fox", pmdoc.Attrs{"id": "auth", "by": "user:bob"}) // Proof-native, never swept
	time.Sleep(100 * time.Millisecond)
	if _, _, ok := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "dangling"); !ok { t.Fatal("swept before TTL") }
	waitFor(t, time.Second, "dangling mark removed", func() bool { _, _, ok := pmdoc.FindMark(liveTree(t, service, artifactID), "proofComment", "dangling"); return !ok }) // add the polling helper from outbox/publisher_test.go
	if _, _, ok := pmdoc.FindMark(liveTree(t, service, artifactID), "proofAuthored", "auth"); !ok { t.Fatal("proofAuthored must survive the sweep") }
}

func TestReplaceTextReanchorsOpenRows(t *testing.T) {
	// seed "The quick brown fox"; ask anchored on "brown", comment anchored on "fox";
	// ReplaceText("A quick brown dog and a slow brown cat") → FindMark finds the ask on the FIRST "brown" (nearest old position); the comment on "fox" is orphaned.
}
```

Re-point `TestReresolveAnchorsClosesRowsBeforeUpdating` to a mark anchor (it still proves rows are closed before the update) and `TestTransactionalApplyReresolvesAnchoredComment` to assert `quote == "target"` and `!orphaned` after the transactional insert (no offsets).

- [ ] **Step 2: Run** — `go test ./internal/dispatch/api/ -run 'TestQuoteAnchor|TestMarkAnchor|TestAnchorInput|TestReplyMayNot' -v` and the docs tests. Expected: compile failures on `Anchor.From`, then 4xx mismatches.

- [ ] **Step 3: Implement** per Interfaces/Behaviour. Delete `text/anchor.go`, `anchor_test.go`, `utf16.go`; `api/anchors.go` keeps only `resolveAnchor` + `lockAnchorArtifact`. `docs.API` drops `ApplyReplace`.

- [ ] **Step 4: Run** — `go vet ./internal/dispatch/... && go test ./internal/dispatch/... -count=1` (all dispatch packages against Postgres). Expected: PASS. Then `cd packages/dispatch && bun run e2e`: PASS (the SPA from Task 2 sends quote-only inputs and never reads `from`/`to`).

- [ ] **Step 5: Commit** — `jj describe -m "feat(dispatch): anchors are marks in the tree — quote and browser-mark inputs, mark-based refresh and orphaning, kind-aware accept/reject, unrecorded-mark sweep" && jj new`

---

### Task 6: one-shot legacy migration and the `check-documents` preflight

**Files:**
- Create: `packages/envoy/internal/dispatch/docs/legacy.go`, `legacy_test.go`
- Modify: `docs/websocket.go` (`onLoadDocument` legacy guard), `cmd/dispatch/main.go` (boot call + subcommand), `cmd/dispatch/main_test.go`

**Interfaces:**
```go
const legacyMigrationVersion = 8 // schema_migrations row owned by this Go migration (reserved; re-check at push time)
type LegacyReport struct {
	ArtifactID, IssueKey, Name string
	State      string // "legacy" | "tree"
	ParseError error  // nil when the legacy markdown parses
	Anchors    int    // anchored rows on the artifact (any state)
	Resolvable int    // rows whose quote FindQuote resolves in the parsed tree
}
func InspectLegacyDocuments(ctx context.Context, database *store.Store) ([]LegacyReport, error) // read-only
func MigrateLegacyDocuments(ctx context.Context, database *store.Store) error                   // idempotent, one tx per artifact, records version 8 when every artifact is converted
var ErrLegacyDocument = errors.New("document holds legacy Y.Text content; run the boot migration")
```
Per-artifact conversion (one transaction; `select pg_advisory_xact_lock(8150001)` — the same lock `store.Migrate` uses — is taken only for the marker check/insert):
1. Merge `doc_updates` for the artifact into a `crdt.Doc`. `prosemirror` fragment non-empty → `State: "tree"`, skip. Otherwise `content := doc.GetText("content").ToString()`.
2. `tree, err := pmdoc.Parse(content)` (**not** `StripAnchorMarks`: legacy text has no marks); `err` → `fmt.Errorf("migrate document %s (%s/%s): %w", id, issueKey, name, err)` → boot fails; nothing for that artifact is written.
3. Fresh `crdt.New()`: one transaction: `pmdoc.Update(txn, frag, tree)`; for every `asks`/`comments` row with `anchor->>'artifact_id' = id` and no `mark_id` key: decode legacy `{version, quote, from, to, orphaned}`; `markID := row id`; if `!orphaned`: `near := pm.ToPM(from)` (from `Render(tree)`; the legacy offsets index the legacy text, so this is a hint, and `FindQuote` picks the nearest exact match), `r, err := FindQuote(tree, quote, nil, &near)`; `err == nil` → `MarkRange(txn, frag, r, MarkSpec{kind by table/suggestion, ID: markID, By: author}.pmMark())` else `orphaned = true`; write `{artifact_id, mark_id, version, quote, orphaned}`; root comments with an anchor get a `marks.Set(markID, record)` with replies from the reply chain.
4. `delete from doc_updates/doc_checkpoints/doc_snapshots where artifact_id = $1`; `NewPgVersioned(database).AppendUpdateTx(ctx, tx, id, crdt.EncodeStateAsUpdateV1(fresh, nil))`; commit.
5. After the loop: `insert into schema_migrations (version) values (8) on conflict do nothing` under the advisory lock. On any later boot the marker short-circuits everything.
`onLoadDocument`: after `issueOpen`, `if doc.GetText("content").Len() > 0 && doc.GetXmlFragment(fragmentName).Len() == 0 { return ErrLegacyDocument }` (room load fails → API `503 DOC_SERVICE_UNAVAILABLE` via `failRoom`, never an empty document).
`main.go`: `os.Args[1] == "check-documents"` → open the store (`DATABASE_URL` only), `InspectLegacyDocuments`, print one line per artifact (`<id> <issue>/<name> <state> parse=<ok|error: …> anchors=<n> resolvable=<m>`), exit 1 when any `ParseError != nil`. Boot path: `store.Migrate` → `docs.MigrateLegacyDocuments` → `seedRepoProjects` → …

- [ ] **Step 1: Failing tests** (`legacy_test.go`):

```go
func seedLegacyDocument(t *testing.T, database *store.Store, artifactID, markdown string) {
	t.Helper() // the pre-cutover recipe: a Y.Text("content") update
	doc := crdt.New()
	content := doc.GetText("content")
	doc.Transact(func(txn *crdt.Transaction) { content.Insert(txn, 0, markdown, nil) })
	tx, _ := database.Pool.Begin(context.Background())
	if _, err := NewPgVersioned(database).AppendUpdateTx(context.Background(), tx, artifactID, crdt.EncodeStateAsUpdateV1(doc, nil)); err != nil { t.Fatal(err) }
	if err := tx.Commit(context.Background()); err != nil { t.Fatal(err) }
}

func TestMigrateLegacyDocumentsConvertsContentAndAnchors(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "")
	seedLegacyDocument(t, database, artifactID, "## Database\nUse SQLite, brown fox, brown dog")
	// legacy offsets index the legacy text: "## Database\n" is 12 units; the second "brown" starts at 12+23 = 35
	askID := insertLegacyRow(t, database, "asks", artifactID, `{"artifact_id":"%s","version":1,"quote":"brown","from":35,"to":40,"orphaned":false}`)
	commentID := insertLegacyRow(t, database, "comments", artifactID, `{"artifact_id":"%s","version":1,"quote":"gone","from":0,"to":4,"orphaned":false}`)
	if err := MigrateLegacyDocuments(context.Background(), database); err != nil { t.Fatal(err) }
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	if text, err := service.Text(context.Background(), artifactID); err != nil || text != "## Database\n\nUse SQLite, brown fox, brown dog\n" { t.Fatalf("Text = %q %v", text, err) }
	tree := liveTree(t, service, artifactID)
	// heading("Database") occupies [0,10); the paragraph opens at 10; "Use SQLite, brown fox, " is 23 units → second brown at [34,39)
	if r, quote, ok := pmdoc.FindMark(tree, "dispatchAsk", askID); !ok || quote != "brown" || r != (pmdoc.Range{From: 34, To: 39}) { t.Fatalf("ask mark = %v %q %v", r, quote, ok) }
	ask := loadAskAnchor(t, service, askID)
	if ask.MarkID != askID || ask.Orphaned || ask.Version != 1 { t.Fatalf("ask anchor = %#v", ask) }
	comment := loadCommentAnchor(t, service, commentID)
	if comment.MarkID != commentID || !comment.Orphaned { t.Fatalf("comment anchor = %#v, want orphaned", comment) }
	var raw []byte
	_ = database.Pool.QueryRow(context.Background(), `select anchor from asks where id = $1`, askID).Scan(&raw)
	if strings.Contains(string(raw), `"from"`) { t.Fatalf("legacy offsets survived: %s", raw) }
	var updates int
	_ = database.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&updates)
	if updates != 1 { t.Fatalf("doc_updates = %d, want the sole new update", updates) }
	var applied bool
	_ = database.Pool.QueryRow(context.Background(), `select exists(select 1 from schema_migrations where version = $1)`, legacyMigrationVersion).Scan(&applied)
	if !applied { t.Fatal("marker row missing") }
	if err := MigrateLegacyDocuments(context.Background(), database); err != nil { t.Fatalf("second run: %v", err) } // idempotent
}

func TestMigrateLegacyDocumentsFailsLoudlyOnUnparseableMarkdown(t *testing.T) {
	database := openTestStore(t)
	good := createDocument(t, database, "")
	seedLegacyDocument(t, database, good, "fine")
	bad := createDocument(t, database, "")
	seedLegacyDocument(t, database, bad, "<details>\nraw block html\n</details>\n")
	err := MigrateLegacyDocuments(context.Background(), database)
	if err == nil || !strings.Contains(err.Error(), bad) || !errors.Is(err, pmdoc.ErrSchema) { t.Fatalf("err = %v, want failure naming %s", err, bad) }
	var applied bool
	_ = database.Pool.QueryRow(context.Background(), `select exists(select 1 from schema_migrations where version = $1)`, legacyMigrationVersion).Scan(&applied)
	if applied { t.Fatal("marker must not be recorded after a failure") }
	var badUpdates int
	_ = database.Pool.QueryRow(context.Background(), `select count(update) from doc_updates where artifact_id = $1`, bad).Scan(&badUpdates)
	if badUpdates != 1 { t.Fatal("the failing artifact must be untouched") }
	reports, err := InspectLegacyDocuments(context.Background(), database)
	if err != nil { t.Fatal(err) }
	// `good` was converted before the failure (one tx per artifact) and reports "tree"; `bad` reports "legacy" with a ParseError mentioning block HTML.
}

func TestUnmigratedLegacyRoomRefusesToLoad(t *testing.T) {
	database := openTestStore(t)
	artifactID := createDocument(t, database, "")
	seedLegacyDocument(t, database, artifactID, "legacy")
	service := New(Deps{Store: database, Events: events.NewBroker(), Settle: time.Hour})
	t.Cleanup(func() { _ = service.Shutdown(context.Background()) })
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrServiceUnavailable) { t.Fatalf("Text err = %v", err) }
}

// cmd/dispatch/main_test.go
func TestCheckDocumentsReportsLegacyParseFailures(t *testing.T) {
	// openSeedTestStore; seed one legacy doc with block HTML (same recipe as seedLegacyDocument); run checkDocuments(ctx, url, &buf) → exit code 1 and the output line contains the artifact id and "block HTML".
}
```

- [ ] **Step 2: Run** — `go test ./internal/dispatch/docs/ -run 'TestMigrateLegacy|TestUnmigrated' -v` and `go test ./cmd/dispatch/ -run TestCheckDocuments -v`. Expected: compile failure.

- [ ] **Step 3: Implement** `legacy.go`, the `onLoadDocument` guard, `main.go` boot call and subcommand (`func checkDocuments(ctx context.Context, databaseURL string, out io.Writer) int`).

- [ ] **Step 4: Run** the tests, then the real surface: `DATABASE_URL=<dev url> go run ./cmd/dispatch check-documents` against the dev database (prints one line per artifact, exits 0), and a full `go run ./cmd/dispatch` boot whose log shows the migration recorded. Paste both into the PR.

- [ ] **Step 5: Commit** — `jj describe -m "feat(dispatch): one-shot boot migration of Y.Text documents and anchors to the Proof tree, with a check-documents preflight" && jj new`

---

### Task 7: contract type change — `Anchor`/`AnchorInput` carry `mark_id`

Type-level only: the SPA (Task 2) no longer reads `from`/`to`; this task makes the TypeScript types match the server (Task 5).

**Files:**
- Modify: `packages/contracts/src/dispatch-api.ts`, `packages/envoy-client/src/__tests__/delivery.test.ts`, `dispatch-execute.test.ts`, `packages/dispatch/web/src/**` fixtures that build `Anchor` literals (`bun run typecheck` lists them), `packages/dispatch/e2e/margin.e2e.ts` (assert `typeof comment.anchor.mark_id === "string"`)

- [ ] **Step 1:** Change the types:

```ts
export interface Anchor {
  readonly artifact_id: string;
  readonly mark_id: string;
  readonly version: number;
  readonly quote: string;
  readonly orphaned: boolean;
}

export type AnchorInput =
  | { readonly artifact: string; readonly quote: string; readonly occurrence?: number; readonly mark_id?: never }
  | { readonly artifact: string; readonly mark_id: string; readonly quote?: never; readonly occurrence?: never };
```

`TargetCandidate` stays `{from, to, context}`; add a doc comment: positions are ProseMirror positions in the live document.

- [ ] **Step 2: Run** — `cd packages/contracts && bun run lint && bun run typecheck && bun run test && bun run build`; `cd packages/envoy-client && bun run typecheck && bun run test`; `cd packages/pi-envoy && bun run typecheck`; `cd packages/claude-envoy-bridge && bun run typecheck`; `cd packages/dispatch && bun run typecheck && bun run test`. Fix every fixture the compiler names (`from`/`to` → `mark_id: "m-1"`). Expected: green.

- [ ] **Step 3: Run** `cd packages/dispatch && bun run e2e` (server = this tree). Expected: PASS, including the new `mark_id` assertion.

- [ ] **Step 4: Commit** — `jj describe -m "feat(contracts): Anchor carries mark_id; AnchorInput is quote or mark_id" && jj new`

---

### Task 8: documentation, leftovers, full verification, PR

**Files:**
- Modify: `packages/envoy/cmd/dispatch/AGENTS.md` (the paragraph "Documents use a Yjs `Y.Text` named `content`…" → the two shared types, canonical rendering, marks-as-anchors, boot migration + `check-documents`), `packages/envoy/cmd/dispatch/README.md` (routes: anchor input shapes on asks/comments; new error codes `INVALID_MARKDOWN`, `ANCHOR_MISSING`, `ANCHOR_ORPHANED`, `DOC_SCHEMA`; a `check-documents` section under Running locally), `packages/envoy/internal/dispatch/docs/api.go` package comment (document model, who writes what, the mark lifecycle, ≤15 lines), `skills/dispatch/SKILL.md` (append to the `dispatch_comment` paragraph: "A reply (`reply_to`/`reply_to_ask`) takes no `quote`; it belongs to its parent's anchor." and add to the error sentence: "`INVALID_MARKDOWN` rejects markdown Proof cannot represent, such as block-level HTML"), `packages/dispatch/AGENTS.md` wherever it describes the editor (grep `CodeMirror|edit mode`).
- Verify no leftovers: `grep -rn 'GetText("content")\|text\.Resolve\|text\.Reresolve\|Slice16\|Len16\|anchor\.from\|anchor\.to\b\|enterEditMode\|y-codemirror\|ApplyReplace' packages/ skills/` returns only `docs/legacy.go` and `docs/legacy_test.go` (which read the legacy text on purpose).

- [ ] **Step 1:** Write the docs; read each edited paragraph back against the behaviour it describes (Tasks 3–6), not against the diff.
- [ ] **Step 2: Full verification** (the PR gate; run all, paste outputs in the PR):
  - `cd packages/envoy && gofmt -l . && go vet ./... && DISPATCH_TEST_DATABASE_URL=<url> sh -c 'go list ./... | grep -v /integration | xargs go test -count=1'`
  - `cd packages/envoy/internal/dispatch/pmdoc/gen && bun run check`
  - `bun install --frozen-lockfile` (repo root)
  - `cd packages/contracts && bun run lint && bun run typecheck && bun run test && bun run build`; same for `packages/envoy-client`, `packages/pi-envoy`, `packages/claude-envoy-bridge` (typecheck + test)
  - `cd packages/dispatch && bun run lint && bun run typecheck && bun run test && bun run e2e`
  - Smoke the real surface: `cd packages/envoy && DATABASE_URL=<dev url> DISPATCH_AGENT_TOKEN=local-agent-token DISPATCH_IDENTITY='header:X-Dispatch-User' DISPATCH_ALLOWED_LOGINS=sjawhar DISPATCH_INSECURE_COOKIE=1 DISPATCH_DEFAULT_PROJECT=LOCAL go run ./cmd/dispatch` (boot log shows the migration ran or was already recorded), then with `curl -H 'X-Dispatch-User: sjawhar'`: create an issue with a `spec`, `POST .../asks` with `{anchor:{artifact:"spec", quote:"…"}}`, `POST .../edits` replacing text around the quote, `GET /api/v1/asks/{id}` shows the moved `quote`; `POST .../comments` with a suggestion and `POST /comments/{id}/accept` changes `GET .../text` and names a version; open `http://127.0.0.1:8766/issues/<KEY>/spec` in a browser (the Playwright screenshot from `bun run e2e` is acceptable evidence) — the document renders and the margin lists the items.
- [ ] **Step 3: Commit** — `jj describe -m "docs(dispatch): tree document model, mark anchors, migration preflight" && jj new`; bookmark `dispatch-doc/tree-server`; `jj git push`; open the PR with the title `feat(dispatch): server on tree documents — marks, anchors, suggestions, migration` and the body below; register it with the merge queue. Thermo pair applies (production Go).

---

## Acceptance pass (surfaces, drivers, tooling)

| Deliverable | Surface a human/operator touches | What drives it today | Gap closed in this plan |
|---|---|---|---|
| Documents as trees; `GET .../text`, versions, settle | `curl` against the running server; the SPA document tab; OMP `dispatch_doc_read`/`dispatch_doc_edit` | Go tests against Postgres (`docs/`, `api/`); Playwright e2e boots the real server (`e2e/run-server.sh`) | e2e adapted (Task 2) so the browser surface stays covered in CI |
| Quote anchors (agent path) and browser-mark anchors | `dispatch_ask`/`dispatch_comment`/`dispatch_suggest` from a real OMP session; `POST` with `mark_id` from PR 4's editor | Go API tests; e2e creates anchors through the API and from a rendered selection | the browser-mark path is exercised by Go tests that write the mark like a browser (`browserMark` helper); the real browser writer is PR 4 |
| Anchor refresh / orphaning | the margin's quote and "Text changed. View original text" after another user edits | Go tests edit the live tree like a browser and assert rows; e2e checks the orphan link renders the version highlight | the browser-driven edit is PR 4's scenario (acceptance 6) |
| Accept/reject/resolve | Margin buttons (human), `POST /comments/{id}/accept` | Go tests; e2e accepts an API-created suggestion in the browser and sees the text change | — |
| Migration | booting the deployed server against the production database (PR 5) | Go test with a seeded legacy room; **`dispatch check-documents`** (new, Task 6) run against a copy of production before PR 5 deploys | the preflight is the reusable fixture; the real run is PR 5's, behind the preflight |
| Contract change | TypeScript consumers compile; envoy-client tests | `bun run typecheck/test` per package in CI | — |

Cheapest real substitute for the restricted path (production boot): `check-documents` against a `pg_dump`/restore of production, then a local `go run ./cmd/dispatch` boot on that copy — both are plain commands an operator types; no privileged shortcut stands in for the real login (the SPA e2e uses header identity exactly as `run-server.sh` configures it).

## PR body checklist (server side of the acceptance bar)

- [ ] **5** — `dispatch_doc_edit` lands live: `TestApplyOpsEditsLiveDocumentAndSettlesVersion`, `TestApplyOpsInsertsAtHeadingsQuotesAndEdges` (one `Server.Apply`, one update, one version); an agent quote anchor becomes a highlight: `TestQuoteAnchorWritesServerMark`; e2e `doc.e2e.ts` shows the agent edit in two browsers.
- [ ] **6** — anchors follow edits and orphan on deletion, keeping the version they were made against: `TestVersionWriteRefreshesAnchorsByMark`, `TestReplaceTextReanchorsOpenRows`; e2e orphan link → `?version=N&comment=<id>` highlight.
- [ ] **7** — accept changes the text, removes the mark, writes a named version; reject leaves the text: `TestAcceptSuggestionReplacesMarkedTextAndRemovesTheMark`, `TestRejectSuggestionIsKindAware`, api accept/reject tests; e2e accepts in the browser.
- [ ] **9** — `dispatch_doc_read` is clean markdown plus open items: `TestMarkQuoteWritesMarkAndReturnsCoveredText` (no span in `Text()`), envoy-client tests unchanged.
- [ ] **10** — reload mid-edit keeps content and anchors: `TestMarkQuoteJoinsTheCallerTransaction` (evict + reload), `TestSettleRendersTreeAndWritesVersion`.
- [ ] **11** — legacy documents open with content and every anchor converted: `TestMigrateLegacyDocumentsConvertsContentAndAnchors`, `TestMigrateLegacyDocumentsFailsLoudlyOnUnparseableMarkdown`, `check-documents` output against the dev database.
- [ ] Error table honoured: `TestAnchorQuoteNotFoundIs404`, ambiguous → 409 with candidates, `TestMarkAnchorMissingAfterWaitIs409`, `TestAcceptOrphanedSuggestionIs409`, `TestSettleSkipsVersionWhenTreeLeavesTheSchema`, `TestApplyOpsRejectsMarkdownOutsideProofSchema`, `TestUnmigratedLegacyRoomRefusesToLoad`.
- [ ] Deleted: `text/anchor.go`, `text/utf16.go`, string splicing in `docs/edits.go`, offset branches in `api/anchors.go`, SPA `anchors.ts` and the CodeMirror edit mode.
- [ ] Interim SPA state and PR 4 hand-off stated (Scope boundary).

## Self-review

- **Spec coverage:** document model (T3/T4: `prosemirror` + `marks`, no `markdown` type) ✓; server operations table — `dispatch_doc_edit` ops (T3), seed/replace (T3 + T5 re-anchor), agent anchor (T4/T5), browser anchor with ≤1 s wait → `ANCHOR_MISSING` (T4/T5), accept/reject/resolve (T4/T5), settle/versions (T3), refresh on version write (T5), `dispatch_doc_read` (T3) ✓; Anchors shape and "originator writes the mark" (T5) ✓; deletions (T2/T3/T5) ✓; events unchanged except `Anchor` (T5/T7), outbox untouched ✓; migration (T6) ✓; error table (T3/T5/T6) ✓; Testing > Server list (T3–T6) ✓; delivery item 3 = this PR ✓.
- **Placeholders:** none — every task names files, tests, commands, and expected output.
- **Type consistency:** `MarkSpec{Kind, ID, By}` (T4) used in T5/T6; `MarkRecord` fields identical in T4/T5/T6; `refreshAnchors(ctx, tx, artifactID, tree)` (T5) matches `writeVersionTx`'s tree parameter introduced in T3; `pmdoc.FindHeading/Size/ListMarks/MarkAttrs` (T1) used in T3/T4/T5; `SeedText/ReplaceText` return `(string, error)` from T3 onward and every caller (api, tests, legacy) uses the value; `liveTree/editLiveTree/replaceRun/deleteRun/browserMark` helpers defined in T3/T4 and reused in T5/T6.
- **Judgment calls the executor must not "fix":** decisions 1–13 above; canonical trailing newline in rendered text; asks not projected; `proofAuthored` never swept; `TARGET_NOT_FOUND` is 404 for anchors; `after: "end"` is a block boundary.

## Hardening ledger

(empty — filled by the implementer/reviewer as hardening items are found and closed)
