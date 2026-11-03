// delimiter-corpus.ts - a corpus of delimiter-run shapes for the pmdoc differential (differential.sh),
// the committed evidence for dispatch://INFRA-534.
//
//   bun delimiter-corpus.ts <out.jsonl> [count]
//
// Writes count (default 20000) distinct one-block documents as {"id", "md"} lines, each a run of
// one to eight pieces - letters, a space, `.`, `<`, tilde runs, escaped tildes, a lone backslash,
// `*`, `**`, `_`, link and image brackets, `&#126;`, a code span, a www URL holding a tilde and a
// footnote reference - placed in a paragraph (bare or between words), a table cell, a heading, a
// link label, an image label, a full reference link with its definition, bold, a quote, a list
// item, or a paragraph followed by the footnote's definition. Pieces are picked with the high
// bits of a linear congruential generator, since its low bits cycle with period 16. The same
// arguments write the same file.
import { writeFileSync } from "node:fs";

const [output, countArgument] = process.argv.slice(2);
if (!output) {
  console.error("usage: bun delimiter-corpus.ts <out.jsonl> [count]");
  process.exit(2);
}
const count = Number(countArgument ?? 20000);

let state = 20261027;
const pick = <T>(values: readonly T[]): T => {
  state = (Math.imul(state, 1103515245) + 12345) >>> 0;
  return values[(state >>> 16) % values.length] as T;
};

const pieces = [
  "a",
  "b",
  " ",
  ".",
  "<",
  "~",
  "~~",
  "\\~",
  "\\~~",
  "\\",
  "*",
  "**",
  "_",
  "[",
  "](u)",
  "&#126;",
  "`c`",
  "![",
  "www.a.com/~x",
  "[^a_b]",
];
const lengths = [1, 2, 3, 4, 5, 6, 7, 8];
const forms = [
  "{}",
  "Plain {} end.",
  "| h |\n| --- |\n| {} |",
  "# {}",
  "[{}](u)",
  "![{}](u)",
  "[{}][r]\n\n[r]: u",
  "**{}**",
  "> {}",
  "- {}",
  "Plain {} end.\n\n[^a_b]: n",
];

const seen = new Set<string>();
const lines: string[] = [];
for (let attempt = 0; lines.length < count && attempt < count * 10; attempt++) {
  let text = "";
  for (let length = pick(lengths); length > 0; length--) text += pick(pieces);
  const markdown = `${pick(forms).replace("{}", () => text)}\n`;
  if (seen.has(markdown)) continue;
  seen.add(markdown);
  lines.push(JSON.stringify({ id: `delimiter-${lines.length}`, md: markdown }));
}
writeFileSync(output, `${lines.join("\n")}\n`);
