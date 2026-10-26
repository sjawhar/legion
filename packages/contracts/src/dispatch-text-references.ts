/**
 * Markdown bodies with the references each one cites, in order of first appearance, as read
 * against the server origin `https://dispatch.test`, or the row's `origin` where it names one.
 * Three readers are tested against it - the dashboard's `composerReferences` (its composer's
 * reference pills and its unfurl cards, which read `dispatch://` and dashboard addresses only), the
 * dashboard's markdown renderers (what they link), and the Go reader `text.Mentions`, which writes
 * the reference graph, through its JSON copy in `packages/envoy/internal/dispatch/text/testdata` -
 * so neither where a reference ends nor what it names can change on one side only.
 *
 * A reference ends at whitespace (Unicode space separators included, a vertical tab and U+FEFF
 * not), an angle or square bracket, a quote, a backtick or a pipe; the sentence punctuation,
 * emphasis and strikethrough delimiters and unbalanced closing parenthesis after it are dropped.
 * External URLs cite nothing.
 *
 * A text also cites what it writes bare: an issue key (`CORE-12`) that names an existing issue, as
 * `dispatch://CORE-12`, and `#` with a question block's id of the text's document, as that
 * question's ask. Either stands alone: after the start of the text, whitespace (or a numeric
 * reference to whitespace, such as the `&#32;` stored markdown writes a space as), or one of
 * ( [ " ' * _ > ` ~ |. A key ends at the end of the text or a character that is not a letter, a
 * digit, `-`, `/`, `#` or `@`, and the sentence punctuation and delimiters `.` `,` `:` `;` `_` `*`
 * `~` end it only when no letter or digit follows them. A `#id` is the longest run of
 * `[A-Za-z0-9_-]`, or that run without its trailing `_` and `-` when no question holds the whole
 * run. Neither is read inside an address. The server reads both in code and in a link's
 * destination, where no renderer links them.
 */
