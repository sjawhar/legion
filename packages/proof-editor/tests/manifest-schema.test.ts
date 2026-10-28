import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { prosemirrorToYXmlFragment } from "y-prosemirror";
import * as Y from "yjs";
import type { BlockSchema } from "../src/lib";
import { createHeadlessProof } from "../src/lib-headless.js";
import { withMarksEditor } from "./marks-editor";
import { pasteHtml, pastePlainText, withHTMLElementGlobal } from "./paste";

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

/** The manifest of a later build, at the same version: a typed block, an attribute, an enum
 *  value, a node and a mark an editor of version 2 reads but may not create yet. */
const laterManifest = {
  ...manifest,
  nodes: manifest.nodes?.map((node) =>
    node.name === "hr" ? { ...node, since: 2, creatable_since: 3 } : node
  ),
  marks: manifest.marks?.map((mark) =>
    mark.name === "strike_through" ? { ...mark, since: 2, creatable_since: 3 } : mark
  ),
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

/** Each element of `laterManifest` that an editor of version 2 may not create, that `doc` holds. */
function uncreatable(doc: ProseMirrorNode): string[] {
  const found = new Set<string>();
  doc.descendants((node) => {
    if (node.type.name === "future") found.add("future");
    if (node.type.name === "hr") found.add("hr");
    if (node.type.name === "callout" && node.attrs.tone === "loud") found.add("tone loud");
    if (node.type.name === "ask" && node.attrs.urgency === "critical")
      found.add("urgency critical");
    if (node.marks.some((mark) => mark.type.name === "strike_through")) found.add("strike_through");
  });
  return [...found].sort();
}

const everyUncreatable = ["future", "hr", "strike_through", "tone loud", "urgency critical"];

/** Each element that an editor of version 2 may not create, as markdown, and why it is refused. */
const uncreatableMarkdown: [string, string][] = [
  [":::future\nFuture text\n:::", 'this editor cannot create typed block "future" yet'],
  [
    ':::callout{tone="loud"}\nLoud text\n:::',
    'this editor cannot write "loud" into typed block "callout" attribute "tone" yet',
  ],
  [
    ':::ask{urgency="critical"}\nCritical question?\n:::',
    'this editor cannot write "critical" into typed block "ask" attribute "urgency" yet',
  ],
  ["~~Struck text~~", 'this editor cannot create mark "strike_through" yet'],
  ["Above\n\n***\n\nBelow", 'this editor cannot create node "hr" yet'],
];

/** A document a later build stored, holding every element an editor of version 2 only reads. */
const storedMarkdown = uncreatableMarkdown.map(([markdown]) => markdown).join("\n\n");

/** The stored document as a reader parses it. */
async function storedDocument(): Promise<ProseMirrorNode> {
  const reader = await createHeadlessProof({ blockSchema: laterManifest, readOnly: true });
  return reader.parseMarkdown(storedMarkdown);
}

/** A collaborator's update that adds `doc`'s blocks reaches `ydoc`, as a stored document's
 *  content reaches an open editor. */
function receive(ydoc: Y.Doc, doc: ProseMirrorNode): void {
  const collaborator = new Y.Doc();
  prosemirrorToYXmlFragment(doc, collaborator.getXmlFragment("prosemirror"));
  Y.applyUpdate(ydoc, Y.encodeStateAsUpdate(collaborator));
}

test("a markdown paste or import of an element the editor cannot create yet is refused, and a stored document holding it still reads", async () => {
  const stored = await storedDocument();
  expect(uncreatable(stored)).toEqual(everyUncreatable);

  await withMarksEditor(
    "Before",
    ({ handle, view, ydoc }) => {
      for (const [markdown, refusal] of uncreatableMarkdown) {
        expect(() => withHTMLElementGlobal(() => pastePlainText(view, markdown))).toThrow(refusal);
        expect(() => handle.setMarkdown(markdown)).toThrow(refusal);
      }
      expect(view.state.doc.textContent).toBe("Before");

      withHTMLElementGlobal(() => pastePlainText(view, ':::callout{kind="warning"}\nFine\n:::'));
      expect(blockOfType(view.state.doc, "callout").attrs.kind).toBe("warning");
      expect(uncreatable(view.state.doc)).toEqual([]);

      receive(ydoc, stored);
      expect(uncreatable(view.state.doc)).toEqual(everyUncreatable);
    },
    { blockSchema: laterManifest }
  );

  // A version view reads a stored version's markdown into a read-only editor.
  await withMarksEditor(
    "",
    ({ handle, view }) => {
      handle.setMarkdown(storedMarkdown);
      expect(uncreatable(view.state.doc)).toEqual(everyUncreatable);
    },
    { blockSchema: laterManifest, readOnly: true }
  );
});

test("an HTML paste drops each element the editor cannot create yet and keeps its text, and a stored document holding it still reads", async () => {
  const stored = await storedDocument();

  await withMarksEditor(
    "Before",
    ({ view, ydoc }) => {
      const pastes: [string, string][] = [
        ['<section data-proof-block-type="future"><p>Future text</p></section>', "Future text"],
        [
          '<section data-proof-block-type="callout" data-proof-block-attr-tone="loud"><p>Loud text</p></section>',
          "Loud text",
        ],
        [
          '<section data-proof-block-type="ask" data-proof-block-attr-urgency="critical"><p>Critical question?</p></section>',
          "Critical question?",
        ],
        ["<p><del>Struck text</del></p>", "Struck text"],
        ["<p>Above</p><hr><p>Below</p>", "Below"],
      ];
      for (const [html, text] of pastes) {
        withHTMLElementGlobal(() => pasteHtml(view, html));
        expect(view.state.doc.textContent).toContain(text);
        expect(uncreatable(view.state.doc)).toEqual([]);
      }

      withHTMLElementGlobal(() =>
        pasteHtml(
          view,
          '<section data-proof-block-type="callout" data-proof-block-attr-kind="warning"><p>Fine</p></section>'
        )
      );
      expect(blockOfType(view.state.doc, "callout").attrs.kind).toBe("warning");

      receive(ydoc, stored);
      expect(uncreatable(view.state.doc)).toEqual(everyUncreatable);
    },
    { blockSchema: laterManifest }
  );
});
