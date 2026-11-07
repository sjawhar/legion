// differential.ts - the browser editor's reading for the pmdoc differential (differential.sh).
//
//   bun differential.ts [--read-only] <in.jsonl> <out.jsonl> <field>...
//
// Each line of <in.jsonl> is a JSON object with an "id". For every named field holding a string,
// the fork's headless engine parses that string as markdown, as the browser editor imports a
// document, and the output line carries the field as {"pm": <ProseMirror JSON>} or {"err": ...};
// a field that is absent or null is written null. Lines keep their input order, so the Go side
// reads every file of one run in lockstep.
//
// With --read-only the read-only engine reads instead, as a body only shown to a reader is read
// (createHeadlessProof's readOnly, packages/proof-editor/src/lib-headless.ts), and each field is
// written under a second key, "<field>_readonly", so one file never holds two readings under one
// name. That is what the cross-reader gate holds Go's display read against.
import { closeSync, openSync, readFileSync, readSync, writeSync } from "node:fs";
import { join } from "node:path";
import { StringDecoder } from "node:string_decoder";
import { setBlockIdGenerator } from "@legion/proof-editor";
import { createHeadlessProof } from "@legion/proof-editor/headless";

/**
 * Reads a file one line at a time, holding one line rather than the whole file: a census corpus
 * runs to gigabytes, past the largest string a runtime will make, so reading it whole fails
 * outright. The lines keep the file's order, which every reader of one run is held to, and a
 * final line with no newline after it is a line like any other.
 */
function* lines(path: string): Generator<string> {
  const handle = openSync(path, "r");
  try {
    const chunk = Buffer.allocUnsafe(1 << 20);
    const decoder = new StringDecoder("utf8");
    let rest = "";
    while (true) {
      const read = readSync(handle, chunk, 0, chunk.length, null);
      if (read === 0) break;
      rest += decoder.write(chunk.subarray(0, read));
      let start = 0;
      let newline = rest.indexOf("\n", start);
      while (newline !== -1) {
        yield rest.slice(start, newline);
        start = newline + 1;
        newline = rest.indexOf("\n", start);
      }
      rest = rest.slice(start);
    }
    rest += decoder.end();
    if (rest !== "") yield rest;
  } finally {
    closeSync(handle);
  }
}

const args = process.argv.slice(2);
const readOnly = args.includes("--read-only");
const [input, output, ...fields] = args.filter((argument) => argument !== "--read-only");
if (!input || !output || fields.length === 0) {
  console.error("usage: bun differential.ts [--read-only] <in.jsonl> <out.jsonl> <field>...");
  process.exit(2);
}
const blockSchema = JSON.parse(
  readFileSync(join(import.meta.dir, "..", "schema", "blocks.json"), "utf8")
);
let blockNumber = 0;
setBlockIdGenerator(() => `b-${String(++blockNumber).padStart(6, "0")}`);
const engine = readOnly
  ? await createHeadlessProof({ blockSchema, readOnly: true })
  : await createHeadlessProof({ blockSchema });
const suffix = readOnly ? "_readonly" : "";

const out = openSync(output, "w");
for (const line of lines(input)) {
  if (!line.trim()) continue;
  const record = JSON.parse(line);
  const read: Record<string, unknown> = { id: record.id };
  for (const field of fields) {
    const markdown = record[field];
    const key = field + suffix;
    if (typeof markdown !== "string") {
      read[key] = null;
      continue;
    }
    blockNumber = 0;
    try {
      read[key] = { pm: engine.parseMarkdown(markdown).toJSON() };
    } catch (error) {
      read[key] = { err: String(error) };
    }
  }
  const text = `${JSON.stringify(read)}\n`;
  const length = Buffer.byteLength(text);
  // On a full disk writeSync writes what fits and returns the short count instead of throwing.
  const written = writeSync(out, text);
  if (written !== length) {
    throw new Error(`short write to ${output}: ${written} of ${length} bytes`);
  }
}
closeSync(out);
