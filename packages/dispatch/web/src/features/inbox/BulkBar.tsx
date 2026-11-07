import type { ReactNode, RefObject } from "react";

import {
  badgeLow,
  borderDefault,
  dangerText,
  focusVisibleRing,
  secondaryButtonBorder,
  secondaryButtonHoverBorder,
  secondaryButtonText,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { badgeSelectBadge, badgeSelectOverlay, badgeSelectWrapper } from "../issue/badge-select";
import type { InboxShow } from "../refs/routes";
import { SNOOZE_PRESETS } from "./snooze";

/**
 * The bar above the bands while rows are marked: how many, what one press does to all of them,
 * and the way out. A selection holds both kinds of row, so the bar offers each kind's own action
 * - **Snooze** for the asks among them, **Dismiss** for the threads - and neither is drawn while
 * the selection holds none of its kind.
 *
 * The press goes up to the Inbox, which owns both writes and hands back what it is doing, because
 * the optimistic move can take every marked row off the view while the write is still in the air
 * - a bar that owned that state would be unmounted before its own refusal arrived.
 */
export function BulkBar({
  all,
  askCount,
  barRef,
  onClear,
  onDismiss,
  onPick,
  onPickerFocus,
  onSelectAll,
  pending,
  refusal,
  show,
  threadCount,
}: {
  /** Whether the reader has selected every row the list's narrowing matches, the ones a fold or
   *  a page is holding back included: the bar says so rather than counting what it can see. */
  all: boolean;
  /** How many open asks the selection holds, which is what **Snooze** would defer. */
  askCount: number;
  /** The bar's own element, which the keymap reaches through this: Escape from the picker
   *  focuses the bar itself when a press has left no row to return to. */
  barRef: RefObject<HTMLFieldSetElement | null>;
  onClear: () => void;
  onDismiss: () => void;
  /** The reader's pick, as an RFC3339 moment. The Inbox writes it. */
  onPick: (until: string) => void;
  /** Where focus came from as the picker took it - `h` from a row, Tab, or a pointer with nothing
   *  behind it - so Escape knows which row is one level out. */
  onPickerFocus: (from: Element | null) => void;
  onSelectAll: () => void;
  /** How many asks the snooze now in flight covers, 0 when none is. The bar says what it is doing
   *  from this rather than from the count, which the optimistic move empties under it. */
  pending: number;
  /** What the reader is told about a pick the server refused in part, or `undefined`. */
  refusal: string | undefined;
  /** Which view the rows are in: in the Dismissed view the threads' press puts them back. */
  show: InboxShow;
  /** How many thread rows the selection holds, which is what **Dismiss** would take out. */
  threadCount: number;
}): ReactNode {
  const dismissLabel = show === "dismissed" ? "Move to Inbox" : "Dismiss";
  const selected = askCount + threadCount;
  // In flight the count is the press's, since the rows it moved have already left this view;
  // settled, it is what a second press would act on - the rows still marked.
  const state =
    pending !== 0 ? `Snoozing ${pending}…` : all ? "Every row selected" : `${selected} selected`;
  return (
    // A fieldset: the group's name comes from its legend, and `min-w-0` holds back the UA's
    // `min-inline-size: min-content` so a long refusal wraps inside it.
    <fieldset
      className={`flex min-w-0 flex-wrap items-center gap-2 rounded-xl border px-3 py-2 ${focusVisibleRing} outline-none focus-visible:ring-2 ${borderDefault}`}
      ref={barRef}
      tabIndex={-1}
    >
      <legend className="sr-only">Selected rows</legend>
      <span className={`text-sm ${textSecondaryOnCanvas}`}>{state}</span>
      {/* The picker stays while its own write is out: the optimistic fold takes every marked ask
          off this view, and a picker unmounted under the reader's hand drops their focus. */}
      {askCount === 0 && pending === 0 ? null : (
        <span className={badgeSelectWrapper}>
          <span
            aria-hidden="true"
            className={`${badgeSelectBadge} ${badgeLow.bg} ${badgeLow.text}`}
          >
            Snooze
          </span>
          {/* One pick at a time: a second while the first is in the air re-sends every id, and its
              answer can settle after the live one's and overwrite what the reader was told. It is
              `aria-disabled` rather than `disabled` because the browser takes focus off a control
              that disables itself under it - dropping the reader on the document mid-write, with
              no Escape to leave and no way back to the bar. */}
          <select
            aria-disabled={pending !== 0}
            aria-label="Snooze selected asks"
            className={badgeSelectOverlay}
            data-inbox-bulk-snooze=""
            onChange={(event) => {
              if (pending !== 0) return;
              const preset = SNOOZE_PRESETS.find((option) => option.id === event.target.value);
              if (preset !== undefined) onPick(preset.until(new Date()));
            }}
            onFocus={(event) => onPickerFocus(event.relatedTarget)}
            value=""
          >
            <option value="">Snooze…</option>
            {SNOOZE_PRESETS.map((preset) => (
              <option key={preset.id} value={preset.id}>
                {preset.label}
              </option>
            ))}
          </select>
        </span>
      )}
      {threadCount === 0 ? null : (
        <button
          aria-label={`${dismissLabel} selected threads`}
          className={`min-h-11 shrink-0 rounded-lg px-2 py-1 text-xs font-medium md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          data-inbox-bulk-dismiss=""
          onClick={onDismiss}
          type="button"
        >
          {dismissLabel}
        </button>
      )}
      {all ? null : (
        <button
          className={`min-h-11 shrink-0 rounded-lg px-2 py-1 text-xs font-medium md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
          data-inbox-select-all=""
          onClick={onSelectAll}
          type="button"
        >
          Select all
        </button>
      )}
      <button
        className={`min-h-11 shrink-0 rounded-lg px-2 py-1 text-xs font-medium md:min-h-8 ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder}`}
        onClick={onClear}
        type="button"
      >
        Clear selection
      </button>
      {refusal === undefined ? null : (
        <p className={`basis-full text-sm ${dangerText}`} role="alert">
          {refusal}
        </p>
      )}
    </fieldset>
  );
}
