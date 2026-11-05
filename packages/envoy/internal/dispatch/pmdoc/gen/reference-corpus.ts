// reference-corpus.ts - a corpus of backslash escapes and character references for the pmdoc
// differential (differential.sh).
//
//   bun reference-corpus.ts <out.jsonl> [count]
//
// Writes count (default 8000) distinct short documents as {"id", "md"} lines, each a text built
// from a character before it, a backslash escape or character reference, a piece of text, a second
// escape or reference and a character after it. The references are named (known, unknown, with a
// name only its start makes known as `&notit;`, in the wrong case, missing their `;`), decimal and
// hexadecimal (padded, over-long, controls, surrogates, noncharacters, past U+10FFFF, a line feed),
// and escaped once or twice. The text is placed in a paragraph, a heading, bold, right after a bare
// URL, in a link's text, address or title, in an image's alt, address or title, in a footnote's
// label at its reference and its definition, in a table cell, or before a line ending. The same
// arguments write the same file.
import { writeFileSync } from "node:fs";

const [output, countArgument] = process.argv.slice(2);
if (!output) {
  console.error("usage: bun reference-corpus.ts <out.jsonl> [count]");
  process.exit(2);
}
const count = Number(countArgument ?? 8000);

let state = 20261027;
const pick = <T>(values: readonly T[]): T => {
  state = (Math.imul(state, 1103515245) + 12345) >>> 0;
  return values[state % values.length] as T;
};

const befores = ["", "a", " ", "x ", "\\", "&", "*", "1"];
const references = [
  "&amp;",
  "&copy;",
  "&lt;",
  "&gt;",
  "&quot;",
  "&nbsp;",
  "&auml;",
  "&AUML;",
  "&Auml;",
  "&not;",
  "&notin;",
  "&notit;",
  "&copyx;",
  "&ampx;",
  "&nLt;",
  "&nGt;",
  "&semi;",
  "&bogus;",
  "&amp",
  "&copy",
  "&ast;",
  "&lbrack;",
  "&bsol;",
  "&#38;",
  "&#x26;",
  "&#X26;",
  "&#0;",
  "&#1;",
  "&#9;",
  "&#10;",
  "&#x0A;",
  "&#12;",
  "&#13;",
  "&#32;",
  "&#42;",
  "&#91;",
  "&#92;",
  "&#93;",
  "&#124;",
  "&#127;",
  "&#128;",
  "&#159;",
  "&#160;",
  "&#xD800;",
  "&#xDFFF;",
  "&#xFDD0;",
  "&#xFFFE;",
  "&#x1FFFF;",
  "&#x10FFFF;",
  "&#x110000;",
  "&#0000065;",
  "&#00000065;",
  "&#x000041;",
  "&#x0000041;",
  "&#9999999;",
  "&#12345678;",
  "&#;",
  "&#x;",
  "&#65",
  "&#x41",
  "\\&amp;",
  "\\\\&amp;",
  "\\&#38;",
  "\\\\&#x26;",
  "\\&copy;",
  "\\&notit;",
  "\\&",
  "\\\\",
  "\\*",
  "\\]",
  "\\[",
  "\\_",
  "\\`",
  "\\#",
  "\\a",
  "&",
  "&&amp;",
  "&amp;amp;",
];
const middles = ["", "b", " ", "x;", "; ", "end"];
const afters = ["", "b", " end", ";", "x;", "\\", "&", "."];
const forms = [
  "{}",
  "Plain {} end.",
  "# {}",
  "**{}**",
  "a {}\nb",
  "https://x.test/a{}b",
  "https://x.test{}",
  "see www.x.test{} end",
  "www.x.test/p{}",
  "[{}](https://x.test)",
  "[t](https://x.test/{})",
  '[t](https://x.test "{}")',
  "[t](https://x.test '{}')",
  "[t](<https://x.test> ({}))",
  "![{}](i.png)",
  "![a{}b](i.png)",
  "![a](i{}.png)",
  "![a](<i {}.png>)",
  '![a](i.png "{}")',
  "![a](i.png '{}')",
  "x[^{}]\n\n[^{}]: note",
  "x[^n{}]\n\n[^n{}]: note",
  "| a |\n| --- |\n| {} |",
  "| a |\n| --- |\n| ![{}](i.png) |",
  "| a |\n| --- |\n| ![a](i{}.png) |",
  '| a |\n| --- |\n| [t](u "{}") |',
  "| a |\n| --- |\n| https://x.test/a{} |",
  "> {}",
  "- {}",
];

const seen = new Set<string>();
const lines: string[] = [];
for (let attempt = 0; lines.length < count && attempt < count * 10; attempt++) {
  const text = pick(befores) + pick(references) + pick(middles) + pick(references) + pick(afters);
  const markdown = `${pick(forms).replaceAll("{}", () => text)}\n`;
  if (seen.has(markdown)) continue;
  seen.add(markdown);
  lines.push(JSON.stringify({ id: `ref-${lines.length}`, md: markdown }));
}
writeFileSync(output, `${lines.join("\n")}\n`);
