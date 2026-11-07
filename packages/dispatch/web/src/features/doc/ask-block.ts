import { DOMSerializer, type Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { type EditorState, Plugin, PluginKey } from "@milkdown/kit/prose/state";
import type {
  EditorView,
  NodeView,
  NodeViewConstructor,
  ViewMutationRecord,
} from "@milkdown/kit/prose/view";

import type { BlockSchema, BlockTypeSchema } from "../../api/types";
import type { ViewBlockSlot } from "../view/view-slots";
import { pictureNodeView, type WatchPicture } from "./picture-view";
import {
  type AskBlockHost,
  applyAskBlockShell,
  askShellSlots,
  renderAskBlockShell,
  renderTypedBlock,
  TYPED_BLOCK_SHELLS,
} from "./typed-block-drawing";
import { viewBlockView } from "./view-block";

/** The node view of a host-owned typed block other than `ask`: `renderTypedBlock`'s drawing,
 * under the block's id. It stamps `data-block-id` the way `AskBlockView` does, from the node's
 * attribute, since a value import from the editor library would pull the library out of
 * `editor.ts`'s lazy chunk into every page that loads this module. */
function typedBlockView(
  node: ProseMirrorNode,
  document: Document,
  type: BlockTypeSchema
): NodeView {
  const { dom, contentDOM } = DOMSerializer.renderSpec(document, renderTypedBlock(node, type));
  const blockId = node.attrs.blockId;
  if (typeof blockId === "string" && blockId !== "") {
    (dom as HTMLElement).dataset.blockId = blockId;
  }
  return { contentDOM, dom };
}

/** The next key a mounted node view's host is published under, for React keys. */
let nextHostKey = 0;

/**
 * A thread's host slot under the block it is anchored to: the element a React portal fills, and
 * which ProseMirror never reads back (`ignoreMutation`).
 */
export interface AnchoredBlockHost {
  readonly blockId: string;
  /** Stable per mounted view in the editor, for React keys. */
  readonly key: number;
  readonly slot: HTMLElement;
}

/** Which block hosts a thread, and who to tell when its slot comes and goes. */
export interface AnchoredBlock {
  readonly blockId: string;
  /** The live slot, or `undefined` once no view holds one: the block left the document, or the
   *  editor was torn down. */
  readonly onHostChange: (host: AnchoredBlockHost | undefined) => void;
}

/** Draws a node exactly as ProseMirror's own default view would: its schema's `toDOM`, which for
 * every block of the Proof schema is the one that stamps `data-block-id`
 * (`withBlockIdSpec` in `@legion/proof-editor`). Reading the spec rather than writing a drawing of
 * our own is what keeps a plain paragraph or heading drawn the way the document draws it. */
function renderBlockSpec(
  node: ProseMirrorNode,
  document: Document
): { dom: HTMLElement; contentDOM: HTMLElement | undefined } {
  const toDOM = node.type.spec.toDOM;
  if (toDOM === undefined) {
    throw new Error(`the ${node.type.name} block has no DOM spec of its own to draw`);
  }
  const { contentDOM, dom } = DOMSerializer.renderSpec(document, toDOM(node));
  return { contentDOM, dom: dom as HTMLElement };
}

/**
 * The node view of a plain block - a paragraph, a heading, a quote - which has none today. It
 * draws the block by its own schema spec and, for the one block a thread is anchored to, wraps
 * that drawing beside a slot a React portal fills, so the thread is drawn inside the document
 * column directly below its block without a line of DOM being injected into the editor's own
 * tree. `AskBlockView` above says why a slot needs a node view: ProseMirror reads a DOM change
 * inside a spec-rendered node as a suspect edit and redraws the node from its state, which would
 * wipe the portal, so `ignoreMutation` excludes the slot and `stopEvent` keeps the composer's
 * keystrokes out of the editor's keymaps.
 *
 * Every other block of the type draws its spec and nothing else, which is what the default view
 * does: one node view per type is installed, and only the anchored block takes a slot.
 */
class AnchoredBlockView implements NodeView {
  readonly dom: HTMLElement;
  readonly contentDOM: HTMLElement | undefined;
  private readonly key = nextHostKey++;
  private readonly slot: HTMLElement | undefined;
  private node: ProseMirrorNode;

  constructor(
    node: ProseMirrorNode,
    document: Document,
    blockId: string,
    private readonly registry: Map<number, AnchoredBlockHost>,
    private readonly publish: () => void
  ) {
    this.node = node;
    const drawn = renderBlockSpec(node, document);
    this.contentDOM = drawn.contentDOM;
    if (node.attrs.blockId !== blockId) {
      this.dom = drawn.dom;
      return;
    }
    const slot = document.createElement("div");
    // `not-prose` keeps the editor's Typography off everything React renders into the slot, as
    // the decision shell's own slots do.
    slot.className = "not-prose";
    slot.contentEditable = "false";
    slot.dataset.dispatchThreadSlot = blockId;
    const frame = document.createElement("div");
    frame.dataset.dispatchThreadAnchor = blockId;
    frame.append(drawn.dom, slot);
    this.dom = frame;
    this.slot = slot;
    this.registry.set(this.key, { blockId, key: this.key, slot });
    this.publish();
  }

  update(node: ProseMirrorNode): boolean {
    // The slot stays mounted, and the portal in it with it, while the block's own text changes;
    // a changed id, type or attribute is another block, which ProseMirror redraws.
    if (!node.sameMarkup(this.node)) {
      return false;
    }
    this.node = node;
    return true;
  }

  ignoreMutation(mutation: ViewMutationRecord): boolean {
    if (this.slot?.contains(mutation.target) === true) {
      return true;
    }
    // ProseMirror's own rule for everything else (`NodeViewDesc.ignoreMutation`).
    return this.contentDOM === undefined && mutation.type !== "selection";
  }

  stopEvent(event: Event): boolean {
    return (
      this.slot !== undefined && event.target instanceof Node && this.slot.contains(event.target)
    );
  }

  destroy(): void {
    if (this.slot === undefined) {
      return;
    }
    this.registry.delete(this.key);
    this.publish();
  }
}

/** The decision block's ProseMirror node view. It owns only the shell — `renderAskBlockShell`'s
 * drawing, the same spec the text engine draws: the card frame with its urgency accent and tint,
 * a header slot, the content hole, and a footer slot — and hands the slots to React through the
 * host registry; `AskBlockCard` renders everything a reader sees in them. Two things make a plain
 * `toDOM` spec unusable here and justify the node view: ProseMirror treats DOM changes inside a
 * spec-rendered node as a suspect edit and redraws the node from its state (wiping a portal and
 * the reader's half-made choice), so `ignoreMutation` excludes the slots; and the slots are
 * `contenteditable="false"` with `stopEvent` keeping keystrokes in the answer controls away from
 * the editor's keymaps. */
class AskBlockView implements NodeView {
  readonly dom: HTMLElement;
  readonly contentDOM: HTMLElement;
  private readonly header: HTMLElement;
  private readonly footer: HTMLElement;
  private readonly key = nextHostKey++;
  private node: ProseMirrorNode;

  constructor(
    node: ProseMirrorNode,
    document: Document,
    private readonly registry: Map<number, AskBlockHost>,
    private readonly publish: () => void
  ) {
    this.node = node;
    const { dom } = DOMSerializer.renderSpec(document, renderAskBlockShell(node));
    this.dom = dom as HTMLElement;
    const slots = askShellSlots(this.dom);
    if (slots === undefined) {
      throw new Error("the decision shell has no header, content or footer slot");
    }
    this.contentDOM = slots.content;
    this.header = slots.header;
    this.footer = slots.footer;
    this.applyShell();
    this.register();
  }

  private applyShell(): void {
    // The block's identity is the editor's alone (`renderAskBlockShell` says why).
    const blockId = String(this.node.attrs.blockId);
    this.dom.dataset.blockId = blockId;
    this.dom.dataset.dispatchAskBlock = blockId;
    applyAskBlockShell(this.dom, this.contentDOM, this.node);
  }

  private register(): void {
    this.registry.set(this.key, {
      footer: this.footer,
      header: this.header,
      key: this.key,
      node: this.node,
    });
    this.publish();
  }

  update(node: ProseMirrorNode): boolean {
    if (node.type !== this.node.type) {
      return false;
    }
    // The content hole stays mounted; only the shell and the card re-render.
    this.node = node;
    this.applyShell();
    this.register();
    return true;
  }

  ignoreMutation(mutation: ViewMutationRecord): boolean {
    return !this.contentDOM.contains(mutation.target);
  }

  stopEvent(event: Event): boolean {
    return event.target instanceof Node && !this.contentDOM.contains(event.target);
  }

  destroy(): void {
    this.registry.delete(this.key);
    this.publish();
  }
}

/** The document position of the `ask` node the selection head sits inside, or undefined when
 * the caret is outside every decision block. */
export function editingAskBlockPos(state: EditorState): number | undefined {
  const $from = state.selection.$from;
  for (let depth = $from.depth; depth > 0; depth -= 1) {
    if ($from.node(depth).type.name === "ask") {
      return $from.before(depth);
    }
  }
  return undefined;
}

/** Marks the decision block the caret is inside with `data-dispatch-ask-editing="true"` and
 * clears it from every other, on each state update. The rows in the card render a block's options
 * for everyone; the source `- Option: description` list is the only place a human edits them, so
 * `styles.css` shows it only on the marked block — click into the question and the source appears
 * under it; click anywhere else in the document and it folds away. */
export const askBlockEditingPlugin = new Plugin({
  key: new PluginKey("dispatch-ask-block-editing"),
  view: (view) => {
    let marked: HTMLElement | undefined;
    const mark = (current: EditorView): void => {
      const pos = editingAskBlockPos(current.state);
      const target = pos === undefined ? null : current.nodeDOM(pos);
      const next = target instanceof HTMLElement ? target : undefined;
      if (marked !== undefined && marked !== next) {
        delete marked.dataset.dispatchAskEditing;
      }
      if (next !== undefined) {
        next.dataset.dispatchAskEditing = "true";
      }
      marked = next;
    };
    mark(view);
    return {
      destroy: () => {
        if (marked !== undefined) {
          delete marked.dataset.dispatchAskEditing;
        }
      },
      update: mark,
    };
  },
});

/** The blocks of one editor that React draws into: the decision cards, in document order, and
 * the frame slots a `ViewBlock` is portalled into. They are one editor's own, never another's:
 * a document page holds the live editor and a version view at once, and each draws its own. */
export interface TypedBlockHosts {
  readonly asks: readonly AskBlockHost[];
  readonly views: readonly ViewBlockSlot[];
}

/** The blocks of an editor that has reported none yet. */
export const NO_TYPED_BLOCKS: TypedBlockHosts = { asks: [], views: [] };

/** Installs the document's typed blocks in the editor: `AskBlockView` for every `ask` node,
 * `viewBlockView` for every `view` node (the sandboxed frame its page runs in, beside the
 * editable caption), reporting both live sets whenever either changes, the decision hosts in
 * document order; `renderTypedBlock`'s node view for every other type in the block schema,
 * `pictureNodeView` for every `image` node (a picture whose source is a Dispatch reference draws
 * from its version's bytes route, a version-less one from its latest version's, which
 * `watchPicture` reports), while keeping the library's other node views, and
 * `askBlockEditingPlugin`. Call once per editor, right after it is created.
 *
 * With `anchoredBlock`, every plain block type of the editor's schema that has no node view yet -
 * a paragraph, a heading, a quote, a list - takes `AnchoredBlockView`, which gives the one block
 * that `anchoredBlock` names a slot for a thread and draws every other block of those types by
 * its own spec, exactly as the default view does. Without it nothing changes: the document's own
 * page installs no such view at all. */
export function installTypedBlocks(
  view: EditorView,
  blockSchema: BlockSchema,
  onHostsChange: (hosts: TypedBlockHosts) => void,
  watchPicture: WatchPicture,
  anchoredBlock?: AnchoredBlock
): void {
  const asks = new Map<number, AskBlockHost>();
  const views = new Map<string, ViewBlockSlot>();
  const publish = () => {
    onHostsChange({
      asks: [...asks.values()].sort((left, right) =>
        left.header.compareDocumentPosition(right.header) & Node.DOCUMENT_POSITION_FOLLOWING
          ? -1
          : 1
      ),
      views: [...views.values()],
    });
  };
  const nodeViews: Record<string, NodeViewConstructor> = {
    ...view.props.nodeViews,
    // A type with a shell of its own (`TYPED_BLOCK_SHELLS`) has a node view of its own below;
    // every other type of the schema draws the header shell through `typedBlockView`.
    ...Object.fromEntries(
      blockSchema.types
        .filter((type) => TYPED_BLOCK_SHELLS[type.name] === undefined)
        .map((type) => [
          type.name,
          (node: ProseMirrorNode) => typedBlockView(node, view.dom.ownerDocument, type),
        ])
    ),
    ask: (node: ProseMirrorNode) => new AskBlockView(node, view.dom.ownerDocument, asks, publish),
    view: (node: ProseMirrorNode) => viewBlockView(node, view.dom.ownerDocument, views, publish),
    image: pictureNodeView(view, watchPicture),
  };
  if (anchoredBlock !== undefined) {
    const anchored = new Map<number, AnchoredBlockHost>();
    // One block of the document carries the slot, so the newest published host is the only live
    // one; a redraw that mounts the next view before destroying the last leaves it last either
    // way.
    const publishAnchored = () => anchoredBlock.onHostChange([...anchored.values()].at(-1));
    for (const type of Object.values(view.state.schema.nodes)) {
      if (!type.isBlock || type.name === "doc" || type.spec.toDOM === undefined) {
        continue;
      }
      if (type.name in nodeViews) {
        continue;
      }
      nodeViews[type.name] = (node: ProseMirrorNode) =>
        new AnchoredBlockView(
          node,
          view.dom.ownerDocument,
          anchoredBlock.blockId,
          anchored,
          publishAnchored
        );
    }
  }
  view.setProps({
    nodeViews,
    plugins: [...(view.props.plugins ?? []), askBlockEditingPlugin],
  });
}
