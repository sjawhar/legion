/**
 * Markdown bodies with the `dispatch://` references each one cites, in order of first appearance,
 * as read against the server origin `https://dispatch.test`, or the row's `origin` where it names
 * one. Two readers are tested against it - the dashboard's `composerReferences` (its composer's
 * reference pills and its unfurl cards; the rendered-text linkifier shares its scanner) and the Go
 * reader `text.Extract`, which writes the reference graph, through its JSON copy in
 * `packages/envoy/internal/dispatch/text/testdata` - so neither where a reference ends nor what it
 * names can change on one side only. A reference ends at whitespace (Unicode space separators
 * included, a vertical tab and U+FEFF not), an angle or square bracket, a quote, a backtick or a
 * pipe; the sentence punctuation, emphasis and strikethrough delimiters and unbalanced closing
 * parenthesis after it are dropped. External URLs cite nothing, and neither does a reference
 * whose target id, as the server stores the edge to it, is past MAX_REFERENCE_TARGET_BYTES. A
 * block reference is its document's reference, `#b-` and the block id as `encodeBlockId` writes
 * it (readers take any percent-encoding; an id past MAX_BLOCK_ID_BYTES names nothing), optionally
 * pinned to version N of the document by a trailing `@v<N>`; each `refs` entry for one is written
 * that canonical way.
 */
