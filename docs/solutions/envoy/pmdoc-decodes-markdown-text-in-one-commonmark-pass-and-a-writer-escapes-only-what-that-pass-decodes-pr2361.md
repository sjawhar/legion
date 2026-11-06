---
title: "pmdoc decodes markdown text in one CommonMark pass, and a writer escapes only what that pass decodes"
category: envoy
tags:
  - pmdoc
  - goldmark
  - character-references
  - backslash-escapes
  - parser-parity
date: 2026-10-27
status: active
module: packages/envoy/internal/dispatch/pmdoc
---

# pmdoc decodes markdown text in one CommonMark pass, and a writer escapes only what that pass decodes

- **Decode with `decodeMarkdownText` (`references.go`).** Do not write a second decoder, and do
  not chain library calls: `util.UnescapePunctuations` then `html.UnescapeString`, or goldmark's
  `ResolveEntityNames`/`ResolveNumericReferences`, follow HTML's rules. A reader that needs
  CommonMark's reading of a value (a link address, an attribute) calls the existing pass.
- **Take an enumerated table from the engine's own installed copy.** Go's `html` table lacks
  `&nLt;` and `&nGt;`; goldmark's `LookUpHTML5EntityByName` lacks `&Abreve;`. Resolve the table
  the browser engine loads through its own dependency chain (`gen/entity-names.ts`), and test every
  entry against `engineReadings`. Add a fixed-seed generated corpus beside the hand-picked rows.
- **Change the writer with the reader.** A value the old reader decoded wrongly may be written in
  a form the new reader decodes, so `RefuseMisreadDocument` refuses a write it used to accept. For
  each place the reader changes (titles, image addresses, labels), render the parsed tree and read
  it back in Go and in the engine (`rendersBackInBothParsers`).
- **Escape where the reader decodes, not where text looks like syntax.** Put `\` before an `&` only
  where `characterReferenceAt` decodes a reference; `&foo;` and `AT&T` keep their bytes, which
  `TestReferenceSpellingsThatReadBackRenderUnchanged` pins. A destination writer doubles a
  backslash before ASCII punctuation and also at the value's end, where it would escape the closing
  `)` or `>`.
- **Trim goldmark text before decoding it.** An `ast.Text` value never holds its own line ending,
  so trim break spaces from the raw bytes and decode after; a trim after decoding eats a decoded
  `&#10;` or `&#32;`. The engine's import (`remarkSoftBreakAsSpace`) reads every line feed in a text
  node as a space, a decoded one included, and keeps an image alt's line feed, since the alt is an
  attribute.

## Evidence

The old decoder was `html.UnescapeString(string(util.UnescapePunctuations(value)))` at
`parse.go:1016-1018` (base `4bbb2a9c`): `Plain \&amp; end.` read as `Plain & end.` where a comment
showed `Plain &amp; end.`; `AT&T &copy 2024` read `©`, `&#00000065;` read `A`, `&#128;` read `€`
(the engine gives U+FFFD) and `&notit;` read `¬it;`. In a seeded run of 4,000 paragraphs, 1,762
disagreed with the engine, 1,689 of them holding an `&` (plan, `specDepartures[0]`).

The replacement is `decodeMarkdownText` (`references.go:16`), `characterReferenceAt` (`:47`, the
`&nLt;`/`&nGt;` case at `:67`), `characterReferenceLength` (`:84`, the one shape the writers and
`escapes()` share) and `numericReferenceCharacter` (`:124`, micromark's U+FFFD rule). The plan's
hand-picked rows passed; `TestEveryNameTheEngineKnowsReadsAsTheEngineReadsIt`
(`render_readback_test.go:594`, 2,125 names) and
`TestGeneratedEscapesAndReferencesReadAsTheEngineReadsThem` (`:562`, 2,000 paragraphs) found
`&nLt;`/`&nGt;` and the decoded line feed. The old trim, `strings.TrimSuffix(value, "\n")` after
decoding, ate a decoded `&#10;` before a break; the trim now runs on the raw segment
(`parse.go:901`), and `parseTextValue` (`parse.go:1029`) reads a decoded line feed as a space
outside an image (`TestALineFeedAReferenceNamesReadsAsTheEngineReadsIt`, `:535`).

With the decoder fixed alone, `[a](u "\&amp;")` and `![a](u\&amp;v)` were refused by the read-back
check (plan, `specDepartures[1]`); `titleSuffix` (`render.go:823`) and `escapeImageSource`
(`render.go:796`) fixed them. The tester then found `![a](u\\)` refused, because the image writer
left a final backslash undoubled; `TestAnImageAddressEndingInABackslashRendersAndReadsBack`
(`render_readback_test.go:701`) pins the fix in 738dbe90.
