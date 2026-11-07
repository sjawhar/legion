/**
 * What the editor's node view says to the frame budget as it is destroyed. The budget is behind
 * `ViewPortals`' `import()` and this node view is in every document's download, so the two meet
 * at the slot element and at one event name (`features/view/view-slots.ts`,
 * `VIEW_FRAME_RELEASE`) rather than at a call. Nothing else checks that the name dispatched here
 * is the name listened for there, or that it is dispatched on the element the budget holds.
 */
import { expect, test } from "bun:test";
import { DecorationSet } from "@milkdown/kit/prose/view";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";

import { VIEW_FRAME_RELEASE, type ViewBlockSlot } from "../view/view-slots";
import { VIEW_FRAME_SLOT } from "./typed-block-drawing";
import { viewBlockView } from "./view-block";

/** A `view` node as the Proof schema holds one: the two attributes, the editor's own block id,
 *  and the caption as its content. */
const schema = new Schema({
  nodes: {
    doc: { content: "block+" },
    text: { group: "inline" },
    view: {
      attrs: { blockId: { default: null }, data: { default: "" }, page: { default: "" } },
      content: "inline*",
      group: "block",
      toDOM: () => ["section", 0],
    },
  },
});

function viewNode(): ProseMirrorNode {
  return schema.node(
    "view",
    {
      blockId: "b-1",
      data: "dispatch://CORE/artifact/rows-json",
      page: "dispatch://CORE/artifact/chart-html",
    },
    [schema.text("The loop")]
  );
}

/** The node view, the slot it published, and every `VIEW_FRAME_RELEASE` that slot has heard. */
function drawn() {
  const registry = new Map<string, ViewBlockSlot>();
  let published = 0;
  const view = viewBlockView(viewNode(), document, registry, () => {
    published += 1;
  });
  const slot = (view.dom as HTMLElement).querySelector<HTMLElement>(`:scope > ${VIEW_FRAME_SLOT}`);
  if (slot === null) throw new Error("the node view drew no frame slot");
  const heard: Element[] = [];
  slot.addEventListener(VIEW_FRAME_RELEASE, (event) => {
    heard.push(event.currentTarget as Element);
  });
  return { heard, published: () => published, registry, slot, view };
}

test("the node view destroyed releases the frame of the slot it published", () => {
  const { heard, published, registry, slot, view } = drawn();
  // The element the budget holds is the one the block was given, and the one the event is
  // dispatched on: a release that went anywhere else would leave the frame running.
  expect([...registry.values()].map((entry) => entry.host)).toEqual([slot]);
  expect(published()).toBe(1);

  view.destroy?.();

  expect(heard).toEqual([slot]);
  expect([...registry.keys()]).toEqual([]);
  expect(published()).toBe(2);
});

test("a caption edit publishes the same slot again and releases no frame", () => {
  const { heard, published, registry, slot, view } = drawn();

  expect(view.update?.(viewNode(), [], DecorationSet.empty)).toBe(true);

  expect(heard).toEqual([]);
  expect([...registry.values()].map((entry) => entry.host)).toEqual([slot]);
  expect(published()).toBe(2);
});
