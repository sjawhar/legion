// Run with bun, not by Playwright: Node can't load the proof editor's TypeScript dependencies
// from node_modules. Reads markdown on stdin and prints the headless engine's reading of it: each
// table's rows, as the text of their cells, and each paragraph's text.
import { readFileSync } from "node:fs";

import { createHeadlessProof } from "@legion/proof-editor/headless";

const blockSchema = JSON.parse(
  readFileSync(
    new URL("../../envoy/internal/dispatch/pmdoc/schema/blocks.json", import.meta.url),
    "utf8"
  )
);
const engine = await createHeadlessProof({ blockSchema });
const tables: string[][][] = [];
const paragraphs: string[] = [];
// A table cell holds a paragraph, so the walk goes on into a table after reading its rows.
engine.parseMarkdown(readFileSync(0, "utf8")).descendants((node) => {
  if (node.type.name === "table") {
    tables.push(
      node.content.content.map((row) => row.content.content.map((cell) => cell.textContent))
    );
  } else if (node.type.name === "paragraph") {
    paragraphs.push(node.textContent);
    return false;
  }
  return true;
});
process.stdout.write(JSON.stringify({ tables, paragraphs }));
