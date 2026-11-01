/**
 * Margin-mode mark events: when a host renders comment threads itself it passes `onMarkClick`
 * and/or `onMarkHover`; this plugin replaces the mark popover. A click inside a proof or
 * dispatch mark span reports the id of the mark it names (./record-mark-target.ts: the narrowest
 * record mark there) - the click is not consumed, so the caret still moves; hover reports that
 * id on enter and null on leave.
 */
import { $prose } from '@milkdown/kit/utils';
import { Plugin, PluginKey } from '@milkdown/kit/prose/state';
import { markIdAt } from './record-mark-target';

const markEventsKey = new PluginKey('dispatch-mark-events');

export interface MarkEventsOptions {
  onMarkClick?: (markId: string) => void;
  onMarkHover?: (markId: string | null) => void;
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
