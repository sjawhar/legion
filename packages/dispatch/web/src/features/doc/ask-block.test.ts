import { expect, test } from "bun:test";
import { EditorState, TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import { Schema } from "prosemirror-model";

import type { BlockTypeSchema } from "../../api/types";
import { askBlockEditingPlugin, editingAskBlockPos, renderTypedBlock } from "./ask-block";

const schema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
    bullet_list: { content: "list_item+", group: "block" },
    list_item: { content: "paragraph+" },
    ask: {
      attrs: { blockId: { default: null } },
      content: "paragraph+ bullet_list?",
      group: "block",
    },
  },
});

function ask(blockId: string, question: string, ...options: string[]) {
  return schema.node("ask", { blockId }, [
    schema.node("paragraph", undefined, [schema.text(question)]),
    schema.node(
      "bullet_list",
      undefined,
      options.map((option) =>
        schema.node("list_item", undefined, [
          schema.node("paragraph", undefined, [schema.text(option)]),
        ])
      )
    ),
  ]);
}

// doc: paragraph "Context" (0..9), ask "storage" (9..), ask "naming" after it.
const doc = schema.node("doc", undefined, [
  schema.node("paragraph", undefined, [schema.text("Context")]),
  ask("storage", "Which engine?", "Postgres", "SQLite"),
  ask("naming", "Which name?", "legion", "lg"),
]);
const storagePos = 9;
const namingPos = storagePos + doc.child(1).nodeSize;

/** The plugin only needs the state and the node-view DOM lookup; a real `EditorView` needs a
 * browser selection this test does not. */
function fakeView(state: EditorState, shells: Record<number, HTMLElement>) {
  const view = {
    nodeDOM: (pos: number) => shells[pos] ?? null,
    state,
  } as unknown as EditorView & { state: EditorState };
  return view;
}

function caretAt(state: EditorState, pos: number): EditorState {
  return state.apply(state.tr.setSelection(TextSelection.create(state.doc, pos)));
}

test("editingAskBlockPos names the ask node around the caret and nothing outside one", () => {
  const start = EditorState.create({ doc, schema });
  expect(editingAskBlockPos(start)).toBeUndefined();
  // Inside the storage question text.
  expect(editingAskBlockPos(caretAt(start, storagePos + 3))).toBe(storagePos);
  // Inside the second option of the storage list, three levels down.
  const sqlite = storagePos + doc.child(1).nodeSize - 4;
  expect(editingAskBlockPos(caretAt(start, sqlite))).toBe(storagePos);
  expect(editingAskBlockPos(caretAt(start, namingPos + 2))).toBe(namingPos);
  expect(editingAskBlockPos(caretAt(start, 3))).toBeUndefined();
});

test("the editing plugin marks exactly the block the caret is inside, and clears it when the caret leaves", () => {
  const storage = document.createElement("section");
  const naming = document.createElement("section");
  const shells = { [storagePos]: storage, [namingPos]: naming };
  const initial = EditorState.create({ doc, schema });
  const view = fakeView(initial, shells);
  const pluginView = askBlockEditingPlugin.spec.view?.(view);
  if (pluginView?.update === undefined) throw new Error("the plugin has no view");

  expect(storage.dataset.dispatchAskEditing).toBeUndefined();
  expect(naming.dataset.dispatchAskEditing).toBeUndefined();

  view.state = caretAt(initial, storagePos + 3);
  pluginView.update(view, initial);
  expect(storage.dataset.dispatchAskEditing).toBe("true");
  expect(naming.dataset.dispatchAskEditing).toBeUndefined();

  view.state = caretAt(initial, namingPos + 2);
  pluginView.update(view, initial);
  expect(storage.dataset.dispatchAskEditing).toBeUndefined();
  expect(naming.dataset.dispatchAskEditing).toBe("true");

  view.state = caretAt(initial, 3);
  pluginView.update(view, initial);
  expect(storage.dataset.dispatchAskEditing).toBeUndefined();
  expect(naming.dataset.dispatchAskEditing).toBeUndefined();

  view.state = caretAt(initial, storagePos + 3);
  pluginView.update(view, initial);
  expect(storage.dataset.dispatchAskEditing).toBe("true");
  pluginView.destroy?.();
  expect(storage.dataset.dispatchAskEditing).toBeUndefined();
});

// The header shows what the document's author wrote and nothing else. Which attributes are the
// server's is the block type's own schema, not a list kept here: a hand-written copy of that
// flag goes stale the moment a type gains a server-owned attribute, and the new one leaks into
// every reader's view of the block.
const calloutSchema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
    callout: {
      attrs: {
        blockId: { default: null },
        kind: { default: "note" },
        // A server-owned attribute this file has never heard of.
        resolved_by: { default: null },
        title: { default: null },
      },
      content: "paragraph+",
      group: "block",
    },
  },
});

const calloutType: BlockTypeSchema = {
  attributes: {
    kind: { kind: "enum", choices: ["note", "warning"] },
    resolved_by: { kind: "string", server: true },
    title: { kind: "string" },
  },
  content: "paragraph+",
  name: "callout",
  render: "host",
};

/** Every string the drawing puts on the page, in order. */
function renderedText(spec: unknown): string[] {
  if (typeof spec === "string") return [spec];
  if (!Array.isArray(spec)) return [];
  return spec.flatMap((child, index) =>
    index === 1 && !Array.isArray(child) && typeof child === "object" ? [] : renderedText(child)
  );
}

test("a typed block's header shows the author's attributes and hides the schema's server ones", () => {
  const node = calloutSchema.node(
    "callout",
    { blockId: "callout-1", kind: "warning", resolved_by: "alice", title: "Read this" },
    [calloutSchema.node("paragraph", undefined, [calloutSchema.text("Body text.")])]
  );

  const text = renderedText(renderTypedBlock(node, calloutType));

  expect(text).toContain("callout");
  expect(text).toContain("warning");
  expect(text).toContain("Read this");
  // The block's identity and the schema's server-owned attribute, neither of them written by
  // the author, appear nowhere - not as a value and not as a name.
  expect(text).not.toContain("callout-1");
  expect(text).not.toContain("blockId");
  expect(text).not.toContain("alice");
  expect(text).not.toContain("resolved_by");
});