export const DISPATCH_TEXT_REFERENCES: readonly {
  body: string;
  origin?: string;
  /** The issue keys that name an existing issue; a row without it has none. */
  issues?: readonly string[];
  /** The question block ids of the text's document, each to its ask's reference. */
  questions?: Readonly<Record<string, string>>;
  /** What the server indexes. */
  refs: readonly string[];
  /** What a renderer links, which is none inside code or a link; omitted, it is `refs`. */
  links?: readonly string[];
}[] = [
  { body: "Tied to dispatch://CORE-1 here.", refs: ["dispatch://CORE-1"] },
  { body: "Tied to **dispatch://CORE-1** here.", refs: ["dispatch://CORE-1"] },
  { body: "Tied to `dispatch://CORE-1` here.", refs: ["dispatch://CORE-1"], links: [] },
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
    links: [],
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
    links: [],
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
  // A bare key that names an existing issue cites it, at each character it may follow.
  { body: "CORE-1 is blocked.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: "Blocked on CORE-1 today.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: "Blocked (CORE-1) today.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: "Blocked [CORE-1] today.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: 'Blocked on "CORE-1" today.', issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: "Blocked on 'CORE-1' today.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: "Blocked on **CORE-1** today.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: "Blocked on _CORE-1_ today.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: ">CORE-1 is blocked.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: "Blocked on ~~CORE-1~~ today.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  {
    body: "|issue|state|\n|---|---|\n|CORE-1|open|",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
  },
  // Stored markdown writes the whitespace at a mark's edge as its numeric reference, decimal or hex.
  { body: "Blocked on&#32;CORE-1 today.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: "Blocked on&#x20;CORE-1 today.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: "Blocked on&#9;CORE-1 today.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: "Blocked on&#10;CORE-1 today.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: "See XCORE-1 here.", issues: ["XCORE-1"], refs: ["dispatch://XCORE-1"] },
  { body: "CORE-1's spec is done.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: "Done in CORE-1.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  { body: "Blocked on CORE-1, then shipped.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"] },
  // A link's destination is a key the server reads and no renderer links: it is inside a link.
  {
    body: "See [the blocker](CORE-1).",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  // A key that does not stand alone, or that names no issue, cites nothing.
  { body: "See legion/CORE-1 here.", issues: ["CORE-1"], refs: [] },
  { body: "See xCORE-1 here.", issues: ["CORE-1"], refs: [] },
  { body: "See core-1 here.", issues: ["CORE-1"], refs: [] },
  { body: "See CORE-1abc here.", issues: ["CORE-1"], refs: [] },
  { body: "See CORE-1-2 here.", issues: ["CORE-1"], refs: [] },
  { body: "See CORE-1/spec here.", issues: ["CORE-1"], refs: [] },
  { body: "See CORE-1.5 here.", issues: ["CORE-1"], refs: [] },
  { body: "See CORE-1_x here.", issues: ["CORE-1"], refs: [] },
  {
    body: "See CORE-1#q2 here.",
    issues: ["CORE-1"],
    questions: { q2: "dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333" },
    refs: [],
  },
  { body: "Blocked on CORE-404.", issues: ["CORE-1"], refs: [] },
  { body: "Blocked on CORE-1, which no issue holds.", refs: [] },
  // `#` and the id of a question block in the text's document cites that question's ask.
  {
    body: "See #q2 for the rollout.",
    questions: { q2: "dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333" },
    refs: ["dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333"],
  },
  {
    body: "As agreed, _see #q2_.",
    questions: { q2: "dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333" },
    refs: ["dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333"],
  },
  {
    body: "Answered in #q2.",
    questions: { q2: "dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333" },
    refs: ["dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333"],
  },
  // An id no question holds, a `#` that does not stand alone and a block's own declaration cite
  // nothing.
  {
    body: "See #nope here.",
    questions: { q2: "dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333" },
    refs: [],
  },
  {
    body: "See owner/repo#1 here.",
    questions: { "1": "dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333" },
    refs: [],
  },
  {
    body: ":::ask{#q2}\nWhich region first?\n:::",
    questions: { q2: "dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333" },
    refs: [],
  },
  // A key or `#id` inside an address is part of that address.
  {
    body: "See https://example.com/search?q=(CORE-1) and https://example.com/docs(#q2).",
    issues: ["CORE-1"],
    questions: { q2: "dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333" },
    refs: [],
  },
  // Each of them in code: the server reads it there, as it reads an address in code, and no
  // renderer links inside code.
  { body: "`CORE-1` is blocked.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"], links: [] },
  {
    body: "Blocked on `CORE-1` today.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  {
    body: "Blocked `(CORE-1)` today.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  {
    body: "Blocked `[CORE-1]` today.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  {
    body: 'Blocked on `"CORE-1"` today.',
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  {
    body: "Blocked on `'CORE-1'` today.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  {
    body: "Blocked on `**CORE-1**` today.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  {
    body: "Blocked on `_CORE-1_` today.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  { body: "`>CORE-1` is blocked.", issues: ["CORE-1"], refs: ["dispatch://CORE-1"], links: [] },
  {
    body: "Blocked on `~~CORE-1~~` today.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  {
    body: "Cells are written `|CORE-1|open|`.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  {
    body: "Blocked on `&#32;CORE-1` today.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  {
    body: "Blocked on `&#x20;CORE-1` today.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  {
    body: "Blocked on `&#9;CORE-1` today.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  {
    body: "Blocked on `&#10;CORE-1` today.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  { body: "See `XCORE-1` here.", issues: ["XCORE-1"], refs: ["dispatch://XCORE-1"], links: [] },
  {
    body: "`CORE-1's` spec is done.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  { body: "Done in `CORE-1.`", issues: ["CORE-1"], refs: ["dispatch://CORE-1"], links: [] },
  {
    body: "Blocked on `CORE-1,` then shipped.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  {
    body: "See `[the blocker](CORE-1)`.",
    issues: ["CORE-1"],
    refs: ["dispatch://CORE-1"],
    links: [],
  },
  {
    body: "See `#q2` for the rollout.",
    questions: { q2: "dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333" },
    refs: ["dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333"],
    links: [],
  },
  {
    body: "As agreed, `_see #q2_`.",
    questions: { q2: "dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333" },
    refs: ["dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333"],
    links: [],
  },
  {
    body: "Answered in `#q2.`",
    questions: { q2: "dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333" },
    refs: ["dispatch://CORE-1/ask/33333333-3333-4333-8333-333333333333"],
    links: [],
  },
];
