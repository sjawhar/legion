import type { Node as ProseMirrorNode } from "@milkdown/kit/prose/model";
import { type EditorState, Plugin, PluginKey } from "@milkdown/kit/prose/state";
import { Decoration, DecorationSet, type EditorView } from "@milkdown/kit/prose/view";
import { $prose } from "@milkdown/kit/utils";
import { blockIdOf } from "./editor/schema/block-ids";

const ACTIVE_MARK_CLASS = "dispatch-mark-active";
const ACTIVE_BLOCK_CLASS = "dispatch-block-active";
const PULSE_CLASS = "dispatch-mark-pulse";
const PULSE_DURATION_MS = 1200;

type HighlightTarget = "mark" | "block";

interface EditorHighlights {
  activeMarks: ReadonlySet<string>;
  activeBlocks: ReadonlySet<string>;
  pulsedMarks: ReadonlyMap<string, number>;
  pulsedBlocks: ReadonlyMap<string, number>;
}

type HighlightChange =
  | { kind: "active"; target: HighlightTarget; ids: ReadonlySet<string> }
  | { kind: "pulse"; target: HighlightTarget; id: string; token: number }
  | { kind: "pulse-end"; target: HighlightTarget; id: string; token: number };

const highlightsKey = new PluginKey<EditorHighlights>("dispatch-editor-highlights");
const noHighlights: EditorHighlights = {
  activeMarks: new Set(),
  activeBlocks: new Set(),
  pulsedMarks: new Map(),
  pulsedBlocks: new Map(),
};

function sameIds(left: ReadonlySet<string>, right: ReadonlySet<string>): boolean {
  if (left.size !== right.size) return false;
  for (const id of left) {
    if (!right.has(id)) return false;
  }
  return true;
}

function withPulse(
  pulses: ReadonlyMap<string, number>,
  id: string,
  token: number
): ReadonlyMap<string, number> {
  const next = new Map(pulses);
  next.set(id, token);
  return next;
}

function withoutPulse(
  pulses: ReadonlyMap<string, number>,
  id: string,
  token: number
): ReadonlyMap<string, number> {
  if (pulses.get(id) !== token) return pulses;
  const next = new Map(pulses);
  next.delete(id);
  return next;
}

function applyChange(value: EditorHighlights, change: HighlightChange): EditorHighlights {
  if (change.kind === "active") {
    return change.target === "mark"
      ? { ...value, activeMarks: change.ids }
      : { ...value, activeBlocks: change.ids };
  }
  if (change.kind === "pulse") {
    return change.target === "mark"
      ? { ...value, pulsedMarks: withPulse(value.pulsedMarks, change.id, change.token) }
      : { ...value, pulsedBlocks: withPulse(value.pulsedBlocks, change.id, change.token) };
  }
  return change.target === "mark"
    ? { ...value, pulsedMarks: withoutPulse(value.pulsedMarks, change.id, change.token) }
    : { ...value, pulsedBlocks: withoutPulse(value.pulsedBlocks, change.id, change.token) };
}

function hasHighlights(value: EditorHighlights): boolean {
  return (
    value.activeMarks.size > 0 ||
    value.activeBlocks.size > 0 ||
    value.pulsedMarks.size > 0 ||
    value.pulsedBlocks.size > 0
  );
}

function markClasses(node: ProseMirrorNode, value: EditorHighlights): string[] {
  let active = false;
  let pulsed = false;
  for (const mark of node.marks) {
    const id = mark.attrs.id;
    if (typeof id !== "string") continue;
    active ||= value.activeMarks.has(id);
    pulsed ||= value.pulsedMarks.has(id);
  }
  return [active ? ACTIVE_MARK_CLASS : "", pulsed ? PULSE_CLASS : ""].filter(Boolean);
}

function blockClasses(node: ProseMirrorNode, value: EditorHighlights): string[] {
  const id = blockIdOf(node);
  if (id === null) return [];
  return [
    value.activeBlocks.has(id) ? ACTIVE_BLOCK_CLASS : "",
    value.pulsedBlocks.has(id) ? PULSE_CLASS : "",
  ].filter(Boolean);
}

function highlightDecorations(state: EditorState): DecorationSet {
  const value = highlightsKey.getState(state) ?? noHighlights;
  if (!hasHighlights(value)) return DecorationSet.empty;
  const decorations: Decoration[] = [];
  state.doc.descendants((node, pos) => {
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
  return DecorationSet.create(state.doc, decorations);
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
          return change === undefined ? value : applyChange(value, change);
        },
      },
      props: {
        decorations: highlightDecorations,
      },
    })
);

export function setActiveHighlights(
  view: EditorView,
  target: HighlightTarget,
  ids: readonly string[]
): void {
  const next = new Set(ids);
  const value = highlightsKey.getState(view.state) ?? noHighlights;
  const current = target === "mark" ? value.activeMarks : value.activeBlocks;
  if (sameIds(current, next)) return;
  dispatchHighlightChange(view, { kind: "active", target, ids: next });
}

let nextPulseToken = 0;

export function pulseHighlight(view: EditorView, target: HighlightTarget, id: string): void {
  nextPulseToken += 1;
  const token = nextPulseToken;
  dispatchHighlightChange(view, { kind: "pulse", target, id, token });
  window.setTimeout(() => {
    if (!view.isDestroyed) dispatchHighlightChange(view, { kind: "pulse-end", target, id, token });
  }, PULSE_DURATION_MS);
}
