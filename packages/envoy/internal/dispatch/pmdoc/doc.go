// Package pmdoc reads, renders, parses, diffs, and marks a Proof/Milkdown
// ProseMirror document stored as a Yjs XmlFragment, in pure Go.
//
// A Proof document lives in Postgres as a Yjs update stream. This package
// owns the tree shape that stream decodes to — a plain Node model restricted
// to Proof's Milkdown schema (commonmark + GFM + frontmatter + code_block,
// plus Proof's marks: proofSuggestion, proofComment, proofFlagged,
// proofApproved, proofAuthored, dispatchAsk) — and its document operations:
//
//   - Read decodes a *crdt.YXmlFragment (via github.com/reearth/ygo) into a *Node.
//   - Render turns a *Node into canonical markdown.
//   - Parse turns markdown (via goldmark) back into a *Node.
//   - Update writes the minimal Yjs change that turns a fragment's current tree
//     into a target *Node, porting y-prosemirror's updateYFragment diff so a
//     one-word edit produces a one-word delta instead of a full-document replace.
//   - FindQuote, FindMark, MarkRange, Unmark, and Splice locate and edit ranges
//     within a tree by quoted text or existing marks.
//
// The three record marks a suggestion, comment or ask anchors to
// (proofSuggestion, proofComment, dispatchAsk) do not exclude their own type,
// so two of one type may cover one character: each is stored under its own
// Y.Text attribute key (markAttributeKey) and found by its type and id.
//
// pmdoc has no Postgres, HTTP, or ygo-server dependency; it imports only
// crdt, goldmark, and the standard library. The Dispatch server composes it.
//
// # Equivalence with the browser
//
// Proof's real editor is a Milkdown/ProseMirror instance running in the
// browser, collaborating over the same Yjs document this package reads and
// writes. pmdoc is only correct if a tree it reads, writes, or edits decodes
// identically to what that browser editor would produce or accept — "identical"
// meaning the same ProseMirror JSON, not byte-identical Yjs updates (Yjs does
// not fix map-attribute key order, and CRDT structure sharing means two
// updates can encode the same tree differently).
//
// The package is tested against that property directly, not against a
// reimplementation of the schema: every fixture's expected shape is produced
// by the headless Milkdown engine (@legion/proof-editor/headless),
// and every Update result is decoded back with the browser's own
// y-prosemirror, not with this package's Read. See read_test.go,
// update_test.go, and gen/decode.ts.
//
// # Regenerating fixtures
//
// testdata/corpus/ is the one markdown table of this repository: this package's
// tests read it through testdata/fixtures.json, Dispatch's web unit tests and
// one browser row per markdown parity family read the same file through
// packages/dispatch/web/src/__tests__/pmdoc-fixtures.ts, and none of the three
// keeps a copy of a shape but for two, each of which the table cannot hold:
//
//   - writer_spelling_test.go copies corpus markdown into its rows, because what
//     it asserts is the bytes Go stores for a shape, which the table does not
//     carry: fixtures.json's rendered_by_milkdown is the browser engine's
//     re-serialization of its own parse (a backslash escape) where Go stores a
//     character reference, and render_test.go's
//     TestRenderMatchesMilkdownForFixtures compares the two only through plain(),
//     which strips both.
//   - packages/dispatch/e2e/unknown-directive.e2e.ts keeps bodies of its own,
//     because each carries a container the block schema does not know, and the
//     generator's writing parse refuses such a document outright, as the document
//     service does. A Conversation body is drawn by the read-only engine instead,
//     so no corpus file can hold them.
//
// testdata/fixtures.json and testdata/splices.json are
// generated, not hand-written. To add a document fixture, drop a markdown file
// in testdata/corpus/ and regenerate:
//
//	cd gen && bun run gen     # rewrite generated fixtures
//	cd gen && bun run check   # verify they are up to date (CI)
//
// gen is a workspace member, so the repo-root bun install covers its
// dependencies, @legion/proof-editor among them.
//
// gen/gen.ts parses corpus files with the headless engine, encodes their
// ProseMirror docs as Yjs updates with y-prosemirror, and records markdown,
// pm_json, base64 updates and the reading a reader sees of the document
// (@legion/proof-editor/reading, the paragraphs and tables of the DOM the
// schema draws) side by side. It also runs ProseMirror's
// replaceRange for its documented splice cases and writes their engine JSON.
// Go tests decode Yjs updates with Read, compare browser fixture JSON, and
// compare Splice with those replaceRange results; Update output is decoded
// by the browser's y-prosemirror through gen/decode.ts.
package pmdoc
