import { readFileSync } from "node:fs";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import type { BlockSchema } from "../src/block-schema.js";

/** The document service's own block schema, the one Dispatch parses its bodies with. */
export const serverBlockSchema: BlockSchema = JSON.parse(
  readFileSync(
    new URL("../../envoy/internal/dispatch/pmdoc/schema/blocks.json", import.meta.url),
    "utf8"
  )
);

/** Each child block of `node` as its type name and its text. */
export function blocks(node: ProseMirrorNode): [string, string][] {
  const out: [string, string][] = [];
  node.forEach((child) => {
    out.push([child.type.name, child.textContent]);
  });
  return out;
}

/** Each inline child of `node` as its text and the names of its marks. */
export function runs(node: ProseMirrorNode): { text: string; marks: string[] }[] {
  const out: { text: string; marks: string[] }[] = [];
  node.forEach((child) => {
    out.push({ text: child.text ?? "", marks: child.marks.map((mark) => mark.type.name) });
  });
  return out;
}
