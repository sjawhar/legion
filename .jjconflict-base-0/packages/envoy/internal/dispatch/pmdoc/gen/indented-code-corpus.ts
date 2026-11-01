// indented-code-corpus.ts - a corpus of indented code blocks ending in whitespace-only lines for the
// pmdoc differential (differential.sh), the committed evidence for sjawhar/legion#2481.
//
//   bun indented-code-corpus.ts <out.jsonl> [count]
//
// Writes count (default 4000) distinct documents as {"id", "md"} lines, each an indented code
// block in a container: at top level after a paragraph or heading, in a quote, a bullet item with
// or without text on its marker line, an ordered item, a nested item, a quote in an item, an item
// in a quote, a footnote definition, a quote in a footnote definition, a footnote definition in a
// quote, a typed block at top level, in an item and in a quote, a typed block in a quote in a
// typed block and a quote in a typed block in a quote, on the marker line of an item, after a
// typed block holding a blank line in an item, a typed block (a callout or an ask) indented in a
// quote or an item, right after a list it is outside of (`100. a`, `-\ta`, `10) a`) or right
// after a quote ending in a heading, a rule or a fence. The code is a
// first line four to eight columns past its container, up to three more lines of code or of
// whitespace, and one to four lines of whitespace - spaces and tabs, up to ten columns, after the
// container's markers or none of them. After it come zero to two empty lines and then the end of
// the document, a paragraph, an indented or lazy line, the next item, a quoted line or item or a
// fence, or a line inside the container; a typed block's closing `:::` follows last. A document
// ending in the code's whitespace-only lines has no final line feed one time in three. Pieces are
// picked with the high bits of a linear congruential generator, since its low bits cycle with
// period 16. The same arguments write the same file.
import { writeFileSync } from "node:fs";

const [output, countArgument] = process.argv.slice(2);
if (!output) {
  console.error("usage: bun indented-code-corpus.ts <out.jsonl> [count]");
  process.exit(2);
}
const count = Number(countArgument ?? 4000);

let state = 20261028;
const pick = <T>(values: readonly T[]): T => {
  state = (Math.imul(state, 1103515245) + 12345) >>> 0;
  return values[(state >>> 16) % values.length] as T;
};

// A container the code sits in.
type Container = {
  kind: string;
  // The lines before the code, one list picked per document.
  leads: readonly (readonly string[])[];
  // What starts the code's first line, before its four to eight columns of indent.
  first: string;
  // What every container on the code's later lines eats.
  prefix: string;
  // What starts a whitespace-only line: the containers' markers alone, or with their padding.
  holds: readonly string[];
  // The lines after the code that close the container; when set, what follows the code stays
  // inside the container and is one of nexts alone.
  close?: readonly string[];
  // The lines after the code that belong to this container, such as its next item.
  nexts: readonly string[];
};

