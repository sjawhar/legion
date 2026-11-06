// exhaustive-corpus.ts - every short string over a few characters, for the pmdoc differential
// (differential.sh), the committed evidence for sjawhar/legion#2458.
//
//   bun exhaustive-corpus.ts <out.jsonl> <max length> <alphabet...>
//
// Writes one paragraph document `p <s> q` as an {"id", "md"} line for every string s of one to
// max length characters over the alphabet, each alphabet argument one character: shorter strings
// first, and strings of one length in lexicographic order of the alphabet as the arguments list it.
// `9 '~' '\' '*' 'a'` writes 349,524 documents and `6 '~' '\' '*' '_' 'a' ' '` writes 55,986. The
// file is written a chunk of lines at a time, so a long corpus is never held whole. The same
// arguments write the same file.
import { closeSync, openSync, writeSync } from "node:fs";

const [output, maxArgument, ...alphabet] = process.argv.slice(2);
const max = Number(maxArgument);
if (
  !output ||
  !Number.isInteger(max) ||
  max < 1 ||
  alphabet.length === 0 ||
  alphabet.some((character) => [...character].length !== 1) ||
  new Set(alphabet).size !== alphabet.length
) {
  console.error("usage: bun exhaustive-corpus.ts <out.jsonl> <max length> <alphabet...>");
  process.exit(2);
}

const file = openSync(output, "w");
let chunk: string[] = [];
let id = 0;
for (let length = 1; length <= max; length++) {
  // digits counts in base alphabet.length, most significant first: each step is the next string
  // of this length in the arguments' order.
  const digits = new Array<number>(length).fill(0);
  for (;;) {
    const text = digits.map((digit) => alphabet[digit]).join("");
    chunk.push(JSON.stringify({ id: `exh-${id++}`, md: `p ${text} q\n` }));
    if (chunk.length === 10000) {
      writeSync(file, `${chunk.join("\n")}\n`);
      chunk = [];
    }
    let position = length - 1;
    while (position >= 0 && digits[position] === alphabet.length - 1) digits[position--] = 0;
    if (position < 0) break;
    digits[position]++;
  }
}
if (chunk.length > 0) writeSync(file, `${chunk.join("\n")}\n`);
closeSync(file);
