import { type ReactNode, useEffect } from "react";

import { QueryError } from "../../components/QueryError";
import { badgeLow, secondaryButtonCompact, textMutedOnCanvas } from "../../theme/classes";
import { badgeSelectBadge, badgeSelectOverlay, badgeSelectWrapper } from "../issue/badge-select";
import { Timestamp } from "../refs/Timestamp";
import { isIndefinite, SNOOZE_PRESETS } from "./snooze";
import { useAskSnooze } from "./useAskSnooze";

/**
 * The per-row defer control. On an ordinary row it is the "Snooze" badge with the same
 * invisible native `<select>` the priority and assignee pickers use, so the menu is one tap on
 * a phone and one keyboard interaction on a desktop; the picks are `snooze.ts`'s presets and
 * the moment is computed when the reader picks, not when the page rendered. On a row already
 * inside its window it is the return time and the one button that ends the snooze early -
 * nothing else does, which is the point: an agent answering a deferred ask leaves it deferred.
 *
 * A snooze folds its row out of the list, which would unmount this control and take its
 * `Snoozing…` and its refusal with it - so, exactly as `AssignToMe` does for the optimistic
 * move into Mine, it tells the Inbox while its write is live and the Inbox keeps the row
 * rendered until it settles. The fold is the feedback for a snooze that lands; a snooze the
 * server refuses rolls the row back with the reason still attached to it.
 *
 * `label` names the row for assistive tech; the Inbox passes the issue key or the document.
 */
export function SnoozeControl({
  askId,
  label,
  onLive,
  snoozedUntil,
}: {
  askId: string;
  label: string;
  /** Registers this row while its write is in flight or failed; unregistered once it settles
   *  cleanly or the control unmounts. */
  onLive: (askId: string, live: boolean) => void;
  /** The row's own snooze while its moment is still ahead; null on an ordinary row. */
  snoozedUntil: string | null;
}): ReactNode {
  const write = useAskSnooze(askId);
  const live = write.pending || write.failed;
  useEffect(() => {
    if (!live) return;
    onLive(askId, true);
    return () => onLive(askId, false);
  }, [askId, live, onLive]);
  const failure = write.failed ? (
    <QueryError
      message={write.error ?? `Could not snooze ${label}.`}
      onRetry={write.retry}
      retrying={write.pending}
    />
  ) : null;
  // Which band the row sits in is the optimistic update's answer and is right either way -
  // a snooze in flight already shows the return time it is saving. What the row cannot say is
  // which way the reader is going, so the in-flight label comes from the write's own variable
  // (a moment is a snooze, null is an un-snooze) and never from `snoozedUntil`, which by then
  // describes the destination. It is read only while a write is live: `variables` outlives the
  // write, so a settled row would otherwise keep reporting the last thing it did.
  const pendingLabel = !write.pending
    ? undefined
    : write.pendingUntil === null
      ? "Un-snoozing…"
      : "Snoozing…";

  if (snoozedUntil !== null) {
    return (
      <>
        <span className={`text-xs ${textMutedOnCanvas}`} data-inbox-snoozed-until={snoozedUntil}>
          {isIndefinite(snoozedUntil) ? (
            "Snoozed until you clear it"
          ) : (
            <>
              Back <Timestamp at={snoozedUntil} />
            </>
          )}
        </span>
        <button
          aria-label={`Un-snooze ${label}`}
          className={secondaryButtonCompact}
          disabled={write.pending}
          onClick={() => write.submit(null)}
          type="button"
        >
          {pendingLabel ?? "Un-snooze"}
        </button>
        {failure}
      </>
    );
  }

  return (
    <>
      <span className={badgeSelectWrapper}>
        <span aria-hidden="true" className={`${badgeSelectBadge} ${badgeLow.bg} ${badgeLow.text}`}>
          {pendingLabel ?? "Snooze"}
        </span>
        <select
          aria-label={`Snooze ${label}`}
          className={badgeSelectOverlay}
          onChange={(event) => {
            const preset = SNOOZE_PRESETS.find((option) => option.id === event.target.value);
            if (preset !== undefined) write.submit(preset.until(new Date()));
          }}
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
      {failure}
    </>
  );
}