const callout = ':::callout{kind="note" title="T"}';
const ask = ':::ask{#ask-1 urgency="high" multiple="false" state="open"}';
const containers: readonly Container[] = [
  {
    kind: "top",
    leads: [["p", ""], ["# h"], ["p", "", "q", ""]],
    first: "",
    prefix: "",
    holds: [""],
    nexts: [],
  },
  {
    kind: "quote",
    leads: [[], ["> a", ">"]],
    first: "> ",
    prefix: "> ",
    holds: [">", "> "],
    nexts: ["> z", ">"],
  },
  {
    kind: "item-text",
    leads: [["- a", ""], ["- a"]],
    first: "  ",
    prefix: "  ",
    holds: ["", "  "],
    nexts: ["- b", "  z"],
  },
  {
    kind: "item-empty",
    leads: [["-"], ["-", ""]],
    first: "  ",
    prefix: "  ",
    holds: ["", "  "],
    nexts: ["- b", "  z"],
  },
  {
    kind: "ordered",
    leads: [["1. a", ""], ["1. a"], ["1."]],
    first: "   ",
    prefix: "   ",
    holds: ["", "   "],
    nexts: ["2. b", "   z"],
  },
  {
    kind: "nested-item",
    leads: [
      ["- a", "  - b", ""],
      ["- a", "  - b"],
      ["- a", "", "  - b", ""],
    ],
    first: "    ",
    prefix: "    ",
    holds: ["", "  ", "    "],
    nexts: ["  - c", "- c", "    z"],
  },
  {
    kind: "quote-in-item",
    leads: [
      ["- a", ""],
      ["- a", "  > q", "  >"],
    ],
    first: "  > ",
    prefix: "  > ",
    holds: ["", "  >", "  > "],
    nexts: ["  > z", "- b", "  z"],
  },
  {
    kind: "item-in-quote",
    leads: [["> -"], ["> - a", ">"], ["> - a"]],
    first: ">   ",
    prefix: ">   ",
    holds: [">", "> ", ">   "],
    nexts: ["> - b", ">   z"],
  },
  {
    kind: "footnote",
    leads: [["x[^f]", "", "[^f]: a", ""]],
    first: "    ",
    prefix: "    ",
    holds: ["", "    "],
    nexts: ["    z", "[^g]: b"],
  },
  {
    kind: "quote-in-footnote",
    leads: [
      ["x[^f]", "", "[^f]: a", "", "    > q", "    >"],
      ["x[^f]", "", "[^f]: > q", "    >"],
    ],
    first: "    > ",
    prefix: "    > ",
    holds: ["", "    >", "    > "],
    nexts: ["    > z", "    z"],
  },
  {
    kind: "footnote-in-quote",
    leads: [["x[^f]", "", "> [^f]: a", ">"]],
    first: ">     ",
    prefix: ">     ",
    holds: [">", "> ", ">     "],
    nexts: [">     z", "> z"],
  },
  {
    kind: "typed",
    leads: [[callout], [callout, "x", ""]],
    first: "",
    prefix: "",
    holds: [""],
    close: [":::"],
    nexts: ["", "y", "- b", "> q"],
  },
  {
    kind: "typed-in-item",
    leads: [
      ["- a", `  ${callout}`],
      ["- a", `  ${callout}`, "  x", ""],
    ],
    first: "  ",
    prefix: "  ",
    holds: ["", "  "],
    close: ["  :::"],
    nexts: ["", "  y", "  - b"],
  },
  {
    kind: "typed-in-quote",
    leads: [[`> ${callout}`], [`> ${callout}`, "> x", ">"]],
    first: "> ",
    prefix: "> ",
    holds: [">", "> "],
    close: ["> :::"],
    nexts: ["", "> y", "> - b"],
  },
  {
    kind: "typed-quote-typed",
    leads: [[`:${callout}`, `> ${callout}`]],
    first: "> ",
    prefix: "> ",
    holds: [">", "> "],
    close: ["> :::", "::::"],
    nexts: ["", "> y"],
  },
  {
    kind: "quote-typed-quote",
    leads: [
      [`> ${callout}`, "> > q", "> >"],
      [`> ${callout}`, "> >"],
    ],
    first: "> > ",
    prefix: "> > ",
    holds: [">", "> >", "> > "],
    close: ["> :::"],
    nexts: ["", "> y", "> > z"],
  },
  {
    kind: "after-list",
    leads: [
      ["100. a", ""],
      ["-\ta", ""],
      ["10) a", ""],
    ],
    first: "",
    prefix: "",
    holds: [""],
    nexts: ["101. b", "11) b", "-\tb"],
  },
  {
    kind: "after-quote",
    leads: [["> # h"], ["> ***"], ["> ```", "> c", "> ```"]],
    first: "",
    prefix: "",
    holds: [""],
    nexts: [],
  },
  {
    kind: "marker-line",
    leads: [[]],
    first: "- ",
    prefix: "  ",
    holds: ["", "  "],
    nexts: ["- b", "  z"],
  },
  {
    kind: "quote-marker-line",
    leads: [[], ["> a", ">"]],
    first: "> - ",
    prefix: ">   ",
    holds: [">", "> ", ">   "],
    nexts: ["> - b", ">   z"],
  },
  {
    kind: "typed-indented-in-quote",
    leads: [[`>   ${callout}`], [`>   ${ask}`]],
    first: ">   ",
    prefix: ">   ",
    holds: [">", "> ", ">   "],
    nexts: ["", ">   y"],
  },
  {
    kind: "typed-indented-in-item",
    leads: [
      ["- a", "", `    ${callout}`],
      ["- a", "", `    ${ask}`],
    ],
    first: "    ",
    prefix: "    ",
    holds: ["", "  ", "    "],
    nexts: ["", "    y", "- b"],
  },
  {
    kind: "item-after-typed-blank",
    leads: [
      ["- a", `  ${callout}`, "  x", "", "  y", "  :::"],
      ["- a", `  ${callout}`, "  x", "", "  y", "  :::", ""],
    ],
    first: "  ",
    prefix: "  ",
    holds: ["", "  "],
    nexts: ["- b", "  z"],
  },
];

// What may follow the code of a container that needs no closing line.
const nexts = [
  "",
  "after",
  "lazy",
  "  y",
  "    z",
  "- next",
  "> quoted",
  ">   b",
  "> - b",
  "```\nf\n```",
];
const gaps = [0, 1, 2];
const firstIndents = [4, 5, 6, 7, 8];
const innerIndents = [4, 5, 6, 8];
const innerCounts = [0, 1, 2, 3];
const trailingCounts = [1, 2, 3, 4];

// Whitespace-only line bodies: zero to ten spaces, or a tab mixed with spaces.
const spaces = Array.from({ length: 11 }, (_, width) => " ".repeat(width));
const tabbed = ["\t", " \t", "  \t", "   \t", "\t ", "\t  ", "\t\t", "\t \t"].flatMap((lead) =>
  ["", " ", "  ", "   "].map((pad) => lead + pad)
);
// Two of three whitespace lines are spaces alone.
const whitespaceSets = [spaces, spaces, tabbed];
const whitespaceLine = (container: Container): string =>
  pick(container.holds) + pick(pick(whitespaceSets));

const markdownIn = (container: Container): string => {
  const lines = [...pick(container.leads)];
  lines.push(`${container.first}${" ".repeat(pick(firstIndents))}code`);
  for (let inner = pick(innerCounts); inner > 0; inner--) {
    lines.push(
      pick([true, false])
        ? `${container.prefix}${" ".repeat(pick(innerIndents))}more`
        : whitespaceLine(container)
    );
  }
  for (let trailing = pick(trailingCounts); trailing > 0; trailing--) {
    lines.push(whitespaceLine(container));
  }
  for (let gap = pick(gaps); gap > 0; gap--) lines.push("");
  const next = pick(container.close ? container.nexts : [...nexts, ...container.nexts]);
  if (next) lines.push(next);
  if (container.close) lines.push(...container.close);
  // A document ending in its whitespace-only lines ends, one time in three, with no line feed.
  const ending = container.close || next ? "\n" : pick(["\n", "\n", ""]);
  return `${lines.join("\n")}${ending}`;
};

const seen = new Set<string>();
const lines: string[] = [];
for (let attempt = 0; lines.length < count && attempt < count * 10; attempt++) {
  const container = pick(containers);
  const markdown = markdownIn(container);
  if (seen.has(markdown)) continue;
  seen.add(markdown);
  lines.push(JSON.stringify({ id: `${container.kind}-${lines.length}`, md: markdown }));
}
writeFileSync(output, `${lines.join("\n")}\n`);
