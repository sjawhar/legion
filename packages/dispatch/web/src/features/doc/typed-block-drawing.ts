import type { DOMOutputSpec, Node as ProseMirrorNode } from "prosemirror-model";

import type { AskOption, AskUrgency, BlockTypeSchema } from "../../api/types";
import {
  askBlockTint,
  askUrgencyAccent,
  borderDefault,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { URGENCY_LABELS } from "../inbox/ask-urgency";

/**
 * `blockId` is the block's own identity rather than one of its schema attributes, so it is the
 * one name this function knows. Everything else it hides comes from the block type's own
 * schema: a hand-written list is a copy of that flag that goes stale the moment a type gains a
 * server-owned attribute (LEGION-67).
 */
const BLOCK_IDENTITY_ATTRIBUTE = "blockId";

function attributeValue(value: unknown): string {
  return Array.isArray(value) ? JSON.stringify(value) : String(value);
}

/**
 * Draws a document's host-owned typed blocks other than `ask`, which `AskBlockView` owns: the
 * block's kind and the author's own attributes as a header the reader sees, above its content.
 * The header reads as a label rather than a dump: the block's name is an eyebrow, a `kind` is a
 * badge beside it, a `title` is the header's own text, and any other attribute the author wrote
 * follows as a quiet name/value pair. What the author did not write never appears - the block's
 * identity, and every attribute the type's schema marks `server`, which is where the parser's
 * complaint and a decision's recorded state live. `type` is that schema entry; without one
 * (a node whose type the schema no longer describes) only the identity is hidden. In the editor
 * it is the node view's drawing (`typedBlockView`), never the node's `toDOM`: HTML of the
 * document - a copy, a drag, or the editor's plain-text paste, which renders the markdown it
 * parsed through `toDOM` - carries only the block's section and content, which is all its parse
 * rule reads. In the read-only text engine (a comment, ask or message body) it is the schema's
 * `toDOM`, through the headless engine's `renderBlock` option; the parse rule still reads only
 * the content hole `[data-proof-block-content]`, so a paste of a body stays exact.
 */
export function renderTypedBlock(
  node: ProseMirrorNode,
  type?: BlockTypeSchema | undefined
): DOMOutputSpec {
  const shown = Object.entries(node.attrs).filter(
    ([name, value]) =>
      name !== BLOCK_IDENTITY_ATTRIBUTE &&
      type?.attributes[name]?.server !== true &&
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

/** What an `ask` node says about itself, read once per render from its attributes and content. */
export interface AskBlockFacts {
  readonly answerText: string;
  readonly answeredAt: string | undefined;
  readonly answeredBy: string;
  readonly blockId: string;
  /** The ask on another document this block is a copy of and that document's `dispatch://`
   *  address, which settlement names in `copied_from` and `copied_from_document`; the copy has no
   *  ask of its own and is answered on that document. */
  readonly copiedFrom: { readonly ask: string; readonly document: string } | undefined;
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
    copiedFrom:
      typeof node.attrs.copied_from === "string" &&
      node.attrs.copied_from !== "" &&
      typeof node.attrs.copied_from_document === "string" &&
      node.attrs.copied_from_document !== ""
        ? { ask: node.attrs.copied_from, document: node.attrs.copied_from_document }
        : undefined,
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

/** How a decision block's shell presents itself: the frame's classes (the card frame with its
 * urgency accent and tint), the content hole's classes, and the facts the shell stamps as data
 * attributes. Both the editor's `AskBlockView` and the text engine's `renderAskBlockShell` draw
 * from it, so the two shells cannot drift apart. */
export interface AskBlockShell {
  readonly frameClass: string;
  readonly contentClass: string;
  readonly urgency: AskUrgency;
  readonly state: string;
  readonly malformed: boolean;
}

export function askBlockShell(facts: AskBlockFacts): AskBlockShell {
  const malformed = facts.malformedReason !== undefined;
  return {
    contentClass: malformed
      ? `${textMutedOnSurfaceMuted} [&_li:has(>p:empty)]:hidden [&_li:has(>p>br:only-child)]:hidden`
      : textPrimaryOnSurface,
    frameClass: `proof-typed-block proof-typed-block-ask my-5 rounded-xl border border-l-4 px-4 pt-3 pb-4 shadow-sm ${borderDefault} ${askUrgencyAccent[facts.urgency]} ${askBlockTint[facts.urgency]}`,
    malformed,
    state: facts.state,
    urgency: facts.urgency,
  };
}

/**
 * The decision block's shell as a static spec, for the read-only text engine (a comment, ask or
 * message body): the same frame, `not-prose` header and footer slots, and content hole as
 * `AskBlockView`, without the node view. Three of the node view's attributes are absent on
 * purpose. `data-dispatch-ask-block` marks a live decision under the editor root, and a body
 * rendered inside a document's decision footer (a reply, an answer note) would otherwise add a
 * second decision id there. `data-block-id` is the schema's own wrapper's to add. And
 * `data-dispatch-ask-options` folds the source option list, so it is set only where the body
 * mounts a card whose rows render the options; without it the list stays visible.
 */
export function renderAskBlockShell(node: ProseMirrorNode): DOMOutputSpec {
  const shell = askBlockShell(askBlockFacts(node));
  return [
    "section",
    {
      class: shell.frameClass,
      "data-proof-block-type": "ask",
      "data-dispatch-ask-urgency": shell.urgency,
      "data-dispatch-ask-state": shell.state,
      ...(shell.malformed ? { "data-dispatch-ask-malformed": "true" } : {}),
    },
    ["div", { class: "not-prose", contenteditable: "false", "data-dispatch-ask-header": "" }],
    ["div", { class: shell.contentClass, "data-proof-block-content": "" }, 0],
    ["div", { class: "not-prose", contenteditable: "false", "data-dispatch-ask-footer": "" }],
  ];
}
