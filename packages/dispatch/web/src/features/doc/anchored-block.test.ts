import { expect, test } from "bun:test";
import type { EditorView, NodeView } from "@milkdown/kit/prose/view";
import { type Node as ProseMirrorNode, Schema } from "prosemirror-model";

import { type AnchoredBlockHost, installTypedBlocks } from "./ask-block";

/** The plain blocks of a document, drawn the way the Proof schema draws them: each stamps its own
 *  `data-block-id`, which is what the node view below has to reproduce. */
const schema = new Schema({
  nodes: {
    doc: { content: "block+" },
    paragraph: {
      attrs: { blockId: { default: null } },
      content: "inline*",
      group: "block",
      toDOM: (node) => ["p", { "data-block-id": node.attrs.blockId }, 0],
    },
    heading: {
      attrs: { blockId: { default: null }, level: { default: 1 } },
      content: "inline*",
      group: "block",
      toDOM: (node) => [`h${node.attrs.level}`, { "data-block-id": node.attrs.blockId }, 0],
    },
    text: { group: "inline" },
  },
});

function paragraph(blockId: string, text: string): ProseMirrorNode {
  return schema.node("paragraph", { blockId }, [schema.text(text)]);
}

/** Installs the typed blocks on a stand-in for the editor's view - the one seam `installTypedBlocks`
 *  needs - and answers the node views it registered and the host slots it published. */
function install(anchoredBlockId: string | undefined) {
  const dom = document.createElement("div");
  const props: { nodeViews?: Record<string, (node: ProseMirrorNode) => NodeView> } = {};
  const hosts: (AnchoredBlockHost | undefined)[] = [];
  // A stand-in for the editor's view: `installTypedBlocks` reads its DOM, its props and its
  // schema, and writes its props back. Nothing of ProseMirror's own behaviour is under test here.
  const view = {
    dom,
    props,
    setProps: (next: Record<string, unknown>) => Object.assign(props, next),
    state: { schema },
  } as unknown as EditorView;
  installTypedBlocks(
    view,
    { types: [], version: 1 },
    () => {},
    () => () => {},
    anchoredBlockId === undefined
      ? undefined
      : { blockId: anchoredBlockId, onHostChange: (host) => hosts.push(host) }
  );
  return { hosts, nodeViews: props.nodeViews ?? {} };
}

test("no anchored block leaves every plain block without a node view of its own", () => {
  const { nodeViews } = install(undefined);
  expect(Object.keys(nodeViews).sort()).toEqual(["ask", "image"]);
});

test("the anchored block's node view draws the block and a slot a thread portals into", () => {
  const { hosts, nodeViews } = install("b-2");
  // Every plain block type of the schema takes the view; `doc` and `text` are no blocks of a
  // document.
  expect(Object.keys(nodeViews).sort()).toEqual(["ask", "heading", "image", "paragraph"]);
  const construct = nodeViews.paragraph;
  if (construct === undefined) throw new Error("no paragraph node view was installed");

  // A block that is not the anchored one is drawn by its own spec and nothing else.
  const other = construct(paragraph("b-1", "Before"));
  expect(other.dom.nodeName).toBe("P");
  expect((other.dom as HTMLElement).getAttribute("data-block-id")).toBe("b-1");
  expect(hosts).toEqual([]);

  const anchored = construct(paragraph("b-2", "The anchored words"));
  const frame = anchored.dom as HTMLElement;
  expect(frame.dataset.dispatchThreadAnchor).toBe("b-2");
  // The block itself is still drawn by its own spec, with its id, and the slot follows it.
  const block = frame.firstElementChild;
  expect(block?.nodeName).toBe("P");
  expect(block?.getAttribute("data-block-id")).toBe("b-2");
  expect(anchored.contentDOM).toBe(block as HTMLElement);
  const slot = frame.lastElementChild as HTMLElement;
  expect(slot.dataset.dispatchThreadSlot).toBe("b-2");
  expect(slot.getAttribute("contenteditable")).toBe("false");
  expect(hosts).toEqual([{ blockId: "b-2", key: expect.any(Number), slot }]);

  // ProseMirror never reads the slot back as an edit, and the composer's keystrokes stay in it.
  const inSlot = document.createElement("textarea");
  slot.append(inSlot);
  expect(anchored.ignoreMutation?.({ target: inSlot, type: "childList" } as never)).toBe(true);
  expect(anchored.ignoreMutation?.({ target: block as Node, type: "childList" } as never)).toBe(
    false
  );
  expect(anchored.stopEvent?.({ target: inSlot } as never)).toBe(true);
  expect(anchored.stopEvent?.({ target: block as Node } as never)).toBe(false);

  // The block's own text changing keeps the slot, and the portal in it, mounted.
  expect(
    anchored.update?.(paragraph("b-2", "The anchored words, reworded"), [], null as never)
  ).toBe(true);
  // A changed id is another block, which ProseMirror redraws.
  expect(anchored.update?.(paragraph("b-3", "Elsewhere"), [], null as never)).toBe(false);

  anchored.destroy?.();
  expect(hosts.at(-1)).toBeUndefined();
});
