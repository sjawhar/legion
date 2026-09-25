import type { InboxRow } from "../../api/types";

const HOUR_MS = 60 * 60 * 1000;

/** "Later today": far enough to clear the row off this morning's list, near enough to still be
 *  today. */
const LATER_TODAY_MS = 3 * HOUR_MS;

/** The hour a "Tomorrow" or "Next week" snooze returns on, in the reader's own timezone. */
const RETURN_HOUR = 9;

/**
 * "Until I clear it": a moment far enough out that only the reader's own un-snooze brings the
 * row back. The server takes any future RFC3339 timestamp and stores one column, so an
 * indefinite snooze is this timestamp rather than a second, nullable shape to carry everywhere.
 */
export const SNOOZE_INDEFINITE = "9999-12-31T00:00:00.000Z";

const INDEFINITE_AT = Date.parse(SNOOZE_INDEFINITE);

export interface SnoozePreset {
  readonly id: string;
  readonly label: string;
  /** The moment to snooze until, as RFC3339, computed when the reader picks it. */
  readonly until: (now: Date) => string;
}

/** `from` shifted by whole days and set to the return hour in the reader's timezone. */
function returnMorning(from: Date, days: number): string {
  const at = new Date(from);
  at.setDate(at.getDate() + days);
  at.setHours(RETURN_HOUR, 0, 0, 0);
  return at.toISOString();
}

/** Days from `from` to the next Monday; a Monday snoozes to the Monday after, never to today. */
function daysToNextMonday(from: Date): number {
  return 7 - ((from.getDay() + 6) % 7);
}

/** The last instant of `from`'s day in the reader's timezone - the latest moment a snooze can
 *  name and still be today. */
function endOfLocalDay(from: Date): Date {
  const at = new Date(from);
  at.setHours(23, 59, 59, 999);
  return at;
}

/** How much of today must remain for "Later today" to name a moment in it. The server takes
 *  only a moment still ahead when the request lands and allows no skew, so this covers the
 *  request's own latency and nothing more: a wider bound would start refusing evening picks
 *  that today has plenty of room for. */
const CLAMP_FLOOR_MS = 5 * 1000;

/** Three hours on, but never past the reader's own midnight: "Later today" that returned
 *  tomorrow would be a lie, so an evening pick lands at the last instant of today instead.
 *  In the last few seconds of the day the clamp would land inside the request's own latency,
 *  so the pick is the one "Tomorrow" would give. */
function laterToday(now: Date): string {
  const inThreeHours = new Date(now.getTime() + LATER_TODAY_MS);
  const lastInstant = endOfLocalDay(now);
  if (inThreeHours <= lastInstant) return inThreeHours.toISOString();
  return lastInstant.getTime() - now.getTime() >= CLAMP_FLOOR_MS
    ? lastInstant.toISOString()
    : returnMorning(now, 1);
}

/** The Snooze menu, in the order it is offered. */
export const SNOOZE_PRESETS: readonly SnoozePreset[] = [
  {
    id: "later-today",
    label: "Later today",
    until: laterToday,
  },
  { id: "tomorrow", label: "Tomorrow", until: (now) => returnMorning(now, 1) },
  {
    id: "next-week",
    label: "Next week",
    until: (now) => returnMorning(now, daysToNextMonday(now)),
  },
  { id: "indefinite", label: "Until I clear it", until: () => SNOOZE_INDEFINITE },
];

/**
 * Whether the reader has deferred this row and its moment is still ahead. The window is
 * absolute: nothing an agent does to the ask - a reply, a progress note, handing the turn back -
 * shortens it, so a deferred row stays deferred until it returns or the reader un-snoozes it.
 * Reads a row that carries no snooze at all (an issue header's `open_asks`) as not snoozed.
 */
export function isSnoozed(
  row: Partial<Pick<InboxRow, "snoozed_until">>,
  now: number = Date.now()
): boolean {
  const until = row.snoozed_until;
  if (until === null || until === undefined) return false;
  const at = Date.parse(until);
  return !Number.isNaN(at) && at > now;
}

/** A snooze with no return date is the one the reader ends themselves. */
export function isIndefinite(snoozedUntil: string): boolean {
  const at = Date.parse(snoozedUntil);
  return !Number.isNaN(at) && at >= INDEFINITE_AT;
}
