/**
 * Markdown bodies with the `dispatch://` references each one cites, in order of first appearance,
 * as read against the server origin `https://dispatch.test`. Two readers are tested against it -
 * the dashboard composer's `composerReferences` (its reference pills and unfurl cards; the
 * rendered-text linkifier shares its scanner) and the Go reader `text.Extract`, which writes the
 * reference graph, through its JSON copy in `packages/envoy/internal/dispatch/text/testdata` - so
 * where a reference ends cannot change on one side only. A reference ends at whitespace (Unicode
 * space separators included), an angle or square bracket, a quote or a backtick; the sentence
 * punctuation, emphasis and strikethrough delimiters and unbalanced closing parenthesis after it
 * are dropped. External URLs cite nothing.
 */
export const DISPATCH_TEXT_REFERENCES: readonly { body: string; refs: readonly string[] }[] = [
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
  {
    body: "~~dispatch://CORE/artifact/handbook-md~~ and **dispatch://CORE-17/artifact/design-md@v3**",
    refs: ["dispatch://CORE/artifact/handbook-md", "dispatch://CORE-17/artifact/design-md@v3"],
  },
  {
    body: "`https://dispatch.test/issues/CORE-1/spec?comment=44444444-4444-4444-8444-444444444444`;",
    refs: ["dispatch://CORE-1/comment/44444444-4444-4444-8444-444444444444"],
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
