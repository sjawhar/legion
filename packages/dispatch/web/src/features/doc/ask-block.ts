import {
  type DOMOutputSpec,
  DOMSerializer,
  type Node as ProseMirrorNode,
} from "@milkdown/kit/prose/model";
import { type EditorState, Plugin, PluginKey } from "@milkdown/kit/prose/state";
import type { EditorView, NodeView, ViewMutationRecord } from "@milkdown/kit/prose/view";

import type { AskOption, AskUrgency, BlockSchema } from "../../api/types";
import {
  askBlockTint,
  askUrgencyAccent,
  borderDefault,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { URGENCY_LABELS } from "../inbox/ask-urgency";

/**
 * Attributes the document's author never wrote and never reads: the block's own identity, the
 * parser's complaint, and the server-owned state a decision carries. Printing them turned a
 * typed block's header into a list of internals - `callout / kind / title / blockId` down the
 * page, block id and all (LEGION-67).
 */
const INTERNAL_BLOCK_ATTRIBUTES: Record<string, true> = {
  answer: true,
  answered_at: true,
  answered_by: true,
  blockId: true,
  invalid: true,
  selected: true,
  state: true,
};

function attributeValue(value: unknown): string {
  return Array.isArray(value) ? JSON.stringify(value) : String(value);
}

/**
 * Draws a document's host-owned typed blocks other than `ask`, which `AskBlockView` owns: the
 * block's kind and the author's own attributes as a header the reader sees, above its content.
 * The header reads as a label rather than a dump: the block's name is an eyebrow, a `kind` is a
 * badge beside it, a `title` is the header's own text, and any other attribute the author wrote
 * follows as a quiet name/value pair. Nothing the author did not write appears at all. It is
 * the node view's drawing (`typedBlockView`), never the node's `toDOM`: HTML of the document -
 * a copy, a drag, or the editor's plain-text paste, which renders the markdown it parsed
 * through `toDOM` - carries only the block's section and content, which is all its parse rule
 * reads.
 */
export function renderTypedBlock(node: ProseMirrorNode): DOMOutputSpec {
  const shown = Object.entries(node.attrs).filter(
    ([name, value]) =>
      INTERNAL_BLOCK_ATTRIBUTES[name] !== true &&
      value !== null &&
      value !== undefined &&
      value !== ""
  );
  const title = shown.find(([name]) => name === "title");
  const kind = shown.find(([name]) => name === "kind");
  const rest = shown.filter(([name]) => name !== "title" && name !== "kind");
  const badge: DOMOutputSpec[] =
    kind === undefined
      ? []
      : [
          [
            "span",
            { "data-proof-block-attribute": "kind", "data-proof-block-badge": "" },
            attributeValue(kind[1]),
          ],
        ];
  const heading: DOMOutputSpec[] =
    title === undefined
      ? []
      : [
          [
            "span",
            { "data-proof-block-attribute": "title", "data-proof-block-title": "" },
            attributeValue(title[1]),
          ],
        ];
  const others: DOMOutputSpec[] =
    rest.length === 0
      ? []
      : [
          [
            "dl",
            { "data-proof-block-attributes": "" },
            ...rest.flatMap(([name, value]) => [
              ["dt", {}, name],
              ["dd", { "data-proof-block-attribute": name }, attributeValue(value)],
            ]),
          ],
        ];
  return [
    "section",
    {
      class: `proof-typed-block proof-typed-block-${node.type.name}`,
      "data-proof-block-type": node.type.name,
    },
    [
      "header",
      { contenteditable: "false", "data-proof-block-summary": "" },
      ["span", { "data-proof-block-name": "" }, node.type.name],
      ...badge,
      ...heading,
      ...others,
    ],
    ["div", { "data-proof-block-content": "" }, 0],
  ];
}

/** The node view of a host-owned typed block other than `ask`: `renderTypedBlock`'s drawing,
 * under the block's id. It stamps `data-block-id` the way `AskBlockView` does, from the node's
 * attribute, since a value import from the editor library would pull the library out of
 * `editor.ts`'s lazy chunk into every page that loads this module. */
function typedBlockView(node: ProseMirrorNode, document: Document): NodeView {
  const { dom, contentDOM } = DOMSerializer.renderSpec(document, renderTypedBlock(node));
  const blockId = node.attrs.blockId;
  if (typeof blockId === "string" && blockId !== "") {
    (dom as HTMLElement).dataset.blockId = blockId;
  }
  return { contentDOM, dom };
}

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
    // The slots hold the app's own card, not document text: `not-prose` keeps the editor
    // root's Typography (`prose`, from the library theme) off everything React renders into
    // them, in every state of the card.
    this.header = document.createElement("div");
    this.header.className = "not-prose";
    this.header.contentEditable = "false";
    this.header.dataset.dispatchAskHeader = "";
    this.contentDOM = document.createElement("div");
    this.contentDOM.dataset.proofBlockContent = "";
    this.footer = document.createElement("div");
    this.footer.className = "not-prose";
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

/** Installs the document's typed blocks in the editor: `AskBlockView` for every `ask` node,
 * reporting the live set of hosts (in document order) whenever it changes,
 * `renderTypedBlock`'s node view for every other type in the block schema while keeping the
 * library's other node views, and `askBlockEditingPlugin`. Call once per editor, right after it
 * is created. */
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
            (node: ProseMirrorNode) => typedBlockView(node, view.dom.ownerDocument),
          ])
      ),
      ask: (node) => new AskBlockView(node, view.dom.ownerDocument, registry, publish),
    },
    plugins: [...(view.props.plugins ?? []), askBlockEditingPlugin],
  });
}
