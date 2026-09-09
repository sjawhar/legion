import { type Extension, type Range, StateEffect, StateField } from "@codemirror/state";
import { Decoration, type DecorationSet, EditorView } from "@codemirror/view";

export interface MappedAnchor {
  from: number;
  orphaned: boolean;
  quote: string;
  to: number;
}

export interface AnchorDecoration {
  anchor: MappedAnchor;
  id: string;
  selected: boolean;
}

export interface AnchorInteractionHandlers {
  onHover: (id: string | undefined) => void;
  onSelect: (id: string) => void;
}

export const setActiveAnchorIds = StateEffect.define<readonly string[]>();
export const setAnchorDecorations = StateEffect.define<AnchorDecoration[]>();

function nearestQuote(text: string, quote: string, preferredFrom: number): number {
  let nearest = -1;
  let distance = Number.POSITIVE_INFINITY;
  let from = text.indexOf(quote);
  while (from !== -1) {
    const nextDistance = Math.abs(from - preferredFrom);
    if (nextDistance < distance) {
      nearest = from;
      distance = nextDistance;
    }
    from = text.indexOf(quote, from + Math.max(quote.length, 1));
  }
  return nearest;
}

/**
 * Resolves an anchor after a CodeMirror/Yjs transaction. Mapped positions
 * preserve an exact quote; a changed range falls back to the closest quote.
 */
export function resolveMappedAnchor(
  anchor: MappedAnchor,
  text: string,
  mapPosition: (position: number, assoc: -1 | 1) => number
): MappedAnchor {
  const from = mapPosition(anchor.from, -1);
  const to = mapPosition(anchor.to, 1);
  if (text.slice(from, to) === anchor.quote) {
    return { ...anchor, from, orphaned: false, to };
  }

  const replacementFrom = nearestQuote(text, anchor.quote, from);
  if (replacementFrom === -1) {
    return { ...anchor, orphaned: true };
  }
  return {
    ...anchor,
    from: replacementFrom,
    orphaned: false,
    to: replacementFrom + anchor.quote.length,
  };
}

function rangesFor(anchors: AnchorDecoration[]): Range<Decoration>[] {
  return anchors.flatMap(({ anchor, id, selected }) => {
    if (anchor.orphaned) {
      return [];
    }
    return [
      Decoration.mark({
        attributes: { "data-dispatch-anchor-id": id },
        class: selected ? "dispatch-anchor dispatch-anchor-active" : "dispatch-anchor",
        spec: { anchor: { anchor, id, selected } satisfies AnchorDecoration },
      }).range(anchor.from, anchor.to),
    ];
  });
}

function decorationsFor(anchors: AnchorDecoration[]): DecorationSet {
  return Decoration.set(rangesFor(anchors), true);
}

const anchorDecorations = StateField.define<DecorationSet>({
  create: () => Decoration.none,
  update(current, transaction) {
    for (const effect of transaction.effects) {
      if (effect.is(setAnchorDecorations)) {
        return decorationsFor(effect.value);
      }
      if (effect.is(setActiveAnchorIds)) {
        const activeIds = new Set(effect.value);
        const anchors: AnchorDecoration[] = [];
        current.between(0, transaction.startState.doc.length, (from, to, decoration) => {
          const source = decoration.spec.anchor as AnchorDecoration | undefined;
          if (source !== undefined) {
            anchors.push({
              ...source,
              anchor: { ...source.anchor, from, to },
              selected: activeIds.has(source.id),
            });
          }
        });
        return decorationsFor(anchors);
      }
    }
    const mapped = current.map(transaction.changes);
    if (!transaction.docChanged) {
      return mapped;
    }

    const anchors: AnchorDecoration[] = [];
    mapped.between(0, transaction.state.doc.length, (from, to, decoration) => {
      const source = decoration.spec.anchor as AnchorDecoration | undefined;
      if (source === undefined) {
        return;
      }
      const anchor = resolveMappedAnchor(
        { ...source.anchor, from, to },
        transaction.state.doc.toString(),
        (position) => position
      );
      anchors.push({ ...source, anchor });
    });
    return decorationsFor(anchors);
  },
  provide: (field) => EditorView.decorations.from(field),
});

function anchorIdAt(event: MouseEvent): string | undefined {
  const element = event.target instanceof Element ? event.target : undefined;
  return element?.closest<HTMLElement>("[data-dispatch-anchor-id]")?.dataset.dispatchAnchorId;
}

/** Adds mapped live-anchor decorations and connects them to margin cards. */
export function anchorDecorationExtension(handlers: AnchorInteractionHandlers): Extension {
  return [
    anchorDecorations,
    EditorView.domEventHandlers({
      mousedown(event) {
        const id = anchorIdAt(event);
        if (id !== undefined) {
          handlers.onSelect(id);
        }
        return false;
      },
      mouseout(event) {
        if (anchorIdAt(event) !== undefined) {
          handlers.onHover(undefined);
        }
        return false;
      },
      mouseover(event) {
        handlers.onHover(anchorIdAt(event));
        return false;
      },
    }),
  ];
}
