/**
 * Markdown bodies with the `dispatch://` references each one cites, in order of first appearance,
 * as read against the server origin `https://dispatch.test`, or the row's `origin` where it names
 * one. Two readers are tested against it - the dashboard's `composerReferences` (its composer's
 * reference pills and its unfurl cards; the rendered-text linkifier shares its scanner) and the Go
 * reader `text.Extract`, which writes the reference graph, through its JSON copy in
 * `packages/envoy/internal/dispatch/text/testdata` - so neither where a reference ends nor what it
 * names can change on one side only. A reference ends at whitespace (Unicode space separators
 * included, a vertical tab and U+FEFF not), an angle or square bracket, a quote or a backtick;
 * the sentence punctuation, emphasis and strikethrough delimiters and unbalanced closing
 * parenthesis after it are dropped. External URLs cite nothing.
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
    body: "A control character or a BOM after a slug or an id: dispatch://CORE-1/artifact/notes-md\u000b dispatch://CORE-1/comment/c1\u0001 dispatch://CORE/artifact/handbook-md\u0085 https://dispatch.test/issues/CORE-1/asks/a1\u007f dispatch://CORE-1/ask/a1\ufeff",
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
];