export const DISPATCH_TEXT_REFERENCES: readonly {
  body: string;
  origin?: string;
  refs: readonly string[];
}[] = [
  { body: "Tied to dispatch://CORE-1 here.", refs: ["dispatch://CORE-1"] },
  { body: "Tied to **dispatch://CORE-1** here.", refs: ["dispatch://CORE-1"] },
  { body: "Tied to `dispatch://CORE-1` here.", refs: ["dispatch://CORE-1"] },
  {
    body: "_dispatch://CORE-1/spec_, ~~dispatch://CORE-2~~ and __dispatch://CORE-3/ask/22222222-2222-4222-8222-222222222222__.",
    refs: [
      "dispatch://CORE-1/spec",
      "dispatch://CORE-2",
      "dispatch://CORE-3/ask/22222222-2222-4222-8222-222222222222",
    ],
  },
  {
    body: "`dispatch://CORE-5`/`dispatch://CORE-6`",
    refs: ["dispatch://CORE-5", "dispatch://CORE-6"],
  },
  // How a document stores the autolink `<dispatch://CORE-7/spec>`.
  {
    body: "See [dispatch://CORE-7/spec](dispatch://CORE-7/spec).",
    refs: ["dispatch://CORE-7/spec"],
  },
  { body: "See [**dispatch://CORE-8**](dispatch://CORE-8).", refs: ["dispatch://CORE-8"] },
  { body: "(**https://dispatch.test/issues/CORE-4**)", refs: ["dispatch://CORE-4"] },
  {
    body: "See [https://dispatch.test/issues/CORE-9](https://dispatch.test/issues/CORE-9).",
    refs: ["dispatch://CORE-9"],
  },
  {
    body: "See [**https://dispatch.test/issues/CORE-9/spec**](https://dispatch.test/issues/CORE-9/spec).",
    refs: ["dispatch://CORE-9/spec"],
  },
  { body: "(see dispatch://CORE-10).", refs: ["dispatch://CORE-10"] },
  {
    body: "Quoted \"dispatch://CORE-11\", 'dispatch://CORE-12' and <dispatch://CORE-13>.",
    refs: ["dispatch://CORE-11", "dispatch://CORE-12", "dispatch://CORE-13"],
  },
  {
    body: "A no-break space after dispatch://CORE-14\u00a0and an ideographic one after dispatch://CORE-15\u3000end",
    refs: ["dispatch://CORE-14", "dispatch://CORE-15"],
  },
  // JavaScript's `\s` matches both and Go's neither, which is why the readers spell out their
  // whitespace; a reference holding either names nothing.
  {
    body: "Neither a vertical tab nor a BOM ends one: dispatch://CORE-18\u000b dispatch://CORE-19\ufeff.",
    refs: [],
  },
  // A server at an IPv6 literal: the bracket that opens its host belongs to the URL, the one that
  // opens a link's text does not.
  {
    body: "See http://[::1]:8766/issues/CORE-20 and [http://[::1]:8766/issues/CORE-21/spec](http://[::1]:8766/issues/CORE-21/spec).",
    origin: "http://[::1]:8766",
    refs: ["dispatch://CORE-20", "dispatch://CORE-21/spec"],
  },
  {
    body: "~~dispatch://CORE/artifact/handbook-md~~ and **dispatch://CORE-17/artifact/design-md@v3**",
    refs: ["dispatch://CORE/artifact/handbook-md", "dispatch://CORE-17/artifact/design-md@v3"],
  },
  {
    body: "`https://dispatch.test/issues/CORE-1/spec?comment=44444444-4444-4444-8444-444444444444`;",
    refs: ["dispatch://CORE-1/comment/44444444-4444-4444-8444-444444444444"],
  },
  // A query is read as the browser reads it, pairs parted at `&` alone, so an id holding a `;`
  // keeps it (and is written escaped), a version holding one is no version, and an `ask` holding
  // one still meets the `comment` beside it, which names nothing.
  {
    body: "Pairs part at & alone: https://dispatch.test/issues/CORE-1/spec?comment=c1;c2, https://dispatch.test/issues/CORE-1/artifacts/notes-md?v=2;3 and https://dispatch.test/issues/CORE-2/spec?ask=a1;x&comment=c1.",
    refs: ["dispatch://CORE-1/comment/c1%3Bc2"],
  },
  {
    body: "A % that two hex digits do not follow stays a %: https://dispatch.test/issues/CORE-3/spec?comment=c%2%34 here",
    refs: ["dispatch://CORE-3/comment/c%24"],
  },
  // A table cell written without padding ends its reference at the cell's `|`, as the dashboard's
  // markdown parser reads the cell.
  {
    body: "|ref|state|\n|---|---|\n|dispatch://CORE-1/artifact/plan-md|done|\n|dispatch://CORE-2|open|\n|https://dispatch.test/issues/CORE-3/spec|open|",
    refs: ["dispatch://CORE-1/artifact/plan-md", "dispatch://CORE-2", "dispatch://CORE-3/spec"],
  },
  {
    body: "A control character or a BOM after a slug or an id: dispatch://CORE-1/artifact/notes-md\u000b dispatch://CORE-1/comment/c1\u0001 dispatch://CORE/artifact/handbook-md\u0085 https://dispatch.test/issues/CORE-1/asks/a1\u007f dispatch://CORE-1/ask/a1\ufeff",
    refs: [],
  },
  // An id that decodes to a NUL names nothing: the decode must not make the control character
  // that a reference written with one is refused for, and Postgres refuses a NUL in a text bind.
  {
    body: "An id decoding to a NUL: dispatch://CORE-1/comment/%00, https://dispatch.test/issues/CORE-1/comments/%00, https://dispatch.test/issues/CORE-1/spec?comment=%2500, https://dispatch.test/issues/CORE-1/spec?comment=%00 and dispatch://CORE/artifact/handbook-md/comment/%00",
    refs: [],
  },
  {
    body: "A slug is all of its segment and a version has no leading zero: dispatch://CORE-1/artifact/notes-md!x, dispatch://CORE/artifact/Notes-md, https://dispatch.test/issues/CORE-1/artifacts/notes_md and dispatch://CORE-1/artifact/notes-md@v03",
    refs: [],
  },
  {
    body: "*https://example.com/~ops/snake_case_path?glob=*.md* and https://dispatch.test/issues/CORE-16_",
    refs: ["dispatch://CORE-16"],
  },
  {
    body: "Broken: **dispatch://CORE-1x**, dispatch://CORE-1_suffix, `dispatch://core-1`, ~~dispatch://CORE~~ and _dispatch://CORE-0_.",
    refs: [],
  },
  // An agent's conversation owns the pictures sent in it: its session id, as written, is the
  // owner, and an id holding a `/` names nothing.
  {
    body: "Pasted: dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551/artifact/shot-png here.",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551/artifact/shot-png"],
  },
  {
    body: "Pasted: dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551/artifact/shot-png@v3 here.",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551/artifact/shot-png@v3"],
  },
  {
    body: "See https://dispatch.test/agents/01a1058e-f14f-7684-87eb-3dc885955551/artifacts/shot-png?v=3 here.",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551/artifact/shot-png@v3"],
  },
  { body: "Not an owner: dispatch://agent/a/b/artifact/shot-png here.", refs: [] },
  // Any other character is part of the id, punctuation included.
  {
    body: "Pasted: dispatch://agent/pi.worker_2~a:b@host/artifact/shot-png here.",
    refs: ["dispatch://agent/pi.worker_2~a:b@host/artifact/shot-png"],
  },
  // A page whose session id decodes to a character that ends a reference names nothing: no
  // reference could carry that id, and Dispatch refuses it as an owner.
  { body: "See https://dispatch.test/agents/a%7Cb/artifacts/shot-png here.", refs: [] },
  // A picture's address is read through the image syntax that shows it.
  {
    body: "![shot.png](dispatch://LEGION-1/artifact/shot-png@v1)",
    refs: ["dispatch://LEGION-1/artifact/shot-png@v1"],
  },
  // A block of a document: its id is read with any percent-encoding and written as
  // `encodeBlockId` writes it.
  {
    body: "see dispatch://CORE-1/spec#b-d%C3%A9cision%3A1.",
    refs: ["dispatch://CORE-1/spec#b-d%C3%A9cision%3A1"],
  },
  { body: "dispatch://CORE-1/spec#b-d%c3%a9", refs: ["dispatch://CORE-1/spec#b-d%C3%A9"] },
  {
    body: "dispatch://CORE-1/artifact/notes-md#b-ship-it",
    refs: ["dispatch://CORE-1/artifact/notes-md#b-ship-it"],
  },
  {
    body: "dispatch://CORE/artifact/handbook-md#b-ship-it",
    refs: ["dispatch://CORE/artifact/handbook-md#b-ship-it"],
  },
  // No block: an empty id, another fragment, a versioned document, an issue, an id that is not
  // UTF-8 or decodes to a control character.
  { body: "dispatch://CORE-1/spec#b-", refs: [] },
  { body: "dispatch://CORE-1/spec#c-x", refs: [] },
  { body: "dispatch://CORE-1/artifact/notes-md@v2#b-x", refs: [] },
  { body: "dispatch://CORE-1#b-x", refs: [] },
  { body: "dispatch://CORE-1/spec#b-%E0%A4", refs: [] },
  { body: "dispatch://CORE-1/spec#b-%0A", refs: [] },
  // An id of MAX_BLOCK_ID_BYTES (256) bytes is a block; one byte more names nothing, and a
  // dashboard page whose fragment holds it is the document. The ids are written out, not built,
  // so a bundle that does not use this table leaves it out.
  {
    body: "dispatch://CORE-1/spec#b-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    refs: [
      "dispatch://CORE-1/spec#b-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    ],
  },
  {
    body: "dispatch://CORE-1/spec#b-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaax",
    refs: [],
  },
  {
    body: "https://dispatch.test/issues/CORE-1/spec#b-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaax",
    refs: ["dispatch://CORE-1/spec"],
  },
  // A block reference whose target id, `<owner>/<slug>#<block id>` as the server stores the edge
  // to it, is MAX_REFERENCE_TARGET_BYTES (2048) bytes is a block; one byte more names nothing, and
  // a dashboard page whose fragment names that block is the document. Written out, as above.
  {
    body: "dispatch://CORE-1/artifact/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa#b-x",
    refs: [
      "dispatch://CORE-1/artifact/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa#b-x",
    ],
  },
  {
    body: "dispatch://CORE-1/artifact/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa#b-xy",
    refs: [],
  },
  {
    body: "https://dispatch.test/issues/CORE-1/artifacts/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa#b-xy",
    refs: [
      "dispatch://CORE-1/artifact/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    ],
  },
  // Every reference's target id, as the server stores the edge to it - a document's
  // `<owner>/<slug>`, a comment's id - is bounded so: at MAX_REFERENCE_TARGET_BYTES (2048) bytes
  // it names its target, one byte more names nothing, and a dashboard page past it is only a URL,
  // whatever block its fragment names. Written out, the slug's 2041 bytes counted in groups.
  {
    body: "dispatch://CORE-1/artifact/a0001-a0002-a0003-a0004-a0005-a0006-a0007-a0008-a0009-a0010-a0011-a0012-a0013-a0014-a0015-a0016-a0017-a0018-a0019-a0020-a0021-a0022-a0023-a0024-a0025-a0026-a0027-a0028-a0029-a0030-a0031-a0032-a0033-a0034-a0035-a0036-a0037-a0038-a0039-a0040-a0041-a0042-a0043-a0044-a0045-a0046-a0047-a0048-a0049-a0050-a0051-a0052-a0053-a0054-a0055-a0056-a0057-a0058-a0059-a0060-a0061-a0062-a0063-a0064-a0065-a0066-a0067-a0068-a0069-a0070-a0071-a0072-a0073-a0074-a0075-a0076-a0077-a0078-a0079-a0080-a0081-a0082-a0083-a0084-a0085-a0086-a0087-a0088-a0089-a0090-a0091-a0092-a0093-a0094-a0095-a0096-a0097-a0098-a0099-a0100-a0101-a0102-a0103-a0104-a0105-a0106-a0107-a0108-a0109-a0110-a0111-a0112-a0113-a0114-a0115-a0116-a0117-a0118-a0119-a0120-a0121-a0122-a0123-a0124-a0125-a0126-a0127-a0128-a0129-a0130-a0131-a0132-a0133-a0134-a0135-a0136-a0137-a0138-a0139-a0140-a0141-a0142-a0143-a0144-a0145-a0146-a0147-a0148-a0149-a0150-a0151-a0152-a0153-a0154-a0155-a0156-a0157-a0158-a0159-a0160-a0161-a0162-a0163-a0164-a0165-a0166-a0167-a0168-a0169-a0170-a0171-a0172-a0173-a0174-a0175-a0176-a0177-a0178-a0179-a0180-a0181-a0182-a0183-a0184-a0185-a0186-a0187-a0188-a0189-a0190-a0191-a0192-a0193-a0194-a0195-a0196-a0197-a0198-a0199-a0200-a0201-a0202-a0203-a0204-a0205-a0206-a0207-a0208-a0209-a0210-a0211-a0212-a0213-a0214-a0215-a0216-a0217-a0218-a0219-a0220-a0221-a0222-a0223-a0224-a0225-a0226-a0227-a0228-a0229-a0230-a0231-a0232-a0233-a0234-a0235-a0236-a0237-a0238-a0239-a0240-a0241-a0242-a0243-a0244-a0245-a0246-a0247-a0248-a0249-a0250-a0251-a0252-a0253-a0254-a0255-a0256-a0257-a0258-a0259-a0260-a0261-a0262-a0263-a0264-a0265-a0266-a0267-a0268-a0269-a0270-a0271-a0272-a0273-a0274-a0275-a0276-a0277-a0278-a0279-a0280-a0281-a0282-a0283-a0284-a0285-a0286-a0287-a0288-a0289-a0290-a0291-a0292-a0293-a0294-a0295-a0296-a0297-a0298-a0299-a0300-a0301-a0302-a0303-a0304-a0305-a0306-a0307-a0308-a0309-a0310-a0311-a0312-a0313-a0314-a0315-a0316-a0317-a0318-a0319-a0320-a0321-a0322-a0323-a0324-a0325-a0326-a0327-a0328-a0329-a0330-a0331-a0332-a0333-a0334-a0335-a0336-a0337-a0338-a0339-a0340-a",
    refs: [
      "dispatch://CORE-1/artifact/a0001-a0002-a0003-a0004-a0005-a0006-a0007-a0008-a0009-a0010-a0011-a0012-a0013-a0014-a0015-a0016-a0017-a0018-a0019-a0020-a0021-a0022-a0023-a0024-a0025-a0026-a0027-a0028-a0029-a0030-a0031-a0032-a0033-a0034-a0035-a0036-a0037-a0038-a0039-a0040-a0041-a0042-a0043-a0044-a0045-a0046-a0047-a0048-a0049-a0050-a0051-a0052-a0053-a0054-a0055-a0056-a0057-a0058-a0059-a0060-a0061-a0062-a0063-a0064-a0065-a0066-a0067-a0068-a0069-a0070-a0071-a0072-a0073-a0074-a0075-a0076-a0077-a0078-a0079-a0080-a0081-a0082-a0083-a0084-a0085-a0086-a0087-a0088-a0089-a0090-a0091-a0092-a0093-a0094-a0095-a0096-a0097-a0098-a0099-a0100-a0101-a0102-a0103-a0104-a0105-a0106-a0107-a0108-a0109-a0110-a0111-a0112-a0113-a0114-a0115-a0116-a0117-a0118-a0119-a0120-a0121-a0122-a0123-a0124-a0125-a0126-a0127-a0128-a0129-a0130-a0131-a0132-a0133-a0134-a0135-a0136-a0137-a0138-a0139-a0140-a0141-a0142-a0143-a0144-a0145-a0146-a0147-a0148-a0149-a0150-a0151-a0152-a0153-a0154-a0155-a0156-a0157-a0158-a0159-a0160-a0161-a0162-a0163-a0164-a0165-a0166-a0167-a0168-a0169-a0170-a0171-a0172-a0173-a0174-a0175-a0176-a0177-a0178-a0179-a0180-a0181-a0182-a0183-a0184-a0185-a0186-a0187-a0188-a0189-a0190-a0191-a0192-a0193-a0194-a0195-a0196-a0197-a0198-a0199-a0200-a0201-a0202-a0203-a0204-a0205-a0206-a0207-a0208-a0209-a0210-a0211-a0212-a0213-a0214-a0215-a0216-a0217-a0218-a0219-a0220-a0221-a0222-a0223-a0224-a0225-a0226-a0227-a0228-a0229-a0230-a0231-a0232-a0233-a0234-a0235-a0236-a0237-a0238-a0239-a0240-a0241-a0242-a0243-a0244-a0245-a0246-a0247-a0248-a0249-a0250-a0251-a0252-a0253-a0254-a0255-a0256-a0257-a0258-a0259-a0260-a0261-a0262-a0263-a0264-a0265-a0266-a0267-a0268-a0269-a0270-a0271-a0272-a0273-a0274-a0275-a0276-a0277-a0278-a0279-a0280-a0281-a0282-a0283-a0284-a0285-a0286-a0287-a0288-a0289-a0290-a0291-a0292-a0293-a0294-a0295-a0296-a0297-a0298-a0299-a0300-a0301-a0302-a0303-a0304-a0305-a0306-a0307-a0308-a0309-a0310-a0311-a0312-a0313-a0314-a0315-a0316-a0317-a0318-a0319-a0320-a0321-a0322-a0323-a0324-a0325-a0326-a0327-a0328-a0329-a0330-a0331-a0332-a0333-a0334-a0335-a0336-a0337-a0338-a0339-a0340-a",
    ],
  },
  {
    body: "dispatch://CORE-1/artifact/a0001-a0002-a0003-a0004-a0005-a0006-a0007-a0008-a0009-a0010-a0011-a0012-a0013-a0014-a0015-a0016-a0017-a0018-a0019-a0020-a0021-a0022-a0023-a0024-a0025-a0026-a0027-a0028-a0029-a0030-a0031-a0032-a0033-a0034-a0035-a0036-a0037-a0038-a0039-a0040-a0041-a0042-a0043-a0044-a0045-a0046-a0047-a0048-a0049-a0050-a0051-a0052-a0053-a0054-a0055-a0056-a0057-a0058-a0059-a0060-a0061-a0062-a0063-a0064-a0065-a0066-a0067-a0068-a0069-a0070-a0071-a0072-a0073-a0074-a0075-a0076-a0077-a0078-a0079-a0080-a0081-a0082-a0083-a0084-a0085-a0086-a0087-a0088-a0089-a0090-a0091-a0092-a0093-a0094-a0095-a0096-a0097-a0098-a0099-a0100-a0101-a0102-a0103-a0104-a0105-a0106-a0107-a0108-a0109-a0110-a0111-a0112-a0113-a0114-a0115-a0116-a0117-a0118-a0119-a0120-a0121-a0122-a0123-a0124-a0125-a0126-a0127-a0128-a0129-a0130-a0131-a0132-a0133-a0134-a0135-a0136-a0137-a0138-a0139-a0140-a0141-a0142-a0143-a0144-a0145-a0146-a0147-a0148-a0149-a0150-a0151-a0152-a0153-a0154-a0155-a0156-a0157-a0158-a0159-a0160-a0161-a0162-a0163-a0164-a0165-a0166-a0167-a0168-a0169-a0170-a0171-a0172-a0173-a0174-a0175-a0176-a0177-a0178-a0179-a0180-a0181-a0182-a0183-a0184-a0185-a0186-a0187-a0188-a0189-a0190-a0191-a0192-a0193-a0194-a0195-a0196-a0197-a0198-a0199-a0200-a0201-a0202-a0203-a0204-a0205-a0206-a0207-a0208-a0209-a0210-a0211-a0212-a0213-a0214-a0215-a0216-a0217-a0218-a0219-a0220-a0221-a0222-a0223-a0224-a0225-a0226-a0227-a0228-a0229-a0230-a0231-a0232-a0233-a0234-a0235-a0236-a0237-a0238-a0239-a0240-a0241-a0242-a0243-a0244-a0245-a0246-a0247-a0248-a0249-a0250-a0251-a0252-a0253-a0254-a0255-a0256-a0257-a0258-a0259-a0260-a0261-a0262-a0263-a0264-a0265-a0266-a0267-a0268-a0269-a0270-a0271-a0272-a0273-a0274-a0275-a0276-a0277-a0278-a0279-a0280-a0281-a0282-a0283-a0284-a0285-a0286-a0287-a0288-a0289-a0290-a0291-a0292-a0293-a0294-a0295-a0296-a0297-a0298-a0299-a0300-a0301-a0302-a0303-a0304-a0305-a0306-a0307-a0308-a0309-a0310-a0311-a0312-a0313-a0314-a0315-a0316-a0317-a0318-a0319-a0320-a0321-a0322-a0323-a0324-a0325-a0326-a0327-a0328-a0329-a0330-a0331-a0332-a0333-a0334-a0335-a0336-a0337-a0338-a0339-a0340-ab",
    refs: [],
  },
  {
    body: "dispatch://CORE-1/comment/a0001-a0002-a0003-a0004-a0005-a0006-a0007-a0008-a0009-a0010-a0011-a0012-a0013-a0014-a0015-a0016-a0017-a0018-a0019-a0020-a0021-a0022-a0023-a0024-a0025-a0026-a0027-a0028-a0029-a0030-a0031-a0032-a0033-a0034-a0035-a0036-a0037-a0038-a0039-a0040-a0041-a0042-a0043-a0044-a0045-a0046-a0047-a0048-a0049-a0050-a0051-a0052-a0053-a0054-a0055-a0056-a0057-a0058-a0059-a0060-a0061-a0062-a0063-a0064-a0065-a0066-a0067-a0068-a0069-a0070-a0071-a0072-a0073-a0074-a0075-a0076-a0077-a0078-a0079-a0080-a0081-a0082-a0083-a0084-a0085-a0086-a0087-a0088-a0089-a0090-a0091-a0092-a0093-a0094-a0095-a0096-a0097-a0098-a0099-a0100-a0101-a0102-a0103-a0104-a0105-a0106-a0107-a0108-a0109-a0110-a0111-a0112-a0113-a0114-a0115-a0116-a0117-a0118-a0119-a0120-a0121-a0122-a0123-a0124-a0125-a0126-a0127-a0128-a0129-a0130-a0131-a0132-a0133-a0134-a0135-a0136-a0137-a0138-a0139-a0140-a0141-a0142-a0143-a0144-a0145-a0146-a0147-a0148-a0149-a0150-a0151-a0152-a0153-a0154-a0155-a0156-a0157-a0158-a0159-a0160-a0161-a0162-a0163-a0164-a0165-a0166-a0167-a0168-a0169-a0170-a0171-a0172-a0173-a0174-a0175-a0176-a0177-a0178-a0179-a0180-a0181-a0182-a0183-a0184-a0185-a0186-a0187-a0188-a0189-a0190-a0191-a0192-a0193-a0194-a0195-a0196-a0197-a0198-a0199-a0200-a0201-a0202-a0203-a0204-a0205-a0206-a0207-a0208-a0209-a0210-a0211-a0212-a0213-a0214-a0215-a0216-a0217-a0218-a0219-a0220-a0221-a0222-a0223-a0224-a0225-a0226-a0227-a0228-a0229-a0230-a0231-a0232-a0233-a0234-a0235-a0236-a0237-a0238-a0239-a0240-a0241-a0242-a0243-a0244-a0245-a0246-a0247-a0248-a0249-a0250-a0251-a0252-a0253-a0254-a0255-a0256-a0257-a0258-a0259-a0260-a0261-a0262-a0263-a0264-a0265-a0266-a0267-a0268-a0269-a0270-a0271-a0272-a0273-a0274-a0275-a0276-a0277-a0278-a0279-a0280-a0281-a0282-a0283-a0284-a0285-a0286-a0287-a0288-a0289-a0290-a0291-a0292-a0293-a0294-a0295-a0296-a0297-a0298-a0299-a0300-a0301-a0302-a0303-a0304-a0305-a0306-a0307-a0308-a0309-a0310-a0311-a0312-a0313-a0314-a0315-a0316-a0317-a0318-a0319-a0320-a0321-a0322-a0323-a0324-a0325-a0326-a0327-a0328-a0329-a0330-a0331-a0332-a0333-a0334-a0335-a0336-a0337-a0338-a0339-a0340-a1234567",
    refs: [
      "dispatch://CORE-1/comment/a0001-a0002-a0003-a0004-a0005-a0006-a0007-a0008-a0009-a0010-a0011-a0012-a0013-a0014-a0015-a0016-a0017-a0018-a0019-a0020-a0021-a0022-a0023-a0024-a0025-a0026-a0027-a0028-a0029-a0030-a0031-a0032-a0033-a0034-a0035-a0036-a0037-a0038-a0039-a0040-a0041-a0042-a0043-a0044-a0045-a0046-a0047-a0048-a0049-a0050-a0051-a0052-a0053-a0054-a0055-a0056-a0057-a0058-a0059-a0060-a0061-a0062-a0063-a0064-a0065-a0066-a0067-a0068-a0069-a0070-a0071-a0072-a0073-a0074-a0075-a0076-a0077-a0078-a0079-a0080-a0081-a0082-a0083-a0084-a0085-a0086-a0087-a0088-a0089-a0090-a0091-a0092-a0093-a0094-a0095-a0096-a0097-a0098-a0099-a0100-a0101-a0102-a0103-a0104-a0105-a0106-a0107-a0108-a0109-a0110-a0111-a0112-a0113-a0114-a0115-a0116-a0117-a0118-a0119-a0120-a0121-a0122-a0123-a0124-a0125-a0126-a0127-a0128-a0129-a0130-a0131-a0132-a0133-a0134-a0135-a0136-a0137-a0138-a0139-a0140-a0141-a0142-a0143-a0144-a0145-a0146-a0147-a0148-a0149-a0150-a0151-a0152-a0153-a0154-a0155-a0156-a0157-a0158-a0159-a0160-a0161-a0162-a0163-a0164-a0165-a0166-a0167-a0168-a0169-a0170-a0171-a0172-a0173-a0174-a0175-a0176-a0177-a0178-a0179-a0180-a0181-a0182-a0183-a0184-a0185-a0186-a0187-a0188-a0189-a0190-a0191-a0192-a0193-a0194-a0195-a0196-a0197-a0198-a0199-a0200-a0201-a0202-a0203-a0204-a0205-a0206-a0207-a0208-a0209-a0210-a0211-a0212-a0213-a0214-a0215-a0216-a0217-a0218-a0219-a0220-a0221-a0222-a0223-a0224-a0225-a0226-a0227-a0228-a0229-a0230-a0231-a0232-a0233-a0234-a0235-a0236-a0237-a0238-a0239-a0240-a0241-a0242-a0243-a0244-a0245-a0246-a0247-a0248-a0249-a0250-a0251-a0252-a0253-a0254-a0255-a0256-a0257-a0258-a0259-a0260-a0261-a0262-a0263-a0264-a0265-a0266-a0267-a0268-a0269-a0270-a0271-a0272-a0273-a0274-a0275-a0276-a0277-a0278-a0279-a0280-a0281-a0282-a0283-a0284-a0285-a0286-a0287-a0288-a0289-a0290-a0291-a0292-a0293-a0294-a0295-a0296-a0297-a0298-a0299-a0300-a0301-a0302-a0303-a0304-a0305-a0306-a0307-a0308-a0309-a0310-a0311-a0312-a0313-a0314-a0315-a0316-a0317-a0318-a0319-a0320-a0321-a0322-a0323-a0324-a0325-a0326-a0327-a0328-a0329-a0330-a0331-a0332-a0333-a0334-a0335-a0336-a0337-a0338-a0339-a0340-a1234567",
    ],
  },
  {
    body: "dispatch://CORE-1/comment/a0001-a0002-a0003-a0004-a0005-a0006-a0007-a0008-a0009-a0010-a0011-a0012-a0013-a0014-a0015-a0016-a0017-a0018-a0019-a0020-a0021-a0022-a0023-a0024-a0025-a0026-a0027-a0028-a0029-a0030-a0031-a0032-a0033-a0034-a0035-a0036-a0037-a0038-a0039-a0040-a0041-a0042-a0043-a0044-a0045-a0046-a0047-a0048-a0049-a0050-a0051-a0052-a0053-a0054-a0055-a0056-a0057-a0058-a0059-a0060-a0061-a0062-a0063-a0064-a0065-a0066-a0067-a0068-a0069-a0070-a0071-a0072-a0073-a0074-a0075-a0076-a0077-a0078-a0079-a0080-a0081-a0082-a0083-a0084-a0085-a0086-a0087-a0088-a0089-a0090-a0091-a0092-a0093-a0094-a0095-a0096-a0097-a0098-a0099-a0100-a0101-a0102-a0103-a0104-a0105-a0106-a0107-a0108-a0109-a0110-a0111-a0112-a0113-a0114-a0115-a0116-a0117-a0118-a0119-a0120-a0121-a0122-a0123-a0124-a0125-a0126-a0127-a0128-a0129-a0130-a0131-a0132-a0133-a0134-a0135-a0136-a0137-a0138-a0139-a0140-a0141-a0142-a0143-a0144-a0145-a0146-a0147-a0148-a0149-a0150-a0151-a0152-a0153-a0154-a0155-a0156-a0157-a0158-a0159-a0160-a0161-a0162-a0163-a0164-a0165-a0166-a0167-a0168-a0169-a0170-a0171-a0172-a0173-a0174-a0175-a0176-a0177-a0178-a0179-a0180-a0181-a0182-a0183-a0184-a0185-a0186-a0187-a0188-a0189-a0190-a0191-a0192-a0193-a0194-a0195-a0196-a0197-a0198-a0199-a0200-a0201-a0202-a0203-a0204-a0205-a0206-a0207-a0208-a0209-a0210-a0211-a0212-a0213-a0214-a0215-a0216-a0217-a0218-a0219-a0220-a0221-a0222-a0223-a0224-a0225-a0226-a0227-a0228-a0229-a0230-a0231-a0232-a0233-a0234-a0235-a0236-a0237-a0238-a0239-a0240-a0241-a0242-a0243-a0244-a0245-a0246-a0247-a0248-a0249-a0250-a0251-a0252-a0253-a0254-a0255-a0256-a0257-a0258-a0259-a0260-a0261-a0262-a0263-a0264-a0265-a0266-a0267-a0268-a0269-a0270-a0271-a0272-a0273-a0274-a0275-a0276-a0277-a0278-a0279-a0280-a0281-a0282-a0283-a0284-a0285-a0286-a0287-a0288-a0289-a0290-a0291-a0292-a0293-a0294-a0295-a0296-a0297-a0298-a0299-a0300-a0301-a0302-a0303-a0304-a0305-a0306-a0307-a0308-a0309-a0310-a0311-a0312-a0313-a0314-a0315-a0316-a0317-a0318-a0319-a0320-a0321-a0322-a0323-a0324-a0325-a0326-a0327-a0328-a0329-a0330-a0331-a0332-a0333-a0334-a0335-a0336-a0337-a0338-a0339-a0340-a12345678",
    refs: [],
  },
  {
    body: "https://dispatch.test/issues/CORE-1/artifacts/a0001-a0002-a0003-a0004-a0005-a0006-a0007-a0008-a0009-a0010-a0011-a0012-a0013-a0014-a0015-a0016-a0017-a0018-a0019-a0020-a0021-a0022-a0023-a0024-a0025-a0026-a0027-a0028-a0029-a0030-a0031-a0032-a0033-a0034-a0035-a0036-a0037-a0038-a0039-a0040-a0041-a0042-a0043-a0044-a0045-a0046-a0047-a0048-a0049-a0050-a0051-a0052-a0053-a0054-a0055-a0056-a0057-a0058-a0059-a0060-a0061-a0062-a0063-a0064-a0065-a0066-a0067-a0068-a0069-a0070-a0071-a0072-a0073-a0074-a0075-a0076-a0077-a0078-a0079-a0080-a0081-a0082-a0083-a0084-a0085-a0086-a0087-a0088-a0089-a0090-a0091-a0092-a0093-a0094-a0095-a0096-a0097-a0098-a0099-a0100-a0101-a0102-a0103-a0104-a0105-a0106-a0107-a0108-a0109-a0110-a0111-a0112-a0113-a0114-a0115-a0116-a0117-a0118-a0119-a0120-a0121-a0122-a0123-a0124-a0125-a0126-a0127-a0128-a0129-a0130-a0131-a0132-a0133-a0134-a0135-a0136-a0137-a0138-a0139-a0140-a0141-a0142-a0143-a0144-a0145-a0146-a0147-a0148-a0149-a0150-a0151-a0152-a0153-a0154-a0155-a0156-a0157-a0158-a0159-a0160-a0161-a0162-a0163-a0164-a0165-a0166-a0167-a0168-a0169-a0170-a0171-a0172-a0173-a0174-a0175-a0176-a0177-a0178-a0179-a0180-a0181-a0182-a0183-a0184-a0185-a0186-a0187-a0188-a0189-a0190-a0191-a0192-a0193-a0194-a0195-a0196-a0197-a0198-a0199-a0200-a0201-a0202-a0203-a0204-a0205-a0206-a0207-a0208-a0209-a0210-a0211-a0212-a0213-a0214-a0215-a0216-a0217-a0218-a0219-a0220-a0221-a0222-a0223-a0224-a0225-a0226-a0227-a0228-a0229-a0230-a0231-a0232-a0233-a0234-a0235-a0236-a0237-a0238-a0239-a0240-a0241-a0242-a0243-a0244-a0245-a0246-a0247-a0248-a0249-a0250-a0251-a0252-a0253-a0254-a0255-a0256-a0257-a0258-a0259-a0260-a0261-a0262-a0263-a0264-a0265-a0266-a0267-a0268-a0269-a0270-a0271-a0272-a0273-a0274-a0275-a0276-a0277-a0278-a0279-a0280-a0281-a0282-a0283-a0284-a0285-a0286-a0287-a0288-a0289-a0290-a0291-a0292-a0293-a0294-a0295-a0296-a0297-a0298-a0299-a0300-a0301-a0302-a0303-a0304-a0305-a0306-a0307-a0308-a0309-a0310-a0311-a0312-a0313-a0314-a0315-a0316-a0317-a0318-a0319-a0320-a0321-a0322-a0323-a0324-a0325-a0326-a0327-a0328-a0329-a0330-a0331-a0332-a0333-a0334-a0335-a0336-a0337-a0338-a0339-a0340-ab#b-x",
    refs: [],
  },
  // A dashboard page's `#b-` fragment names the block it lands on.
  {
    body: "https://dispatch.test/issues/CORE-1/spec#b-ship-it",
    refs: ["dispatch://CORE-1/spec#b-ship-it"],
  },
  // A `block[]` attribute's value: both readers end the second reference at the `"`.
  {
    body: ':::callout{#d1 affects="dispatch://CORE-1/spec#b-a dispatch://CORE-1/spec#b-b"}',
    refs: ["dispatch://CORE-1/spec#b-a", "dispatch://CORE-1/spec#b-b"],
  },
  // A block pinned to version N of its document: `@v` and N without a sign or a leading zero.
  {
    body: "dispatch://CORE-1/spec#b-ship-it@v2",
    refs: ["dispatch://CORE-1/spec#b-ship-it@v2"],
  },
  {
    body: "see dispatch://CORE-1/artifact/notes-md#b-d%C3%A9cision%3A1@v12.",
    refs: ["dispatch://CORE-1/artifact/notes-md#b-d%C3%A9cision%3A1@v12"],
  },
  // N up to the largest Go's `int` holds, kept digit for digit past what a JavaScript number holds
  // exactly; one more names nothing.
  {
    body: "dispatch://CORE-1/spec#b-ship-it@v9223372036854775807",
    refs: ["dispatch://CORE-1/spec#b-ship-it@v9223372036854775807"],
  },
  { body: "dispatch://CORE-1/spec#b-ship-it@v9223372036854775808", refs: [] },
  // A malformed pin, or a raw `@` that starts no pin, names nothing.
  { body: "dispatch://CORE-1/spec#b-ship-it@v", refs: [] },
  { body: "dispatch://CORE-1/spec#b-ship-it@v0", refs: [] },
  { body: "dispatch://CORE-1/spec#b-ship-it@v01", refs: [] },
  { body: "dispatch://CORE-1/spec#b-ship-it@vx", refs: [] },
  { body: "dispatch://CORE-1/spec#b-a@b", refs: [] },
  // An `@` in the id is written `%40`: the block `it@v2`, unpinned.
  { body: "dispatch://CORE-1/spec#b-it%40v2", refs: ["dispatch://CORE-1/spec#b-it%40v2"] },
  // A version page is the versioned document whatever its fragment: pins are written only as
  // `dispatch://` references.
  {
    body: "https://dispatch.test/issues/CORE-1/artifacts/notes-md?v=2#b-ship-it",
    refs: ["dispatch://CORE-1/artifact/notes-md@v2"],
  },
];
