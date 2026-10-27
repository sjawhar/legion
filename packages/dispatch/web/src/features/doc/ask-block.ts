import { DOMSerializer, type Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { type EditorState, Plugin, PluginKey } from "@milkdown/kit/prose/state";
import type { EditorView, NodeView, ViewMutationRecord } from "@milkdown/kit/prose/view";

import type { BlockSchema, BlockTypeSchema } from "../../api/types";
import { pictureNodeView } from "./picture-view";
import {
  type AskBlockHost,
  askBlockFacts,
  askBlockShell,
  renderAskBlockShell,
  renderTypedBlock,
} from "./typed-block-drawing";

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

let nextHostKey = 0;

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
    const { dom, contentDOM } = DOMSerializer.renderSpec(document, renderAskBlockShell(node));
    this.dom = dom as HTMLElement;
    if (contentDOM === undefined) {
      throw new Error("the decision shell has no content hole");
    }
    this.contentDOM = contentDOM;
    this.header = slot(this.dom, "data-dispatch-ask-header");
    this.footer = slot(this.dom, "data-dispatch-ask-footer");
    this.applyShell();
    this.register();
  }

  private applyShell(): void {
    const facts = askBlockFacts(this.node);
    const shell = askBlockShell(facts);
    this.dom.className = shell.frameClass;
    this.dom.dataset.blockId = facts.blockId;
    this.dom.dataset.dispatchAskBlock = facts.blockId;
    for (const [name, value] of Object.entries(shell.stamps)) {
      this.dom.setAttribute(name, value);
    }
    if (shell.stamps["data-dispatch-ask-malformed"] === undefined) {
      this.dom.removeAttribute("data-dispatch-ask-malformed");
    }
    if (shell.foldsOptions) {
      // The card renders the options as rows; the source list shows while malformed, or while
      // the caret is inside the block (`askBlockEditingPlugin`).
      this.dom.dataset.dispatchAskOptions = "rows";
    } else {
      delete this.dom.dataset.dispatchAskOptions;
    }
    this.contentDOM.className = shell.contentClass;
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

/** The shell's own slot element marked with `attribute`: a child of the shell, never the slot of
 *  a decision nested in its content. */
function slot(dom: HTMLElement, attribute: string): HTMLElement {
  const element = dom.querySelector<HTMLElement>(`:scope > [${attribute}]`);
  if (element === null) {
    throw new Error(`the decision shell has no ${attribute} slot`);
  }
  return element;
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

/** Installs the document's typed blocks in the editor: `AskBlockView` for every `ask` node,
 * reporting the live set of hosts (in document order) whenever it changes,
 * `renderTypedBlock`'s node view for every other type in the block schema, `pictureNodeView` for
 * every `image` node (a picture whose source is a Dispatch reference draws from its version's
 * bytes route), while keeping the library's other node views, and `askBlockEditingPlugin`. Call
 * once per editor, right after it is created. */
export function installTypedBlocks(
  view: EditorView,
  blockSchema: BlockSchema,
  onHostsChange: (hosts: readonly AskBlockHost[]) => void
): void {
  const registry = new Map<number, AskBlockHost>();
  const publish = () => {
    onHostsChange(
      [...registry.values()].sort((left, right) =>
        left.header.compareDocumentPosition(right.header) & Node.DOCUMENT_POSITION_FOLLOWING
          ? -1
          : 1
      )
    );
  };
  view.setProps({
    nodeViews: {
      ...view.props.nodeViews,
      ...Object.fromEntries(
        blockSchema.types
          .filter((type) => type.name !== "ask")
          .map((type) => [
            type.name,
            (node: ProseMirrorNode) => typedBlockView(node, view.dom.ownerDocument, type),
          ])
      ),
      ask: (node) => new AskBlockView(node, view.dom.ownerDocument, registry, publish),
      image: pictureNodeView(view),
    },
    plugins: [...(view.props.plugins ?? []), askBlockEditingPlugin],
  });
}
