// @ts-nocheck — verbatim proof-sdk source. The fork emits this tree's declarations with
// `noCheck` (its tsconfig.lib.json), so it has never type-checked; see AGENTS.md.
/**
 * Margin-mode mark events: when a host renders comment threads itself it passes `onMarkClick`
 * and/or `onMarkHover`; this plugin replaces the mark popover. A click inside a proof or
 * dispatch mark span reports the span's id (the click is not consumed, so the caret still
 * moves); hover reports the id on enter and null on leave.
 */
import { $prose } from '@milkdown/kit/utils';
import { Plugin, PluginKey } from '@milkdown/kit/prose/state';

const markEventsKey = new PluginKey('dispatch-mark-events');
const MARK_SELECTOR = 'span[data-proof][data-id], span[data-dispatch][data-id]';

export interface MarkEventsOptions {
  onMarkClick?: (markId: string) => void;
  onMarkHover?: (markId: string | null) => void;
}

function markIdAt(view: { dom: HTMLElement }, target: EventTarget | null): string | null {
  const element = target instanceof Element ? target : null;
  const span = element?.closest<HTMLElement>(MARK_SELECTOR) ?? null;
  if (!span || !view.dom.contains(span) || !span.dataset.id) return null;
  return span.dataset.id;
}

export const dispatchMarkEventsPlugin = (options: MarkEventsOptions) =>
  $prose(
    () =>
      new Plugin({
        key: markEventsKey,
        props: {
          handleClick(view, _pos, event) {
            const id = markIdAt(view, event.target);
            if (id && options.onMarkClick) options.onMarkClick(id);
            return false;
          },
          handleDOMEvents: {
            mouseover(view, event) {
              const id = markIdAt(view, event.target);
              if (id && options.onMarkHover) options.onMarkHover(id);
              return false;
            },
            mouseout(view, event) {
              const from = markIdAt(view, event.target);
              const relatedTarget = event instanceof MouseEvent ? event.relatedTarget : null;
              const to = markIdAt(view, relatedTarget);
              if (from && from !== to && options.onMarkHover) options.onMarkHover(null);
              return false;
            },
          },
        },
      }),
  );
