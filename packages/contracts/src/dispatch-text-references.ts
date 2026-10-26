import { MAX_BLOCK_ID_BYTES } from "./dispatch-block-ref";

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
 * parenthesis after it are dropped. External URLs cite nothing. A block reference is its
 * document's reference, `#b-` and the block id as `encodeBlockId` writes it (readers take any
 * percent-encoding; an id past MAX_BLOCK_ID_BYTES names nothing), optionally pinned to version N
 * of the document by a trailing `@v<N>`; each `refs` entry for one is written that canonical way.
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
  // An id of MAX_BLOCK_ID_BYTES UTF-8 bytes (an `é` is two) is a block; one byte more names
  // nothing, and a dashboard page whose fragment holds it is the document.
  {
    body: `dispatch://CORE-1/spec#b-${"%C3%A9".repeat(MAX_BLOCK_ID_BYTES / 2)}`,
    refs: [`dispatch://CORE-1/spec#b-${"%C3%A9".repeat(MAX_BLOCK_ID_BYTES / 2)}`],
  },
  { body: `dispatch://CORE-1/spec#b-${"%C3%A9".repeat(MAX_BLOCK_ID_BYTES / 2)}x`, refs: [] },
  {
    body: `https://dispatch.test/issues/CORE-1/spec#b-${"%C3%A9".repeat(MAX_BLOCK_ID_BYTES / 2)}x`,
    refs: ["dispatch://CORE-1/spec"],
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
