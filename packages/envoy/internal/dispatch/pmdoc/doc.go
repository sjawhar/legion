// Package pmdoc reads, renders, parses, diffs, and marks a Proof/Milkdown
// ProseMirror document stored as a Yjs XmlFragment, in pure Go.
//
// A Proof document lives in Postgres as a Yjs update stream. This package
// owns the tree shape that stream decodes to — a plain Node model restricted
// to Proof's Milkdown schema (commonmark + GFM + frontmatter + code_block,
// plus Proof's marks: proofSuggestion, proofComment, proofFlagged,
// proofApproved, proofAuthored, dispatchAsk) — and the five operations Dispatch
// needs on it:
//
//   - Read decodes a *crdt.YXmlFragment (via github.com/reearth/ygo) into a *Node.
//   - Render turns a *Node into canonical markdown plus a PositionMap between
//     markdown UTF-16 offsets and ProseMirror positions.
//   - Parse turns markdown (via goldmark) back into a *Node.
//   - Update writes the minimal Yjs change that turns a fragment's current tree
//     into a target *Node, porting y-prosemirror's updateYFragment diff so a
//     one-word edit produces a one-word delta instead of a full-document replace.
//   - FindQuote, FindMark, MarkRange, Unmark, and Splice locate and edit ranges
//     within a tree by quoted text or existing marks.
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
// by the fork's headless Milkdown engine (@sjawhar/proof-editor/headless),
// and every Update result is decoded back with the browser's own
// y-prosemirror, not with this package's Read. See read_test.go,
// update_test.go, and gen/decode.ts.
//
// # Regenerating fixtures
//
// testdata/fixtures.json and testdata/splices.json are generated, not
// hand-written. To add a document fixture, drop a markdown file in
// testdata/corpus/ and regenerate:
//
//	cd gen && bun install && bun run gen     # rewrite generated fixtures
//	cd gen && bun run check                  # verify they are up to date (CI)
//
// gen/gen.ts parses corpus files with the headless engine, encodes their
// ProseMirror docs as Yjs updates with y-prosemirror, and records markdown,
// pm_json, and base64 updates side by side. It also runs ProseMirror's
// replaceRange for its documented splice cases and writes their engine JSON.
// Go tests decode Yjs updates with Read, compare browser fixture JSON, and
// compare Splice with those replaceRange results; Update output is decoded
// by the browser's y-prosemirror through gen/decode.ts.
package pmdoc
