// Run with bun, not by Playwright: Node can't load the proof editor's TypeScript dependencies
// from node_modules. Reads markdown on stdin and prints the headless engine's reading of it: each
// table's rows, as the text of their cells.
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
engine.parseMarkdown(readFileSync(0, "utf8")).descendants((node) => {
  if (node.type.name !== "table") return true;
  const rows: string[][] = [];
  node.forEach((row) => {
    const cells: string[] = [];
    row.forEach((cell) => {
      cells.push(cell.textContent);
    });
    rows.push(cells);
  });
  tables.push(rows);
  return false;
});
process.stdout.write(JSON.stringify(tables));
