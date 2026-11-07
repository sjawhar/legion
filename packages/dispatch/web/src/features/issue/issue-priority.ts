import type { IssuePriority } from "../../api/types";

/** `value` as a priority. The route table carries it as a plain number; Dispatch writes 0 to 3
 *  and nothing else, and a number outside that reads as no priority rather than as a lane and a
 *  label no surface has.
 *
 *  It stands here rather than beside the write hook (`useIssuePriority.ts`) because every
 *  surface that only draws a priority reads it - the Inbox's row rule among them - and that
 *  hook's module reaches the query client, which reaches those surfaces back. */
export function issuePriority(value: number | null | undefined): IssuePriority | null {
  return value === 0 || value === 1 || value === 2 || value === 3 ? value : null;
}
