import { expect, test } from "bun:test";
import { EditorState, TextSelection } from "@milkdown/kit/prose/state";
import type { EditorView } from "@milkdown/kit/prose/view";
import { Schema } from "prosemirror-model";

import { askBlockEditingPlugin, editingAskBlockPos } from "./ask-block";

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
