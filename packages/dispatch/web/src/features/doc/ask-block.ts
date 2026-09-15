import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { type EditorState, Plugin, PluginKey } from "@milkdown/kit/prose/state";
import type { EditorView, NodeView, ViewMutationRecord } from "@milkdown/kit/prose/view";
import type { HostBlockRenderer } from "@sjawhar/proof-editor";

import type { AskOption, AskUrgency } from "../../api/types";
import {
  askBlockTint,
  askUrgencyAccent,
  borderDefault,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { URGENCY_LABELS } from "../inbox/ask-urgency";

/** Renders a document's host-owned typed blocks other than `ask`, which `AskBlockView` owns. */
export const renderTypedBlock: HostBlockRenderer = (node) => [
  "section",
  {
    class: `proof-typed-block proof-typed-block-${node.type.name}`,
    "data-proof-block-type": node.type.name,
  },
  [
    "header",
    { "data-proof-block-summary": "" },
    ["span", { "data-proof-block-name": "" }, node.type.name],
    [
      "dl",
      { "data-proof-block-attributes": "" },
      ...Object.entries(node.attrs).flatMap(([name, value]) => [
        ["dt", {}, name],
        [
          "dd",
          { "data-proof-block-attribute": name },
          Array.isArray(value) ? JSON.stringify(value) : String(value),
        ],
      ]),
    ],
  ],
  ["div", { "data-proof-block-content": "" }, 0],
];

/** What an `ask` node says about itself, read once per render from its attributes and content. */
export interface AskBlockFacts {
  readonly answerText: string;
  readonly answeredAt: string | undefined;
  readonly answeredBy: string;
  readonly blockId: string;
  /** Why the block cannot be answered as written, or undefined when it parses. */
  readonly malformedReason: string | undefined;
  readonly multiple: boolean;
  readonly options: AskOption[];
  readonly question: string;
  readonly selected: string[];
  readonly state: string;
  readonly urgency: AskUrgency;
}

export function askBlockFacts(node: ProseMirrorNode): AskBlockFacts {
  const options: AskOption[] = [];
  let blankOption: number | undefined;
  if (node.lastChild?.type.name === "bullet_list") {
    node.lastChild.forEach((item, _offset, index) => {
      const text = item.textContent;
      const separator = text.indexOf(": ");
      const label = (separator < 0 ? text : text.slice(0, separator)).trim();
      const description = separator < 0 ? "" : text.slice(separator + 2).trim();
      if (label === "") {
        blankOption ??= index + 1;
        return;
      }
      options.push(description === "" ? { label } : { description, label });
    });
  }
  const invalid =
    typeof node.attrs.invalid === "string" && node.attrs.invalid.trim() !== ""
      ? node.attrs.invalid.trim()
      : undefined;
  const question =
    node.firstChild?.type.name === "bullet_list" ? "" : (node.firstChild?.textContent.trim() ?? "");
  return {
    answerText: typeof node.attrs.answer === "string" ? node.attrs.answer.trim() : "",
    answeredAt: typeof node.attrs.answered_at === "string" ? node.attrs.answered_at : undefined,
    answeredBy: String(node.attrs.answered_by ?? ""),
    blockId: String(node.attrs.blockId),
    malformedReason:
      invalid ??
      (question === ""
        ? "The question is empty"
        : blankOption === undefined
          ? undefined
          : `Option ${blankOption} has no label`),
    multiple: node.attrs.multiple === true,
    options,
    question,
    selected: Array.isArray(node.attrs.selected)
      ? node.attrs.selected.filter((value): value is string => typeof value === "string")
      : [],
    state: String(node.attrs.state),
    urgency: node.attrs.urgency in URGENCY_LABELS ? (node.attrs.urgency as AskUrgency) : "med",
  };
}

/** A mounted decision block: the React card portals into its two slots. A new object is issued
 * on every node change so a host list in React state re-renders. */
export interface AskBlockHost {
  /** Stable per mounted view, for React keys. */
  readonly key: number;
  readonly node: ProseMirrorNode;
  readonly header: HTMLElement;
  readonly footer: HTMLElement;
}

let nextHostKey = 0;

/** The decision block's ProseMirror node view. It owns only the shell — the card frame with its
 * urgency accent and tint, a header slot, the content hole, and a footer slot — and hands the
 * slots to React through the host registry; `AskBlockCard` renders everything a reader sees in
 * them. Two things make a plain `toDOM` spec unusable here and justify the node view: ProseMirror
 * treats DOM changes inside a spec-rendered node as a suspect edit and redraws the node from its
 * state (wiping a portal and the reader's half-made choice), so `ignoreMutation` excludes the
 * slots; and the slots are `contenteditable="false"` with `stopEvent` keeping keystrokes in the
 * answer controls away from the editor's keymaps. */
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
    this.dom = document.createElement("section");
    this.dom.dataset.proofBlockType = "ask";
    this.header = document.createElement("div");
    this.header.contentEditable = "false";
    this.header.dataset.dispatchAskHeader = "";
    this.contentDOM = document.createElement("div");
    this.contentDOM.dataset.proofBlockContent = "";
    this.footer = document.createElement("div");
    this.footer.contentEditable = "false";
    this.footer.dataset.dispatchAskFooter = "";
    this.dom.append(this.header, this.contentDOM, this.footer);
    this.applyShell();
    this.register();
  }

  private applyShell(): void {
    const facts = askBlockFacts(this.node);
    const malformed = facts.malformedReason !== undefined;
    this.dom.className = `proof-typed-block proof-typed-block-ask my-5 rounded-xl border border-l-4 px-4 pt-3 pb-4 shadow-sm ${borderDefault} ${askUrgencyAccent[facts.urgency]} ${askBlockTint[facts.urgency]}`;
    this.dom.dataset.blockId = facts.blockId;
    this.dom.dataset.dispatchAskBlock = facts.blockId;
    this.dom.dataset.dispatchAskUrgency = facts.urgency;
    this.dom.dataset.dispatchAskState = facts.state;
    if (malformed) {
      this.dom.dataset.dispatchAskMalformed = "true";
      delete this.dom.dataset.dispatchAskOptions;
    } else {
      delete this.dom.dataset.dispatchAskMalformed;
      // The card renders the options as rows; the source list shows while malformed, or while
      // the caret is inside the block (`askBlockEditingPlugin`).
      this.dom.dataset.dispatchAskOptions = "rows";
    }
    this.contentDOM.className = malformed
      ? `${textMutedOnSurfaceMuted} [&_li:has(>p:empty)]:hidden [&_li:has(>p>br:only-child)]:hidden`
      : textPrimaryOnSurface;
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

/** Mounts `AskBlockView` for every `ask` node in the editor, keeping the library's other node
 * views, adds `askBlockEditingPlugin`, and reports the live set of hosts (in document order)
 * whenever it changes. Call once per editor, right after it is created. */
export function installAskBlockView(
  view: EditorView,
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
      ask: (node) => new AskBlockView(node, view.dom.ownerDocument, registry, publish),
    },
    plugins: [...(view.props.plugins ?? []), askBlockEditingPlugin],
  });
}
