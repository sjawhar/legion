import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import type { BlockSchema } from "../src/lib";
import { createHeadlessProof } from "../src/lib-headless.js";
import { withMarksEditor } from "./marks-editor";

/** The document schema manifest the document service embeds and serves. */
const manifest = JSON.parse(
  readFileSync(
    new URL("../../envoy/internal/dispatch/pmdoc/schema/blocks.json", import.meta.url),
    "utf8"
  )
) as BlockSchema;

function names(entries: readonly { name: string }[] | undefined): string[] {
  return (entries ?? []).map(({ name }) => name).sort();
}

test("the ProseMirror schema the editor builds has exactly the manifest's nodes and marks", async () => {
  const wantNodes = [...names(manifest.nodes), ...names(manifest.types)].sort();
  const wantMarks = names(manifest.marks);
  expect(names(manifest.nodes)).toHaveLength(21);
  expect(wantMarks).toHaveLength(11);

  await withMarksEditor(
    "Before",
    ({ view }) => {
      expect(Object.keys(view.state.schema.nodes).sort()).toEqual(wantNodes);
      expect(Object.keys(view.state.schema.marks).sort()).toEqual(wantMarks);
    },
    { blockSchema: manifest }
  );
  const headless = await createHeadlessProof({ blockSchema: manifest });
  expect(Object.keys(headless.schema.nodes).sort()).toEqual(wantNodes);
  expect(Object.keys(headless.schema.marks).sort()).toEqual(wantMarks);
});

/** The manifest of a later build, at the same version: a typed block, an attribute and an enum
 *  value an editor of version 2 reads but may not create yet. */
const laterManifest = {
  ...manifest,
  types: [
    ...manifest.types.map((type) => {
      if (type.name === "callout") {
        return {
          ...type,
          attributes: {
            ...type.attributes,
            tone: {
              kind: "enum",
              choices: ["plain", "loud"],
              default: "plain",
              since: 2,
              creatable_since: 3,
            },
          },
        };
      }
      if (type.name === "ask") {
        const urgency = type.attributes.urgency;
        return {
          ...type,
          attributes: {
            ...type.attributes,
            urgency: {
              ...urgency,
              choices: [...(urgency?.choices ?? []), "critical"],
              choice_versions: { critical: { since: 2, creatable_since: 3 } },
            },
          },
        };
      }
      return type;
    }),
    {
      name: "future",
      content: "block+",
      render: "host",
      attributes: {},
      since: 2,
      creatable_since: 3,
    },
  ],
} as BlockSchema;

function blockOfType(doc: ProseMirrorNode, typeName: string): ProseMirrorNode {
  let found: ProseMirrorNode | undefined;
  doc.descendants((node) => {
    if (found === undefined && node.type.name === typeName) found = node;
    return found === undefined;
  });
  if (found === undefined) throw new Error(`the document holds no ${typeName}`);
  return found;
}

test("the editor reads but never offers a typed block, attribute or value it cannot create yet", async () => {
  expect(laterManifest.version).toBe(2);
  await withMarksEditor(
    "Before",
    ({ handle, view }) => {
      expect(view.state.schema.nodes.future).toBeDefined();
      const labels = handle.blockMenuItems().map(({ label }) => label);
      expect(labels).toContain("Insert Callout");
      expect(labels.filter((label) => label.includes("Future"))).toEqual([]);
      expect(() => handle.insertTypedBlock("future")).toThrow();

      expect(handle.insertTypedBlock("callout")).toBe(true);
      const callout = blockOfType(view.state.doc, "callout");
      expect(callout.attrs.kind).toBe("note");
      expect(callout.attrs.tone).toBeUndefined();
      expect(() => handle.setBlockAttributes(callout.attrs.blockId, { tone: "loud" })).toThrow();

      expect(handle.insertTypedBlock("ask")).toBe(true);
      const ask = blockOfType(view.state.doc, "ask");
      expect(() => handle.setBlockAttributes(ask.attrs.blockId, { urgency: "critical" })).toThrow();
      expect(handle.setBlockAttributes(ask.attrs.blockId, { urgency: "high" })).toBe(true);
    },
    { blockSchema: laterManifest }
  );
});
