import { expect, test } from "bun:test";
import { EditorState, TextSelection } from "@milkdown/kit/prose/state";
import { EditorView } from "@milkdown/kit/prose/view";
import { DOMSerializer, Schema } from "prosemirror-model";

import type { BlockTypeSchema } from "../../api/types";
import { askBlockEditingPlugin, editingAskBlockPos, installTypedBlocks } from "./ask-block";
import { renderTypedBlock } from "./typed-block-drawing";

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

// A `check` block, whose `serves` points at the requirements it proves. The drawing printed
// every array attribute with `JSON.stringify`, so driving the real page showed a reader
// `serves ["dispatch://VER-1/spec#b-r1"]` - the value's JSON instead of the blocks it names
// (INFRA-174).
const checkSchema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: { content: "inline*", group: "block" },
    text: { group: "inline" },
    check: {
      attrs: {
        blockId: { default: null },
        run: { default: null },
        serves: { default: null },
        from: { default: null },
      },
      content: "paragraph+",
      group: "block",
    },
  },
});

const checkType: BlockTypeSchema = {
  attributes: {
    from: { kind: "block[]", targets: ["ask"], items: ["ask", "comment", "message"] },
    run: { kind: "string" },
    serves: { kind: "block[]", targets: ["requirement"] },
  },
  content: "paragraph+",
  name: "check",
  render: "host",
};

function checkBlock(serves: string[]): HTMLElement {
  const node = checkSchema.node("check", { blockId: "c1", run: "bun test web/src", serves }, [
    checkSchema.node("paragraph", undefined, [checkSchema.text("The tab lists every check.")]),
  ]);
  return DOMSerializer.renderSpec(document, renderTypedBlock(node, checkType)).dom as HTMLElement;
}

test("a block[] attribute draws the blocks it points at, each linked to its block", () => {
  const drawn = checkBlock([
    "dispatch://CORE-1/spec#b-r1",
    "dispatch://CORE-1/artifact/plan-md#b-t1",
  ]);

  const serves = drawn.querySelector('dd[data-proof-block-attribute="serves"]');
  expect(
    Array.from(serves?.querySelectorAll("a") ?? [], (link) => ({
      href: link.getAttribute("href"),
      // What the document's own click handler and hover card read a drawn link's target from.
      reference: link.getAttribute("data-dispatch-href"),
      text: link.textContent,
    }))
  ).toEqual([
    {
      href: "/issues/CORE-1/spec#b-r1",
      reference: "dispatch://CORE-1/spec#b-r1",
      text: "CORE-1 spec block",
    },
    {
      href: "/issues/CORE-1/artifacts/plan-md#b-t1",
      reference: "dispatch://CORE-1/artifact/plan-md#b-t1",
      text: "CORE-1 plan-md block",
    },
  ]);
  // None of the JSON a reader used to see, and the pointers read as separate words.
  expect(serves?.textContent).toBe("CORE-1 spec block CORE-1 plan-md block");
  // Every other kind is drawn as it was: the check's `run` is still its own text.
  expect(drawn.querySelector('dd[data-proof-block-attribute="run"]')?.textContent).toBe(
    "bun test web/src"
  );
});

test("a block[] attribute pointing at no block is not drawn at all", () => {
  const drawn = checkBlock([]);
  expect(drawn.querySelector('[data-proof-block-attribute="serves"]')).toBeNull();
  expect(drawn.textContent).not.toContain("serves");
  expect(drawn.querySelector('dd[data-proof-block-attribute="run"]')?.textContent).toBe(
    "bun test web/src"
  );
});

test("a from header links to a declared comment block but serves does not", () => {
  const source = "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-b1";
  const node = checkSchema.node("check", { blockId: "c1", from: [source], serves: [source] }, [
    checkSchema.node("paragraph", undefined, [checkSchema.text("Check retries.")]),
  ]);
  const dom = DOMSerializer.renderSpec(document, renderTypedBlock(node, checkType))
    .dom as HTMLElement;
  const link = dom.querySelector<HTMLAnchorElement>('dd[data-proof-block-attribute="from"] a');
  expect(link?.getAttribute("href")).toBe(
    "/issues/CORE-1/comments/11111111-1111-4111-8111-111111111111#b-b1"
  );
  expect(link?.getAttribute("data-dispatch-href")).toBe(source);
  expect(dom.querySelector('dd[data-proof-block-attribute="serves"]')).toBeNull();
});

test("hovering a typed header link preserves its DOM while content edits stay observed", async () => {
  const host = document.createElement("div");
  document.body.append(host);
  const view = new EditorView(host, {
    state: EditorState.create({ schema: checkSchema }),
    nodeViews: {
      paragraph: () => {
        const dom = document.createElement("p");
        return { contentDOM: dom, dom };
      },
    },
  });
  try {
    installTypedBlocks(
      view,
      { version: 4, types: [checkType] },
      () => {},
      () => () => {}
    );
    const source = "dispatch://CORE-1/comment/11111111-1111-4111-8111-111111111111#b-b1";
    view.updateState(
      EditorState.create({
        doc: checkSchema.node("doc", undefined, [
          checkSchema.node("check", { blockId: "c1", from: [source] }, [
            checkSchema.node("paragraph", undefined, [checkSchema.text("Check retries.")]),
          ]),
        ]),
        schema: checkSchema,
      })
    );
    const link = view.dom.querySelector("a");
    if (link === null) throw new Error("the from header has no source link");
    const hovered = Promise.withResolvers<void>();
    const hoverObserver = new MutationObserver(() => {
      hoverObserver.disconnect();
      hovered.resolve();
    });
    hoverObserver.observe(link, { attributes: true });
    link.classList.add("markdown-link-hover-target");
    await hovered.promise;
    expect(view.dom.querySelector("a")).toBe(link);
    const paragraph = view.dom.querySelector("p");
    if (paragraph === null) throw new Error("the check has no editable content");
    const edited = Promise.withResolvers<void>();
    const editObserver = new MutationObserver(() => {
      editObserver.disconnect();
      edited.resolve();
    });
    editObserver.observe(paragraph, { childList: true });
    paragraph.textContent = "Changed check content.";
    await edited.promise;
    expect(view.state.doc.textContent).toBe("Changed check content.");
  } finally {
    view.destroy();
    host.remove();
  }
});
