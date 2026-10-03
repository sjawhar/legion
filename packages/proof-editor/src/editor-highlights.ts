import type { Fragment, Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { type EditorState, Plugin, PluginKey, type Transaction } from "@milkdown/kit/prose/state";
import { ReplaceStep } from "@milkdown/kit/prose/transform";
import { Decoration, DecorationSet, type EditorView } from "@milkdown/kit/prose/view";
import { $prose } from "@milkdown/kit/utils";
import { blockIdOf } from "./editor/schema/block-ids";

type HighlightTarget = "mark" | "block";

const ACTIVE_CLASS: Readonly<Record<HighlightTarget, string>> = {
  mark: "dispatch-mark-active",
  block: "dispatch-block-active",
};
const PULSE_CLASS = "dispatch-mark-pulse";
const PULSE_DURATION_MS = 1200;

/** One target's highlighted ids: the selected ones, and the pulsing ones by the token of the pulse
 *  that started each, so the timer of a pulse a later pulse of the same id replaced ends nothing. */
interface TargetHighlights {
  active: ReadonlySet<string>;
  pulsed: ReadonlyMap<string, number>;
}

interface EditorHighlights {
  mark: TargetHighlights;
  block: TargetHighlights;
  /** The token of this editor's latest pulse; each pulse takes the next one. */
  lastPulse: number;
  decorations: DecorationSet;
}

type HighlightChange =
  | { kind: "active"; target: HighlightTarget; ids: ReadonlySet<string> }
  | { kind: "pulse"; target: HighlightTarget; id: string }
  | { kind: "pulse-end"; target: HighlightTarget; id: string; token: number };

const highlightsKey = new PluginKey<EditorHighlights>("dispatch-editor-highlights");
const noTargetHighlights: TargetHighlights = { active: new Set(), pulsed: new Map() };
const noHighlights: EditorHighlights = {
  mark: noTargetHighlights,
  block: noTargetHighlights,
  lastPulse: 0,
  decorations: DecorationSet.empty,
};

function sameIds(left: ReadonlySet<string>, right: ReadonlySet<string>): boolean {
  if (left.size !== right.size) return false;
  for (const id of left) {
    if (!right.has(id)) return false;
  }
  return true;
}

function withTarget(
  value: EditorHighlights,
  target: HighlightTarget,
  highlights: TargetHighlights
): EditorHighlights {
  return { ...value, [target]: highlights };
}

function applyChange(value: EditorHighlights, change: HighlightChange): EditorHighlights {
  const current = value[change.target];
  switch (change.kind) {
    case "active":
      if (sameIds(current.active, change.ids)) return value;
      return withTarget(value, change.target, { ...current, active: change.ids });
    case "pulse": {
      const token = value.lastPulse + 1;
      const pulsed = new Map(current.pulsed).set(change.id, token);
      return { ...withTarget(value, change.target, { ...current, pulsed }), lastPulse: token };
    }
    case "pulse-end": {
      if (current.pulsed.get(change.id) !== change.token) return value;
      const pulsed = new Map(current.pulsed);
      pulsed.delete(change.id);
      return withTarget(value, change.target, { ...current, pulsed });
    }
  }
}

function hasHighlights(value: EditorHighlights): boolean {
  return [value.mark, value.block].some(
    (highlights) => highlights.active.size > 0 || highlights.pulsed.size > 0
  );
}

/** The classes a node carrying `ids` of `target` draws: active when one of them is selected, and
 *  pulsing when one of them is pulsing. */
function targetClasses(
  value: EditorHighlights,
  target: HighlightTarget,
  ids: readonly string[]
): string[] {
  const { active, pulsed } = value[target];
  const classes: string[] = [];
  if (ids.some((id) => active.has(id))) classes.push(ACTIVE_CLASS[target]);
  if (ids.some((id) => pulsed.has(id))) classes.push(PULSE_CLASS);
  return classes;
}

function markClasses(node: ProseMirrorNode, value: EditorHighlights): string[] {
  const ids: string[] = [];
  for (const mark of node.marks) {
    if (typeof mark.attrs.id === "string") ids.push(mark.attrs.id);
  }
  return targetClasses(value, "mark", ids);
}

function blockClasses(node: ProseMirrorNode, value: EditorHighlights): string[] {
  const id = blockIdOf(node);
  return id === null ? [] : targetClasses(value, "block", [id]);
}

function highlightDecorations(doc: ProseMirrorNode, value: EditorHighlights): DecorationSet {
  if (!hasHighlights(value)) return DecorationSet.empty;
  const decorations: Decoration[] = [];
  doc.descendants((node, pos) => {
    const blockClass = blockClasses(node, value).join(" ");
    if (blockClass !== "") {
      decorations.push(Decoration.node(pos, pos + node.nodeSize, { class: blockClass }));
    }
    if (!node.isInline) return true;
    const markClass = markClasses(node, value).join(" ");
    if (markClass !== "") {
      decorations.push(Decoration.inline(pos, pos + node.nodeSize, { class: markClass }));
    }
    return false;
  });
  return DecorationSet.create(doc, decorations);
}

/** Whether any node in `content` carries a highlighted block or mark id. */
function holdsHighlightedTarget(content: Fragment, value: EditorHighlights): boolean {
  let found = false;
  content.descendants((node) => {
    found ||=
      blockClasses(node, value).length > 0 ||
      (node.isInline && markClasses(node, value).length > 0);
    return !found;
  });
  return found;
}

/** Edits inside one node move decorations, and mapping carries them along. An edit that can change
 *  which nodes carry a highlighted id, or where a highlighted block ends, rebuilds: a mark or
 *  attribute step, inserted content that holds a highlighted target, and, while a block is
 *  highlighted, a replace whose range starts and ends in different nodes. That replace deletes the
 *  boundary between them (a join, or a deletion across a block's end), and mapping drops a node
 *  decoration whose closing token it deleted, though the block and its id remain. */
function mapsHighlights(transaction: Transaction, value: EditorHighlights): boolean {
  if (!hasHighlights(value)) return true;
  const blockHighlighted = value.block.active.size > 0 || value.block.pulsed.size > 0;
  return transaction.steps.every((step, index) => {
    if (!(step instanceof ReplaceStep) || holdsHighlightedTarget(step.slice.content, value)) {
      return false;
    }
    const before = transaction.docs[index];
    return !blockHighlighted || before.resolve(step.from).sameParent(before.resolve(step.to));
  });
}

function dispatchHighlightChange(view: EditorView, change: HighlightChange): void {
  view.dispatch(view.state.tr.setMeta(highlightsKey, change).setMeta("addToHistory", false));
}

/**
 * Presentation-only highlights owned by ProseMirror: the selected margin item and the brief pulse
 * after focusing one. A host that writes classes into mark or block DOM makes ProseMirror read that
 * DOM back as a document edit, so these classes are decorations computed from editor state.
 */
export const editorHighlightsPlugin = $prose(
  () =>
    new Plugin<EditorHighlights>({
      key: highlightsKey,
      state: {
        init: () => noHighlights,
        apply(transaction, value) {
          const change = transaction.getMeta(highlightsKey) as HighlightChange | undefined;
          if (change === undefined) {
            if (!transaction.docChanged) return value;
            return mapsHighlights(transaction, value)
              ? {
                  ...value,
                  decorations: value.decorations.map(transaction.mapping, transaction.doc),
                }
              : { ...value, decorations: highlightDecorations(transaction.doc, value) };
          }
          const next = applyChange(value, change);
          return next === value
            ? value
            : { ...next, decorations: highlightDecorations(transaction.doc, next) };
        },
      },
      props: {
        decorations: (state: EditorState) =>
          (highlightsKey.getState(state) ?? noHighlights).decorations,
      },
    })
);

export function setActiveHighlights(
  view: EditorView,
  target: HighlightTarget,
  ids: readonly string[]
): void {
  const next = new Set(ids);
  const current = (highlightsKey.getState(view.state) ?? noHighlights)[target].active;
  if (sameIds(current, next)) return;
  dispatchHighlightChange(view, { kind: "active", target, ids: next });
}

export function pulseHighlight(view: EditorView, target: HighlightTarget, id: string): void {
  dispatchHighlightChange(view, { kind: "pulse", target, id });
  const token = (highlightsKey.getState(view.state) ?? noHighlights).lastPulse;
  window.setTimeout(() => {
    if (!view.isDestroyed) dispatchHighlightChange(view, { kind: "pulse-end", target, id, token });
  }, PULSE_DURATION_MS);
}
