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
import { SNOOZE_PRESETS } from "./snooze";

/**
 * The bar above the bands while rows are marked: how many, one snooze for all of them, and the
 * way out. The pick goes up to the Inbox, which owns the write and hands back what it is doing,
 * because the optimistic move can take every marked row off the view while the write is still in
 * the air - a bar that owned that state would be unmounted before its own refusal arrived.
 */
export function BulkSnoozeBar({
  barRef,
  count,
  onClear,
  onPick,
  onPickerFocus,
  pending,
  refusal,
}: {
  /** The bar's own element. The Inbox renders it in two places - beside the bands, and in the
   *  empty state a pick can leave behind - so the keymap reaches it through this rather than by
   *  searching the list, which the empty state is not inside. */
  barRef: RefObject<HTMLFieldSetElement | null>;
  /** How many asks a pick would write: the marked rows this view has on the page. */
  count: number;
  onClear: () => void;
  /** The reader's pick, as an RFC3339 moment. The Inbox writes it. */
  onPick: (until: string) => void;
  /** Where focus came from as the picker took it - `h` from a row, Tab, or a pointer with nothing
   *  behind it - so Escape knows which row is one level out. */
  onPickerFocus: (from: Element | null) => void;
  /** How many asks the write now in flight covers, 0 when none is. The bar says what it is doing
   *  from this rather than from the count, which the optimistic move empties under it. */
  pending: number;
  /** What the reader is told about a pick the server refused in part, or `undefined`. */
  refusal: string | undefined;
}): ReactNode {
  return (
    // A fieldset, like the view switch: the group's name comes from its legend, and `min-w-0`
    // holds back the UA's `min-inline-size: min-content` so a long refusal wraps inside it.
    <fieldset
      className={`flex min-w-0 flex-wrap items-center gap-2 rounded-xl border px-3 py-2 ${focusVisibleRing} outline-none focus-visible:ring-2 ${borderDefault}`}
      ref={barRef}
      tabIndex={-1}
    >
      <legend className="sr-only">Selected asks</legend>
      {/* In flight the count is the pick's, since the rows it snoozed have already left this
          view; settled, it is what a second pick would act on - the rows still marked. The
          badge stays the picker's name, so the state is said once. */}
      <span className={`text-sm ${textSecondaryOnCanvas}`}>
        {pending === 0 ? `${count} selected` : `Snoozing ${pending}…`}
      </span>
      <span className={badgeSelectWrapper}>
        <span aria-hidden="true" className={`${badgeSelectBadge} ${badgeLow.bg} ${badgeLow.text}`}>
          Snooze
        </span>
        {/* One pick at a time: a second while the first is in the air re-sends every id, and its
            answer can settle after the live one's and overwrite what the reader was told. */}
        <select
          aria-label="Snooze selected asks"
          className={badgeSelectOverlay}
          data-inbox-bulk-snooze=""
          disabled={pending !== 0}
          onChange={(event) => {
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
